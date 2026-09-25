package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/worker"
)

// sayItCannotBeDone stands in for an implementer that finds it cannot carry
// the request out: it changes nothing and says why. That is what the agent
// is told to do, and it is the engine — not the requester — that decides
// what happens next.
const sayItCannotBeDone = `echo "この依頼は、いまのままでは実現できません。" >&2; ` +
	`echo "理由を報告します。何も変更していません。その判断は依頼者に返します。"`

// The implementer's report used to travel to the first review card as an
// empty working copy: the seal refused it, the card blocked, and the
// requester was told "model_failed" with neither the report nor its reason
// (live 2026-09-25). The card stops before the review instead, and the
// record beside it is what the engine reads to answer the round.
func TestRunInstructionStopsTheCardWhenTheImplementerReportsInsteadOfChanging(t *testing.T) {
	fixture := newAgentFixture(t, sayItCannotBeDone, "true")
	instruction := filepath.Join(t.TempDir(), "INSTRUCTION.md")
	if err := os.WriteFile(instruction, []byte("Change the label exactly as the ticket says.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	record := fixture.path("implementer-run.json")
	err := run(context.Background(), []string{
		"run-instruction", "--config", fixture.configPath, "--tool-sha", cliToolSHA, "--draft", fixture.draftPath,
		"--instruction", instruction, "--repo-root", fixture.repoRoot, "--base-sha", fixture.baseSHA, "--stage", "1",
		"--role", "implementer", "--out", record,
	})
	if err == nil || !strings.Contains(err.Error(), "the round is answered and run again") {
		t.Fatalf("the card did not stop on the report: %v", err)
	}

	// The run record is the evidence the report is read from afterwards.
	var sealed worker.AgentRun
	if err := worker.ReadJSONFile(record, worker.MaxArtifactJSONBytes, &sealed); err != nil {
		t.Fatalf("run record: %v", err)
	}
	if !worker.IsSendBack(sealed) || !strings.Contains(sealed.Transcript, "依頼者に返します") {
		t.Fatalf("run record = %+v", sealed)
	}

	// An implementer that did the work is untouched by any of this.
	working := newAgentFixture(t, editTheLabel, "true")
	err = run(context.Background(), []string{
		"run-instruction", "--config", working.configPath, "--tool-sha", cliToolSHA, "--draft", working.draftPath,
		"--instruction", instruction, "--repo-root", working.repoRoot, "--base-sha", working.baseSHA, "--stage", "1",
		"--role", "implementer", "--out", working.path("implementer-run.json"),
	})
	if err != nil {
		t.Fatalf("a round that changed a file: %v", err)
	}
}

// The trail of a run that sealed no candidate came out as the fallback line
// while the implementer's report sat in the run directory; compose-trail
// renders that report instead of refusing to compose.
func TestComposeTrailRendersARoundThatSealedNoCandidate(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.json")
	writeTestJSON(t, configPath, cliTestConfig())
	history := filepath.Join(directory, "history")
	stageDir := filepath.Join(history, "stage-1")
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	report := "この依頼は、いまのままでは実現できません。\nその判断は依頼者に返します。"
	sealed, err := worker.SealAgentRun(worker.AgentRun{
		SchemaVersion: worker.ArtifactSchemaVersion, Stage: 1, AgentID: "implementer",
		Transcript: report, RanAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.WriteJSONFileExclusive(filepath.Join(stageDir, "implementer-run.json"), sealed, worker.MaxArtifactJSONBytes); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(directory, "m1-trail.txt")
	if err := run(context.Background(), []string{
		"compose-trail", "--config", configPath, "--tool-sha", cliToolSHA,
		"--history", history, "--blocked-step", "変更の確定", "--out", out,
	}); err != nil {
		t.Fatalf("compose-trail: %v", err)
	}
	trail, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	// The whole report, its line breaks kept, under the heading that says
	// whose words they are.
	if !strings.Contains(string(trail), report) {
		t.Fatalf("the report was not carried whole:\n%s", trail)
	}
	for _, expected := range []string{"実装役の報告", "止まった段階: 変更の確定"} {
		if !strings.Contains(string(trail), expected) {
			t.Fatalf("trail lacks %q:\n%s", expected, trail)
		}
	}

	// A history with nothing readable in it still fails, so the caller
	// writes the fallback line rather than an empty record.
	if err := run(context.Background(), []string{
		"compose-trail", "--config", configPath, "--tool-sha", cliToolSHA,
		"--history", t.TempDir(), "--out", filepath.Join(directory, "empty-trail.txt"),
	}); err == nil {
		t.Fatal("a history with no round composed a trail")
	}
}
