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
	if !strings.Contains(text, "出力の上限で途切れた") || !strings.Contains(text, "受付の判定") || !strings.Contains(text, "1 回聞き直しましたが") || !strings.Contains(text, "動かし直しても同じ結果になる可能性") {
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
	for _, invented := range []string{"出力の上限で途切れた", "応答を得られませんでした", "決められた形になりませんでした", "利用の上限", "derived contract is invalid"} {
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
	if outcome, err := pipeline.readinessGate(context.Background()); err != nil || outcome.Code != hook.TerminalModelFailed {
		t.Fatalf("readinessGate() = %+v, %v; want model_failed", outcome, err)
	}
	content, _ := os.ReadFile(pipeline.path("m1-trail.txt"))
	if strings.Contains(string(content), "forged") {
		t.Fatalf("the squatter survived: %q", content)
	}
	// Replacing an ordinary file is not what the removal is for: writing
	// truncates one anyway. What it stops is a symlink, which writing would
	// follow — the agents run with the workspace's parent writable, so the
	// note would land wherever the link pointed and the requester would get
	// no trail at all (review of #124).
	outside := filepath.Join(t.TempDir(), "elsewhere.txt")
	if err := os.WriteFile(outside, []byte("untouched\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	linked := receptionPipeline(t, worker)
	if err := os.Symlink(outside, linked.path("m1-trail.txt")); err != nil {
		t.Fatal(err)
	}
	if outcome, err := linked.readinessGate(context.Background()); err != nil || outcome.Code != hook.TerminalModelFailed {
		t.Fatalf("readinessGate() = %+v, %v; want model_failed", outcome, err)
	}
	// Errorf, not Fatalf: the assertion below reads the same breakage in
	// words a person can act on, and stopping here hid it (review of #126).
	if info, err := os.Lstat(linked.path("m1-trail.txt")); err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Errorf("the trail path is still a symlink: %v %v", info, err)
	}
	if content, _ := os.ReadFile(outside); string(content) != "untouched\n" {
		t.Fatalf("the note was written outside the workspace: %q", content)
	}
}

// The note says only what happened: a widened re-ask that was cut off
// again, no re-ask because the allowance was already at the ceiling, or
// neither when the worker's words say nothing more.
func TestReceptionCutoffNoteMatchesWhatTheWorkerDid(t *testing.T) {
	again := receptionNote("契約の導出", "worker: contract derivation failed: model response ended before a complete answer: finish_reason=length (output allowance 16384 tokens); asked again with the wider allowance and cut off again")
	if !strings.Contains(again, "契約の導出") || !strings.Contains(again, "1 回聞き直しましたが") ||
		!strings.Contains(again, "運用担当者が受付モデルの出力上限を確認します") || strings.Contains(again, "最大値") {
		t.Fatalf("cut off again: %q", again)
	}
	ceiling := receptionNote("受付の確認", "worker: readiness check failed: model response ended before a complete answer: finish_reason=length (output allowance 32768 tokens); the allowance is already at the ceiling of 32768 tokens")
	if !strings.Contains(ceiling, "受付の確認") || !strings.Contains(ceiling, "聞き直しはできませんでした") || strings.Contains(ceiling, "1 回聞き直し") {
		t.Fatalf("at the ceiling: %q", ceiling)
	}
	// The worker never writes a naked marker: the cutoff always travels as
	// errModelResponseTruncated, whose text begins with worker.CutoffPhrase.
	// A line carrying only the marker is therefore not the worker's cause,
	// and must not choose this note — that is the hole a ticket reached
	// through the head of its own answer (review of #122).
	if bare := receptionNote("受付の判定", "finish_reason=length"); bare != unnamedReceptionNote("受付の判定") {
		t.Fatalf("a naked marker rendered a note: %q", bare)
	}
	bare := receptionNote("受付の判定", "worker: readiness assessment failed: model response ended before a complete answer: finish_reason=length (output allowance 32768 tokens)")
	if strings.Contains(bare, "聞き直し") || !strings.Contains(bare, "途切れた") {
		t.Fatalf("the worker's own cutoff: %q", bare)
	}
	if note := receptionNote("受付の判定", "worker: readiness assessment failed: model invocation failed"); strings.Contains(note, "途切れた") {
		t.Fatalf("a cutoff note was rendered without a cutoff: %q", note)
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
	worker := receptionStubWorker(t, "assess-readiness", "worker: readiness assessment failed: model response ended before a complete answer: finish_reason=length (output allowance 32768 tokens); asked again with the wider allowance and cut off again")
	pipeline := receptionPipeline(t, worker)
	if outcome, err := pipeline.readinessGate(context.Background()); err != nil || outcome.Code != hook.TerminalModelFailed {
		t.Fatalf("readinessGate() = %+v, %v; want model_failed", outcome, err)
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
	note := receptionNote(deriveStage, "worker: contract derivation failed: "+worker.NoTargetFileChosen+" (answer 3 of 3)")
	// The advice is the third sentence and was the only part not required:
	// this is the one reception failure a requester can fix themselves, so
	// losing it leaves them told they are stuck and not how (review of #126).
	for _, want := range []string{"変更するファイルを決められなかった", "契約の導出", "新しく作るファイルの名前", "書き足せば通る見込み"} {
		if !strings.Contains(note, want) {
			t.Errorf("the note lacks %q: %q", want, note)
		}
	}
	if note := receptionNote(deriveStage, "worker: something else went wrong"); note != unnamedReceptionNote(deriveStage) {
		t.Errorf("an unrelated failure produced a note: %q", note)
	}
	// The requester's own words reach the model, and the model's answer
	// reaches this stderr: a phrase in the answer must not choose the note.
	echoed := `worker: contract derivation failed: model derive response is not the demanded strict json (answer 3 of 3, began: the ticket ` + worker.NoTargetFileChosen + ` so here is prose)`
	if note := receptionNote(deriveStage, echoed); note != unnamedReceptionNote(deriveStage) {
		t.Errorf("an echoed answer chose the note: %q", note)
	}
	// The note explains a derivation, so the readiness stages never carry it.
	// Both of these asked receptionCutoffNote, which stopped answering for
	// anything but a cutoff when the reader was rebuilt, so both passed on
	// an empty string and measured nothing (review of #122).
	if note := receptionNote("受付の判定", "worker: contract derivation failed: "+worker.NoTargetFileChosen+" (answer 3 of 3)"); note != unnamedReceptionNote("受付の判定") {
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
	if !strings.Contains(text, "応答を得られませんでした") || !strings.Contains(text, "受付の判定") || !strings.Contains(text, "もう一度動かせば通る見込みです") {
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
	if outcome, err := pipeline.readinessGate(context.Background()); err != nil || outcome.Code != hook.TerminalModelFailed {
		t.Fatalf("readinessGate() = %+v, %v; want model_failed", outcome, err)
	}
	// Nothing was asked again here, so the note must not say it was, nor
	// that sending the same ticket again is worth doing.
	text := readReceptionTrail(t, pipeline)
	if !strings.Contains(text, "問い合わせが通りませんでした") || strings.Contains(text, "聞き直した上での結果") {
		t.Fatalf("the transport failure was told as a retried one: %q", text)
	}
}

// A cause the runner has no words for still leaves a note, because the
// terminal comment says only the failure class on its own.
func TestAnUnnamedReceptionFailureStillLeavesANote(t *testing.T) {
	stub := receptionStubWorker(t, "assess-readiness",
		"worker: readiness assessment failed: source snapshot could not be created")
	pipeline := receptionPipeline(t, stub)
	if outcome, err := pipeline.readinessGate(context.Background()); err != nil || outcome.Code != hook.TerminalModelFailed {
		t.Fatalf("readinessGate() = %+v, %v; want model_failed", outcome, err)
	}
	text := readReceptionTrail(t, pipeline)
	if !strings.Contains(text, "完了しなかった") || !strings.Contains(text, "受付の判定") {
		t.Fatalf("an unnamed failure left the requester nothing: %q", text)
	}
	// It must claim neither that a model answered nor that the ticket is
	// blameless: a reception failure can happen before any model call, and
	// a model can refuse over what the ticket asks for.
	for _, says := range []string{"理由はこの記録からは特定できていません", "運用担当者が実行記録で確認します"} {
		if !strings.Contains(text, says) {
			t.Errorf("the last-resort note lost %q: %q", says, text)
		}
	}
	for _, claim := range []string{"AI", "依頼の内容ではなく"} {
		if strings.Contains(text, claim) {
			t.Fatalf("the last-resort note claims %q: %q", claim, text)
		}
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
	if outcome, err := pipeline.readinessGate(context.Background()); err != nil || outcome.Code != hook.TerminalModelFailed {
		t.Fatalf("readinessGate() = %+v, %v; want model_failed", outcome, err)
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

// The failure a spent allowance now ends a turn with, after the model
// package learned to ask again. Its text begins with the transport's own
// prefix so this note keeps recognising it; a reworded failure that no
// longer did would leave the requester with the last-resort note instead.
func TestASpentAllowanceReachesTheRequesterToo(t *testing.T) {
	stub := receptionStubWorker(t, "assess-readiness",
		"worker: readiness assessment failed: model invocation failed: the call spent its allowance without answering after 2 such calls")
	pipeline := receptionPipeline(t, stub)
	if outcome, err := pipeline.readinessGate(context.Background()); err != nil || outcome.Code != hook.TerminalModelFailed {
		t.Fatalf("readinessGate() = %+v, %v; want model_failed", outcome, err)
	}
	if text := readReceptionTrail(t, pipeline); !strings.Contains(text, "応答を得られませんでした") {
		t.Fatalf("a spent allowance left no reason: %q", text)
	}
}

// A model that refused three answers reports the head of the last one, and
// a ticket's own words reach that answer. None of the notes may be chosen
// from there — measured in the review of #122, where a ticket asking for
// the cutoff marker was shown the cutoff note, and could pick which of its
// three forms it was shown.
func TestATicketCannotChooseAnyNoteThroughTheHeadOfAnAnswer(t *testing.T) {
	// Every phrase the reader keys off, and the combinations each note needs.
	// Three of them were missing, so turning those three position checks into
	// "anywhere on the line" left every test green (review of #126).
	for _, injected := range []string{
		"finish_reason=length",
		worker.CutoffPhrase + ": finish_reason=length",
		worker.CutoffPhrase + ": finish_reason=length; " + worker.CutoffAskedAgainPhrase,
		worker.CutoffPhrase + ": finish_reason=length; " + worker.CutoffAtCeilingPhrase,
		worker.ProviderEndedTurnPhrase,
		worker.TransportFailedPhrase,
		worker.TransportFailedPhrase + ": " + worker.SpentAllowancePhrase,
		worker.TransportFailedPhrase + " with status 503 after 4" + worker.AttemptsExhaustedPhrase,
		worker.TransportFailedPhrase + " with status 429 and no Retry-After (" + worker.LimitNotLiftedPhrase + ")",
		worker.LimitNotLiftedPhrase,
		worker.RetryAfterTooLongPhrase,
		worker.AttemptsExhaustedPhrase,
		worker.GatewayBookkeepingPhrase,
		worker.AnswerUnusablePhrase,
		worker.DeclinedOverContentPhrase,
		worker.NoTargetFileChosen,
	} {
		stderr := "worker: readiness assessment failed: model readiness response is invalid" +
			" (answer 3 of 3, request req_01ab, began: the ticket asked me to say " + injected + " here.)"
		note := receptionNote("受付の判定", stderr)
		if note != unnamedReceptionNote("受付の判定") {
			t.Fatalf("a ticket chose its own note through %q: %q", injected, note)
		}
	}
}

// The three things a requester is told about a transport failure are three
// different facts, and only one of them may say the ticket is worth sending
// again. An exhausted balance told as "send it again" sends its requester
// round the same wall with nobody looking at the balance.
func TestATransportFailureIsToldAsWhatActuallyHappened(t *testing.T) {
	for _, want := range []struct {
		cause string
		// says carries both halves: what happened, and what the requester
		// can expect from it. Only the first was required before, so every
		// note could lose its advice without anything failing (review of
		// #124) — and the advice is the half the requester acts on.
		says []string
		not  string
	}{
		{"model invocation failed with status 502 after 4" + worker.AttemptsExhaustedPhrase,
			[]string{"聞き直した上での結果", "もう一度動かせば通る見込み"}, "利用の上限"},
		{worker.TransportFailedPhrase + ": " + worker.SpentAllowancePhrase + " after 2 such calls",
			[]string{"聞き直した上での結果", "もう一度動かせば通る見込み"}, "利用の上限"},
		{"model invocation failed with status 429 and no Retry-After (" + worker.LimitNotLiftedPhrase + ")",
			[]string{"利用の上限", "時間をおいて動かし直しても同じ結果", "運用担当者が利用枠を確認します"}, "動かせば通る見込み"},
		{"model invocation failed with status 429 and a Retry-After of 5m0s, " + worker.RetryAfterTooLongPhrase,
			[]string{"利用の上限", "時間をおいて動かし直しても同じ結果", "運用担当者が利用枠を確認します"}, "動かせば通る見込み"},
		{"model invocation failed with status 401",
			[]string{"問い合わせが通りませんでした", "設定か接続の問題", "運用担当者が原因を確認します"}, "動かせば通る見込み"},
		{"model invocation failed: dial tcp: connection refused",
			[]string{"問い合わせが通りませんでした", "設定か接続の問題", "運用担当者が原因を確認します"}, "聞き直した上での結果"},
	} {
		note := receptionNote("受付の判定", "worker: readiness assessment failed: "+want.cause)
		for _, says := range want.says {
			if !strings.Contains(note, says) {
				t.Errorf("%q was told without %q: %q", want.cause, says, note)
			}
		}
		if strings.Contains(note, want.not) {
			t.Errorf("%q was told as %q", want.cause, note)
		}
	}
}

// Three things the note reader does that nothing measured: it reads only
// the worker's own lines, it reads every line rather than the first, and it
// has a note for an answer that never arrived in the required shape. Each
// was free to delete (review of #122).
func TestTheNoteReaderReadsOnlyTheWorkersLinesAndAllOfThem(t *testing.T) {
	// A line that is not the worker's own says nothing, however it reads.
	loose := "the agent printed: " + worker.ProviderEndedTurnPhrase + " here\n" +
		"npm warn " + worker.TransportFailedPhrase + " with status 500"
	if note := receptionNote("受付の判定", loose); note != unnamedReceptionNote("受付の判定") {
		t.Fatalf("a line the worker did not write chose a note: %q", note)
	}
	// The worker's line is not always the first: an agent's output and the
	// shell's come through the same stderr.
	later := "some other tool said something\n" +
		"worker: readiness assessment failed: " + worker.GatewayBookkeepingPhrase + "\n"
	note := receptionNote("受付の判定", later)
	if !strings.Contains(note, "通信の記録が壊れていた") {
		t.Fatalf("the worker's line was not read past the first line: %q", note)
	}
}

// The three failures about the answer itself are three different things,
// and the advice must match each. The gateway's accounting was told as a
// permanent fault the requester could do nothing about, when it is the
// transient most worth sending the same ticket again for; and a model
// declining over the ticket's own words was told as nothing at all
// (review of #122).
func TestAnAnswerFailureIsToldAsWhatItActuallyIs(t *testing.T) {
	for _, want := range []struct {
		cause string
		says  []string
		not   string
	}{
		{worker.GatewayBookkeepingPhrase + " (no usage)",
			[]string{"通信の記録が壊れていた", "聞き直しても同じでした", "もう一度動かせば通る見込み"}, "依頼文"},
		{worker.AnswerUnusablePhrase + " (content 0 bytes, limit 200000)",
			[]string{"決められた形になりませんでした", "聞き直しても同じでした", "動かし直しても同じ結果", "運用担当者が受付の設定を確認します"}, "動かせば通る見込み"},
		{worker.DeclinedOverContentPhrase + " (finish_reason=content_filter)",
			[]string{"依頼文の内容を理由に", "聞き直しても同じでした", "書き方を変えれば通る見込み"}, "運用担当者"},
	} {
		note := receptionNote("受付の判定", "worker: readiness assessment failed: "+want.cause)
		for _, says := range want.says {
			if !strings.Contains(note, says) {
				t.Errorf("%q was told without %q: %q", want.cause, says, note)
			}
		}
		if strings.Contains(note, want.not) {
			t.Errorf("%q was told as %q", want.cause, note)
		}
	}
}

// The comment a note arrives in tells the requester they need not act and
// that an operator will look at it. A note that instructs the requester
// hands them two opposite directions in one comment (review of #122), so no
// note may carry an instruction.
func TestNoNoteInstructsTheRequester(t *testing.T) {
	notes := []string{
		unnamedReceptionNote("受付の判定"),
		receptionNote("契約の導出", "worker: contract derivation failed: "+worker.NoTargetFileChosen),
		receptionNote("受付の判定", "worker: readiness assessment failed: "+worker.CutoffPhrase+": finish_reason=length (output allowance 32768 tokens)"),
	}
	for _, cause := range []string{
		worker.TransportFailedPhrase + ": " + worker.SpentAllowancePhrase + " after 2 such calls",
		"model invocation failed with status 429 and no Retry-After (" + worker.LimitNotLiftedPhrase + ")",
		"model invocation failed with status 401",
		worker.GatewayBookkeepingPhrase + " (no usage)",
		worker.AnswerUnusablePhrase + " (content 0 bytes, limit 200000)",
		worker.DeclinedOverContentPhrase + " (finish_reason=content_filter)",
	} {
		notes = append(notes, receptionNote("受付の判定", "worker: readiness assessment failed: "+cause))
	}
	for _, note := range notes {
		if note == "" {
			t.Fatal("a note was empty")
		}
		for _, instruction := range []string{"ください", "出し直すと", "出し直して"} {
			if strings.Contains(note, instruction) {
				t.Errorf("a note instructs the requester (%q): %q", instruction, note)
			}
		}
	}
}

// What the worker really writes when a stage ends: its own re-ask notices
// first, then the line that ends the stage. The note must come from the
// last one — a transient the run recovered from must not decide what the
// requester is told (review of #122). Both readers scan the same way now,
// so the cutoff and the cause cannot disagree about which line is the one.
func TestTheNoteComesFromTheLineThatEndedTheStage(t *testing.T) {
	recovered := "worker: the provider ended the turn with an error; asking again in 2s (retry 1 of 3)\n" +
		"worker: the call spent its allowance without answering; asking again in 2s (retry 1 of 1)\n" +
		"worker: readiness assessment failed: " + worker.CutoffPhrase + ": finish_reason=length (output allowance 32768 tokens)\n"
	note := receptionNote("受付の判定", recovered)
	if !strings.Contains(note, "出力の上限で途切れた") {
		t.Fatalf("an earlier line decided the note: %q", note)
	}
	// And the other way: an ending line that is not a cutoff must not be
	// overruled by a cutoff mentioned earlier.
	earlier := "worker: readiness assessment failed: " + worker.CutoffPhrase + ": finish_reason=length (output allowance 32768 tokens)\n" +
		"worker: readiness check failed: " + worker.DeclinedOverContentPhrase + " (finish_reason=content_filter)\n"
	if note := receptionNote("受付の確認", earlier); !strings.Contains(note, "依頼文の内容を理由に") {
		t.Fatalf("an earlier cutoff decided the note: %q", note)
	}
}

// The readiness gate has seven ways to end as a model failure. Two of them
// come from a stage the model ran; the other five come from a record this
// pipeline could not read or could not accept, and they left the ticket
// with the failure class and nothing else (review of #122). A verdict
// outside its two values is one of the five.
func TestARecordTheGateCouldNotAcceptAlsoLeavesAReason(t *testing.T) {
	stub := receptionStubWorker(t, "no-such-subcommand", "")
	pipeline := receptionPipeline(t, stub)
	// Both model stages succeed and write a check whose verdict is neither
	// pass nor fail, which is the exit this measures.
	if err := os.MkdirAll(pipeline.path("history/readiness"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pipeline.path("history/readiness/check-1.json"), []byte(`{"verdict":"maybe"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	outcome, err := pipeline.readinessGate(context.Background())
	if err != nil || outcome.Code != hook.TerminalModelFailed {
		t.Fatalf("readinessGate() = %+v, %v; want model_failed", outcome, err)
	}
	text := readReceptionTrail(t, pipeline)
	for _, says := range []string{"記録を読めなかった", "受付の確認", "依頼の内容とは別のところで止まっています", "運用担当者が記録を確認します"} {
		if !strings.Contains(text, says) {
			t.Errorf("a record the gate could not accept left out %q: %q", says, text)
		}
	}
	if err := hook.ValidateTrailText(text); err != nil {
		t.Fatalf("the trail would be refused by the report: %v", err)
	}
}

// Two of the three stage names begin with 受付の, so a note that prefixes
// them with 受付の again reads as 受付の 受付の判定 (review of #122). The
// notes a requester sees most are exactly these two.
func TestNoNoteRepeatsTheStagesOwnPrefix(t *testing.T) {
	for _, stage := range []string{"受付の判定", "受付の確認", deriveStage} {
		for _, note := range []string{
			unnamedReceptionNote(stage),
			receptionNote(stage, "worker: readiness assessment failed: "+worker.TransportFailedPhrase+" with status 401"),
			receptionNote(stage, "worker: readiness assessment failed: "+worker.CutoffPhrase+": finish_reason=length (output allowance 32768 tokens)"),
		} {
			if strings.Contains(note, "受付の "+stage) {
				t.Errorf("the note repeats the stage's own prefix: %q", note)
			}
		}
	}
}

// The requester is told which stage stopped, and the two model stages are
// separate. Only the first was measured, so the second could name the first
// and nothing would say so (review of #126).
func TestTheNoteNamesTheStageThatActuallyStopped(t *testing.T) {
	stub := receptionStubWorker(t, "check-readiness",
		"worker: readiness check failed: "+worker.DeclinedOverContentPhrase+" (finish_reason=content_filter)")
	pipeline := receptionPipeline(t, stub)
	if outcome, err := pipeline.readinessGate(context.Background()); err != nil || outcome.Code != hook.TerminalModelFailed {
		t.Fatalf("readinessGate() = %+v, %v; want model_failed", outcome, err)
	}
	text := readReceptionTrail(t, pipeline)
	if !strings.Contains(text, "受付の確認") || strings.Contains(text, "受付の判定") {
		t.Fatalf("the note names the wrong stage: %q", text)
	}
}
