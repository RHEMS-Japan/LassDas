package runtime

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"automation.internal/ticket-ingress/internal/backlog"
	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/state"
)

// Services is the local wiring of everything the Lambda used to assemble:
// the same hook services over the local ledger and the tracker client, with
// no HTTP between the callers and the store. The attendant and the runner
// both build this from the same runtime.json, which is what keeps them on
// the same ledger, the same routes and the same identities.
type Services struct {
	Config   Config
	Store    *state.LocalStore
	Backlog  *backlog.Client
	Hook     *hook.Service
	Report   *hook.TerminalReportService
	Question *hook.QuestionReportService
	Tick     *hook.QuestionTickService
	Route    hook.ReportRouteConfig
	Logger   *slog.Logger
}

// BuildServices opens the ledger and wires the services. The Backlog API
// key arrives via BACKLOG_API_KEY — the one secret this constitution needs
// beyond the model keys the agent contract already handles.
func BuildServices(config Config, logger *slog.Logger) (*Services, error) {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}
	apiKey := os.Getenv("BACKLOG_API_KEY")
	if apiKey == "" {
		return nil, errors.New("BACKLOG_API_KEY is required")
	}
	hmacKey, err := localHMACKey(config.LedgerPath)
	if err != nil {
		return nil, err
	}
	store, err := state.NewLocalStore(config.LedgerPath)
	if err != nil {
		return nil, err
	}
	backlogClient, err := backlog.NewClient(backlog.Config{
		SpaceKey: config.Tracker.SpaceKey, Origin: config.Tracker.Origin, APIKey: apiKey,
		Timeout: 10 * time.Second, MaxResponseBytes: 1024 * 1024,
	}, nil)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("tracker client: %w", err)
	}
	target := config.Target()
	hookService, err := hook.NewService(hook.Config{
		SpaceKey: config.Tracker.SpaceKey, ProjectID: config.Tracker.ProjectID, ProjectKey: config.Tracker.ProjectKey,
		AllowedCreatorID: config.Tracker.AllowedCreatorID, AllowedActivityType: config.Tracker.AllowedActivityType,
		RequiredCategoryID: config.Tracker.RequiredCategoryID,
		RunMarker:          "Automation-Run-ID", ExpectedRunID: config.AutomationRunID, Target: target,
		MaxEnvelopeBytes: 60 * 1024,
	}, backlogClient, store, logger)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("hook service: %w", err)
	}
	route := hook.ReportRouteConfig{
		HMACKey: hmacKey, RepositoryID: config.Identity.RepositoryID,
		RepositorySHA256:  hook.HashIdentity(config.Identity.Repository),
		WorkflowRefSHA256: hook.HashIdentity(config.Identity.WorkflowRef),
		ExpectedRunID:     config.AutomationRunID,
		Destinations:      config.ReportDestinations,
		ClockSkew:         2 * time.Minute, LeaseDuration: 2 * time.Minute,
		SpaceKey: config.Tracker.SpaceKey, ProjectID: config.Tracker.ProjectID, ProjectKey: config.Tracker.ProjectKey,
		AllowedCreatorID: config.Tracker.AllowedCreatorID, AllowedActivityType: config.Tracker.AllowedActivityType,
		Target: target,
	}
	reportService, err := hook.NewTerminalReportService(route, store, backlogClient, logger)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("report service: %w", err)
	}
	questionService, err := hook.NewQuestionReportService(route, store, backlogClient, logger)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("question service: %w", err)
	}
	tickService, err := hook.NewQuestionTickService(route, store, backlogClient, reportService, hookService, logger)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("tick service: %w", err)
	}
	hookService.UseAnswerSignal(tickService)
	statuses := config.Tracker.BoardStatuses
	if statuses.Running > 0 || statuses.AwaitingAnswer > 0 || statuses.Delivered > 0 || statuses.NeedsAttention > 0 {
		projection, err := backlog.NewBoardProjection(backlogClient, backlog.BoardStatusMap{
			Running: statuses.Running, AwaitingAnswer: statuses.AwaitingAnswer,
			Delivered: statuses.Delivered, NeedsAttention: statuses.NeedsAttention,
		})
		if err != nil {
			_ = store.Close()
			return nil, fmt.Errorf("board projection: %w", err)
		}
		hookService.UseBoard(projection)
		questionService.UseBoard(projection)
		tickService.UseBoard(projection)
		reportService.UseBoard(projection)
	}
	return &Services{
		Config: config, Store: store, Backlog: backlogClient,
		Hook: hookService, Report: reportService, Question: questionService, Tick: tickService,
		Route: route, Logger: logger,
	}, nil
}

// Close releases the ledger.
func (s *Services) Close() error { return s.Store.Close() }

// localHMACKey loads (or first creates) the deployment's route key next to
// the ledger. The routes require a key as part of their identity even
// though no HTTP travels between the local callers and the store; it is
// generated once and never needs to be shown to anyone.
func localHMACKey(ledgerPath string) ([]byte, error) {
	path := filepath.Join(filepath.Dir(ledgerPath), "route.key")
	if raw, err := os.ReadFile(path); err == nil {
		decoded, decodeErr := hex.DecodeString(string(raw))
		if decodeErr != nil || hook.ValidatePullKey(decoded) != nil {
			return nil, errors.New("route.key exists but is not a valid key")
		}
		return decoded, nil
	}
	fresh := make([]byte, 32)
	if _, err := rand.Read(fresh); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(fresh)), 0o600); err != nil {
		return nil, err
	}
	return fresh, nil
}
