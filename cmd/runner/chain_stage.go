package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"strings"

	"automation.internal/ticket-ingress/internal/cardsecret"
	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
)

// runChainStage is the per-card entry: every stage profile's
// worker.command invokes this binary as `runner chain-stage
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
	workspace := os.Getenv("HERMES_KANBAN_WORKSPACE")
	if workspace == "" {
		return errors.New("HERMES_KANBAN_WORKSPACE is required (dispatched by the Hermes kanban only)")
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	pipeline := &runner.Pipeline{Config: config, Workspace: workspace, Logger: logger}
	// The destination credential is read from the operator-provisioned file,
	// never from this process's inherited environment: the dispatcher spawns
	// every stage — the untrusted implementer included — from the same
	// environment, so a token there would ride into the implementing agent.
	// Only the stages that reach the destination get it at all.
	if *stage == runtime.StageValidate || *stage == runtime.StagePublish {
		token, err := destinationToken(config)
		if err != nil {
			// The pipeline never runs, so it never reaches its own seal; this
			// card would otherwise be the one kind of failure that leaves no
			// account of itself in the round.
			pipeline.SealStageFailure(*stage, err)
			return err
		}
		pipeline.TargetToken = token
	}
	// The means this card was handed. Read here, per card, for the same
	// reason the destination token is: a value in the dispatcher's own
	// environment would reach every card, and the point of naming stages is
	// that it reaches those and no others.
	credentials, err := stageCredentials(config, *stage)
	if err != nil {
		pipeline.SealStageFailure(*stage, err)
		return err
	}
	pipeline.StageCredentials = credentials
	// Whatever this card was running is no longer running when it returns.
	// Where this was missing, the record outlived every card and the
	// ticket page kept a pulsing "いま動いています" beside a run that had
	// finished, for two hours (review of #200).
	defer runner.ClearCurrentStep(workspace)
	return pipeline.RunChainStage(ctx, *stage)
}

// stageCredentials reads the files this card was named in and returns them
// as environment assignments. A file the operator declared and did not
// provision fails the card rather than running it without: a delivery that
// was told it may reach a service, and silently could not, spends a whole
// round finding out in the worst possible way.
//
// The contents are registered as this process's secrets whichever way the
// variable is handed over. A credential given as a path exports a file name
// that is not itself secret, but the tool that reads the file prints what is
// in it when it fails, and that output travels into records and onto a
// screen.
func stageCredentials(config runtime.Config, stage string) ([]string, error) {
	var assignments, variables, paths []string
	var entries []cardsecret.Entry
	for _, credential := range config.Chain.CredentialsFor(stage) {
		contents, err := credentialValue(credential)
		if err != nil {
			return nil, err
		}
		exported := contents
		if credential.HandsOverPath() {
			exported = credential.Path
		}
		for _, variable := range credential.Env {
			assignments = append(assignments, variable+"="+exported)
			variables = append(variables, variable)
			if credential.HandsOverPath() {
				paths = append(paths, variable)
			}
			entries = append(entries, cardsecret.Entry{
				Name: variable, Secret: contents, Path: credential.HandsOverPath(),
			})
		}
	}
	if len(variables) == 0 {
		return nil, nil
	}
	cardsecret.Register(entries)
	// The names travel to every process this card starts, so a worker — and
	// the agent it launches — knows which of the variables it inherited are
	// secret and which name a file. The values travel as the variables
	// themselves.
	assignments = append(assignments, cardsecret.NamesEnv+"="+strings.Join(variables, ":"))
	if len(paths) > 0 {
		assignments = append(assignments, cardsecret.PathNamesEnv+"="+strings.Join(paths, ":"))
	}
	return assignments, nil
}

// credentialValue reads one provisioned file. The name is in the error and
// the content never is — a refusal travels into the round's record and onto
// the ticket.
func credentialValue(credential runtime.Credential) (string, error) {
	raw, err := cardsecret.ReadCredentialFile(credential.Path)
	if err != nil {
		return "", errors.New("credential " + credential.Name + " is " + err.Error())
	}
	// Trailing whitespace only: a credentials file has its own interior
	// newlines, and a token written by an editor has one at the end.
	value := strings.TrimRight(string(raw), " \t\r\n")
	if value == "" {
		return "", errors.New("credential " + credential.Name + " is empty")
	}
	if strings.ContainsRune(value, 0) {
		return "", errors.New("credential " + credential.Name + " holds a null byte, which cannot be an environment value")
	}
	return value, nil
}

func destinationToken(config runtime.Config) (string, error) {
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
