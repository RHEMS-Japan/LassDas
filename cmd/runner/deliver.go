package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"strings"

	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
)

// runDeliver is the v2 delivery cards' entry: dispatched by the kanban like
// a chain stage, it advances one delivery to the requested milestone
// (checks green / staging observed / production observed) and seals reports
// as artifacts. Like every stage, it never touches the ledger — reporting
// and the Go decision belong to the attendant.
func runDeliver(ctx context.Context, arguments []string) error {
	flags := flag.NewFlagSet("deliver", flag.ContinueOnError)
	configPath := flags.String("config", os.Getenv("LASSDAS_RUNTIME_CONFIG"), "runtime.json path")
	until := flags.String("until", "", "milestone: checks | staging-observed | production-observed")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	config, err := runtime.Load(*configPath)
	if err != nil {
		return err
	}
	workspace := os.Getenv("HERMES_KANBAN_WORKSPACE")
	if workspace == "" {
		return errors.New("HERMES_KANBAN_WORKSPACE is required (dispatched by the Hermes kanban only)")
	}
	raw, err := os.ReadFile(config.Chain.TargetTokenPath)
	if err != nil {
		return errors.New("target token unreadable")
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return errors.New("target token file is empty")
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	pipeline := &runner.Pipeline{Config: config, Workspace: workspace, TargetToken: token, Logger: logger}
	// The card is done when this returns; nothing is running.
	defer runner.ClearCurrentStep(workspace)
	err = pipeline.RunDeliver(ctx, *until)
	// Why this card is about to return non-zero, sealed where the tick
	// looks for it. Without it every delivery failure reads as one nobody
	// could name, and the ladder goes straight to waiting — so a volume
	// that filled would be waited on instead of swept, and a registry that
	// answered once would be waited on instead of reached again.
	pipeline.SealStageFailure(deliverStageOf(*until), err)
	return err
}

// deliverStageOf names the card behind a milestone. The verb is told how
// far to go; the record has to name which card wrote it, because that is
// what the tick reads it by.
func deliverStageOf(until string) string {
	switch until {
	case runner.DeliverUntilChecks:
		return runtime.DeliverStageChecks
	case runner.DeliverUntilStaging:
		return runtime.DeliverStageIntegrate
	case runner.DeliverUntilProduction:
		return runtime.DeliverStagePromote
	}
	return ""
}
