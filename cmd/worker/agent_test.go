package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/worker"
)

// agentFixture is one run of the real commands against real processes: an
// agent that edits the working copy, and a reviewer that reads it and reports.
type agentFixture struct {
	directory  string
	configPath string
	draftPath  string
	repoRoot   string
	baseRoot   string
	baseSHA    string
	config     worker.Config
}

// writeStandInAgent installs a script that stands in for a coding agent. The
// framework does not know what an agent is beyond "a program run in a working
// copy", so a script exercises the same path the real binaries take.
func writeStandInAgent(t *testing.T, directory, name, body string) {
	t.Helper()
	script := filepath.Join(directory, name)
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func newAgentFixture(t *testing.T, implementerBody, reviewerBody string) agentFixture {
	t.Helper()
	directory := t.TempDir()
	binaries := filepath.Join(directory, "bin")
	if err := os.MkdirAll(binaries, 0o700); err != nil {
		t.Fatal(err)
	}
	writeStandInAgent(t, binaries, "stand-in-author", implementerBody)
	writeStandInAgent(t, binaries, "stand-in-reviewer", reviewerBody)
	t.Setenv("PATH", binaries+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TEST_MODEL_KEY_A", "author-credential")
	t.Setenv("TEST_MODEL_KEY_B", "reviewer-credential")

	config := cliTestConfig()
	config.Agents.Implementer.Command = "stand-in-author"
	config.Agents.Reviewer.Command = "stand-in-reviewer"
	configPath := filepath.Join(directory, "config.json")
	writeTestJSON(t, configPath, config)

	envelopePath := filepath.Join(directory, "envelope.json")
	draftPath := filepath.Join(directory, "draft.json")
	writeTestJSON(t, envelopePath, cliTestEnvelopeWithDescription(t, strings.Join([]string{
		"Automation-Run-ID: run_20260802_alpha",
		"Automation-Mode: client-visible-change",
		"Verification-Path: /settings",
		"Expected-Text: Updated label",
		"Absent-Text: Old label",
		"---",
		"Please reword the visible label.",
	}, "\n")))
	if err := run(context.Background(), []string{
		"parse-ticket", "--config", configPath, "--tool-sha", cliToolSHA, "--envelope", envelopePath,
		"--draft-out", draftPath, "--out", filepath.Join(directory, "ticket-unused.json"),
	}); err != nil {
		t.Fatal(err)
	}

	repoRoot := filepath.Join(directory, "repo")
	target := filepath.Join(repoRoot, "client", "src", "label.ts")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("export const label = 'Old label';\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runCLITestGit(t, repoRoot, "init", "-q")
	runCLITestGit(t, repoRoot, "add", "-A")
	runCLITestGit(t, repoRoot, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-qm", "fixture")

	// The base the agent started from is kept out of its reach, which is where
	// the before-bytes of every change are read from.
	baseRoot := filepath.Join(directory, "base")
	if output, err := exec.Command("cp", "-R", repoRoot, baseRoot).CombinedOutput(); err != nil {
		t.Fatalf("base copy failed: %v: %s", err, output)
	}
	if err := os.RemoveAll(filepath.Join(baseRoot, ".git")); err != nil {
		t.Fatal(err)
	}

	return agentFixture{
		directory: directory, configPath: configPath, draftPath: draftPath, repoRoot: repoRoot,
		baseRoot: baseRoot, baseSHA: strings.TrimSpace(runCLITestGit(t, repoRoot, "rev-parse", "HEAD")), config: config,
	}
}

func (f agentFixture) path(name string) string { return filepath.Join(f.directory, name) }

func (f agentFixture) implement(t *testing.T) error {
	t.Helper()
	return run(context.Background(), []string{
		"implement", "--config", f.configPath, "--tool-sha", cliToolSHA, "--draft", f.draftPath,
		"--repo-root", f.repoRoot, "--base-root", f.baseRoot, "--base-sha", f.baseSHA, "--stage", "1",
		"--run-out", f.path("run.json"), "--ticket-out", f.path("ticket.json"),
		"--source-out", f.path("source.json"), "--out", f.path("candidate.json"),
	})
}

func (f agentFixture) review(t *testing.T) error {
	t.Helper()
	return run(context.Background(), []string{
		"agent-review", "--config", f.configPath, "--tool-sha", cliToolSHA,
		"--ticket", f.path("ticket.json"), "--source", f.path("source.json"),
		"--candidate", f.path("candidate.json"), "--reviewer", "review-b",
		"--repo-root", f.repoRoot, "--base-sha", f.baseSHA,
		"--run-out", f.path("review-run.json"), "--out", f.path("review.json"),
	})
}

const editTheLabel = `printf "export const label = 'Updated label';\n" > client/src/label.ts; echo "I changed the label."`

// The whole point of running an agent is that nobody names the file in
// advance: the agent finds it, and the contract is completed from what it
// actually changed.
func TestImplementSealsWhatTheAgentChangedIntoTheUsualArtifacts(t *testing.T) {
	fixture := newAgentFixture(t, editTheLabel, "true")
	if err := fixture.implement(t); err != nil {
		t.Fatal(err)
	}

	var request worker.TicketRequest
	var source worker.SourceSnapshot
	var candidate worker.Candidate
	readAgentArtifact(t, fixture.path("ticket.json"), worker.MaxTicketJSONBytes, &request)
	readAgentArtifact(t, fixture.path("source.json"), worker.MaxArtifactJSONBytes, &source)
	readAgentArtifact(t, fixture.path("candidate.json"), worker.MaxArtifactJSONBytes, &candidate)

	if len(request.TargetFiles) != 1 || request.TargetFiles[0] != "client/src/label.ts" {
		t.Fatalf("the contract did not follow what the agent changed: %v", request.TargetFiles)
	}
	if err := candidate.Validate(source, request, fixture.config); err != nil {
		t.Fatalf("the sealed candidate was rejected: %v", err)
	}
	if source.Files[0].Content != "export const label = 'Old label';\n" {
		t.Fatalf("the before-bytes were not taken from the base revision: %q", source.Files[0].Content)
	}
	if candidate.Files[0].Content != "export const label = 'Updated label';\n" {
		t.Fatalf("the after-bytes were not taken from the working copy: %q", candidate.Files[0].Content)
	}
	if !strings.Contains(candidate.Rationale, "I changed the label.") {
		t.Fatalf("what the agent said was not recorded: %q", candidate.Rationale)
	}
}

// An agent that writes outside what the destination allows must not produce a
// candidate, whatever it says it did.
func TestImplementRejectsAnAgentThatWritesOutsideTheAllowedScope(t *testing.T) {
	fixture := newAgentFixture(t, `printf 'x\n' > outside.txt; echo "done"`, "true")
	if err := fixture.implement(t); err == nil {
		t.Fatal("a change outside the allowed scope produced a candidate")
	}
	if _, err := os.Stat(fixture.path("candidate.json")); err == nil {
		t.Fatal("a candidate was written for a rejected run")
	}
}

func TestImplementRejectsAnAgentThatChangesNothing(t *testing.T) {
	fixture := newAgentFixture(t, `echo "I decided nothing needed changing."`, "true")
	if err := fixture.implement(t); err == nil {
		t.Fatal("a run that changed nothing produced a candidate")
	}
}

// A failed agent still leaves the record of what happened, because that record
// is how a failed run is diagnosed.
func TestImplementRecordsARunThatFailed(t *testing.T) {
	fixture := newAgentFixture(t, `echo "I could not reach the model"; exit 1`, "true")
	if err := fixture.implement(t); err == nil {
		t.Fatal("a failed agent run produced a candidate")
	}
	var record worker.AgentRun
	readAgentArtifact(t, fixture.path("run.json"), worker.MaxArtifactJSONBytes, &record)
	if record.ExitCode != 1 || !strings.Contains(record.Transcript, "could not reach the model") {
		t.Fatalf("the failed run was not recorded: %+v", record)
	}
}

func TestAgentReviewSealsTheVerdictTheReviewerPrinted(t *testing.T) {
	fixture := newAgentFixture(t, editTheLabel,
		`echo "I read the file and its callers."; echo '{"verdict":"revise","findings":[{"code":"stale-caller","path":"client/src/label.ts","message":"A caller still expects the old text."}]}'`)
	if err := fixture.implement(t); err != nil {
		t.Fatal(err)
	}
	if err := fixture.review(t); err != nil {
		t.Fatal(err)
	}

	var request worker.TicketRequest
	var candidate worker.Candidate
	var review worker.Review
	readAgentArtifact(t, fixture.path("ticket.json"), worker.MaxTicketJSONBytes, &request)
	readAgentArtifact(t, fixture.path("candidate.json"), worker.MaxArtifactJSONBytes, &candidate)
	readAgentArtifact(t, fixture.path("review.json"), worker.MaxReviewJSONBytes, &review)

	if review.Verdict != "revise" || len(review.Findings) != 1 || review.Findings[0].Code != "stale-caller" {
		t.Fatalf("the verdict was not sealed: %+v", review)
	}
	if err := review.Validate(fixture.config.Models.Reviewers[1], candidate, request); err != nil {
		t.Fatalf("the sealed review was rejected: %v", err)
	}
	if review.Invocation.RequestID == candidate.Invocation.RequestID {
		t.Fatal("the review and the change were recorded as the same run")
	}
}

// The reviewer is told to read only. A reviewer that quietly fixes what it was
// asked to judge would make the review meaningless.
func TestAgentReviewRejectsAReviewerThatEditsTheTree(t *testing.T) {
	fixture := newAgentFixture(t, editTheLabel,
		`printf "export const label = 'Reviewer wrote this';\n" > client/src/label.ts; echo '{"verdict":"pass","findings":[]}'`)
	if err := fixture.implement(t); err != nil {
		t.Fatal(err)
	}
	if err := fixture.review(t); err == nil {
		t.Fatal("a reviewer that rewrote the change was accepted")
	}
	if _, err := os.Stat(fixture.path("review.json")); err == nil {
		t.Fatal("a review was written for a reviewer that edited the tree")
	}
}

func TestAgentReviewRejectsAReviewerThatReportsNoVerdict(t *testing.T) {
	fixture := newAgentFixture(t, editTheLabel, `echo "Looks good to me."`)
	if err := fixture.implement(t); err != nil {
		t.Fatal(err)
	}
	if err := fixture.review(t); err == nil {
		t.Fatal("a review without a verdict was accepted")
	}
}

// A verdict that names a file the change did not touch is a reviewer talking
// about something else, which must not seal.
func TestAgentReviewRejectsAVerdictAboutAnUnrelatedFile(t *testing.T) {
	fixture := newAgentFixture(t, editTheLabel,
		`echo '{"verdict":"revise","findings":[{"code":"elsewhere","path":"client/src/other.ts","message":"Unrelated."}]}'`)
	if err := fixture.implement(t); err != nil {
		t.Fatal(err)
	}
	if err := fixture.review(t); err == nil {
		t.Fatal("a verdict about an untouched file was accepted")
	}
}

func readAgentArtifact(t *testing.T, filename string, limit int64, into any) {
	t.Helper()
	if err := worker.ReadJSONFile(filename, limit, into); err != nil {
		t.Fatalf("%s could not be read: %v", filepath.Base(filename), err)
	}
}

// A second attempt must start from every reviewer's objections, not one
// reviewer's share of them.
func TestImplementFeedsBothPreviousReviewsToTheAgent(t *testing.T) {
	// The stand-in prints its instruction, so what the agent was told is
	// observable in the sealed run record.
	fixture := newAgentFixture(t, `printf '%s\n' "$@" | tail -c 3000; `+editTheLabel, "true")
	first := fixture.path("review-one.json")
	second := fixture.path("review-two.json")
	writeTestJSON(t, first, map[string]any{"findings": []map[string]any{
		{"code": "from-first-reviewer", "path": "client/src/label.ts", "message": "First objection."},
	}})
	writeTestJSON(t, second, map[string]any{"findings": []map[string]any{
		{"code": "from-second-reviewer", "path": "client/src/label.ts", "message": "Second objection."},
	}})

	if err := run(context.Background(), []string{
		"implement", "--config", fixture.configPath, "--tool-sha", cliToolSHA, "--draft", fixture.draftPath,
		"--repo-root", fixture.repoRoot, "--base-root", fixture.baseRoot, "--base-sha", fixture.baseSHA, "--stage", "2",
		"--previous-findings", first, "--previous-findings", second,
		"--run-out", fixture.path("run.json"), "--ticket-out", fixture.path("ticket.json"),
		"--source-out", fixture.path("source.json"), "--out", fixture.path("candidate.json"),
	}); err != nil {
		t.Fatal(err)
	}
	var record worker.AgentRun
	readAgentArtifact(t, fixture.path("run.json"), worker.MaxArtifactJSONBytes, &record)
	if !strings.Contains(record.Transcript, "from-first-reviewer") || !strings.Contains(record.Transcript, "from-second-reviewer") {
		t.Fatalf("the agent was not told both objections: %q", record.Transcript)
	}
}

// A review whose first conversations die fast gets fresh rolls; the one that
// answers is sealed like any healthy review, and the loop really ran three
// times. The counter lives outside the workspace so the reviewer is not
// mistaken for one that edits the tree.
func TestAgentReviewRetriesFastDeathsUntilOneAnswers(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "attempts")
	fixture := newAgentFixture(t, editTheLabel,
		`n=$(cat `+counter+` 2>/dev/null || echo 0); n=$((n+1)); printf %s "$n" > `+counter+`; if [ "$n" -lt 3 ]; then echo "fast transient death"; exit 1; fi; echo '{"verdict":"pass","findings":[]}'`)
	if err := fixture.implement(t); err != nil {
		t.Fatal(err)
	}
	if err := fixture.review(t); err != nil {
		t.Fatalf("the third fast roll must be accepted: %v", err)
	}
	attempts, err := os.ReadFile(counter)
	if err != nil || string(attempts) != "3" {
		t.Fatalf("attempts = %q, %v", attempts, err)
	}
}

// Three fast deaths exhaust the limit: the review fails, and no verdict is
// sealed from a conversation that never answered.
func TestAgentReviewStopsAfterTheAttemptLimit(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "attempts")
	fixture := newAgentFixture(t, editTheLabel,
		`n=$(cat `+counter+` 2>/dev/null || echo 0); n=$((n+1)); printf %s "$n" > `+counter+`; echo "fast transient death"; exit 1`)
	if err := fixture.implement(t); err != nil {
		t.Fatal(err)
	}
	if err := fixture.review(t); err == nil {
		t.Fatal("a review that never answered was accepted")
	}
	attempts, err := os.ReadFile(counter)
	if err != nil || string(attempts) != "3" {
		t.Fatalf("attempts = %q, %v", attempts, err)
	}
	if _, err := os.Stat(fixture.path("review.json")); err == nil {
		t.Fatal("a verdict was sealed from a review that never answered")
	}
}
