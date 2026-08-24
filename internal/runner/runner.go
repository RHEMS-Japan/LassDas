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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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
	// TargetToken is the destination credential, held in memory only: main
	// strips it from the process environment before any stage child runs,
	// so os.Environ() passthrough cannot hand it to the model stages (the
	// workflow's model job held it in no process at all; the remaining
	// same-UID /proc exposure of the runner's own exec image is recorded in
	// docs/RUNTIME_POD.md as the Phase-3 UID-separation gate).
	TargetToken string
	Logger      interface {
		Info(string, ...any)
		Error(string, ...any)
	}

	consumerRepository string
	delivery           string
	// prepared is set once Prepare has cleared the workspace, so nothing
	// left by an earlier dispatch can pass for this run's history.
	prepared bool
	// trailWritten is set when this run composed (or fell back to) the trail
	// file itself; only that is trusted — a file that merely exists could
	// have been left by anyone.
	trailWritten bool
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

// maxWorkspaceReadBytes bounds every read of a model- or tool-produced
// workspace file. The stage outputs the pipeline reads back are small JSON
// decisions; anything larger is not one of them.
const maxWorkspaceReadBytes = 4 * 1024 * 1024

// readWorkspaceFile reads one workspace artifact with the same guards the
// question-decision loader always had: a regular file (no symlink out of
// the workspace), size-capped before the read.
func readWorkspaceFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, fmt.Errorf("%s is not a regular file within %d bytes", path, limit)
	}
	return os.ReadFile(path)
}

// step runs one binary with the working directory pinned to the workspace,
// streaming output to the runner's own stdout/stderr (which the Hermes
// per-task log captures). Exit codes are returned, not translated: each
// call site owns its outcome table exactly as the workflow steps did. The
// child gets its own process group and a context cancel (SIGTERM from the
// supervisor) tears the whole group down — under GitHub Actions the runner
// killed the job's process tree; the pod must do that itself.
func (p *Pipeline) step(ctx context.Context, name string, argv []string, extraEnv ...string) (int, error) {
	p.Logger.Info("step", "name", name, "argv0", argv[0])
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Dir = p.Workspace
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.Env = append(os.Environ(), extraEnv...)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
	}
	command.WaitDelay = 20 * time.Second
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
	return p.step(ctx, name, argv, "TARGET_GITHUB_TOKEN="+p.TargetToken)
}

// readJSONField reads one string field out of a workspace JSON file.
func (p *Pipeline) readJSONField(name string, keys ...string) (string, error) {
	raw, err := readWorkspaceFile(p.path(name), maxWorkspaceReadBytes)
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

// Prepare clears the workspace and materializes the claimed envelope. The
// workflow got a fresh runner filesystem per attempt; a Hermes task keeps
// its workspace across re-dispatches, so a retried card must not see the
// previous attempt's artifacts (a stale clarification.json alone would
// hand model stages answers this run never adopted).
func (p *Pipeline) Prepare() error {
	entries, err := os.ReadDir(p.Workspace)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := forceRemoveAll(filepath.Join(p.Workspace, entry.Name())); err != nil {
			return err
		}
	}
	if err := p.verifyToolPins(); err != nil {
		return err
	}
	encoded, err := json.Marshal(p.Envelope)
	if err != nil {
		return err
	}
	if err := os.WriteFile(p.path("ticket-envelope.json"), encoded, 0o600); err != nil {
		return err
	}
	p.prepared = true
	return nil
}

// verifyToolPins measures the stage binaries against the configured pins.
// The workflow verified its checkout and binary digests before every use;
// this is the pod's equivalent when the operator pins them.
func (p *Pipeline) verifyToolPins() error {
	for _, pin := range []struct{ path, want, name string }{
		{p.Config.WorkerBin, p.Config.WorkerSHA256, "worker"},
		{p.Config.ControllerBin, p.Config.ControllerSHA256, "controller"},
	} {
		if pin.want == "" {
			continue
		}
		file, err := os.Open(pin.path)
		if err != nil {
			return fmt.Errorf("%s binary unreadable: %w", pin.name, err)
		}
		digest := sha256.New()
		_, err = io.Copy(digest, file)
		_ = file.Close()
		if err != nil {
			return fmt.Errorf("%s binary unreadable: %w", pin.name, err)
		}
		if hex.EncodeToString(digest.Sum(nil)) != pin.want {
			return fmt.Errorf("%s binary does not match its configured sha256 pin", pin.name)
		}
	}
	return nil
}

// resolveConsumer reads the delivery mode for the single consumer the draft
// located — the workflow's source-job jq over m1-consumer.json. The pod
// runtime ships the pull_request stopping point first; a consumer that
// stops at integration or production is refused here, before any work,
// because its success report needs browser evidence steps this runtime
// does not carry yet (an honest early stop instead of an unsealable
// terminal after a real merge).
func (p *Pipeline) resolveConsumer() error {
	raw, err := readWorkspaceFile(p.Config.ConsumerConfigPath, maxWorkspaceReadBytes)
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
		case "pull_request":
		case "integration", "production":
			return fmt.Errorf("consumer %s stops at %s; the pod runtime ships pull_request delivery only", repository, consumer.Delivery)
		default:
			return fmt.Errorf("consumer %s has unknown delivery %q", repository, consumer.Delivery)
		}
		p.consumerRepository = repository
		p.delivery = consumer.Delivery
		for _, tool := range consumer.Mode.Toolchain {
			// The workflow provisioned this toolchain per run (pinned Node
			// and pnpm); the pod image ships it. Assert it is really there
			// rather than letting validation fail obliquely later.
			if tool.Binary != "" {
				if _, err := exec.LookPath(tool.Binary); err != nil {
					return fmt.Errorf("consumer %s needs %q on PATH and the image does not provide it", repository, tool.Binary)
				}
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

// forceRemoveAll removes a tree that may contain write-protected
// directories (the previous dispatch's read-only base copy): unlinking
// inside an a-w directory is refused for a non-root pod, so directories
// are opened up first. os.RemoveAll alone wedged every re-dispatch of a
// card whose earlier attempt had shaped its model workspace.
func forceRemoveAll(path string) error {
	_ = filepath.WalkDir(path, func(entry string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			_ = os.Chmod(entry, 0o755)
		}
		return nil
	})
	return os.RemoveAll(path)
}

// nowUTC is indirected for tests.
var nowUTC = func() time.Time { return time.Now().UTC() }
