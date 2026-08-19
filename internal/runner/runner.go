// Package runner is the stage pipeline of the single-pod constitution: the
// Go transcription of .github/workflows/m1-worker.yml's ticket path. The
// workflow was a single chain of jobs passing files through artifacts; here
// the chain is a sequence of functions passing files through one working
// directory, and every step still shells out to the same cmd/worker,
// cmd/controller and cmd/browsercheck binaries with the same arguments.
// What the workflow expressed in bash — outcome parsing, the readiness
// attempts, the three finite model stages, the terminal-code priority table
// — lives here, and any deliberate divergence from the YAML is called out
// in place.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runtime"
)

// Pipeline drives one claimed envelope through the stages.
type Pipeline struct {
	Config    runtime.Config
	Services  *runtime.Services
	Envelope  hook.DispatchEnvelope
	Workspace string // the Hermes task workspace; every artifact file lives here
	Logger    interface {
		Info(string, ...any)
		Error(string, ...any)
	}

	// stepEnv carries the model keys and agent secrets exactly as the
	// workflow exported them; populated from the process environment.
	consumerRepository string
	delivery           string
	needsNode          bool
}

// Outcome is what the pipeline hands back to the runner's terminal logic.
type Outcome struct {
	// Code is empty for a delivered success; otherwise the terminal code.
	Code hook.TerminalCode
	// Question is set when the run must ask instead of report; the decision
	// file feeds the question service.
	QuestionDecisionPath string
	// Stage is the adopted stage number for a published candidate (0 when
	// none).
	Stage int
	// Evidence collects the terminal report's evidence fields, keyed by the
	// same names the workflow's report step assembled.
	Evidence map[string]string
	// ParseRejected marks the build-draft rejection path (input_rejected).
	ParseRejected bool
}

func (p *Pipeline) path(name string) string { return filepath.Join(p.Workspace, name) }

func (p *Pipeline) exists(name string) bool {
	_, err := os.Stat(p.path(name))
	return err == nil
}

// step runs one binary with the working directory pinned to the workspace,
// streaming output to the runner's own stdout/stderr (which the Hermes
// per-task log captures). Exit codes are returned, not translated: each
// call site owns its outcome table exactly as the workflow steps did.
func (p *Pipeline) step(ctx context.Context, name string, argv []string, extraEnv ...string) (int, error) {
	p.Logger.Info("step", "name", name, "argv0", argv[0])
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Dir = p.Workspace
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.Env = append(os.Environ(), extraEnv...)
	err := command.Run()
	if err == nil {
		return 0, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	return -1, fmt.Errorf("step %s could not run: %w", name, err)
}

func (p *Pipeline) worker(ctx context.Context, name string, arguments []string, extraEnv ...string) (int, error) {
	argv := append([]string{p.Config.WorkerBin}, arguments...)
	return p.step(ctx, name, argv, extraEnv...)
}

func (p *Pipeline) controller(ctx context.Context, name string, arguments []string) (int, error) {
	argv := append([]string{p.Config.ControllerBin}, arguments...)
	return p.step(ctx, name, argv, "TARGET_GITHUB_TOKEN="+os.Getenv("TARGET_GITHUB_TOKEN"))
}

// readJSONField reads one string field out of a workspace JSON file.
func (p *Pipeline) readJSONField(name string, keys ...string) (string, error) {
	raw, err := os.ReadFile(p.path(name))
	if err != nil {
		return "", err
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "", fmt.Errorf("%s is not JSON: %w", name, err)
	}
	current := decoded
	for _, key := range keys {
		object, ok := current.(map[string]any)
		if !ok {
			return "", fmt.Errorf("%s: %s is not an object", name, strings.Join(keys, "."))
		}
		current = object[key]
	}
	switch value := current.(type) {
	case string:
		return value, nil
	case float64:
		return strings.TrimSuffix(fmt.Sprintf("%f", value), ".000000"), nil
	case bool:
		return fmt.Sprintf("%t", value), nil
	case nil:
		return "", nil
	default:
		encoded, _ := json.Marshal(value)
		return string(encoded), nil
	}
}

// WriteEnvelope materializes the claimed envelope for the worker steps.
func (p *Pipeline) WriteEnvelope() error {
	encoded, err := json.Marshal(p.Envelope)
	if err != nil {
		return err
	}
	return os.WriteFile(p.path("ticket-envelope.json"), encoded, 0o600)
}

// resolveConsumer reads the delivery mode for the single consumer the draft
// located — the workflow's source-job jq over m1-consumer.json.
func (p *Pipeline) resolveConsumer() error {
	raw, err := os.ReadFile(p.Config.ConsumerConfigPath)
	if err != nil {
		return err
	}
	var config struct {
		Consumers []struct {
			Repository string `json:"repository"`
			Delivery   string `json:"delivery"`
			Mode       struct {
				Toolchain []struct {
					Binary string `json:"binary"`
				} `json:"toolchain"`
			} `json:"mode"`
		} `json:"consumers"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return err
	}
	repository, err := p.readJSONField("ticket-draft.json", "repository")
	if err != nil {
		return err
	}
	for _, consumer := range config.Consumers {
		if consumer.Repository != repository {
			continue
		}
		switch consumer.Delivery {
		case "pull_request", "integration", "production":
		default:
			return fmt.Errorf("consumer %s has unknown delivery %q", repository, consumer.Delivery)
		}
		p.consumerRepository = repository
		p.delivery = consumer.Delivery
		for _, tool := range consumer.Mode.Toolchain {
			if tool.Binary == "node" {
				p.needsNode = true
			}
		}
		return nil
	}
	return fmt.Errorf("draft names repository %q but no consumer defines it", repository)
}

// Repository exposes the resolved consumer repository for the terminal
// report ("" when the run failed before the draft named one — the report
// protocol accepts that explicitly).
func (p *Pipeline) Repository() string { return p.consumerRepository }

// nowUTC is indirected for tests.
var nowUTC = func() time.Time { return time.Now().UTC() }
