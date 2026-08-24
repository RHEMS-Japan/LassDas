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

// runChainStage is the cards orchestration's per-card entry: every stage
// profile's worker.command invokes this binary as `runner chain-stage
// --stage <name>`, and the kanban dispatcher supplies the shared run
// directory in HERMES_KANBAN_WORKSPACE. No ledger access happens here —
// claims, questions and terminal reports belong to the attendant — so a
// stage's whole contract is its artifacts and its exit code.
func runChainStage(ctx context.Context, arguments []string) error {
	flags := flag.NewFlagSet("chain-stage", flag.ContinueOnError)
	stage := flags.String("stage", "", "chain stage to run")
	configPath := flags.String("config", os.Getenv("LASSDAS_RUNTIME_CONFIG"), "runtime.json path")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *stage == "" {
		return errors.New("chain-stage needs --stage")
	}
	config, err := runtime.Load(*configPath)
	if err != nil {
		return err
	}
	if !config.OrchestrationCards() {
		return errors.New("chain-stage requires the cards orchestration")
	}
	workspace := os.Getenv("HERMES_KANBAN_WORKSPACE")
	if workspace == "" {
		return errors.New("HERMES_KANBAN_WORKSPACE is required (dispatched by the Hermes kanban only)")
	}
	// The destination credential is read from the operator-provisioned file,
	// never from this process's inherited environment: the dispatcher spawns
	// every stage — the untrusted implementer included — from the same
	// environment, so a token there would ride into the implementing agent.
	// Only the stages that reach the destination get it at all.
	token := ""
	if *stage == runtime.StageValidate || *stage == runtime.StagePublish {
		raw, err := os.ReadFile(config.Chain.TargetTokenPath)
		if err != nil {
			return errors.New("target token unreadable")
		}
		token = strings.TrimSpace(string(raw))
		if token == "" {
			return errors.New("target token file is empty")
		}
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	pipeline := &runner.Pipeline{Config: config, Workspace: workspace, TargetToken: token, Logger: logger}
	return pipeline.RunChainStage(ctx, *stage)
}
