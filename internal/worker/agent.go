package worker

import (
	"automation.internal/ticket-ingress/internal/probe"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
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
	// budget kill ends the whole chain as model_failed. a live ticket's second
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

// reservedEnvName rejects the variables this process sets itself (TMPDIR
// among them: the agent's temporary files stay in its own home). Letting
// configuration shadow them would hand the outcome to whichever duplicate
// the operating system happens to prefer.
func reservedEnvName(name string) bool {
	return name == "PATH" || name == "HOME" || name == "LANG" || name == "TMPDIR"
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
	outcome, root, err := runAgentProcess(ctx, config, workspace, prompt, nil, "")
	if err != nil {
		return outcome, err
	}
	changed, err := ChangedFilesUnder(root, allowedPrefixes, ignoredByproducts)
	if err != nil {
		return outcome, err
	}
	outcome.ChangedFiles = changed
	return outcome, nil
}

// RunReviewingAgentWithHomeFiles runs a reviewer without scanning the tree
// afterwards (a reviewer's changes are not a deliverable, and the strict
// scan died on the hidden caches a reviewer's tooling leaves behind, killing
// reviews that had passed; what a reviewer may do to the tree is judged by
// ConfirmTreeMatchesCandidate against the sealed candidate instead), with
// files placed in
// the home made for the launch (relative path in the home → source path),
// readable to the agent user: what a reviewer must read whole but cannot
// open where the engine keeps it (the measurements, 0600 to the engine).
// Without a launcher the agent shares this user's home and the files are
// not copied; AgentLauncherConfigured tells the caller which path to name.
func RunReviewingAgentWithHomeFiles(ctx context.Context, config AgentConfig, workspace, prompt string, homeFiles map[string]string, homeToken string) (AgentOutcome, error) {
	outcome, _, err := runAgentProcess(ctx, config, workspace, prompt, homeFiles, homeToken)
	return outcome, err
}

// AgentLauncherConfigured reports whether agents run under their own user
// with a home made per launch.
func AgentLauncherConfigured() bool { return agentLauncher() != "" }

// agentHomeTokenPattern is the shape NewAgentHomeToken draws. Checking it
// catches a caller that went back to a fixed marker; the protection from
// data that happens to contain a stand-in is the ninety-six bits of
// randomness, not this pattern.
var agentHomeTokenPattern = regexp.MustCompile(`^\{\{AGENT_HOME:[0-9a-f]{24}\}\}$`)

// NewAgentHomeToken returns the stand-in a prompt uses for the home the
// launch will make; runAgentProcess replaces it with the real path once the
// home exists. The prompt reaches the agent as an argument, not through a
// shell, so "$HOME" would arrive unexpanded. The token is drawn per launch
// rather than fixed: a prompt carries recorded output of the repository
// under work, that output can contain any fixed marker this engine defines
// (its own source does), and replacing a marker inside a record would hand
// the reviewer an excerpt that no longer matches the sealed record it
// judges (review of #101, 2026-09-09).
func NewAgentHomeToken() string {
	buffer := make([]byte, 12)
	if _, err := rand.Read(buffer); err != nil {
		// A launch without a usable token names no home; the caller's
		// prompt then carries no stand-in and nothing is replaced.
		return ""
	}
	return "{{AGENT_HOME:" + hex.EncodeToString(buffer) + "}}"
}

// MaxAgentHomeFileBytes bounds a file copied into a launch's home. The
// records file a design reviewer reads is the largest of them, and a round
// may store up to the measurement budget, so the bound is derived from that
// budget rather than written down beside it: a bound below the budget fails
// every review of a run that measured to it, before the agent even starts
// (review of #101). The factor covers what JSON encoding adds to stored
// output.
var MaxAgentHomeFileBytes = 2 * probe.DefaultLimits.MaxTotalBytes

// AgentHomePathReserve is the room a prompt using an agent home token
// leaves for the real path: the placeholder is short and the home is a
// path, so the replacement grows the prompt. A generator that fits a prompt
// to MaxAgentPromptBytes - AgentHomePathReserve is safe to launch; a
// generator that filled the limit exactly used to have its launch refused
// as invalid input, which the caller then retried twice and failed the card
// (review of #101, 2026-09-09).
const AgentHomePathReserve = 1024

func runAgentProcess(ctx context.Context, config AgentConfig, workspace, prompt string, homeFiles map[string]string, homeToken string) (AgentOutcome, string, error) {
	if ctx == nil || prompt == "" || len(prompt) > MaxAgentPromptBytes {
		return AgentOutcome{}, "", errors.New("agent input is invalid")
	}
	if err := config.validate(); err != nil {
		return AgentOutcome{}, "", err
	}
	root, err := validatedWorkspace(workspace)
	if err != nil {
		return AgentOutcome{}, "", err
	}
	launcher := agentLauncher()
	agentHome := os.Getenv("HOME")
	var user *agentUser
	if launcher != "" {
		// A launch that died with the pod left the tree to the agent user;
		// it comes back before it is lent again.
		reclaimWorkspace(launcher, root)
		agentHome, err = prepareAgentHome(config, root)
		if err != nil {
			return AgentOutcome{}, "", err
		}
		if err := copyHomeFiles(agentHome, homeFiles); err != nil {
			_ = os.RemoveAll(agentHome)
			return AgentOutcome{}, "", err
		}
		user, err = acquireAgentUser()
		if err != nil {
			_ = os.RemoveAll(agentHome)
			return AgentOutcome{}, "", err
		}
		defer user.release()
	}
	// The prompt may name the home this launch made (a reviewer's copy of
	// the measurements lives there); the real path is known only now. The
	// replacement is allowed to spend the reserve the generator left, so a
	// prompt that was legal before the launch is not refused after it. What
	// is refused is growth beyond the reserve: the placeholder can also
	// arrive inside the data a prompt carries (a record of a repository
	// that contains this text), and that must not inflate the launch.
	if homeToken != "" && !agentHomeTokenPattern.MatchString(homeToken) {
		// Only a token of the shape this run draws may be replaced: a fixed
		// marker would
		// also match text the prompt merely carries (a record of a
		// repository whose source defines one), and rewriting that hands
		// the reviewer an excerpt its sealed record does not match.
		if launcher != "" {
			_ = os.RemoveAll(agentHome)
		}
		return AgentOutcome{}, "", errors.New("the launch home stand-in is not one this run drew")
	}
	if occurrences := 0; homeToken != "" {
		occurrences = strings.Count(prompt, homeToken)
		growth := occurrences * (len(agentHome) - len(homeToken))
		if growth > AgentHomePathReserve || len(prompt)+growth > MaxAgentPromptBytes+AgentHomePathReserve {
			if launcher != "" {
				// This launch made the home; nothing else will remove it.
				_ = os.RemoveAll(agentHome)
			}
			return AgentOutcome{}, "", errors.New("agent input is invalid")
		}
		prompt = strings.ReplaceAll(prompt, homeToken, agentHome)
	}
	environment, err := agentEnvironment(config, agentHome)
	if err != nil {
		return AgentOutcome{}, "", err
	}
	if launcher != "" {
		// The agent's temporary files live in its own home, not in the
		// /tmp every user shares; the tree root reaches the launcher (which
		// keeps it from the agent) so its bounds hold at the launch too.
		environment = append(environment, "TMPDIR="+filepath.Join(agentHome, "tmp"))
		if root := os.Getenv(AgentTreeRootEnv); root != "" {
			environment = append(environment, AgentTreeRootEnv+"="+root)
		}
	}

	runContext, cancel := context.WithTimeout(ctx, time.Duration(config.TimeoutSeconds)*time.Second)
	defer cancel()

	arguments := append(append([]string(nil), config.Args...), prompt)
	program := config.Command
	if launcher != "" {
		// The agent runs as the agent user: the launcher lends it the
		// workspace and the home made for this launch, and takes the
		// workspace back when the agent exits (docs/RUNTIME_POD.md,
		// "Agents under their own user"); both come back here too, for an
		// agent the engine killed together with the launcher, and so the
		// engine can read what the agent left in its home.
		arguments = append([]string{"--uid", strconv.Itoa(int(user.uid)), "--workspace", root, "--home", agentHome, "--", config.Command}, arguments...)
		program = launcher
		defer reclaimWorkspace(launcher, root)
		defer func() {
			// The home made for this launch is taken back and removed: the
			// run record holds the transcript, and nothing of a launch is
			// left for the next one to read.
			reclaimWorkspace(launcher, agentHome)
			if err := os.RemoveAll(agentHome); err != nil {
				fmt.Fprintf(os.Stderr, "worker: launch home not removed: %v\n", err)
			}
		}()
	}
	command := exec.CommandContext(runContext, program, arguments...) // #nosec G204 -- command and arguments come from validated configuration.
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
		if launcher != "" {
			// The agent user's processes are out of this user's reach: the
			// launcher is asked to stop, kills the agent's own process group,
			// and the agent dies with the launcher in any case (the
			// parent-death signal); WaitDelay below kills a launcher that
			// does not answer.
			return syscall.Kill(command.Process.Pid, syscall.SIGTERM)
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
		return outcome, root, errors.New("agent run exceeded its time limit")
	}
	if runErr != nil {
		return outcome, root, errors.New("agent run failed")
	}
	return outcome, root, nil
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
	var out, problems bytes.Buffer
	command.Stdout = &out
	command.Stderr = &problems
	if err := command.Run(); err != nil {
		// git says why it refused on stderr ("not a git repository", a lock
		// held by another process); without it a failure is just "exit 128".
		detail := strings.TrimSpace(problems.String())
		if len(detail) > 512 {
			detail = detail[:512]
		}
		if detail != "" {
			return "", fmt.Errorf("%w: %s", err, detail)
		}
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

// The agent's environment is built from nothing (agentEnvironment): only
// PATH, a home, a locale and the configured values, so the agent cannot
// read this process's other credentials — nor the session jar paths, the
// tracker key or the destination token, which a prompt-injected agent
// would otherwise read.
//
// AgentLauncherEnv names the launcher that runs agents as the agent user
// (docs/RUNTIME_POD.md, "Agents under their own user"). Unset, agents run
// as this process does, at its home — the runner mode outside the pod.
const AgentLauncherEnv = "LASSDAS_AGENT_LAUNCHER"

// AgentTreeRootEnv names the directory under which the launcher lends and
// returns trees; the worker hands it to the launcher at every launch.
const AgentTreeRootEnv = "LASSDAS_AGENT_TREE_ROOT"

func agentLauncher() string { return os.Getenv(AgentLauncherEnv) }

// reclaimWorkspace asks the launcher to return a workspace to this user.
// The launcher does it itself when the agent exits; this covers an agent
// the engine killed together with the launcher (the process group).
func reclaimWorkspace(launcher, root string) {
	if output, err := exec.Command(launcher, "--reclaim", root).CombinedOutput(); err != nil { // #nosec G204 -- the configured launcher.
		fmt.Fprintf(os.Stderr, "worker: workspace not reclaimed: %v: %s\n", err, strings.TrimSpace(string(output)))
	}
}

// The agent users the image carries: agent1 … agent63, uid 2001 to 2063,
// all in the agent group (agent, uid 2000, is the boot check's probe user
// and runs no launch, so a launch returning its tree never stops a
// probe). A launch holds one for its life, so two agents running at once
// are different users: neither can read the other's processes (their
// environment, their keys), workspace or home. The pool is a directory of
// lock files under the state directory; a lock dies with the worker that
// holds it, so a crash frees the user.
const (
	agentUIDBase  = 2001
	agentUIDCount = 63
)

type agentUser struct {
	uid  uint32
	lock *os.File
}

func (u *agentUser) release() {
	if u != nil && u.lock != nil {
		_ = u.lock.Close()
	}
}

// acquireAgentUser takes the first free agent user, or fails closed when
// every one is in use.
func acquireAgentUser() (*agentUser, error) {
	root, err := agentPoolRoot()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(root, "agent-uids")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, errors.New("the agent user pool is unavailable")
	}
	for i := 0; i < agentUIDCount; i++ {
		uid := agentUIDBase + i
		file, err := os.OpenFile(filepath.Join(dir, strconv.Itoa(uid)), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, errors.New("the agent user pool is unavailable")
		}
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return &agentUser{uid: uint32(uid), lock: file}, nil
		}
		_ = file.Close()
	}
	return nil, errors.New("every agent user is in use")
}

// agentPoolRoot is where the pool's lock files live: the state directory
// of the pod, and nowhere else — a shared temporary directory would let
// an agent plant the lock files first.
func agentPoolRoot() (string, error) {
	if state := os.Getenv("LASSDAS_STATE_DIR"); state != "" {
		return state, nil
	}
	return "", errors.New("the agent user pool needs LASSDAS_STATE_DIR")
}

// ReclaimWorkspace returns a tree to this user when a launcher is
// configured — for a caller about to clean or read a workspace that a
// launch which died with the pod may have left to the agent user. Without
// a launcher there is nothing to return.
func ReclaimWorkspace(root string) {
	if launcher := agentLauncher(); launcher != "" {
		reclaimWorkspace(launcher, root)
	}
}

// agentHomeSeeds are the files of this user's home an agent's program
// reads from its own: a reviewer program's configuration the runner wrote.
// The Hermes profile and the knowledge rules are added per agent.
var agentHomeSeeds = []string{".codex/config.toml"}

// prepareAgentHome makes the home one launch gets under the agent user: a
// fresh directory beside the workspace (`agent-home/<id>-…` in the run
// directory), seeded from this user's home with what the agent's program
// reads there — its Hermes profile (config.yaml), the knowledge rules the
// worker placed, a reviewer program's configuration. Nothing an agent
// wrote into an earlier home reaches the next launch, and two agents
// running at once never share one. The launcher lends the directory to
// the agent user; the worker takes it back when the run ends, so the
// engine can read what was left and the next dispatch can clear it.
func prepareAgentHome(config AgentConfig, root string) (string, error) {
	base := filepath.Join(filepath.Dir(root), "agent-home")
	if err := os.MkdirAll(base, 0o711); err != nil {
		return "", errors.New("agent home could not be prepared")
	}
	home, err := os.MkdirTemp(base, config.ID+"-")
	if err != nil {
		return "", errors.New("agent home could not be prepared")
	}
	if err := os.Mkdir(filepath.Join(home, "tmp"), 0o700); err != nil {
		return "", errors.New("agent home could not be prepared")
	}
	seeds := append([]string(nil), agentHomeSeeds...)
	if config.Profile != "" {
		seeds = append(seeds, path.Join(".hermes", "profiles", config.Profile, "config.yaml"))
	}
	for _, rule := range config.Knowledge.Rules {
		seeds = append(seeds, rule.To)
	}
	engineHome := os.Getenv("HOME")
	for _, relative := range seeds {
		if err := copyHomeSeed(engineHome, home, relative); err != nil {
			return "", err
		}
	}
	return home, nil
}

// copyHomeFiles places the caller's files in the agent's home, read-only to
// everyone (the agent user reads them; the engine took them from its own
// files). A relative path outside the home or a source that cannot be read
// is an error: a reviewer pointed at a file that is not there would judge
// on the excerpt and call the rest absent.
func copyHomeFiles(agentHome string, files map[string]string) error {
	for relative, source := range files {
		if !validAgentHomePath(relative) {
			return errors.New("agent home file path is invalid")
		}
		content, err := ReadBoundedRegularFile(source, int64(MaxAgentHomeFileBytes))
		if err != nil {
			return errors.New("agent home file could not be read")
		}
		target := filepath.Join(agentHome, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return errors.New("agent home could not be prepared")
		}
		if err := os.WriteFile(target, content, 0o444); err != nil {
			return errors.New("agent home could not be prepared")
		}
	}
	return nil
}

// copyHomeSeed copies one file of the engine's home into the agent's,
// readable to the agent user; a seed that does not exist is not an error
// (the program does not read it then).
func copyHomeSeed(engineHome, agentHome, relative string) error {
	if !validAgentHomePath(relative) {
		return errors.New("agent home seed path is invalid")
	}
	content, err := os.ReadFile(filepath.Join(engineHome, filepath.FromSlash(relative)))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return errors.New("agent home seed could not be read")
	}
	target := filepath.Join(agentHome, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return errors.New("agent home could not be prepared")
	}
	if err := os.WriteFile(target, content, 0o644); err != nil {
		return errors.New("agent home could not be prepared")
	}
	return nil
}

func agentEnvironment(config AgentConfig, home string) ([]string, error) {
	environment := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
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
