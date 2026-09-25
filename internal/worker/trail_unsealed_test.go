package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// A round that sealed no candidate has no chain to render, and the trail
// came out as the fallback line — "the record could not be generated
// automatically" — while the implementing agent's own report of why it
// changed nothing sat unread in the run directory (live 2026-09-25). The
// report is the record of such a round, so it is what the requester gets.
func TestTheTrailOfARoundThatSealedNothingCarriesTheImplementersReport(t *testing.T) {
	history := t.TempDir()
	config := Config{MaxStages: 3}
	if _, err := LoadUnsealedRound(history, config); err == nil {
		t.Fatal("an empty history produced a round")
	}
	report := "client/src/plugin.ts も変えないと実現できません。\nその判断は依頼者に返します。"
	run := returnedRun(report)
	run.EmptyAttempts = 1
	writeRoundRun(t, history, 1, "implementer-run.json", run)

	round, err := LoadUnsealedRound(history, config)
	if err != nil {
		t.Fatalf("LoadUnsealedRound: %v", err)
	}
	if round.Round != 1 || !round.ReturnedWork || round.EmptyAttempts != 1 {
		t.Fatalf("round = %+v", round)
	}
	trail := ComposeUnsealedTrail(round, "変更の確定")
	for _, expected := range []string{
		"実装の経過 (1 周目で停止)",
		"変更を加えずに理由を報告して作業を返しました",
		"変更がないまま終わった試行が、この前に 1 回",
		"止まった段階: 変更の確定",
		"実装役の報告",
	} {
		if !strings.Contains(trail, expected) {
			t.Fatalf("trail lacks %q:\n%s", expected, trail)
		}
	}
	// The report goes in whole, its own line breaks kept: the requester
	// reads what the agent wrote, not a summary of it.
	if !strings.Contains(trail, report) {
		t.Fatalf("the report was not carried whole:\n%s", trail)
	}
	if len(trail) > MaxTrailBytes {
		t.Fatalf("trail exceeds the bound: %d bytes", len(trail))
	}

	// A round that did seal a candidate is the ordinary trail's, whatever
	// failed afterwards.
	if err := os.WriteFile(filepath.Join(history, "stage-1", "candidate.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadUnsealedRound(history, config); err == nil {
		t.Fatal("a round with a candidate was rendered as an unsealed one")
	}
}

// The one-process mode names the implement verb's own record, the cards
// name the role that ran, and a later round is the one that stopped.
func TestTheUnsealedRoundIsTheNewestOneWithAReport(t *testing.T) {
	history := t.TempDir()
	config := Config{MaxStages: 3}
	first := returnedRun("1 周目の報告")
	first.ChangedFiles = []string{"client/src/label.ts"}
	writeRoundRun(t, history, 1, "implement-run.json", first)
	second := returnedRun("2 周目の報告")
	second.AgentID = "applier"
	second.Stage = 2
	writeRoundRun(t, history, 2, "applier-run.json", second)

	round, err := LoadUnsealedRound(history, config)
	if err != nil {
		t.Fatalf("LoadUnsealedRound: %v", err)
	}
	if round.Round != 2 || round.AgentID != "applier" || !strings.Contains(round.Report, "2 周目の報告") {
		t.Fatalf("round = %+v", round)
	}

	// A round that ran and left the tree changed is rendered too: the
	// candidate is what is missing, not the work.
	trail := ComposeUnsealedTrail(UnsealedRound{Round: 1, Report: "1 周目の報告"}, "")
	if !strings.Contains(trail, "変更を確定できなかったため") || strings.Contains(trail, "止まった段階") {
		t.Fatalf("trail = %q", trail)
	}
}

// A report too long for the trail is cut at the bound and keeps what fits.
// It used to lose everything: the cut rewound to the last line break, and a
// report written as one long line — which agents do — had none after the
// heading, so the whole section came back empty.
func TestALongReportKeepsWhatFitsInsideTheBound(t *testing.T) {
	for _, shape := range []struct{ name, line string }{
		{"one long line", "この依頼は実現できません。"},
		{"many lines", "この依頼は実現できません。\n"},
	} {
		t.Run(shape.name, func(t *testing.T) {
			history := t.TempDir()
			// Sized from the bound rather than from a number written here:
			// how much a trail may carry is not this file's to decide, and a
			// test that assumed it would quietly stop testing anything when
			// the bound moved.
			writeRoundRun(t, history, 1, "implementer-run.json",
				returnedRun(strings.Repeat(shape.line, MaxTrailBytes/len(shape.line)+50)))
			round, err := LoadUnsealedRound(history, Config{MaxStages: 3})
			if err != nil {
				t.Fatalf("LoadUnsealedRound: %v", err)
			}
			trail := ComposeUnsealedTrail(round, "")
			if len(trail) > MaxTrailBytes {
				t.Fatalf("trail exceeds the bound: %d bytes", len(trail))
			}
			if !strings.Contains(trail, "切り詰め") {
				t.Fatal("a cut trail does not say it was cut")
			}
			// The report itself, not only the heading above it: a bounded
			// prefix of what the agent wrote has to survive the cut.
			const heading = "### 実装役の報告\n"
			at := strings.Index(trail, heading)
			if at < 0 {
				t.Fatalf("the report section is missing:\n%s", trail)
			}
			body := trail[at+len(heading):]
			if !strings.HasPrefix(body, strings.Repeat(shape.line, 20)) {
				t.Fatalf("the report body did not survive the cut: %.120q", body)
			}
			if !utf8.ValidString(trail) {
				t.Fatal("the cut landed inside a character")
			}
		})
	}
}
