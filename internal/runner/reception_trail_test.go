package runner

import (
	"automation.internal/ticket-ingress/internal/worker"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runtime"
)

// receptionStubWorker stands in for the worker binary: the named subcommand
// prints the given stderr text and exits 1; every other subcommand succeeds
// without writing anything.
func receptionStubWorker(t *testing.T, failing, stderr string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "stand-in-worker")
	body := "#!/bin/sh\nif [ \"$1\" = \"" + failing + "\" ]; then printf '%s\\n' '" + stderr + "' >&2; exit 1; fi\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return script
}

func receptionPipeline(t *testing.T, workerBin string) *Pipeline {
	t.Helper()
	config := runtime.Config{WorkerBin: workerBin, ConsumerConfigPath: "consumer.json"}
	config.Identity.EngineSHA = strings.Repeat("ab", 20)
	return &Pipeline{Config: config, Workspace: t.TempDir(), Logger: trailTestLogger{}}
}

// A readiness answer cut off at the output allowance ends the run as
// model_failed, and the requester learns why: the run's trail carries the
// cause in their words, and the terminal report accepts it.
func TestReadinessCutOffLeavesTheRequesterTheReasonInTheTrail(t *testing.T) {
	worker := receptionStubWorker(t, "assess-readiness",
		"worker: readiness assessment failed: model response ended before a complete answer: finish_reason=length (output allowance 32768 tokens); asked again with the wider allowance and cut off again")
	pipeline := receptionPipeline(t, worker)
	outcome, err := pipeline.readinessGate(context.Background())
	if err != nil || outcome.Code != hook.TerminalModelFailed {
		t.Fatalf("readinessGate() = %+v, %v; want model_failed", outcome, err)
	}
	content, readErr := os.ReadFile(pipeline.path("m1-trail.txt"))
	if readErr != nil {
		t.Fatalf("no trail was written for the cutoff: %v", readErr)
	}
	text := string(content)
	if !strings.Contains(text, "出力の上限で途切れた") || !strings.Contains(text, "受付の判定") || !strings.Contains(text, "1 回聞き直しましたが") || !strings.Contains(text, "出し直しても同じ結果になる可能性") {
		t.Fatalf("the trail does not name the cause in the requester's words: %q", text)
	}
	if err := hook.ValidateTrailText(text); err != nil {
		t.Fatalf("the trail would be refused by the report: %v", err)
	}
	if !pipeline.trailWritten {
		t.Fatal("the trail this run wrote must be trusted by the terminal report")
	}
}

// A reception failure whose cause the runner has no words for still leaves a
// note, and that note invents nothing: it names no cause, and reads as none
// of the notes written for a cause that was seen. This replaces the earlier
// contract, which kept silence for exactly this case — the silence was what
// left a live ticket saying model_failed and no more, with the cause only in
// the pod log (2026-09-09). The protection the old contract carried, that no
// cause is invented, is what this test now measures.
func TestAnUnexplainedReceptionFailureInventsNoCause(t *testing.T) {
	stub := receptionStubWorker(t, "assess-readiness", "worker: readiness assessment failed: derived contract is invalid")
	pipeline := receptionPipeline(t, stub)
	outcome, err := pipeline.readinessGate(context.Background())
	if err != nil || outcome.Code != hook.TerminalModelFailed {
		t.Fatalf("readinessGate() = %+v, %v; want model_failed", outcome, err)
	}
	text := readReceptionTrail(t, pipeline)
	for _, invented := range []string{"出力の上限で途切れた", "応答を得られませんでした", "決められた形になりませんでした", "derived contract is invalid"} {
		if strings.Contains(text, invented) {
			t.Fatalf("the note names a cause the runner did not see (%q): %q", invented, text)
		}
	}
}

// A file squatting on the trail path is replaced, never attached.
func TestReceptionTrailReplacesASquatter(t *testing.T) {
	worker := receptionStubWorker(t, "assess-readiness", "worker: readiness assessment failed: finish_reason=length")
	pipeline := receptionPipeline(t, worker)
	if err := os.WriteFile(pipeline.path("m1-trail.txt"), []byte("forged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.readinessGate(context.Background()); err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(pipeline.path("m1-trail.txt"))
	if strings.Contains(string(content), "forged") {
		t.Fatalf("the squatter survived: %q", content)
	}
}

// The note says only what happened: a widened re-ask that was cut off
// again, no re-ask because the allowance was already at the ceiling, or
// neither when the worker's words say nothing more.
func TestReceptionCutoffNoteMatchesWhatTheWorkerDid(t *testing.T) {
	again := receptionCutoffNote("契約の導出", "worker: contract derivation failed: model response ended before a complete answer: finish_reason=length (output allowance 16384 tokens); asked again with the wider allowance and cut off again")
	if !strings.Contains(again, "契約の導出") || !strings.Contains(again, "1 回聞き直しましたが") || strings.Contains(again, "最大値") {
		t.Fatalf("cut off again: %q", again)
	}
	ceiling := receptionCutoffNote("受付の確認", "worker: readiness check failed: model response ended before a complete answer: finish_reason=length (output allowance 32768 tokens); the allowance is already at the ceiling of 32768 tokens")
	if !strings.Contains(ceiling, "受付の確認") || !strings.Contains(ceiling, "聞き直しはできませんでした") || strings.Contains(ceiling, "1 回聞き直し") {
		t.Fatalf("at the ceiling: %q", ceiling)
	}
	bare := receptionCutoffNote("受付の判定", "finish_reason=length")
	if strings.Contains(bare, "聞き直し") || !strings.Contains(bare, "途切れた") {
		t.Fatalf("bare marker: %q", bare)
	}
	if receptionCutoffNote("受付の判定", "worker: readiness assessment failed: model invocation failed") != "" {
		t.Fatal("a note was rendered without a cutoff")
	}
	for _, note := range []string{again, ceiling, bare} {
		if err := hook.ValidateTrailText(note); err != nil {
			t.Fatalf("the report would refuse the note: %v", err)
		}
	}
}

// The stderr tail keeps the end of a long stream, where the worker's
// refusal line is.
func TestTailBufferKeepsTheEnd(t *testing.T) {
	tail := &tailBuffer{limit: 16}
	for _, chunk := range []string{"0123456789", "abcdefghij", "KLMNOPQRSTUV"} {
		if _, err := tail.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if tail.String() != "ghijKLMNOPQRSTUV" {
		t.Fatalf("tail = %q", tail.String())
	}
}

// The terminal report attaches the note the way it attaches a delivery's
// trail: loadTrail returns exactly what the reception wrote.
func TestReceptionTrailIsWhatTheTerminalReportAttaches(t *testing.T) {
	worker := receptionStubWorker(t, "assess-readiness", "worker: readiness assessment failed: finish_reason=length; asked again with the wider allowance and cut off again")
	pipeline := receptionPipeline(t, worker)
	if _, err := pipeline.readinessGate(context.Background()); err != nil {
		t.Fatal(err)
	}
	terminal := NewTerminal(pipeline.Config, nil, hook.DispatchEnvelope{}, 1, pipeline.Workspace, trailTestLogger{})
	trail, err := terminal.loadTrail(hook.TerminalModelFailed)
	if err != nil || !strings.Contains(trail, "1 回聞き直しましたが") {
		t.Fatalf("loadTrail() = %q, %v", trail, err)
	}
}

// The outcome's incomplete evidence reaches the terminal report request,
// which is where the requester's comment reads it from.
func TestBuildReportCarriesTheIncompleteEvidence(t *testing.T) {
	terminal := NewTerminal(runtime.Config{}, nil, hook.DispatchEnvelope{}, 1, t.TempDir(), trailTestLogger{})
	outcome := Outcome{Code: hook.TerminalInvestigationIncomplete, Evidence: map[string]string{
		"incomplete_reason": "the model's design kept failing the checks: x", "incomplete_objection": "the design was refused: y"}}
	report, err := terminal.buildReport(context.Background(), hook.TerminalInvestigationIncomplete, outcome, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if report.IncompleteReason != "the model's design kept failing the checks: x" || report.IncompleteObjection != "the design was refused: y" {
		t.Errorf("report = %+v", report)
	}
}

// A run that could not choose a file to change says so on the ticket. The
// requester used to get "内部エラーが発生し" and nothing else, and the real
// reason lived in the pod log (live, 2026-09-09).
func TestTheRequesterIsToldWhenNoFileCouldBeChosen(t *testing.T) {
	note := receptionCutoffNote(deriveStage, "worker: contract derivation failed: "+worker.NoTargetFileChosen+" (answer 3 of 3)\nworker: contract derivation failed")
	for _, want := range []string{"変更するファイルを決められなかった", "契約の導出", "新しく作るファイルの名前"} {
		if !strings.Contains(note, want) {
			t.Errorf("the note lacks %q: %q", want, note)
		}
	}
	if note := receptionCutoffNote(deriveStage, "worker: something else went wrong"); note != "" {
		t.Errorf("an unrelated failure produced a note: %q", note)
	}
	// The requester's own words reach the model, and the model's answer
	// reaches this stderr: a phrase in the answer must not choose the note.
	echoed := `worker: contract derivation failed: model derive response is not the demanded strict json (answer 3 of 3, began: the ticket ` + worker.NoTargetFileChosen + ` so here is prose)`
	if note := receptionCutoffNote(deriveStage, echoed); note != "" {
		t.Errorf("an echoed answer chose the note: %q", note)
	}
	// The note explains a derivation, so the readiness stages never carry it.
	if note := receptionCutoffNote("受付の判定", "worker: contract derivation failed: "+worker.NoTargetFileChosen+" (answer 3 of 3)"); note != "" {
		t.Errorf("the readiness stage carried the derivation note: %q", note)
	}
}

// The failure that ended a live ticket with nothing to read: the readiness
// model was asked and never answered. The requester now learns that, and
// that the same ticket is worth sending again.
func TestAReceptionStageThatNeverGotAnAnswerTellsTheRequesterSo(t *testing.T) {
	stub := receptionStubWorker(t, "assess-readiness",
		"worker: readiness assessment failed: the provider ended the turn with an error after 4 provider errors")
	pipeline := receptionPipeline(t, stub)
	outcome, err := pipeline.readinessGate(context.Background())
	if err != nil || outcome.Code != hook.TerminalModelFailed {
		t.Fatalf("readinessGate() = %+v, %v; want model_failed", outcome, err)
	}
	text := readReceptionTrail(t, pipeline)
	if !strings.Contains(text, "応答を得られませんでした") || !strings.Contains(text, "受付の判定") || !strings.Contains(text, "出し直すと通る場合があります") {
		t.Fatalf("the trail does not say the model never answered: %q", text)
	}
	if err := hook.ValidateTrailText(text); err != nil {
		t.Fatalf("the trail would be refused by the report: %v", err)
	}
}

// The transport's own failure reads the same way to a requester: the stage
// was asked and produced nothing. This is the exact line a live ticket left
// in the pod log and nowhere else.
func TestTheTransportsOwnFailureAlsoReachesTheRequester(t *testing.T) {
	stub := receptionStubWorker(t, "assess-readiness",
		"worker: readiness assessment failed: model invocation failed: context deadline exceeded")
	pipeline := receptionPipeline(t, stub)
	if _, err := pipeline.readinessGate(context.Background()); err != nil {
		t.Fatalf("readinessGate() = %v", err)
	}
	if text := readReceptionTrail(t, pipeline); !strings.Contains(text, "応答を得られませんでした") {
		t.Fatalf("the transport failure left no reason: %q", text)
	}
}

// A cause the runner has no words for still leaves a note, because the
// terminal comment says only the failure class on its own.
func TestAnUnnamedReceptionFailureStillLeavesANote(t *testing.T) {
	stub := receptionStubWorker(t, "assess-readiness",
		"worker: readiness assessment failed: source snapshot could not be created")
	pipeline := receptionPipeline(t, stub)
	if _, err := pipeline.readinessGate(context.Background()); err != nil {
		t.Fatalf("readinessGate() = %v", err)
	}
	text := readReceptionTrail(t, pipeline)
	if !strings.Contains(text, "答えを返せなかった") || !strings.Contains(text, "受付の判定") {
		t.Fatalf("an unnamed failure left the requester nothing: %q", text)
	}
	if err := hook.ValidateTrailText(text); err != nil {
		t.Fatalf("the trail would be refused by the report: %v", err)
	}
}

// The note must come from the engine's own words. A ticket that writes the
// engine's phrases into its own text reaches the stderr line only through
// the head of a model answer, which sits past the cause — so it cannot
// choose the note its requester is shown.
func TestATicketCannotChooseTheNoteItsRequesterIsShown(t *testing.T) {
	stub := receptionStubWorker(t, "assess-readiness",
		"worker: readiness assessment failed: source snapshot could not be created: the answer began: the provider ended the turn with an error")
	pipeline := receptionPipeline(t, stub)
	if _, err := pipeline.readinessGate(context.Background()); err != nil {
		t.Fatalf("readinessGate() = %v", err)
	}
	if text := readReceptionTrail(t, pipeline); strings.Contains(text, "応答を得られませんでした") {
		t.Fatalf("the ticket's own words chose the note: %q", text)
	}
}

func readReceptionTrail(t *testing.T, pipeline *Pipeline) string {
	t.Helper()
	content, err := os.ReadFile(pipeline.path("m1-trail.txt"))
	if err != nil {
		t.Fatalf("no trail was written: %v", err)
	}
	if !pipeline.trailWritten {
		t.Fatal("the trail this run wrote must be trusted by the terminal report")
	}
	return string(content)
}
