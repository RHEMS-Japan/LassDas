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

// runE2ECheck is the debug role's card entry: dispatched by the kanban like
// a chain stage, it waits for the human merge and the staging deployment,
// observes the deployed page and seals the verdict as artifacts. Like every
// stage, it never touches the ledger — reporting belongs to the attendant.
func runE2ECheck(ctx context.Context, arguments []string) error {
	flags := flag.NewFlagSet("e2e-check", flag.ContinueOnError)
	configPath := flags.String("config", os.Getenv("LASSDAS_RUNTIME_CONFIG"), "runtime.json path")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	config, err := runtime.Load(*configPath)
	if err != nil {
		return err
	}
	if !config.OrchestrationCards() {
		return errors.New("e2e-check requires the cards orchestration")
	}
	workspace := os.Getenv("HERMES_KANBAN_WORKSPACE")
	if workspace == "" {
		return errors.New("HERMES_KANBAN_WORKSPACE is required (dispatched by the Hermes kanban only)")
	}
	// The merge/deploy wait talks to GitHub through the controller, which
	// needs the destination credential — read from the operator file, never
	// the inherited environment, exactly as validate and publish do.
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
	return pipeline.RunE2ECheck(ctx)
}
