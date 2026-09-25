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
	"bytes"
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

	"automation.internal/ticket-ingress/internal/cardsecret"
	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/livelog"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/worker"
	"net/http"
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
	// StageCredentials are the operator-provisioned secrets this card was
	// named in, already read, as NAME=value assignments. Every step this
	// card runs gets them, the launched agent included: a card is named in
	// a credential precisely so that what runs inside it can reach the
	// service. No other card sees them, because no other card's entry read
	// the files. Empty for a card no credential names.
	StageCredentials []string
	Logger           interface {
		Info(string, ...any)
		Error(string, ...any)
	}

	consumerRepository string
	// cloneTarget lets package tests stand in for the network clone of the
	// destination repository; nil means the real github.com clone.
	// Production never sets it.
	cloneTarget func(ctx context.Context, destination string) error
	// usageTransport lets package tests stand in for the billing endpoint
	// the baseline is read from; nil means the real network. Production
	// never sets it.
	usageTransport http.RoundTripper
	// prepared is set once Prepare has cleared the workspace, so nothing
	// left by an earlier dispatch can pass for this run's history.
	prepared bool
	// trailWritten is set when this run composed (or fell back to) the trail
	// file itself; only that is trusted — a file that merely exists could
	// have been left by anyone.
	trailWritten bool
	// lastStepStderr keeps the tail of the most recent step's stderr, so a
	// stage that failed can tell the requester why in the trail (the worker
	// explains its refusal there and nowhere else).
	lastStepStderr string
	// blockedStep is the requester-facing name of the step a card stopped
	// on, set by the attendant before it composes the trail of a run it is
	// ending. Empty where no card stopped.
	blockedStep string
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
	// One source of truth for what this card carries. The assignments are
	// what the child process gets and the same values are what must never
	// appear in a log or a record; registering them here, rather than
	// leaving the caller to do both, is what keeps the two from drifting
	// apart. Registration is idempotent, so every step after the first
	// registers nothing.
	p.registerCredentials()
	p.recordCurrentStep(name)
	// Every step is told where to append what it is producing, so a reader
	// can watch the work instead of waiting for the record that lands when
	// the step ends.
	// The step's own child processes append to the same file, so what a
	// step produced survives the container that produced it: until now the
	// only copy of a failed step's output was the container's log, and a
	// rebuilt container took the reason with it.
	livePath := LiveLogPath(p.Workspace, name)
	extraEnv = append(extraEnv, livelog.PathEnv+"="+livePath)
	if err := os.Setenv(livelog.PathEnv, livePath); err != nil {
		p.Logger.Error("live log path not set", "error", err.Error())
	}
	// It belongs to this step only: the clone and the terminal phase run
	// outside any step and must not append to the last one's file.
	defer func() { _ = os.Unsetenv(livelog.PathEnv) }()
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Dir = p.Workspace
	live := livelog.Open()
	defer live.Close()
	command.Stdout = live.Tee(os.Stdout)
	stderrTail := &tailBuffer{limit: stepStderrTailBytes}
	command.Stderr = live.Tee(io.MultiWriter(os.Stderr, stderrTail))
	// Redacted on the way in, at the one place a child's output becomes
	// something this process keeps. From here the tail travels into the
	// round's failure record, into the next round's instruction, and onto
	// the ticket; a credential echoed by a failing command would ride all
	// three, and each of them outlives the card.
	defer func() { p.lastStepStderr = p.redactCredentials(stderrTail.String()) }()
	command.Env = append(append(os.Environ(), p.StageCredentials...), extraEnv...)
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
	if errors.Is(err, exec.ErrWaitDelay) && command.ProcessState != nil {
		// The step itself exited; a grandchild kept the captured stderr
		// open past the wait delay. Before stderr was captured the fd was
		// inherited and this was not a failure, and it still is not.
		return command.ProcessState.ExitCode(), nil
	}
	return -1, fmt.Errorf("step %s could not run: %w", name, err)
}

// stepStderrTailBytes bounds what a step's stderr leaves behind for the
// trail note; the worker's refusal line sits at the end of it.
//
// Larger than one whole evidence line, deliberately. That line carries the
// worker's machine-readable account of a model turn and can run to its own
// limit; kept in a buffer no bigger, a long one would arrive with its
// prefix cut away — no longer parseable as evidence, and still present as
// text for the classifier to read words out of, which is the one thing the
// evidence line exists to prevent. Room for the line and for the refusal
// sentence that follows it.
const stepStderrTailBytes = 2 * worker.MaxFailureDetailLineBytes

// tailBuffer keeps the last limit bytes written to it.
type tailBuffer struct {
	limit int
	data  []byte
	// partial is set while the kept text begins in the middle of a line.
	partial bool
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	if len(b.data) <= b.limit {
		return len(p), nil
	}
	cut := len(b.data) - b.limit
	// Cut on a line boundary. A value that straddles the cut would
	// otherwise survive as its own last few characters, which no
	// whole-value replacement finds: one byte off the front of a token is
	// still the token. Dropping the part-line loses nothing a reader could
	// have used, because it begins in the middle of a sentence.
	//
	// Unless the boundary is the end of everything kept. A step that
	// printed one line longer than this buffer has exactly one break in the
	// window, at the very end, and cutting there leaves nothing at all —
	// the reason the card failed disappears from the record, which is the
	// one thing the tail exists to carry. The byte cut stands in that case
	// and the text says its first line starts part-way through; a value
	// beginning mid-line is taken out by the redaction, which looks at
	// every line's start.
	boundary := bytes.IndexByte(b.data[cut:], '\n')
	if b.partial = boundary < 0 || cut+boundary+1 >= len(b.data); !b.partial {
		cut += boundary + 1
	}
	b.data = append([]byte(nil), b.data[cut:]...)
	return len(p), nil
}

func (b *tailBuffer) String() string {
	if b.partial {
		return partialLineNotice + string(b.data)
	}
	return string(b.data)
}

// partialLineNotice heads a tail whose first line begins part-way through,
// so a reader does not take the first words for the start of a sentence.
const partialLineNotice = "[この行は途中から始まります]\n"

// registerCredentials makes this card's credential values known to
// everything in this process that writes text a person may read: the live
// log's masker, the records this pipeline keeps, and the agent transcript
// captured in another package again.
func (p *Pipeline) registerCredentials() {
	if len(p.StageCredentials) == 0 {
		return
	}
	entries := make([]cardsecret.Entry, 0, len(p.StageCredentials))
	for _, assignment := range p.StageCredentials {
		name, value, found := strings.Cut(assignment, "=")
		if !found {
			continue
		}
		entry := cardsecret.Entry{Name: name}
		// A variable holding a file's name is not a secret, and registering
		// it as one would take every build line that mentions the file out
		// of the live log. The card's entry point registered what is in the
		// file when it read it; this is the backstop for a value handed
		// over directly.
		if cardsecret.HandedAsPath(name) {
			entry.Path = true
		} else {
			entry.Secret = value
		}
		entries = append(entries, entry)
	}
	cardsecret.Register(entries)
}

// redactCredentials removes this card's credentials from text the engine is
// about to keep. The card registered them when it read the files, whole and
// line by line: a credentials file reaches a log one line at a time, and a
// record holding one of those lines has published it.
func (p *Pipeline) redactCredentials(text string) string {
	return cardsecret.Redact(text)
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

// ReceptionAgainFile is the note a delivery carries when its reception was
// run a second time because the decision the first one sealed could not be
// read. It lives in the run directory and survives Prepare's clearing, so
// that the regeneration it records can happen once and never twice
// (internal/attendant/reception_again.go).
const ReceptionAgainFile = "reception-again.json"

// Prepare clears the workspace and materializes the claimed envelope. The
// workflow got a fresh runner filesystem per attempt; a Hermes task keeps
// its workspace across re-dispatches, so a retried card must not see the
// previous attempt's artifacts (a stale clarification.json alone would
// hand model stages answers this run never adopted).
func (p *Pipeline) Prepare() error {
	// A launch that died with the pod may have left the tree to the agent
	// user; it comes back before anything here is cleared or read.
	worker.ReclaimWorkspace(p.Workspace)
	entries, err := os.ReadDir(p.Workspace)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == ".agent-lend.lock" {
			// The launcher's lend lock beside the workspace stays: an
			// earlier launch may still hold it while returning the tree.
			continue
		}
		if entry.Name() == ReceptionAgainFile {
			// The note that this delivery's reception has already been run
			// a second time. Everything else here is cleared so that a
			// restarted delivery re-derives from the ticket instead of
			// from what the last attempt left; this has to outlive the
			// clearing, because it is the whole bound on the regeneration
			// it records. Without it the second unreadable decision would
			// ask for a third reception, and a fourth, and the delivery
			// would never end.
			continue
		}
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
		{p.Config.BrowserCheckBin, p.Config.BrowserCheckSHA256, "browsercheck"},
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
// located — the workflow's source-job jq over m1-consumer.json.
//
// Every depth is accepted. A destination that asks for integration or
// production used to be refused here, before any work, because the pod
// runtime could only propose; the cards that merge, wait for the
// deployment and observe the screen now run inside the delivery, so the
// depth is something this run carries out rather than something it turns
// away. What this stage does is the same either way: publish the change and
// open the pull request. Everything past it belongs to the delivery cards.
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
		// The depth is read, not kept: what this stage does is the same at
		// every depth, and the delivery cards after it are told how far to
		// go by the attendant, which reads the destination configuration
		// through its own loader. A value that is not one of the three is
		// still refused here — the run would otherwise start against a
		// configuration nothing downstream can act on.
		switch consumer.Delivery {
		case "pull_request", "integration", "production", "":
		default:
			return fmt.Errorf("consumer %s has unknown delivery %q", repository, consumer.Delivery)
		}
		p.consumerRepository = repository
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
