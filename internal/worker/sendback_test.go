package worker

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func writeRoundRun(t *testing.T, historyDir string, round int, name string, run AgentRun) {
	t.Helper()
	stageDir := filepath.Join(historyDir, "stage-"+strconv.Itoa(round))
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sealed, err := SealAgentRun(run)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteJSONFileExclusive(filepath.Join(stageDir, name), sealed, MaxArtifactJSONBytes); err != nil {
		t.Fatal(err)
	}
}

func returnedRun(transcript string) AgentRun {
	return AgentRun{
		SchemaVersion: ArtifactSchemaVersion, Stage: 1, AgentID: "implementer",
		ExitCode: 0, ChangedFiles: nil, Transcript: transcript, RanAt: time.Now().UTC(),
	}
}

// The implementer is told to change nothing and say why when it cannot
// carry the request out. That answer used to go to the first review card as
// an empty change, be refused by the seal, and reach the requester as
// "model_failed" with neither the refusal nor its reason (live 2026-09-25).
// Reading it apart from a failure starts here.
func TestReturnedWorkIsTheOneEmptyRunThatSaidSomething(t *testing.T) {
	run := returnedRun("この依頼は、いまのままでは実現できません。理由は次のとおりです。")
	if !IsSendBack(run) {
		t.Fatal("a finished run that changed nothing and said why was not read as returned work")
	}
	// A run that changed nothing and said nothing produced nothing; that is
	// still the model failure it always was.
	silent := run
	silent.Transcript = "   \n\t "
	if IsSendBack(silent) {
		t.Fatal("a silent empty run was read as returned work")
	}
	// Nor is a run that died, however much it wrote on the way out.
	died := run
	died.ExitCode = 1
	if IsSendBack(died) {
		t.Fatal("a failed run was read as returned work")
	}
	// Nor is one that did the work.
	worked := run
	worked.ChangedFiles = []string{"client/src/label.ts"}
	if IsSendBack(worked) {
		t.Fatal("a round that changed a file was read as returned work")
	}
}

// The record is read from whichever role wrote it, and a round that got as
// far as a candidate is never read as returned work however its cards ended
// afterwards.
func TestARoundsReturnedWorkIsReadFromWhicheverRoleRan(t *testing.T) {
	history := t.TempDir()
	if RoundReturnedWork(history, 1) {
		t.Fatal("an empty round returned work")
	}
	writeRoundRun(t, history, 1, "implementer-run.json", returnedRun("理由を報告します。"))
	if !RoundReturnedWork(history, 1) {
		t.Fatal("the implementer's record was not read")
	}
	run, err := ReadImplementingRun(history, 1)
	if err != nil || run.AgentID != "implementer" {
		t.Fatalf("ReadImplementingRun = %+v, %v", run, err)
	}

	applied := returnedRun("設計どおりには書けません。")
	applied.AgentID = "applier"
	applied.Stage = 2
	writeRoundRun(t, history, 2, "applier-run.json", applied)
	if !RoundReturnedWork(history, 2) {
		t.Fatal("the applier's record was not read")
	}

	// The seal ran, so the round changed something: whatever failed later
	// is not the implementer handing the work back.
	if err := os.WriteFile(filepath.Join(history, "stage-1", "candidate.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if RoundReturnedWork(history, 1) {
		t.Fatal("a round with a sealed candidate was read as returned work")
	}
}

// The report travels to a ticket comment, which may not hold every byte an
// agent can emit. What it may not hold is removed; nothing is shortened,
// because the trail's own bound is the only limit the report has.
func TestTheReportIsCleanedButNeverShortened(t *testing.T) {
	long := strings.Repeat("この依頼は実現できません。\n", 400)
	if got := ReportText(long); got != strings.TrimSpace(long) {
		t.Fatalf("the report was shortened: %d bytes in, %d out", len(long), len(got))
	}
	cleaned := ReportText("  行 1\r\n行 2\x00\x1b[31m色\x1b[0m\n\t字下げ  ")
	if strings.ContainsAny(cleaned, "\x00\r\x1b") {
		t.Fatalf("a character a trail may not hold survived: %q", cleaned)
	}
	for _, expected := range []string{"行 1\n行 2", "色", "\t字下げ"} {
		if !strings.Contains(cleaned, expected) {
			t.Fatalf("cleaning lost %q: %q", expected, cleaned)
		}
	}
	if strings.HasPrefix(cleaned, " ") || strings.HasSuffix(cleaned, " ") {
		t.Fatalf("the report was not trimmed: %q", cleaned)
	}
	if ReportText(" \n\t ") != "" {
		t.Fatal("a report of nothing came out as something")
	}
}
