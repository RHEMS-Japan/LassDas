package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	runtimecfg "automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/worker"
)

// runCheckRuntime constructs only the existing local services. It creates or
// opens the local ledger and route key but never polls, posts, or starts roles.
func runCheckRuntime(args []string) error {
	flags := commandFlags("check-runtime")
	configPath := flags.String("config", "", "")
	if !parseFlags(flags, args) || !allPresent(*configPath) {
		return errors.New("check-runtime arguments are invalid")
	}
	config, err := runtimecfg.Load(*configPath)
	if err != nil {
		return err
	}
	services, err := runtimecfg.BuildServices(config, nil)
	if err != nil {
		return err
	}
	return services.Close()
}

func runCheckConsumer(ctx context.Context, args []string) error {
	flags := commandFlags("check-consumer")
	consumerPath := flags.String("consumer", "", "")
	repoRoot := flags.String("repo-root", "", "")
	baseSHA := flags.String("base-sha", "", "")
	outputPath := flags.String("out", "", "")
	if !parseFlags(flags, args) || !allPresent(*consumerPath, *repoRoot, *baseSHA, *outputPath) {
		return errors.New("check-consumer arguments are invalid")
	}
	var consumer worker.ConsumerConfig
	if err := worker.ReadJSONFile(*consumerPath, worker.MaxConfigJSONBytes, &consumer); err != nil {
		return errors.New("consumer configuration could not be read")
	}
	result, err := worker.CheckConsumer(ctx, *repoRoot, consumer, *baseSHA)
	if err != nil {
		var commandFailure *worker.ValidationCommandError
		if errors.As(err, &commandFailure) {
			// The trial runs without credentials; build output remains local.
			fmt.Fprintf(os.Stderr, "worker: consumer check failed: %s\n%s", strings.Join(commandFailure.Arguments, " "), commandFailure.Tail)
		}
		return err
	}
	return worker.WriteJSONFileExclusive(*outputPath, result, worker.MaxArtifactJSONBytes)
}
