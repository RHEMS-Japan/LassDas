package worker

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	// MaxAgentTranscriptBytes bounds what one agent run may report back. The
	// transcript is evidence, not a channel: anything larger is truncated
	// rather than allowed to grow without limit.
	MaxAgentTranscriptBytes = 1024 * 1024
	// MaxAgentRuntime is the ceiling on a single agent run regardless of what
	// the configuration asks for. It sits above the chain cards' kanban wall
	// on purpose: when the configured budget clears this wall, an overrunning
	// agent is stopped by the card's max_runtime instead — a timed_out card is
	// re-spawned by the dispatcher for a second attempt, while an in-process
	// budget kill ends the whole chain as model_failed. An affected run's second
	// review burned its full 60 minutes mid-investigation and took the run
	// with it; the wall would have given it a fresh attempt.
	MaxAgentRuntime = 120 * time.Minute
	// agentWaitDelay bounds how long a stopped run may keep its output open.
	agentWaitDelay = 10 * time.Second
)

// AgentConfig names one coding agent the framework may run. The framework does
// not know which agent this is: it runs the configured command in a working
// copy and judges the result. Everything specific to a particular agent — its
// binary, its flags, the environment variables it reads — is configuration.
type AgentConfig struct {
	ID      string   `json:"id"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
	// Env are fixed values passed to the agent (endpoint, model name).
	Env map[string]string `json:"env,omitempty"`
	// SecretEnv maps an environment variable the agent reads to the name of
	// the variable this process reads the credential from, so the credential
	// never appears in configuration.
	SecretEnv      map[string]string `json:"secret_env,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds"`
	// Profile names the in-program identity this launch runs under, for
	// programs that host more than one role behind one binary. A declared
	// profile must be carried in Args — a name the launch does not actually
	// pass is a claim, not a separation.
	Profile string `json:"profile,omitempty"`
	// Knowledge is what this agent is given to read before it works.
	Knowledge KnowledgeConfig `json:"knowledge,omitempty"`
}

var agentEnvNamePattern = envNamePattern

// reservedEnvName rejects the variables this process sets itself. Letting
// configuration shadow them would hand the outcome to whichever duplicate the
// operating system happens to prefer.
func reservedEnvName(name string) bool {
	return name == "PATH" || name == "HOME" || name == "LANG"
}

func (a AgentConfig) validate() error {
	if !identifierPattern.MatchString(a.ID) {
		return errors.New("agent id is invalid")
	}
	if a.Command == "" || len(a.Command) > 256 || strings.ContainsAny(a.Command, "\r\n\x00 /") {
		return errors.New("agent command is invalid")
	}
	if len(a.Args) > 64 {
		return errors.New("agent arguments are invalid")
	}
	for _, argument := range a.Args {
		if argument == "" || len(argument) > 512 || strings.ContainsAny(argument, "\r\n\x00") {
			return errors.New("agent argument is invalid")
		}
	}
	if len(a.Env) > 16 || len(a.SecretEnv) > 4 {
		return errors.New("agent environment is invalid")
	}
	for name, value := range a.Env {
		if !agentEnvNamePattern.MatchString(name) || reservedEnvName(name) || len(value) > 512 || strings.ContainsAny(value, "\r\n\x00") {
			return errors.New("agent environment is invalid")
		}
	}
	for name, source := range a.SecretEnv {
		if !agentEnvNamePattern.MatchString(name) || reservedEnvName(name) || !agentEnvNamePattern.MatchString(source) {
			return errors.New("agent secret environment is invalid")
		}
		// The same name in Env would put a literal credential in
		// configuration and leave the winner between the duplicates to the
		// child environment's sort order.
		if _, shadowed := a.Env[name]; shadowed {
			return errors.New("agent environment must not shadow a secret variable")
		}
	}
	if a.TimeoutSeconds < 60 || time.Duration(a.TimeoutSeconds)*time.Second > MaxAgentRuntime {
		return errors.New("agent timeout is invalid")
	}
	if a.Profile != "" && (!identifierPattern.MatchString(a.Profile) || !slices.Contains(a.Args, a.Profile)) {
		return errors.New("agent profile is invalid")
	}
	return a.Knowledge.validate()
}

// AgentOutcome is what one agent run produced: the files it changed, and its
// own account of what it did. The transcript is recorded as evidence; nothing
// downstream trusts it, because the changed files are read from disk.
type AgentOutcome struct {
	AgentID      string        `json:"agent_id"`
	Command      string        `json:"command"`
	ExitCode     int           `json:"exit_code"`
	Duration     time.Duration `json:"duration"`
	Transcript   string        `json:"transcript"`
	ChangedFiles []string      `json:"changed_files"`
}

// RunAgent runs the configured agent inside workspace and reports which files
// it changed. The credential is read from this process's environment and
// passed to the child; it is never written to configuration or transcript.
func RunAgent(ctx context.Context, config AgentConfig, workspace, prompt string, allowedPrefixes []string, ignoredByproducts []string) (AgentOutcome, error) {
	if ctx == nil || prompt == "" || len(prompt) > MaxAgentPromptBytes {
		return AgentOutcome{}, errors.New("agent input is invalid")
	}
	if err := config.validate(); err != nil {
		return AgentOutcome{}, err
	}
	root, err := validatedWorkspace(workspace)
	if err != nil {
		return AgentOutcome{}, err
	}
	environment, err := agentEnvironment(config)
	if err != nil {
		return AgentOutcome{}, err
	}

	runContext, cancel := context.WithTimeout(ctx, time.Duration(config.TimeoutSeconds)*time.Second)
	defer cancel()

	arguments := append(append([]string(nil), config.Args...), prompt)
	command := exec.CommandContext(runContext, config.Command, arguments...) // #nosec G204 -- command and arguments come from validated configuration.
	command.Dir = root
	command.Env = environment
	var transcript bytes.Buffer
	command.Stdout = &transcript
	command.Stderr = &transcript
	// A coding agent runs tools of its own, so stopping it means stopping
	// everything it started. Its children are put in one process group and the
	// whole group is signalled; without this a timed-out run keeps waiting for
	// a grandchild that still holds the output pipe open.
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	// Bounds how long a stopped run may go on holding its output open.
	command.WaitDelay = agentWaitDelay

	started := time.Now()
	runErr := command.Run()
	duration := time.Since(started)

	outcome := AgentOutcome{
		AgentID: config.ID, Command: config.Command,
		Duration:   duration,
		Transcript: boundedTranscript(transcript.String()),
	}
	if command.ProcessState != nil {
		outcome.ExitCode = command.ProcessState.ExitCode()
	}
	if runContext.Err() != nil {
		return outcome, errors.New("agent run exceeded its time limit")
	}
	if runErr != nil {
		return outcome, errors.New("agent run failed")
	}

	changed, err := ChangedFilesUnder(root, allowedPrefixes, ignoredByproducts)
	if err != nil {
		return outcome, err
	}
	outcome.ChangedFiles = changed
	return outcome, nil
}

// MaxAgentPromptBytes bounds the instruction handed to an agent.
const MaxAgentPromptBytes = 64 * 1024

// ReviewAttemptLimit is how many conversations a review may open in total.
// Each attempt is a fresh roll of the dice the reviewer's upstream loads: the
// relay spreads even a pinned provider's turns across its own internal
// projects, and a turn landing in a different project than the one before it
// cannot decrypt the carried reasoning (invalid_encrypted_content, read from
// the captured upstream response of the sixth live marked ticket).
const ReviewAttemptLimit = 3

// ReviewRetryEligible is the ceiling under which a failed review attempt is
// worth rolling again. The decryption mismatch kills a conversation in under
// a minute; an attempt that burned longer failed for a slower reason (a
// timeout above all) that a fresh conversation will not fix, and retrying it
// would multiply the stage's worst case past the job's budget. The workflow
// timeout guard test computes its budget from these two constants.
const ReviewRetryEligible = 10 * time.Minute

// ReviewRetryPause is how long a failed fast attempt waits before its fresh
// conversation. Three instant retries measurably landed inside the same
// per-minute rate window and burned the whole attempt budget in four seconds
// against a 429 (2026-08-17); a pause longer than the window makes the retry
// a genuinely fresh chance instead of the same collision.
const ReviewRetryPause = 75 * time.Second

// RetryableReviewFailure reports whether a failed review attempt died fast
// enough to be the upstream lottery rather than a budget problem.
func RetryableReviewFailure(outcome AgentOutcome) bool {
	return outcome.Duration < ReviewRetryEligible
}

// ChangedFilesUnder reports which tracked files the working copy has modified,
// as repository-relative paths. A change outside the allowed prefixes is an
// error rather than a filtered-out result: the agent was told where it may
// write, and writing elsewhere is a failure of the run, not noise to discard.
func ChangedFilesUnder(root string, allowedPrefixes []string, ignoredByproducts []string) ([]string, error) {
	output, err := gitOutput(root, "status", "--porcelain=v1", "--untracked-files=all", "--ignored=matching", "-z")
	if err != nil {
		return nil, errors.New("changed files could not be read")
	}
	changed := make([]string, 0, 8)
	entries := strings.Split(output, "\x00")
	appendPath := func(path string) error {
		if !validRelativePath(path) || hasHiddenComponent(path) {
			return errors.New("agent changed a path that is not addressable")
		}
		if len(allowedPrefixes) > 0 && !allowedPath(path, allowedPrefixes) {
			return errors.New("agent changed a file outside the writable scope")
		}
		changed = append(changed, path)
		return nil
	}
	for index := 0; index < len(entries); index++ {
		entry := entries[index]
		if len(entry) < 4 {
			continue
		}
		if entry[0] == '!' && entry[1] == '!' {
			// An ignored file never enters the candidate, so a deliverable
			// that the repository ignores would otherwise vanish without a
			// trace - the run would report success and ship a PR with the
			// file missing. The check guards writable-scope files only:
			// - No scope (the reviewer's read-only run): nothing to protect.
			// - A directory entry (git collapses a matching ignored
			//   directory to one "dir/" record): that is a toolchain's
			//   byproduct - a dependency install or build output - not a
			//   deliverable. The first live run died on api/node_modules
			//   appearing when the implementer ran the repo's own tests.
			// - Hidden paths: a .DS_Store or an editor cache, never a
			//   deliverable.
			if len(allowedPrefixes) == 0 || strings.HasSuffix(entry, "/") {
				continue
			}
			path := entry[3:]
			if !hasHiddenComponent(path) && allowedPath(path, allowedPrefixes) && !isDeclaredByproduct(path, ignoredByproducts) {
				return nil, errors.New("the repository ignores a file inside the writable scope: " + path)
			}
			continue
		}
		if err := appendPath(entry[3:]); err != nil {
			return nil, err
		}
		// A rename or copy is two records: the new path above, then the
		// original path as its own bare entry. Both are part of what the
		// agent did to the tree, and reading the second record here is what
		// keeps it from being misread as a mangled path of its own.
		if entry[0] == 'R' || entry[0] == 'C' || entry[1] == 'R' || entry[1] == 'C' {
			index++
			if index >= len(entries) || entries[index] == "" {
				return nil, errors.New("changed files could not be read")
			}
			if err := appendPath(entries[index]); err != nil {
				return nil, err
			}
		}
	}
	sort.Strings(changed)
	return changed, nil
}

// isDeclaredByproduct reports whether the path's base name is one the
// consumer declared as toolchain residue.
func isDeclaredByproduct(path string, names []string) bool {
	base := path
	if index := strings.LastIndexByte(path, '/'); index >= 0 {
		base = path[index+1:]
	}
	for _, name := range names {
		if base == name {
			return true
		}
	}
	return false
}

func gitOutput(root string, arguments ...string) (string, error) {
	command := exec.Command("git", append([]string{"-C", root, "-c", "core.hooksPath=/dev/null"}, arguments...)...) // #nosec G204 -- fixed arguments.
	var out bytes.Buffer
	command.Stdout = &out
	command.Stderr = nil
	if err := command.Run(); err != nil {
		return "", err
	}
	return out.String(), nil
}

func validatedWorkspace(workspace string) (string, error) {
	root, err := filepath.Abs(workspace)
	if err != nil || filepath.Clean(root) != root {
		return "", errors.New("agent workspace is invalid")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("agent workspace is invalid")
	}
	return root, nil
}

// agentEnvironment builds the child environment from nothing: only PATH, HOME
// and the configured values, so the agent cannot read this process's other
// credentials.
func agentEnvironment(config AgentConfig) ([]string, error) {
	environment := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"LANG=C.UTF-8",
	}
	for name, value := range config.Env {
		environment = append(environment, name+"="+value)
	}
	for name, source := range config.SecretEnv {
		value := os.Getenv(source)
		if value == "" || strings.ContainsAny(value, "\r\n\x00") {
			return nil, errors.New("agent credential is unavailable")
		}
		environment = append(environment, name+"="+value)
	}
	sort.Strings(environment)
	return environment, nil
}

func boundedTranscript(value string) string {
	if len(value) <= MaxAgentTranscriptBytes {
		return value
	}
	return value[:MaxAgentTranscriptBytes]
}
