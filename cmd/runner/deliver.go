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
	if !config.OrchestrationCards() {
		return errors.New("deliver requires the cards orchestration")
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
	return pipeline.RunDeliver(ctx, *until)
}
