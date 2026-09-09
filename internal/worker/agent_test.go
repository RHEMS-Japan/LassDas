package worker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// buildAgentRepository makes a real repository with one committed file, which
// is what an agent is pointed at.
func buildAgentRepository(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "client", "src"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeAgentFile(t, root, "client/src/label.ts", "export const submitLabel = 'Send';\n")
	writeAgentFile(t, root, "README.md", "fixture\n")
	agentGit(t, root, "init", "--initial-branch=stg")
	agentGit(t, root, "add", "-A")
	agentGit(t, root, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "base")
	head := strings.TrimSpace(agentGit(t, root, "rev-parse", "HEAD"))
	return root, head
}

func agentGit(t *testing.T, root string, arguments ...string) string {
	t.Helper()
	// No background maintenance: after a commit, git starts a detached
	// `maintenance run --auto`, which takes .git/objects/maintenance.lock
	// and removes it when it finishes — while the test copies or walks the
	// tree. maintenance.auto=false keeps that child from starting (the
	// switch git consults); gc.auto=0 is the same guard for older git.
	command := exec.Command("git", append([]string{"-C", root, "-c", "gc.auto=0", "-c", "maintenance.auto=false"}, arguments...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
	return string(output)
}

func writeAgentFile(t *testing.T, root, relative, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeFakeAgent installs a script that stands in for a coding agent: it runs
// the given shell body in the workspace it is handed.
func writeFakeAgent(t *testing.T, body string) (string, string) {
	t.Helper()
	directory := t.TempDir()
	name := "fixture-agent"
	script := filepath.Join(directory, name)
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	return name, script
}

func fixtureAgentConfig(id, command string) AgentConfig {
	return AgentConfig{
		ID: id, Command: command, TimeoutSeconds: 120,
		Env:       map[string]string{"AGENT_ENDPOINT": "https://gateway.example.com/api"},
		SecretEnv: map[string]string{"AGENT_TOKEN": "FIXTURE_AGENT_CREDENTIAL"},
	}
}

func TestRunAgentReportsWhatTheAgentActuallyChanged(t *testing.T) {
	root, _ := buildAgentRepository(t)
	name, _ := writeFakeAgent(t, "printf \"export const submitLabel = 'Submit';\\n\" > client/src/label.ts; echo done")
	t.Setenv("FIXTURE_AGENT_CREDENTIAL", "secret-value")

	outcome, err := RunAgent(context.Background(), fixtureAgentConfig("author-agent", name), root, "do the thing", []string{"client/src/"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.ChangedFiles) != 1 || outcome.ChangedFiles[0] != "client/src/label.ts" {
		t.Fatalf("changed files were not reported: %v", outcome.ChangedFiles)
	}
	if outcome.ExitCode != 0 || !strings.Contains(outcome.Transcript, "done") {
		t.Fatalf("run was not recorded: exit=%d transcript=%q", outcome.ExitCode, outcome.Transcript)
	}
}

func TestRunAgentRejectsAChangeOutsideTheWritableScope(t *testing.T) {
	root, _ := buildAgentRepository(t)
	name, _ := writeFakeAgent(t, "printf 'edited\\n' > README.md")
	t.Setenv("FIXTURE_AGENT_CREDENTIAL", "secret-value")

	if _, err := RunAgent(context.Background(), fixtureAgentConfig("author-agent", name), root, "do the thing", []string{"client/src/"}, nil); err == nil {
		t.Fatal("a change outside the writable scope was accepted")
	}
}

// The agent must not be able to read this process's other credentials, which
// is what confines a compromised or careless agent to what it was given.
func TestRunAgentHandsTheAgentOnlyTheConfiguredEnvironment(t *testing.T) {
	root, _ := buildAgentRepository(t)
	name, _ := writeFakeAgent(t, "env | sort > client/src/label.ts")
	t.Setenv("FIXTURE_AGENT_CREDENTIAL", "secret-value")
	t.Setenv("UNRELATED_DEPLOY_TOKEN", "must-not-be-visible")

	if _, err := RunAgent(context.Background(), fixtureAgentConfig("author-agent", name), root, "do the thing", []string{"client/src/"}, nil); err != nil {
		t.Fatal(err)
	}
	captured, err := os.ReadFile(filepath.Join(root, "client", "src", "label.ts"))
	if err != nil {
		t.Fatal(err)
	}
	environment := string(captured)
	if strings.Contains(environment, "must-not-be-visible") || strings.Contains(environment, "UNRELATED_DEPLOY_TOKEN") {
		t.Fatal("the agent could read an unrelated credential")
	}
	if !strings.Contains(environment, "AGENT_TOKEN=secret-value") {
		t.Fatal("the agent did not receive the credential it needs")
	}
	if !strings.Contains(environment, "AGENT_ENDPOINT=https://gateway.example.com/api") {
		t.Fatal("the agent did not receive its configured endpoint")
	}
}

func TestRunAgentFailsWhenTheCredentialIsMissing(t *testing.T) {
	root, _ := buildAgentRepository(t)
	name, _ := writeFakeAgent(t, "true")
	t.Setenv("FIXTURE_AGENT_CREDENTIAL", "")

	if _, err := RunAgent(context.Background(), fixtureAgentConfig("author-agent", name), root, "do the thing", nil, nil); err == nil {
		t.Fatal("the agent ran without its credential")
	}
}

func TestRunAgentStopsAnAgentThatDoesNotFinish(t *testing.T) {
	root, _ := buildAgentRepository(t)
	name, _ := writeFakeAgent(t, "sleep 30")
	t.Setenv("FIXTURE_AGENT_CREDENTIAL", "secret-value")
	config := fixtureAgentConfig("author-agent", name)
	config.TimeoutSeconds = 60

	context, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := RunAgent(context, config, root, "do the thing", nil, nil); err == nil {
		t.Fatal("an unfinished agent run was accepted")
	}
	if time.Since(started) > 20*time.Second {
		t.Fatal("the run was not stopped")
	}
}

// The before-bytes must come from the sealed base revision, not from whatever
// the agent left behind, or an agent could rewrite its own starting point.
func TestReadObservedChangesTakesBeforeBytesFromTheUntouchedBase(t *testing.T) {
	root, _ := buildAgentRepository(t)
	base := copyAgentBase(t, root)
	writeAgentFile(t, root, "client/src/label.ts", "export const submitLabel = 'Submit';\n")

	observed, err := ReadObservedChanges(root, base, []string{"client/src/label.ts"}, fixtureConsumerForAgent())
	if err != nil {
		t.Fatal(err)
	}
	if len(observed) != 1 {
		t.Fatalf("expected one observed change, got %d", len(observed))
	}
	if string(observed[0].Before) != "export const submitLabel = 'Send';\n" {
		t.Fatalf("before-bytes did not come from the untouched base: %q", observed[0].Before)
	}
	if string(observed[0].After) != "export const submitLabel = 'Submit';\n" {
		t.Fatalf("after-bytes did not come from the working copy: %q", observed[0].After)
	}
}

// A created file is carried as Created with an empty before-side - adding a
// numbered migration file is ordinary development, and the first live
// migration ticket measurably died on the old outright rejection.
func TestReadObservedChangesCarriesACreatedFile(t *testing.T) {
	root, _ := buildAgentRepository(t)
	base := copyAgentBase(t, root)
	writeAgentFile(t, root, "client/src/new.ts", "export const added = true;\n")

	observed, err := ReadObservedChanges(root, base, []string{"client/src/new.ts"}, fixtureConsumerForAgent())
	if err != nil {
		t.Fatalf("ReadObservedChanges() error = %v", err)
	}
	if len(observed) != 1 || !observed[0].Created || len(observed[0].Before) != 0 ||
		string(observed[0].After) != "export const added = true;\n" {
		t.Fatalf("observed = %+v, want a created file with empty before-bytes", observed)
	}
}

// Creation stays scoped: a new file outside the writable prefixes is still
// rejected exactly like an edit outside them.
func TestReadObservedChangesRejectsACreatedFileOutsideTheScope(t *testing.T) {
	root, _ := buildAgentRepository(t)
	base := copyAgentBase(t, root)
	writeAgentFile(t, root, "outside/new.ts", "export const added = true;\n")

	if _, err := ReadObservedChanges(root, base, []string{"outside/new.ts"}, fixtureConsumerForAgent()); err == nil {
		t.Fatal("a created file outside the writable scope was accepted")
	}
}

func TestReadObservedChangesRejectsMoreFilesThanTheDestinationAllows(t *testing.T) {
	root, _ := buildAgentRepository(t)
	base := copyAgentBase(t, root)
	consumer := fixtureConsumerForAgent()
	consumer.Mode.MaxFiles = 1
	writeAgentFile(t, root, "client/src/label.ts", "a\n")

	if _, err := ReadObservedChanges(root, base, []string{"client/src/label.ts", "client/src/other.ts"}, consumer); err == nil {
		t.Fatal("a change larger than the destination allows was accepted")
	}
}

// copyAgentBase keeps an untouched copy of the base out of the agent's reach.
// Only the working tree is copied: git's background maintenance writes and
// removes lock files under .git after a commit, and a whole-repository copy
// racing it failed on the hosted runner.
func copyAgentBase(t *testing.T, root string) string {
	t.Helper()
	base := filepath.Join(t.TempDir(), "base")
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" && relative != "." {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(base, relative), 0o750)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(base, relative), content, 0o600)
	})
	if err != nil {
		t.Fatalf("base copy failed: %v", err)
	}
	return base
}

func fixtureConsumerForAgent() ConsumerConfig {
	return ConsumerConfig{
		Repository: "example/consumer", RepositoryID: 101,
		Delivery: DeliverPullRequest, IntegrationBranch: "stg", ReleaseBranch: "prod",
		Mode: ModeConfig{
			ID: "client-visible-change", AllowedFilePrefixes: []string{"client/src/"},
			MaxFiles: 3, MaxFileBytes: 256 * 1024, MaxTotalBytes: 512 * 1024,
			MaxChangedLines: 200, MaxChangedBytes: 64 * 1024,
		},
	}
}

// A staged rename is two records on the wire; misreading the second one used
// to reject the run for a mangled path that never existed.
func TestChangedFilesUnderReadsAStagedRenameAsBothPaths(t *testing.T) {
	root, _ := buildAgentRepository(t)
	agentGit(t, root, "mv", "client/src/label.ts", "client/src/renamed.ts")

	changed, err := ChangedFilesUnder(root, []string{"client/src/"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"client/src/label.ts", "client/src/renamed.ts"}
	if len(changed) != len(want) || changed[0] != want[0] || changed[1] != want[1] {
		t.Fatalf("changed = %v, want %v", changed, want)
	}
}

func TestChangedFilesUnderRejectsARenameLeavingTheScope(t *testing.T) {
	root, _ := buildAgentRepository(t)
	agentGit(t, root, "mv", "client/src/label.ts", "moved-out.ts")

	if _, err := ChangedFilesUnder(root, []string{"client/src/"}, nil); err == nil {
		t.Fatal("a rename out of the writable scope was accepted")
	}
}

// Configuration must not be able to shadow the variables this process sets:
// which duplicate wins would be up to the operating system.
func TestAgentConfigRejectsReservedEnvironmentNames(t *testing.T) {
	for _, name := range []string{"PATH", "HOME", "LANG", "TMPDIR"} {
		config := fixtureAgentConfig("author-agent", "fixture-agent")
		config.Env = map[string]string{name: "/tmp/shadow"}
		if err := config.validate(); err == nil {
			t.Fatalf("an agent overriding %s was accepted", name)
		}
		config = fixtureAgentConfig("author-agent", "fixture-agent")
		config.SecretEnv = map[string]string{name: "SOME_SOURCE"}
		if err := config.validate(); err == nil {
			t.Fatalf("an agent secret overriding %s was accepted", name)
		}
	}
}

// The retry gate is duration-based: a fast death is the upstream lottery and
// worth a fresh roll, a slow one is a budget problem and is not.
func TestRetryableReviewFailureIsDurationBased(t *testing.T) {
	if !RetryableReviewFailure(AgentOutcome{Duration: 45 * time.Second}) {
		t.Fatal("a fast death must be retryable")
	}
	if RetryableReviewFailure(AgentOutcome{Duration: ReviewRetryEligible}) {
		t.Fatal("a death at the ceiling must not be retryable")
	}
	if RetryableReviewFailure(AgentOutcome{Duration: 30 * time.Minute}) {
		t.Fatal("a timeout-class death must not be retryable")
	}
}

// A created deliverable that the repository ignores never enters the
// candidate, so it would ship as a silently missing file in the PR. The run
// must fail loudly instead. Measured live risk: a numbered SQL migration
// matching a *.generated.* ignore pattern.
func TestChangedFilesUnderFailsWhenACreatedFileIsIgnored(t *testing.T) {
	root, _ := buildAgentRepository(t)
	writeAgentFile(t, root, ".gitignore", "*.generated.ts\n")
	agentGit(t, root, "add", ".gitignore")
	agentGit(t, root, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "ignore rule")
	writeAgentFile(t, root, "client/src/messages.generated.ts", "export const x = 1;\n")

	_, err := ChangedFilesUnder(root, []string{"client/src/"}, nil)
	if err == nil {
		t.Fatal("an ignored created file inside the scope was silently dropped")
	}
	if !strings.Contains(err.Error(), "ignores") || !strings.Contains(err.Error(), "client/src/messages.generated.ts") {
		t.Fatalf("the failure does not name the ignored file: %v", err)
	}
}

// Ignored byproducts outside the writable scope, and hidden ones inside it,
// are the normal residue of a toolchain, not lost deliverables.
func TestChangedFilesUnderKeepsToleratingIrrelevantIgnoredFiles(t *testing.T) {
	root, _ := buildAgentRepository(t)
	writeAgentFile(t, root, ".gitignore", "node_modules/\n.DS_Store\n")
	agentGit(t, root, "add", ".gitignore")
	agentGit(t, root, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "ignore rule")
	writeAgentFile(t, root, "node_modules/left/pad.js", "module.exports = 1\n")
	writeAgentFile(t, root, "client/src/.DS_Store", "junk\n")
	writeAgentFile(t, root, "client/src/label.ts", "export const submitLabel = 'Submit';\n")

	changed, err := ChangedFilesUnder(root, []string{"client/src/"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || changed[0] != "client/src/label.ts" {
		t.Fatalf("changed = %v", changed)
	}
}

// The reviewer's runs pass no writable scope. No scope means nothing to
// protect from ignore rules - and the implementer's legitimate byproducts
// outside the scope (node_modules, dist) must not kill the review that
// follows it in the same workspace.
func TestChangedFilesUnderIgnoreCheckIsScopedToWritablePrefixes(t *testing.T) {
	root, _ := buildAgentRepository(t)
	writeAgentFile(t, root, ".gitignore", "node_modules/\n")
	agentGit(t, root, "add", ".gitignore")
	agentGit(t, root, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "ignore rule")
	writeAgentFile(t, root, "node_modules/left/pad.js", "module.exports = 1\n")

	changed, err := ChangedFilesUnder(root, nil, nil)
	if err != nil {
		t.Fatalf("a run without a writable scope must not police ignore rules: %v", err)
	}
	if len(changed) != 0 {
		t.Fatalf("changed = %v, want none", changed)
	}
}

// The implementer runs the repository's own tests, and a dependency install
// drops an ignored directory inside the writable scope (api/node_modules on
// the first live run). A collapsed ignored directory is a toolchain's
// byproduct, never a deliverable - only ignored files fail the run.
func TestChangedFilesUnderToleratesIgnoredDirectoriesInsideTheScope(t *testing.T) {
	root, _ := buildAgentRepository(t)
	writeAgentFile(t, root, ".gitignore", "client/src/node_modules\nclient/src/generated/\n")
	agentGit(t, root, "add", ".gitignore")
	agentGit(t, root, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "ignore rule")
	writeAgentFile(t, root, "client/src/node_modules/left/pad.js", "module.exports = 1\n")
	writeAgentFile(t, root, "client/src/generated/out.js", "generated\n")
	writeAgentFile(t, root, "client/src/label.ts", "export const submitLabel = 'Submit';\n")

	changed, err := ChangedFilesUnder(root, []string{"client/src/"}, nil)
	if err != nil {
		t.Fatalf("an ignored directory inside the scope killed the run: %v", err)
	}
	if len(changed) != 1 || changed[0] != "client/src/label.ts" {
		t.Fatalf("changed = %v", changed)
	}
}

// A lockfile from the wrong package manager is toolchain residue the
// consumer can declare, not a swallowed deliverable: declared names pass,
// undeclared ignored files still fail the run. Measured live: an
// implementer ran npm in a pnpm repository and its package-lock.json
// killed a finished implementation at the tally.
func TestChangedFilesUnderToleratesDeclaredByproducts(t *testing.T) {
	root, _ := buildAgentRepository(t)
	writeAgentFile(t, root, ".gitignore", "package-lock.json\n")
	agentGit(t, root, "add", ".gitignore")
	agentGit(t, root, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "ignore rule")
	writeAgentFile(t, root, "client/src/package-lock.json", "{}\n")
	writeAgentFile(t, root, "client/src/label.ts", "export const submitLabel = 'Submit';\n")

	if _, err := ChangedFilesUnder(root, []string{"client/src/"}, nil); err == nil {
		t.Fatal("an undeclared ignored file inside the scope was tolerated")
	}
	changed, err := ChangedFilesUnder(root, []string{"client/src/"}, []string{"package-lock.json"})
	if err != nil {
		t.Fatalf("a declared byproduct killed the run: %v", err)
	}
	if len(changed) != 1 || changed[0] != "client/src/label.ts" {
		t.Fatalf("changed = %v", changed)
	}
}

// With the launcher configured, the worker runs the agent through it —
// workspace, agent home and the command after "--" — and asks it to return
// the workspace afterwards; the agent's home is the agent's, not ours.
func TestRunAgentProcessGoesThroughTheLauncher(t *testing.T) {
	root, _ := buildAgentRepository(t)
	record := filepath.Join(t.TempDir(), "launcher.log")
	launcher := filepath.Join(t.TempDir(), "fake-agentexec")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + record + "\nif [ \"$1\" = \"--reclaim\" ]; then exit 0; fi\nwhile [ \"$1\" != \"--\" ]; do shift; done; shift\necho \"HOME=$HOME\"\necho \"TREE=$LASSDAS_AGENT_TREE_ROOT\"\nexec \"$@\"\n"
	if err := os.WriteFile(launcher, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	// What the engine's home holds for the agent's program: its Hermes
	// profile and the knowledge rules the worker placed.
	engineHome := t.TempDir()
	t.Setenv("HOME", engineHome)
	for relative, content := range map[string]string{
		".hermes/profiles/stand-in/config.yaml": "model: stand-in\n",
		".claude/RULES.md":                      "the rule\n",
	} {
		target := filepath.Join(engineHome, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(AgentLauncherEnv, launcher)
	t.Setenv(AgentTreeRootEnv, filepath.Dir(root))
	t.Setenv("LASSDAS_STATE_DIR", t.TempDir())
	t.Setenv("FIXTURE_AGENT_CREDENTIAL", "credential")
	name, _ := writeFakeAgent(t, `echo "agent ran in $(pwd)"; echo "TMPDIR=$TMPDIR"; test -d "$TMPDIR" && echo "tmpdir exists"; cat "$HOME/.hermes/profiles/stand-in/config.yaml" "$HOME/.claude/RULES.md"`)
	config := fixtureAgentConfig("author", name)
	config.Args = []string{"--profile", "stand-in"}
	config.Profile = "stand-in"
	config.Knowledge.Rules = []KnowledgePlacement{{From: "rules/RULES.md", To: ".claude/RULES.md"}}
	outcome, _, err := runAgentProcess(context.Background(), config, root, "do the thing", nil)
	if err != nil {
		t.Fatalf("runAgentProcess() error = %v (%s)", err, outcome.Transcript)
	}
	logged, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(logged)), "\n")
	// The first free agent user of an empty pool; the launch names it.
	launchPrefix := "--uid 2001 --workspace " + root + " --home "
	if len(lines) != 4 || lines[0] != "--reclaim "+root || !strings.HasPrefix(lines[1], launchPrefix) {
		t.Fatalf("launcher calls = %q", lines)
	}
	home := strings.TrimPrefix(lines[1], launchPrefix)
	home = home[:strings.Index(home, " -- ")]
	// The home is made for this launch, beside the workspace, and seeded
	// with what the program reads; both come back when the run ends, and
	// the home is removed.
	if !strings.HasPrefix(home, filepath.Join(filepath.Dir(root), "agent-home", "author-")) {
		t.Fatalf("agent home = %q, want one made under the run directory", home)
	}
	if !strings.HasSuffix(lines[1], " -- "+name+" --profile stand-in do the thing") || lines[2] != "--reclaim "+home || lines[3] != "--reclaim "+root {
		t.Fatalf("launcher calls = %q", lines)
	}
	if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the launch home was kept: %v", err)
	}
	for _, want := range []string{"agent ran in", "HOME=" + home, "TREE=" + filepath.Dir(root), "TMPDIR=" + filepath.Join(home, "tmp"), "tmpdir exists", "model: stand-in", "the rule"} {
		if !strings.Contains(outcome.Transcript, want) {
			t.Fatalf("the agent did not run through the launcher with its seeded home: %q lacks %q", outcome.Transcript, want)
		}
	}
	// Unset, the agent runs as us, at home.
	t.Setenv(AgentLauncherEnv, "")
	if agentLauncher() != "" {
		t.Fatal("an unset launcher was reported set")
	}
}

// The session jar paths and every other variable of this process never
// reach an agent: its environment is the launch definition's alone.
func TestAgentEnvironmentNeverCarriesTheSessionJar(t *testing.T) {
	t.Setenv("LASSDAS_E2E_SESSION_FILE", "/etc/lassdas-e2e/session.json")
	t.Setenv("LASSDAS_E2E_SESSION_STATE_FILE", "/data/e2e-session/session.json")
	t.Setenv("TARGET_GITHUB_TOKEN", "should-not-leak")
	t.Setenv("FIXTURE_AGENT_CREDENTIAL", "credential")
	environment, err := agentEnvironment(fixtureAgentConfig("author", "fixture-agent"), "/data/agent-home")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(environment, "\n")
	for _, forbidden := range []string{"E2E_SESSION", "TARGET_GITHUB_TOKEN", "should-not-leak", "HOME=" + os.Getenv("HOME")} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("the agent environment carries %q: %q", forbidden, environment)
		}
	}
	// Exactly the launch definition's variables plus PATH, HOME and LANG.
	names := map[string]bool{}
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		names[name] = true
	}
	for name := range names {
		switch name {
		case "PATH", "HOME", "LANG", "AGENT_ENDPOINT", "AGENT_TOKEN":
		default:
			t.Fatalf("the agent environment carries a variable of this process: %s", name)
		}
	}
	if !strings.Contains(joined, "HOME=/data/agent-home") || !strings.Contains(joined, "AGENT_TOKEN=credential") {
		t.Fatalf("the agent environment lacks its home or its own credential: %q", environment)
	}
}

// Two agents running at once are different users: the pool hands out the
// first free user and frees it when the holder lets go (or dies).
func TestAgentUsersAreDistinctWhileHeld(t *testing.T) {
	t.Setenv("LASSDAS_STATE_DIR", t.TempDir())
	first, err := acquireAgentUser()
	if err != nil {
		t.Fatal(err)
	}
	second, err := acquireAgentUser()
	if err != nil {
		t.Fatal(err)
	}
	if first.uid != agentUIDBase || second.uid != agentUIDBase+1 {
		t.Fatalf("users = %d, %d; want %d and %d", first.uid, second.uid, agentUIDBase, agentUIDBase+1)
	}
	first.release()
	third, err := acquireAgentUser()
	if err != nil {
		t.Fatal(err)
	}
	defer third.release()
	defer second.release()
	if third.uid != agentUIDBase {
		t.Fatalf("a released user was not reused: got %d", third.uid)
	}
}

// Files a launch needs in the agent's home (the measurements a reviewer
// must read whole) are copied read-only; a path outside the home or a
// missing source is refused rather than leaving the reviewer to judge on
// the excerpt.
func TestCopyHomeFilesPlacesReadOnlyCopies(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(t.TempDir(), "measurements.jsonl")
	if err := os.WriteFile(source, []byte("{\"id\":\"m-0001\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyHomeFiles(home, map[string]string{"measurements.jsonl": source}); err != nil {
		t.Fatalf("copyHomeFiles: %v", err)
	}
	info, err := os.Stat(filepath.Join(home, "measurements.jsonl"))
	if err != nil || info.Mode().Perm() != 0o444 {
		t.Fatalf("copy = %v, %v; want a 0444 file", info, err)
	}
	if err := copyHomeFiles(home, map[string]string{"../escape": source}); err == nil {
		t.Error("a path outside the home was accepted")
	}
	if err := copyHomeFiles(home, map[string]string{"missing.jsonl": filepath.Join(t.TempDir(), "nope")}); err == nil {
		t.Error("a missing source was accepted")
	}
	if err := copyHomeFiles(home, nil); err != nil {
		t.Errorf("no files: %v", err)
	}
}
