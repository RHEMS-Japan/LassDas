package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"automation.internal/ticket-ingress/internal/githubapi"
	"automation.internal/ticket-ingress/internal/worker"
)

const githubTokenEnvironment = "TARGET_GITHUB_TOKEN"

type controllerFailure string

func (failure controllerFailure) Error() string { return string(failure) }

func fail(code string) error { return controllerFailure(code) }

// failFrom keeps the fixed failure code the workflow keys on, and prints what
// actually failed underneath it. A delivery that dies with only its code
// cannot be diagnosed at all (measured 2026-08-14: a feature publish failed
// terse and the cause was unrecoverable after the fact - the API status and
// endpoint had been discarded here). The detail is the wrapped error text
// only: an endpoint path and a status, never a credential.
func failFrom(code string, err error) error {
	if err != nil {
		fmt.Fprintf(os.Stderr, "controller: %s: %v\n", code, err)
	}
	return fail(code)
}

func run(ctx context.Context, args []string, getenv func(string) string, transport http.RoundTripper) error {
	if ctx == nil || len(args) == 0 {
		return fail("command_invalid")
	}
	switch args[0] {
	case "baseline":
		return runBaseline(ctx, args[1:], getenv, transport)
	case "publish-feature":
		return runPublishFeature(ctx, args[1:], getenv, transport)
	case "create-feature-pr":
		return runCreateFeaturePR(ctx, args[1:], getenv, transport)
	case "wait-feature":
		return runWaitFeature(ctx, args[1:], getenv, transport)
	case "merge-feature":
		return runMergeFeature(ctx, args[1:], getenv, transport)
	case "await-staging":
		return runAwaitStaging(ctx, args[1:], getenv, transport)
	case "await-merged-staging":
		return runAwaitMergedStaging(ctx, args[1:], getenv, transport)
	case "create-promotion-pr":
		return runCreatePromotionPR(ctx, args[1:], getenv, transport)
	case "merge-promotion":
		return runMergePromotion(ctx, args[1:], getenv, transport)
	case "await-production":
		return runAwaitProduction(ctx, args[1:], getenv, transport)
	default:
		return fail("command_invalid")
	}
}

type commandArguments map[string][]string

func parseCommandArguments(args []string, required []string, repeated ...string) (commandArguments, error) {
	if len(args) == 0 || len(args)%2 != 0 || len(args) > 64 {
		return nil, fail("arguments_invalid")
	}
	allowed := make(map[string]bool, len(required)+len(repeated))
	for _, name := range required {
		allowed[name] = false
	}
	for _, name := range repeated {
		if _, exists := allowed[name]; exists {
			return nil, fail("arguments_invalid")
		}
		allowed[name] = true
	}
	values := make(commandArguments, len(allowed))
	for index := 0; index < len(args); index += 2 {
		name, value := args[index], args[index+1]
		repeatable, exists := allowed[name]
		if !exists || value == "" || strings.ContainsAny(name+value, "\x00\r\n") {
			return nil, fail("arguments_invalid")
		}
		if len(values[name]) != 0 && !repeatable {
			return nil, fail("arguments_invalid")
		}
		if repeatable {
			for _, previous := range values[name] {
				if previous == value {
					return nil, fail("arguments_invalid")
				}
			}
		}
		values[name] = append(values[name], value)
	}
	for _, name := range required {
		if len(values[name]) != 1 {
			return nil, fail("arguments_invalid")
		}
	}
	for _, name := range repeated {
		if len(values[name]) == 0 {
			return nil, fail("arguments_invalid")
		}
	}
	return values, nil
}

func (arguments commandArguments) one(name string) string {
	values := arguments[name]
	if len(values) != 1 {
		return ""
	}
	return values[0]
}

type commandRuntime struct {
	config     worker.Config
	consumer   worker.ConsumerConfig
	contract   githubapi.Contract
	controller *githubapi.Controller
}

// loadCommandConfig reads the fixed configuration and checks the output
// destination before anything talks to GitHub.
func loadCommandConfig(configPath, outputPath string) (worker.Config, error) {
	if validateOutputDestination(outputPath) != nil {
		return worker.Config{}, fail("output_path_invalid")
	}
	config, err := loadFixedConfig(configPath)
	if err != nil {
		return worker.Config{}, fail("config_invalid")
	}
	return config, nil
}

// prepareRuntime builds the GitHub controller for the destination the ticket
// names. The repository must be a configured consumer and carry a reviewed
// fixed contract; there is no discovery.
func prepareRuntime(
	ctx context.Context,
	config worker.Config,
	repository string,
	getenv func(string) string,
	transport http.RoundTripper,
) (commandRuntime, error) {
	if getenv == nil {
		return commandRuntime{}, fail("environment_invalid")
	}
	consumer, err := config.ConsumerFor(repository)
	if err != nil {
		return commandRuntime{}, fail("config_invalid")
	}
	contract := consumer.Contract()
	token := getenv(githubTokenEnvironment)
	if token == "" || len(token) > 4096 || strings.TrimSpace(token) != token || strings.ContainsAny(token, "\x00\r\n\t ") {
		return commandRuntime{}, fail("github_token_invalid")
	}
	client, err := githubapi.NewClient(controllerGitHubConfig(token, consumer), transport)
	if err != nil {
		return commandRuntime{}, fail("github_client_invalid")
	}
	// A run that only proposes a pull request never merges, and its
	// read-scoped token cannot even see the merge settings; demanding them
	// there turns an invisible field into a false mismatch (measured
	// 2026-08-06 on the first live ticket).
	newController := githubapi.NewController
	if consumer.Delivery == worker.DeliverPullRequest {
		newController = githubapi.NewProposalController
	}
	controller, err := newController(client, contract)
	if err != nil {
		return commandRuntime{}, fail("github_contract_invalid")
	}
	if _, err := controller.Verify(ctx); err != nil {
		// The invariant name says which check failed; without it every
		// mismatch reads the same and cannot be diagnosed from the run log.
		fmt.Fprintf(os.Stderr, "controller: verify: %v\n", err)
		return commandRuntime{}, fail("github_verify_failed")
	}
	return commandRuntime{config: config, consumer: consumer, contract: contract, controller: controller}, nil
}

func failureCode(err error) string {
	var failure controllerFailure
	if errors.As(err, &failure) {
		return string(failure)
	}
	return "unexpected_failure"
}
