package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func answeredAt() time.Time { return time.Date(2026, 9, 25, 3, 0, 0, 0, time.UTC) }

// A report that says the ticket left something open is answered by the
// engine, not by the requester: the reading that is easiest to defend is
// stated, recorded as an assumption, and put into the instruction the same
// round runs under next.
func TestAReportThatLacksInformationIsAnsweredWithADefensibleDefault(t *testing.T) {
	const report = "依頼に、並び順を新しい順にするか古い順にするかが書かれていません。\nどちらにするかの判断は依頼者に返します。"
	answer := AnswerReturn(report, nil, answeredAt())

	if answer.Attempt != 1 || answer.Repeated {
		t.Fatalf("attempt = %d, repeated = %v, want the first return", answer.Attempt, answer.Repeated)
	}
	if answer.Assumption.Kind != AssumptionImplementerReturn {
		t.Fatalf("assumption kind = %q", answer.Assumption.Kind)
	}
	if !strings.Contains(answer.Assumption.Statement, "最も擁護できる既定") {
		t.Fatalf("the assumption does not state the reading taken: %q", answer.Assumption.Statement)
	}
	if !strings.Contains(answer.Assumption.Evidence, shortDigest(answer.ReportSHA256)) {
		t.Fatalf("the assumption is not bound to the report it answers: %q", answer.Assumption.Evidence)
	}
	if len(answer.Supply) != 0 {
		t.Fatalf("a report that named no key asked for one to be supplied: %q", answer.Supply)
	}
	// The three things the instruction has to say, in the requester's own
	// language: there is nowhere to hand this back to, choose the default
	// yourself, and finishing with nothing changed is not an option.
	for _, want := range []string{"作業を返す先はありません", "最も擁護できる既定", "変更を 1 つも加えずに終了することはできません"} {
		if !strings.Contains(answer.Instruction, want) {
			t.Fatalf("the instruction does not say %q:\n%s", want, answer.Instruction)
		}
	}
	if answer.Report != ReportText(report) {
		t.Fatalf("the record lost the agent's own words: %q", answer.Report)
	}
}

// A report that asks for a key is answered with a stand-in, and what has to
// be supplied for the real thing is written down in the agent's own words —
// which is what the operator later reads instead of a question.
func TestAReportThatAsksForAKeyGetsAStandInAndTheSupplyIsRecorded(t *testing.T) {
	const report = "外部の天気サービスを呼ぶ必要がありますが、API キーが渡されていません。\n" +
		"取得先の URL は決まっています。\n" +
		"アクセストークンを用意してもらえれば実装できます。"
	answer := AnswerReturn(report, nil, answeredAt())

	if answer.Assumption.Kind != AssumptionCredentialSubstituted {
		t.Fatalf("assumption kind = %q, want the stand-in", answer.Assumption.Kind)
	}
	if len(answer.Supply) != 2 {
		t.Fatalf("what must be supplied = %q, want the two lines that named it", answer.Supply)
	}
	if !strings.Contains(answer.Supply[0], "API キー") || !strings.Contains(answer.Supply[1], "アクセストークン") {
		t.Fatalf("the supply does not quote the report: %q", answer.Supply)
	}
	for _, want := range []string{"代役 (test double / fake)", "供給すべきか"} {
		if !strings.Contains(answer.Instruction, want) {
			t.Fatalf("the instruction does not ask for a stand-in: %q\n%s", want, answer.Instruction)
		}
	}
	if !strings.Contains(answer.Instruction, "API キー") {
		t.Fatalf("the instruction does not carry what was named as missing:\n%s", answer.Instruction)
	}
	if !strings.Contains(answer.Assumption.Evidence, "API キー") {
		t.Fatalf("the assumption does not carry what was named as missing: %q", answer.Assumption.Evidence)
	}
}

// The same report twice is the round saying nothing new. It is recorded as
// exactly that — same digest, marked as a repeat — so a later reader can
// see a round coming back rather than a relaunch nobody counted, and the
// instruction stops restating the rule and names what must be produced.
func TestTheSameReportTwiceIsRecordedAsARepeat(t *testing.T) {
	const report = "この依頼は、いまのままでは実現できません。"
	record := &ReturnedRound{}
	first := AnswerReturn(report, record, answeredAt())
	record.Append(1, first)
	second := AnswerReturn(report, record, answeredAt().Add(time.Minute))
	record.Append(1, second)

	if second.Attempt != 2 || !second.Repeated {
		t.Fatalf("second answer attempt = %d, repeated = %v", second.Attempt, second.Repeated)
	}
	if second.ReportSHA256 != first.ReportSHA256 {
		t.Fatalf("the same report carries two digests: %q / %q", first.ReportSHA256, second.ReportSHA256)
	}
	if strings.Contains(first.Instruction, "同じ報告をもう一度返しても") {
		t.Fatalf("the first answer already called the round a repeat:\n%s", first.Instruction)
	}
	if !strings.Contains(second.Instruction, "同じ報告をもう一度返しても") {
		t.Fatalf("the repeat is not said in the instruction:\n%s", second.Instruction)
	}
	if len(record.Returns) != 2 {
		t.Fatalf("the round's record holds %d returns, want both", len(record.Returns))
	}
	if kinds := record.Assumptions(); len(kinds) != 2 {
		t.Fatalf("the round's assumptions = %d, want one per return", len(kinds))
	}

	// A different report is a different position, and answering it is not a
	// repeat however many times the round has been handed back.
	third := AnswerReturn(report+"\n別の理由も見つかりました。", record, answeredAt().Add(2*time.Minute))
	if third.Attempt != 3 || third.Repeated {
		t.Fatalf("third answer attempt = %d, repeated = %v", third.Attempt, third.Repeated)
	}
}

// The record is read whole, so it cannot grow without end; the opening
// return and the newest one are the two that are never dropped, because the
// report quotes the first and the next repeat is compared against the last.
func TestTheRoundsRecordStaysBounded(t *testing.T) {
	record := &ReturnedRound{}
	for attempt := 1; attempt <= maxReturnAttempts+3; attempt++ {
		record.Append(1, AnswerReturn(strings.Repeat("x", attempt), record, answeredAt()))
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
	answer := AnswerReturn(strings.Repeat("報", maxReturnedReportBytes), nil, answeredAt())
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
