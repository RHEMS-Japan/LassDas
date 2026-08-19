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
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
)

func main() {
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

	ctx := context.Background()
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

	pipeline := &runner.Pipeline{
		Config: config, Services: services, Envelope: envelope,
		Workspace: workspace, Logger: logger,
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
			// The question is posted and sealed; the attendant's sync pass
			// re-blocks an escaped card. Log, do not fail the run.
			logger.Error("card block failed; attendant will recover", "error", err.Error())
		}
		return nil
	}
	code := outcome.Code
	if code == "" {
		code = hook.TerminalSuccess
	}
	if err := terminal.Report(ctx, code, outcome, pipeline.Repository()); err != nil {
		return fmt.Errorf("terminal report failed: %w", err)
	}
	if code != hook.TerminalSuccess {
		return fmt.Errorf("run ended %s", code)
	}
	return nil
}
