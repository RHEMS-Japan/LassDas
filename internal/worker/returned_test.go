package worker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func answeredAt() time.Time { return time.Date(2026, 9, 25, 3, 0, 0, 0, time.UTC) }

// launches counts the stand-in launches a test has made, so that two of
// them never share an identity however alike their reports are — which is
// exactly what the real records do, because the digest covers when the
// agent ran.
var launches int

// launch is one implementing run that reported and changed nothing.
func launch(t *testing.T, transcript string) AgentRun {
	t.Helper()
	launches++
	run, err := SealAgentRun(AgentRun{
		SchemaVersion: ArtifactSchemaVersion, Stage: 1, AgentID: "implementer",
		Transcript: transcript, DurationMs: int64(launches), RanAt: answeredAt(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// A report that says the ticket left something open is answered by the
// engine, not by the requester: the reading that is easiest to defend is
// requested only within unspecified details of the original contract. The
// record says what was instructed, not that a default was already adopted.
func TestAReportThatLacksInformationIsAnsweredWithADefensibleDefault(t *testing.T) {
	const report = "依頼に、並び順を新しい順にするか古い順にするかが書かれていません。\nどちらにするかの判断は依頼者に返します。"
	answer := AnswerReturn(launch(t, report), nil, answeredAt())

	if answer.Attempt != 1 || answer.Repeated {
		t.Fatalf("attempt = %d, repeated = %v, want the first return", answer.Attempt, answer.Repeated)
	}
	if answer.Assumption.Kind != AssumptionImplementerReturn {
		t.Fatalf("assumption kind = %q", answer.Assumption.Kind)
	}
	if !strings.Contains(answer.Assumption.Statement, "再試行するよう指示した") {
		t.Fatalf("the record does not state the instruction actually given: %q", answer.Assumption.Statement)
	}
	if !strings.Contains(answer.Assumption.Evidence, shortDigest(answer.ReportSHA256)) {
		t.Fatalf("the assumption is not bound to the report it answers: %q", answer.Assumption.Evidence)
	}
	if len(answer.Supply) != 0 {
		t.Fatalf("a report that named no key asked for one to be supplied: %q", answer.Supply)
	}
	for _, want := range []string{"依頼者へ質問せず", "未指定の実装詳細に限り", "最も擁護できる既定", "元の条件を優先", "変更禁止の条件を守って"} {
		if !strings.Contains(answer.Instruction, want) {
			t.Fatalf("the instruction does not say %q:\n%s", want, answer.Instruction)
		}
	}
	if answer.Report != ReportText(report) {
		t.Fatalf("the record lost the agent's own words: %q", answer.Report)
	}
}

// A mention of a key is kept as a reported obstacle. It does not authorize
// a substitute or make a promise that an operator will supply anything.
func TestAReportThatAsksForAKeyDoesNotAuthorizeASubstitute(t *testing.T) {
	const report = "外部の天気サービスを呼ぶ必要がありますが、API キーが渡されていません。\n" +
		"取得先の URL は決まっています。\n" +
		"アクセストークンを用意してもらえれば実装できます。"
	answer := AnswerReturn(launch(t, report), nil, answeredAt())

	if answer.Assumption.Kind != AssumptionImplementerReturn {
		t.Fatalf("assumption kind = %q, want a constrained retry", answer.Assumption.Kind)
	}
	if len(answer.Supply) != 2 {
		t.Fatalf("what must be supplied = %q, want the two lines that named it", answer.Supply)
	}
	if !strings.Contains(answer.Supply[0], "API キー") || !strings.Contains(answer.Supply[1], "アクセストークン") {
		t.Fatalf("the supply does not quote the report: %q", answer.Supply)
	}
	for _, want := range []string{"既に許可された設定と手段の範囲", "要求された実接続や本番納品の代わりにしてはいけません"} {
		if !strings.Contains(answer.Instruction, want) {
			t.Fatalf("the instruction lost a recovery boundary: %q\n%s", want, answer.Instruction)
		}
	}
	if !strings.Contains(answer.Instruction, "API キー") {
		t.Fatalf("the instruction does not carry what was named as missing:\n%s", answer.Instruction)
	}
	if !strings.Contains(answer.Assumption.Evidence, "API キー") {
		t.Fatalf("the assumption does not carry what was named as missing: %q", answer.Assumption.Evidence)
	}
}

// The same report twice is the round saying nothing new, and the engine's
// answer has plainly not moved it. That return is not answered again: it is
// recorded as a repeat, carries no assumption and no instruction, and its
// caller hands the round to the ladder.
func TestARepeatedReportIsNotAnsweredAgain(t *testing.T) {
	const report = "この依頼は、いまのままでは実現できません。"
	record := &ReturnedRound{}
	first := AnswerReturn(launch(t, report), record, answeredAt())
	record.Append(1, first)
	second := AnswerReturn(launch(t, report), record, answeredAt().Add(time.Minute))
	record.Append(1, second)

	if !first.Answered || first.Attempt != 1 || first.Repeated {
		t.Fatalf("first answer = %+v", first)
	}
	if second.Attempt != 2 || !second.Repeated || second.Answered {
		t.Fatalf("second answer attempt = %d, repeated = %v, answered = %v",
			second.Attempt, second.Repeated, second.Answered)
	}
	if second.ReportSHA256 != first.ReportSHA256 {
		t.Fatalf("the same report carries two digests: %q / %q", first.ReportSHA256, second.ReportSHA256)
	}
	if second.Instruction != "" || second.Assumption.Kind != "" {
		t.Fatalf("a return nobody answered decided something: %+v", second)
	}
	if kinds := record.Assumptions(); len(kinds) != 1 {
		t.Fatalf("the round's assumptions = %d, want only the one that was answered", len(kinds))
	}

	// A different report is a different position, and it is answered — the
	// bound is on answers that changed nothing, not on rounds.
	third := AnswerReturn(launch(t, report+"\n別の理由も見つかりました。"), record, answeredAt().Add(2*time.Minute))
	if third.Attempt != 3 || third.Repeated || !third.Answered {
		t.Fatalf("third answer = %+v", third)
	}
	if !strings.Contains(third.Instruction, "この巡が戻ってきたのは 3 回目です") {
		t.Fatalf("a later answer does not say where the round stands:\n%s", third.Instruction)
	}
}

// The bound on the answering. The engine's answer is the same three rules
// every time, so a round that has heard them three times and come back with
// something new a fourth is not being persuaded; past that the return is a
// model declining the work and the ladder is what this engine does about
// one.
func TestTheEngineStopsAnsweringOneRoundAfterThreeAnswers(t *testing.T) {
	record := &ReturnedRound{}
	for attempt := 1; attempt <= maxAnsweredReturns; attempt++ {
		answer := AnswerReturn(launch(t, fmt.Sprintf("理由 %d を見つけました。", attempt)), record, answeredAt())
		if !answer.Answered || answer.Attempt != attempt {
			t.Fatalf("attempt %d = %+v, want it answered", attempt, answer)
		}
		record.Append(1, answer)
	}
	beyond := AnswerReturn(launch(t, "さらに別の理由です。"), record, answeredAt())
	if beyond.Attempt != maxAnsweredReturns+1 || beyond.Repeated || beyond.Answered {
		t.Fatalf("the return past the bound = %+v, want it left to the ladder", beyond)
	}
	if beyond.Instruction != "" {
		t.Fatalf("a return past the bound was still told something:\n%s", beyond.Instruction)
	}
	if kinds := record.Assumptions(); len(kinds) != maxAnsweredReturns {
		t.Fatalf("assumptions = %d, want one per answer the engine made", len(kinds))
	}
}

// The record is read whole, so it cannot grow without end; the opening
// return and the newest one are the two that are never dropped, because the
// report quotes the first and the next repeat is compared against the last.
func TestTheRoundsRecordStaysBounded(t *testing.T) {
	record := &ReturnedRound{}
	for attempt := 1; attempt <= maxReturnAttempts+3; attempt++ {
		record.Append(1, AnswerReturn(launch(t, strings.Repeat("x", attempt)), record, answeredAt()))
	}
	if len(record.Returns) != maxReturnAttempts {
		t.Fatalf("the record holds %d returns, want it bounded at %d", len(record.Returns), maxReturnAttempts)
	}
	if record.Returns[0].Attempt != 1 {
		t.Fatalf("the opening return was dropped: attempt %d", record.Returns[0].Attempt)
	}
	if latest := record.Latest(); latest == nil || latest.Attempt != maxReturnAttempts+3 {
		t.Fatalf("the newest return was dropped: %+v", record.Latest())
	}
}

// A report longer than the record's budget is cut on a rune boundary and
// says it was cut. The whole report still reaches the requester through the
// trail; this bound is on what the next attempt is shown of itself.
func TestALongReportIsCutWhereARuneEnds(t *testing.T) {
	answer := AnswerReturn(launch(t, strings.Repeat("報", maxReturnedReportBytes)), nil, answeredAt())
	if len(answer.Report) > maxReturnedReportBytes {
		t.Fatalf("the record holds %d bytes, over its budget", len(answer.Report))
	}
	if !strings.HasSuffix(answer.Report, "ここまでを記録しています)") {
		t.Fatalf("a cut report does not say it was cut: %q", answer.Report[len(answer.Report)-60:])
	}
	if strings.ContainsRune(answer.Report, '\uFFFD') {
		t.Fatal("the report was cut in the middle of a character")
	}
}

// A round nobody handed back reads as no record and no error; a record that
// is there and will not read is an error, because read as an absence the
// same report would be answered as a first return forever.
func TestAnUnreadableReturnRecordIsNotAnAbsence(t *testing.T) {
	directory := t.TempDir()
	missing := filepath.Join(directory, ReturnRecordFileName)
	record, err := ReadReturnedRoundFile(missing)
	if record != nil || err != nil {
		t.Fatalf("a round nobody handed back = %+v, %v", record, err)
	}
	if err := os.WriteFile(missing, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadReturnedRoundFile(missing); err == nil {
		t.Fatal("an unreadable record read as a round nobody handed back")
	}
}

// A delivery that stopped at a return boundary leaves a record saying what
// happened to the round. It used to say the work had been returned, which
// stopped being true: the engine answers such a round and starts it again,
// and a record that does not say so reads as though nobody had done
// anything about it.
func TestTheRecordSaysTheEngineAnsweredTheReturn(t *testing.T) {
	history := t.TempDir()
	const report = "外部の決済サービスの API キーが渡されていません。"
	writeRoundRun(t, history, 1, "implementer-run.json", returnedRun(report))
	record := &ReturnedRound{}
	record.Append(1, AnswerReturn(launch(t, report), record, answeredAt()))
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(history, "stage-1", ReturnRecordFileName), encoded, 0o600); err != nil {
		t.Fatal(err)
	}

	round, err := LoadUnsealedRound(history, Config{})
	if err != nil {
		t.Fatalf("LoadUnsealedRound: %v", err)
	}
	if round.EngineAnswers != 1 {
		t.Fatalf("the round's answers = %d, want the one the engine made", round.EngineAnswers)
	}
	trail := ComposeUnsealedTrail(round, "変更の確定")
	if !strings.Contains(trail, "この報告は依頼者に返していません") {
		t.Fatalf("the record still reads as work handed back:\n%s", trail)
	}
	if !strings.Contains(trail, "本体が 1 回") {
		t.Fatalf("the record does not say how often the engine decided:\n%s", trail)
	}

	// A round nobody answered says nothing of the sort.
	plain := t.TempDir()
	writeRoundRun(t, plain, 1, "implementer-run.json", returnedRun(report))
	untouched, err := LoadUnsealedRound(plain, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if untouched.EngineAnswers != 0 {
		t.Fatalf("a round nobody answered counted %d answers", untouched.EngineAnswers)
	}
	if strings.Contains(ComposeUnsealedTrail(untouched, ""), "この報告は依頼者に返していません") {
		t.Fatal("a round nobody answered claimed the engine had")
	}
}
