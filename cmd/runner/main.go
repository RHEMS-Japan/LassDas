// Command runner is the stage worker of the single-pod constitution: the
// binary a Hermes profile's worker.command points at. Dispatched with the
// standard kanban worker environment, it claims the queued envelope from
// the local ledger, drives the stage pipeline (internal/runner), and ends
// its run the way the runtime design defines:
//
//   - delivered or failed → it reports the terminal outcome to the tracker
//     itself (the same report service the Lambda wired), then exits 0 for
//     success or 1 for failure — the Hermes supervisor translates that into
//     complete/block on the card;
//   - a question → it posts the question through the question service,
//     blocks its own card through the canonical CLI (awaiting-answer:…),
//     and exits 0 — the already-blocked card keeps its own word.
//
// It never touches kanban.db, never talks HTTP to itself, and holds no
// state outside the ledger and the task workspace.
//
// Not carried over from the workflow: the operator-facing model-preflight
// operation (a smoke probe of the model endpoints). Probing is an operator
// action against the pod, not a card path.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "chain-stage" {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		if err := runChainStage(ctx, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "runner:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "e2e-check" {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		if err := runE2ECheck(ctx, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "runner:", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "runner:", err)
		os.Exit(1)
	}
}

func run() error {
	flags := flag.NewFlagSet("runner", flag.ContinueOnError)
	configPath := flags.String("config", os.Getenv("LASSDAS_RUNTIME_CONFIG"), "runtime.json path")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	taskID := os.Getenv("HERMES_KANBAN_TASK")
	workspace := os.Getenv("HERMES_KANBAN_WORKSPACE")
	rawRunID := os.Getenv("HERMES_KANBAN_RUN_ID")
	if taskID == "" || workspace == "" || rawRunID == "" {
		return errors.New("HERMES_KANBAN_TASK, HERMES_KANBAN_WORKSPACE and HERMES_KANBAN_RUN_ID are required (dispatched by the Hermes kanban only)")
	}
	hermesRunID, err := strconv.ParseInt(rawRunID, 10, 64)
	if err != nil || hermesRunID <= 0 {
		return errors.New("HERMES_KANBAN_RUN_ID is not a positive integer")
	}
	config, err := runtime.Load(*configPath)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	services, err := runtime.BuildServices(config, logger)
	if err != nil {
		return err
	}
	defer func() { _ = services.Close() }()

	// SIGTERM from the supervisor cancels the context; every stage child
	// runs in its own process group wired to that cancel, so the whole
	// tree dies with the runner instead of orphaning a model call.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	now := time.Now().UTC()
	envelope, disposition, err := services.Store.Pull(ctx, hook.PullClaimRequest{
		SpaceKey:            config.Tracker.SpaceKey,
		ProjectID:           config.Tracker.ProjectID,
		ProjectKey:          config.Tracker.ProjectKey,
		AllowedCreatorID:    config.Tracker.AllowedCreatorID,
		AllowedActivityType: config.Tracker.AllowedActivityType,
		Target:              config.Target(),
		Owner:               config.Owner(hermesRunID),
		IssuedAt:            now,
		ClaimedAt:           now,
		ClockSkew:           2 * time.Minute,
	})
	if err != nil {
		return fmt.Errorf("pull failed: %w", err)
	}
	switch disposition {
	case hook.PullAcquired:
	case hook.PullEmpty:
		// The card exists but the ledger has no waiting run: reception and
		// the board disagree. Fail loudly — the ordinary crash/retry path
		// and the attendant's next sync surface it.
		return errors.New("no waiting run in the ledger for this card")
	default:
		return fmt.Errorf("pull disposition %s: another run owns this ticket", disposition)
	}

	// The destination credential leaves the process environment before any
	// stage child can inherit it; only the clone and the controller receive
	// it, explicitly.
	targetToken := os.Getenv("TARGET_GITHUB_TOKEN")
	_ = os.Unsetenv("TARGET_GITHUB_TOKEN")
	pipeline := &runner.Pipeline{
		Config: config, Services: services, Envelope: envelope,
		Workspace: workspace, TargetToken: targetToken, Logger: logger,
	}
	outcome, runErr := pipeline.Run(ctx)
	if runErr != nil {
		logger.Error("pipeline error", "error", runErr.Error())
	}

	terminal := runner.NewTerminal(config, services, envelope, hermesRunID, workspace, logger)
	if outcome.QuestionDecisionPath != "" {
		if err := terminal.AskQuestion(ctx, outcome.QuestionDecisionPath); err != nil {
			return fmt.Errorf("question could not be posted: %w", err)
		}
		hermes := runtime.NewHermes(config)
		if err := hermes.Block(ctx, taskID, "awaiting-answer:"+envelope.DeliveryID); err != nil {
			// The question is sealed but the card is still running. Exiting
			// 0 here would COMPLETE the card — a completed card is never
			// re-dispatched, so the adopted answer could never re-run.
			// Exiting non-zero makes the supervisor block the card itself,
			// and the attendant's sync normalizes it from there.
			return fmt.Errorf("question posted but card block failed: %w", err)
		}
		// Exit 0 after blocking our own card: the supervisor's complete
		// translation is a no-op on a card that is not running (fork
		// worker.command contract), so the block keeps its word.
		return nil
	}
	code := outcome.Code
	if code == "" {
		if runErr != nil {
			// A pipeline error with no terminal code is infrastructure
			// breakage, not a delivered success; report it as such rather
			// than attempting a success report the evidence gate refuses.
			code = hook.TerminalInternalFailed
		} else {
			code = hook.TerminalSuccess
		}
	}
	if code != hook.TerminalSuccess && pipeline.AttemptedImplementation() {
		// A failure report carries the same round-by-round record a delivery
		// would have carried (#10). Best effort only: a trail that cannot be
		// rendered must never stop the report itself.
		_ = pipeline.EnsureTrail(ctx)
	}
	if err := terminal.Report(ctx, code, outcome, pipeline.Repository()); err != nil {
		return fmt.Errorf("terminal report failed: %w", err)
	}
	if code != hook.TerminalSuccess {
		return fmt.Errorf("run ended %s", code)
	}
	return nil
}
