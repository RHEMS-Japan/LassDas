// Package attendant drives the cards orchestration: one resident process
// that claims queued runs, prepares their shared run directory, keeps each
// delivery's stage-card chain aligned with reality, and owns every ledger
// transition — questions and terminal reports included. The stage cards
// themselves never touch the ledger; their whole contract is artifacts and
// exit codes, which is what keeps a failed stage a fact the attendant
// reads, never a state a worker claims.
package attendant

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
)

// Logger is the narrow logging surface the sync needs; it is also exactly
// the logger shape the runner pipeline takes.
type Logger interface {
	Info(string, ...any)
	Error(string, ...any)
}

// failedCardStatuses are the card states the attendant terminalizes.
// Measured against the fork's status vocabulary (buddy review): timeouts,
// spawn failures, crashes and breaker trips are events and run outcomes,
// not statuses — every one of them lands the card in "blocked" (below the
// retry threshold the kanban re-dispatches once first, which is why the
// stages resume idempotently). Detection leads immediately to a sealed
// terminal report on the tracker and the chain's retirement; the board is
// never the failure record. A card an operator moved to "triage" is a
// deliberate human lane and is left waiting.
var failedCardStatuses = map[string]bool{
	"blocked": true,
}

// chainOwnerRunID is the attendant's per-delivery claim identity. The
// ledger seals an owner into every claim and the terminal report must
// present the same one, possibly many ticks and process restarts later, so
// the identity is derived, stateless, from the delivery id itself.
func chainOwnerRunID(deliveryID string) int64 {
	digest := sha256.Sum256([]byte(deliveryID))
	value := int64(binary.BigEndian.Uint64(digest[:8]) &^ (1 << 63)) // #nosec G115 -- top bit cleared; non-negative by construction.
	if value == 0 {
		value = 1
	}
	return value
}

// SyncChains is the cards orchestration's per-tick protocol: queued runs
// are claimed, prepared and given their first round; claimed runs have
// their chain healed, their finished publish reported, and their failures
// classified from artifacts into regeneration, a question, or an honest
// terminal.
func SyncChains(ctx context.Context, config runtime.Config, services *runtime.Services, hermes *runtime.Hermes, logger Logger) error {
	runs, err := services.Store.ScanRuns(ctx)
	if err != nil {
		return err
	}
	if len(runs) == 0 {
		return nil
	}
	tasks, err := hermes.ListBoardTasks(ctx)
	if err != nil {
		return err
	}
	for _, run := range runs {
		view := chainViewFor(tasks, run.DeliveryID)
		switch run.State {
		case "queued":
			if err := startQueuedRun(ctx, config, services, hermes, run, view, logger); err != nil {
				logger.Error("chain start failed", "run", run.RunID, "error", err.Error())
			}
		case "claimed":
			if err := advanceClaimedRun(ctx, config, services, hermes, run, view, logger); err != nil {
				logger.Error("chain advance failed", "run", run.RunID, "error", err.Error())
			}
		case "terminal":
			// The run itself is closed; what may remain is the debug
			// role's post-merge observation, or the v2 delivery
			// continuation (config makes the two mutually exclusive).
			if err := syncE2E(ctx, config, services, hermes, run, tasks, logger); err != nil {
				logger.Error("e2e sync failed", "run", run.RunID, "error", err.Error())
			}
			if err := syncDeliver(ctx, config, services, hermes, run, tasks, logger); err != nil {
				logger.Error("deliver sync failed", "run", run.RunID, "error", err.Error())
			}
		default:
			// awaiting_answer and the question-sealed report flavors
			// belong to the reception tick's own machinery.
		}
	}
	return nil
}

// chainView is one delivery's chain as the board tells it: the current
// (highest) round's cards by stage, and every living chain card of any
// round for retirement sweeps.
type chainView struct {
	round int
	cards map[string]runtime.BoardTask
	all   []runtime.BoardTask
}

func chainViewFor(tasks []runtime.BoardTask, deliveryID string) chainView {
	view := chainView{cards: map[string]runtime.BoardTask{}}
	rounds := map[int]map[string]runtime.BoardTask{}
	for _, task := range tasks {
		delivery, stage, round, ok := runtime.ParseChainCardKey(task.IdempotencyKey)
		if !ok || delivery != deliveryID || task.Status == "archived" {
			continue
		}
		view.all = append(view.all, task)
		if rounds[round] == nil {
			rounds[round] = map[string]runtime.BoardTask{}
		}
		rounds[round][stage] = task
		if round > view.round {
			view.round = round
		}
	}
	if view.round > 0 {
		view.cards = rounds[view.round]
	}
	return view
}

func (v chainView) existingKeys(deliveryID string) map[string]runtime.BoardTask {
	existing := make(map[string]runtime.BoardTask, len(v.cards))
	for stage, task := range v.cards {
		existing[runtime.ChainCardKey(deliveryID, stage, v.round)] = task
	}
	return existing
}

func runDirectory(config runtime.Config, deliveryID string) string {
	return runtime.RunDirectory(config.Chain, deliveryID)
}

func readTargetToken(config runtime.Config) (string, error) {
	raw, err := os.ReadFile(config.Chain.TargetTokenPath)
	if err != nil {
		return "", errors.New("target token unreadable")
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", errors.New("target token file is empty")
	}
	return token, nil
}

// startQueuedRun claims one queued run by name, prepares its run directory
// from scratch and creates the first round — or ends the run honestly when
// preparation itself decides it (an intake rejection, a readiness stop, a
// readiness question). A queued run with leftover chain cards is a resumed
// or crash-recovered one; the old chain leaves dispatch entirely before the
// fresh attempt, whose preparation also clears the directory.
func startQueuedRun(
	ctx context.Context,
	config runtime.Config,
	services *runtime.Services,
	hermes *runtime.Hermes,
	run state.RunOverview,
	view chainView,
	logger Logger,
) error {
	for _, task := range view.all {
		if err := hermes.Archive(ctx, task.ID); err != nil {
			return fmt.Errorf("stale chain card %s: %w", task.ID, err)
		}
	}
	runDir := runDirectory(config, run.DeliveryID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return err
	}
	now := time.Now().UTC()
	envelope, disposition, err := services.Store.Pull(ctx, hook.PullClaimRequest{
		SpaceKey:            config.Tracker.SpaceKey,
		ProjectID:           config.Tracker.ProjectID,
		ProjectKey:          config.Tracker.ProjectKey,
		AllowedCreatorID:    config.Tracker.AllowedCreatorID,
		AllowedActivityType: config.Tracker.AllowedActivityType,
		RunID:               run.RunID,
		Target:              config.Target(),
		Owner:               config.Owner(chainOwnerRunID(run.DeliveryID)),
		IssuedAt:            now,
		ClaimedAt:           now,
		ClockSkew:           2 * time.Minute,
	})
	if err != nil {
		return fmt.Errorf("pull failed: %w", err)
	}
	if disposition != hook.PullAcquired {
		logger.Info("queued run not claimable this tick", "run", run.RunID, "disposition", string(disposition))
		return nil
	}
	// A stop request already on the ticket cancels the run before any model
	// work is spent. The check sits before the intake pipeline on purpose:
	// when the comment listing fails here, the retried tick has lost nothing,
	// whereas a failure after the readiness gate would re-run the whole
	// assessment on every retry. A stop arriving later is honoured at the
	// next round boundary.
	stopped, err := stopRequested(ctx, services.Backlog, config.Tracker.AllowedCreatorID, envelope.Snapshot.IssueID)
	if err != nil {
		return fmt.Errorf("stop check before intake: %w", err)
	}
	if stopped {
		terminal := runner.NewTerminal(config, services, envelope, chainOwnerRunID(run.DeliveryID), runDir, logger)
		return terminal.Report(ctx, hook.TerminalCancelled, runner.Outcome{Code: hook.TerminalCancelled}, "")
	}
	token, err := readTargetToken(config)
	if err != nil {
		return err
	}
	pipeline := &runner.Pipeline{
		Config: config, Services: services, Envelope: envelope,
		Workspace: runDir, TargetToken: token, Logger: logger,
	}
	_, outcome, runErr := pipeline.PrepareChainRun(ctx)
	terminal := runner.NewTerminal(config, services, envelope, chainOwnerRunID(run.DeliveryID), runDir, logger)
	if outcome.QuestionDecisionPath != "" {
		return terminal.AskQuestion(ctx, outcome.QuestionDecisionPath)
	}
	if runErr != nil || outcome.Code != "" {
		if runErr != nil {
			logger.Error("chain preparation error", "run", run.RunID, "error", runErr.Error())
		}
		if outcome.Code == "" {
			outcome.Code = hook.TerminalInternalFailed
		}
		return terminal.Report(ctx, outcome.Code, outcome, pipeline.Repository())
	}
	// Readiness passed. Post the implementation-plan notice — a notice, not
	// a gate: a failed post is logged and the run continues.
	if !services.Tick.PostPlanComment(ctx, envelope.DeliveryID, hook.PlanCommentContent(envelope.Snapshot.RunID, loadPlanFacts(runDir))) {
		logger.Error("plan notice not posted; run continues", "run", run.RunID)
	}
	// One opportunistic re-check before the first cards, for a stop request
	// that arrived while the gate was running. Best-effort on purpose: a
	// listing failure here must not send the whole gate into a retry loop,
	// so it logs and proceeds — the fail-closed checks are the claim-time
	// one above and every round boundary after this.
	if stopped, stopErr := stopRequested(ctx, services.Backlog, config.Tracker.AllowedCreatorID, envelope.Snapshot.IssueID); stopErr != nil {
		logger.Error("stop re-check unreadable; proceeding", "run", run.RunID, "error", stopErr.Error())
	} else if stopped {
		repository, repositoryErr := readField(runDir, "ticket-draft.json", "repository")
		if repositoryErr != nil {
			repository = ""
		}
		return terminal.Report(ctx, hook.TerminalCancelled, runner.Outcome{Code: hook.TerminalCancelled}, repository)
	}
	if err := pipeline.RenderImplementInstruction(ctx, 1); err != nil {
		return err
	}
	terminalCard, err := runtime.EnsureChain(ctx, hermes, config.Chain, nil, run.DeliveryID, run.RunID, run.Summary, 1)
	if err != nil {
		return err
	}
	logger.Info("chain created", "run", run.RunID, "round", 1, "terminal_card", terminalCard)
	return nil
}

// advanceClaimedRun keeps one in-flight chain honest: heal missing cards,
// report a finished publish, and classify any failed card from the sealed
// artifacts — a revise regenerates the next round, an impasse question goes
// to the requester, everything else becomes an honest terminal. A claim
// with no chain at all is a preparation that died between claiming and
// creating cards; the run goes back to the queue and restarts cleanly.
func advanceClaimedRun(
	ctx context.Context,
	config runtime.Config,
	services *runtime.Services,
	hermes *runtime.Hermes,
	run state.RunOverview,
	view chainView,
	logger Logger,
) error {
	if view.round == 0 {
		logger.Info("claimed run has no chain; requeueing", "run", run.RunID)
		return services.Store.RecoverLostClaim(ctx, run.Key, run.ClaimedAt, time.Now().UTC())
	}
	runDir := runDirectory(config, run.DeliveryID)
	envelope, err := readEnvelope(runDir, run.DeliveryID)
	if err != nil {
		// Without the sealed envelope nothing can be reported; a fresh
		// attempt rebuilds the directory from the ledger's own copy.
		logger.Error("run envelope unreadable; requeueing", "run", run.RunID, "error", err.Error())
		for _, task := range view.all {
			if archiveErr := hermes.Archive(ctx, task.ID); archiveErr != nil {
				return archiveErr
			}
		}
		return services.Store.RecoverLostClaim(ctx, run.Key, run.ClaimedAt, time.Now().UTC())
	}
	if _, err := runtime.EnsureChain(ctx, hermes, config.Chain, view.existingKeys(run.DeliveryID), run.DeliveryID, run.RunID, run.Summary, view.round); err != nil {
		return err
	}
	if task, ok := view.cards[runtime.StagePublish]; ok && task.Status == "done" {
		return reportChainSuccess(ctx, config, services, envelope, run, logger)
	}
	for _, stage := range runtime.ChainStages(config.Chain) {
		task, ok := view.cards[stage.Name]
		if !ok || !failedCardStatuses[task.Status] {
			continue
		}
		// The cards mode never parks a card for a question, so a
		// needs_input block is as terminal as any other — logged with its
		// kind so an unexpected one is diagnosable from the record.
		logger.Info("chain card failed", "run", run.RunID, "stage", stage.Name,
			"status", task.Status, "block_kind", task.BlockKind)
		return handleChainFailure(ctx, config, services, hermes, envelope, run, view, stage.Name, logger)
	}
	return nil
}

func reportChainSuccess(
	ctx context.Context,
	config runtime.Config,
	services *runtime.Services,
	envelope hook.DispatchEnvelope,
	run state.RunOverview,
	logger Logger,
) error {
	runDir := runDirectory(config, run.DeliveryID)
	raw, err := os.ReadFile(filepath.Join(runDir, runner.ChainOutcomeFile))
	if err != nil {
		return errors.New("chain outcome artifact unreadable")
	}
	var outcome runner.ChainOutcome
	if err := json.Unmarshal(raw, &outcome); err != nil || outcome.Stage < 1 {
		return errors.New("chain outcome artifact invalid")
	}
	repository, err := readField(runDir, "ticket-draft.json", "repository")
	if err != nil {
		return errors.New("run repository unreadable")
	}
	terminal := runner.NewTerminal(config, services, envelope, chainOwnerRunID(run.DeliveryID), runDir, logger)
	if err := terminal.Report(ctx, hook.TerminalSuccess, runner.Outcome{Stage: outcome.Stage, Evidence: outcome.Evidence}, repository); err != nil {
		return err
	}
	logger.Info("chain delivered", "run", run.RunID, "round", outcome.Stage)
	return nil
}

func handleChainFailure(
	ctx context.Context,
	config runtime.Config,
	services *runtime.Services,
	hermes *runtime.Hermes,
	envelope hook.DispatchEnvelope,
	run state.RunOverview,
	view chainView,
	stageName string,
	logger Logger,
) error {
	runDir := runDirectory(config, run.DeliveryID)
	terminal := runner.NewTerminal(config, services, envelope, chainOwnerRunID(run.DeliveryID), runDir, logger)
	action, code := classifyChainFailure(stageName, func() (string, error) {
		return readField(runDir, fmt.Sprintf("history/stage-%d/decision.json", view.round), "outcome")
	}, func() (string, error) {
		return readField(runDir, "history/question/decision.json", "outcome")
	})
	switch action {
	case actionRegenerate:
		limit, limitErr := consumerMaxStages(config.ConsumerConfigPath)
		if limitErr != nil {
			return limitErr
		}
		stopped, stopErr := stopRequested(ctx, services.Backlog, config.Tracker.AllowedCreatorID, envelope.Snapshot.IssueID)
		if stopErr != nil {
			return fmt.Errorf("stop check before round %d: %w", view.round+1, stopErr)
		}
		switch {
		case stopped:
			// The requester asked the run to stop: finished cards stay
			// finished, no next round is created, and the run ends honestly.
			code = hook.TerminalCancelled
		case view.round < limit:
			return regenerateRound(ctx, hermes, config, run, view, logger)
		default:
			// The decide verb converts a final-round revise into nonconverged;
			// a revise at the limit means the artifacts and the configuration
			// disagree, and the run ends honestly instead of looping.
			code = hook.TerminalModelFailed
		}
	case actionAskQuestion:
		if err := terminal.AskQuestion(ctx, filepath.Join(runDir, "history/question/decision.json")); err != nil {
			return err
		}
		return archiveChain(ctx, hermes, view.all)
	}
	// The failure report carries the same round record a delivery would
	// have (#10); composition failure never blocks the report.
	if _, err := os.Stat(filepath.Join(runDir, "history", "stage-1")); err == nil {
		pipeline := &runner.Pipeline{Config: config, Workspace: runDir, Logger: logger}
		_ = pipeline.EnsureTrail(ctx)
		// The publish card records why a delivery stopped in its own
		// process; the recomposed trail would silently drop it otherwise.
		pipeline.AttachDeliveryStopReason()
	}
	repository, err := readField(runDir, "ticket-draft.json", "repository")
	if err != nil {
		repository = ""
	}
	if err := terminal.Report(ctx, code, runner.Outcome{Code: code}, repository); err != nil {
		return err
	}
	logger.Info("chain terminalized", "run", run.RunID, "stage", stageName, "code", string(code))
	return archiveChain(ctx, hermes, view.all)
}

// failureAction is what a failed card's artifacts call for.
type failureAction int

const (
	// actionReport ends the run with the accompanying terminal code.
	actionReport failureAction = iota
	// actionRegenerate retires the round's remnant and starts the next.
	actionRegenerate
	// actionAskQuestion routes the sealed impasse question to the requester.
	actionAskQuestion
)

// classifyChainFailure reads a failed card into an action. The card's state
// is only the alarm; the sealed artifacts are the classification: a decided
// revise regenerates, a nonconverged with a sealed question asks, a decided
// converge that still failed means the deterministic validation refused,
// and anything undecided is the machinery's own death.
func classifyChainFailure(stageName string, decision, question func() (string, error)) (failureAction, hook.TerminalCode) {
	switch stageName {
	case runtime.StagePublish:
		return actionReport, hook.TerminalReleaseFailed
	case runtime.StageValidate:
		outcome, err := decision()
		if err != nil {
			return actionReport, hook.TerminalModelFailed
		}
		switch outcome {
		case "revise":
			return actionRegenerate, hook.TerminalModelFailed
		case "nonconverged":
			if asked, askErr := question(); askErr == nil && asked == "clarification_required" {
				return actionAskQuestion, hook.TerminalNonconverged
			}
			return actionReport, hook.TerminalNonconverged
		case "converged":
			return actionReport, hook.TerminalValidationFailed
		default:
			return actionReport, hook.TerminalModelFailed
		}
	default:
		return actionReport, hook.TerminalModelFailed
	}
}

// regenerateRound retires the failed round's undone remnant and creates the
// next round's chain on the same run directory: the implementer continues
// from the tree it already changed, told what the judges objected to.
func regenerateRound(
	ctx context.Context,
	hermes *runtime.Hermes,
	config runtime.Config,
	run state.RunOverview,
	view chainView,
	logger Logger,
) error {
	for _, task := range view.all {
		if task.Status == "done" {
			continue
		}
		if err := hermes.Archive(ctx, task.ID); err != nil {
			return err
		}
	}
	pipeline := &runner.Pipeline{Config: config, Workspace: runDirectory(config, run.DeliveryID), Logger: logger}
	if err := pipeline.RenderImplementInstruction(ctx, view.round+1); err != nil {
		return err
	}
	terminalCard, err := runtime.EnsureChain(ctx, hermes, config.Chain, nil, run.DeliveryID, run.RunID, run.Summary, view.round+1)
	if err != nil {
		return err
	}
	logger.Info("round regenerated", "run", run.RunID, "round", view.round+1, "terminal_card", terminalCard)
	return nil
}

func archiveChain(ctx context.Context, hermes *runtime.Hermes, tasks []runtime.BoardTask) error {
	for _, task := range tasks {
		if err := hermes.Archive(ctx, task.ID); err != nil {
			return err
		}
	}
	return nil
}

// readEnvelope reloads the sealed dispatch envelope the preparation left in
// the run directory, and refuses one that names another delivery.
func readEnvelope(runDir, deliveryID string) (hook.DispatchEnvelope, error) {
	raw, err := os.ReadFile(filepath.Join(runDir, "ticket-envelope.json"))
	if err != nil {
		return hook.DispatchEnvelope{}, errors.New("envelope unreadable")
	}
	var envelope hook.DispatchEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return hook.DispatchEnvelope{}, errors.New("envelope invalid")
	}
	if envelope.DeliveryID != deliveryID {
		return hook.DispatchEnvelope{}, errors.New("envelope names another delivery")
	}
	return envelope, nil
}

// maxWorkspaceFieldBytes bounds every artifact read here; the files are
// small sealed JSON records.
const maxWorkspaceFieldBytes = 1 << 20

// readField walks one string field out of a JSON artifact under the run
// directory.
func readField(runDir, name string, keys ...string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(runDir, name))
	if err != nil {
		return "", err
	}
	if len(raw) > maxWorkspaceFieldBytes {
		return "", errors.New("artifact too large")
	}
	var current any
	if err := json.Unmarshal(raw, &current); err != nil {
		return "", err
	}
	for _, key := range keys {
		object, ok := current.(map[string]any)
		if !ok {
			return "", errors.New("artifact field path invalid")
		}
		current = object[key]
	}
	value, ok := current.(string)
	if !ok {
		return "", errors.New("artifact field is not a string")
	}
	return value, nil
}

// consumerMaxStages reads the round limit out of the consumer configuration
// — the same value the decide verb enforces, so the attendant's
// regeneration and the kernel's nonconverged conversion stay one number.
func consumerMaxStages(consumerConfigPath string) (int, error) {
	raw, err := os.ReadFile(consumerConfigPath)
	if err != nil {
		return 0, errors.New("consumer config unreadable")
	}
	var parsed struct {
		MaxStages int `json:"max_stages"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || parsed.MaxStages < 1 || parsed.MaxStages > 5 {
		return 0, errors.New("consumer config max_stages invalid")
	}
	return parsed.MaxStages, nil
}
