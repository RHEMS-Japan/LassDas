package hook

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// tenQuestionSet is what a reception that asks everything at once produces:
// ten questions, each a sentence with a reason and three choices whose
// effects say what the requester gets.
func tenQuestionSet(count int) string {
	items := make([]string, 0, count)
	for index := 1; index <= count; index++ {
		items = append(items, fmt.Sprintf(`{"id":"Q%d","dimension":"user_visible_behavior",`+
			`"question":"%d 件目の確認です。一覧の絞り込みを保存したまま画面を離れて戻ったとき、絞り込みは元のまま残すべきですか。",`+
			`"why_blocking":"残すかどうかで、戻ってきた利用者が最初に見る一覧の中身が変わります。どちらでも実装はできます。",`+
			`"choices":[{"id":"a","label":"絞り込みを残す","effect":"戻ると前回と同じ絞り込みのままの一覧が出ます。"},`+
			`{"id":"b","label":"毎回すべて表示に戻す","effect":"戻るたびに絞り込みなしの一覧が出ます。"},`+
			`{"id":"c","label":"その日のうちだけ残す","effect":"同じ日は前回の絞り込み、翌日はすべて表示になります。"}]}`, index, index))
	}
	return "[" + strings.Join(items, ",") + "]"
}

// The reception asks once, so the one comment has to carry every question it
// has. Ten of them fit inside the record the store seals and inside the
// comment the tracker accepts, with the copy-paste template that answers
// them all in one post.
func TestTenQuestionsFitOneComment(t *testing.T) {
	questions := tenQuestionSet(10)
	record := intakeTestRecord(questions)
	sealed, err := MarshalQuestionRecord(record)
	if err != nil {
		t.Fatalf("a ten-question record does not seal: %v", err)
	}
	content, err := QuestionCommentContent(record)
	if err != nil {
		t.Fatalf("QuestionCommentContent() error = %v", err)
	}
	if len(questions) > MaxQuestionSetBytes || len(sealed) > MaxQuestionRecordBytes || len(content) > MaxTrackerCommentBytes {
		t.Fatalf("ten questions overflow: set %d/%d, record %d/%d, comment %d/%d",
			len(questions), MaxQuestionSetBytes, len(sealed), MaxQuestionRecordBytes, len(content), MaxTrackerCommentBytes)
	}
	t.Logf("ten questions: set %d bytes, record %d bytes, comment %d bytes (limit %d)",
		len(questions), len(sealed), len(content), MaxTrackerCommentBytes)

	// The measured size is an upper bound on the posted one, so a reception
	// that holds itself to it is never refused at the posting.
	measured, err := RenderedQuestionCommentBytes(questions)
	if err != nil {
		t.Fatal(err)
	}
	if measured < len(content) {
		t.Fatalf("measured %d bytes, posted %d: the measure has to be the wider one", measured, len(content))
	}
	// Including the longest run the marker admits, which is the only part of
	// the envelope a set cannot see when it is being measured.
	widest := record
	widest.AutomationRunID = "r" + strings.Repeat("9", 127)
	widestContent, err := QuestionCommentContent(widest)
	if err != nil {
		t.Fatalf("a record with the longest run id does not render: %v", err)
	}
	if measured < len(widestContent) {
		t.Fatalf("measured %d bytes, the widest posting is %d", measured, len(widestContent))
	}

	// Every question is printed with its own copy-paste line per choice, and
	// the template answers all ten at once.
	for index := 1; index <= 10; index++ {
		id := fmt.Sprintf("Q%d", index)
		if !strings.Contains(content, "回答 C1 "+id+":a") {
			t.Fatalf("the comment has no copy-paste line for %s", id)
		}
		if !strings.Contains(content, "\n"+id+": _\n") {
			t.Fatalf("the template has no line for %s", id)
		}
	}
	if !strings.Contains(content, "【回答テンプレート】") || !strings.Contains(content, "\n回答 C1\n") {
		t.Fatalf("the comment carries no answer template:\n%s", content)
	}
	if !strings.Contains(content, "確認をお願いするのはこの 1 回だけです") {
		t.Fatal("the comment does not say that this is the only round")
	}
	if err := ValidateCommentContract(content, CommentMarker("question", record.AutomationRunID, "C1")); err != nil {
		t.Fatalf("the comment broke its contract: %v", err)
	}
	again, err := QuestionCommentContent(record)
	if err != nil || again != content {
		t.Fatal("the ten-question comment is not deterministic")
	}
}

// The template the comment prints is the grammar the engine reads: pasted
// back with the blanks filled in, it is a complete answer to all ten, and
// the tenth question is Q10 - a number the ids reach only now that the
// reception asks everything at once.
func TestTheTemplateIsReadBackAsACompleteAnswer(t *testing.T) {
	questions, err := decodeIntakeQuestions(tenQuestionSet(10))
	if err != nil {
		t.Fatalf("a ten-question set does not decode: %v", err)
	}
	if len(questions) != 10 || questions[9].id != "Q10" {
		t.Fatalf("decoded %d questions, last id %q", len(questions), questions[9].id)
	}
	lines := []string{"回答 C1"}
	for index := 1; index <= 10; index++ {
		lines = append(lines, fmt.Sprintf("Q%d: b", index))
	}
	answers, missing, ok := parseAnswerBody(strings.Join(lines, "\n"), 1, questions)
	if !ok {
		t.Fatal("the filled-in template was not read as an answer")
	}
	if len(missing) != 0 || len(answers) != 10 || answers["Q10"] != "b" {
		t.Fatalf("answers = %v, missing = %v", answers, missing)
	}

	// A post that stops early is read for what it says and reports the rest
	// as unanswered, in the order the questions were numbered.
	partial, stillMissing, ok := parseAnswerBody(strings.Join(lines[:9], "\n"), 1, questions)
	if !ok {
		t.Fatal("a partial answer was not read at all")
	}
	if len(partial) != 8 || strings.Join(stillMissing, ",") != "Q9,Q10" {
		t.Fatalf("answered %d, missing %v, want eight answered and Q9,Q10 missing", len(partial), stillMissing)
	}
}

// How long a requester gets is the destination's, and every window still
// carries three reminders on three separate weekdays before the deadline -
// the shape the sealed record refuses to be without.
func TestTheAnswerWindowIsTheDestinationsAndStaysValid(t *testing.T) {
	posted := jstDate(t, 2026, 8, 3, 9)
	defaultNotify, defaultDeadline := ComputeQuestionSchedule(posted)
	sameByWidth, sameDeadline := ComputeQuestionScheduleWithin(posted, DefaultQuestionDeadlineWeekdays)
	if sameByWidth != defaultNotify || sameDeadline != defaultDeadline {
		t.Fatal("the five-weekday window is no longer what the default computes")
	}
	for weekdays := MinQuestionDeadlineWeekdays; weekdays <= MaxQuestionDeadlineWeekdays; weekdays++ {
		notifyAt, deadlineAt := ComputeQuestionScheduleWithin(posted, weekdays)
		record := intakeTestRecord(questionTestSetJSON)
		record.NotifyAt, record.AnswerDeadlineAt = notifyAt, deadlineAt
		if err := record.ValidateShape(); err != nil {
			t.Fatalf("%d weekdays produced a schedule the record refuses: %v", weekdays, err)
		}
		last := time.UnixMilli(deadlineAt).In(questionZone)
		if last.Hour() != questionDeadlineHour || last.Weekday() == time.Saturday || last.Weekday() == time.Sunday {
			t.Fatalf("%d weekdays ends at %s", weekdays, last)
		}
	}
	// A number outside the offered range is not a schedule nobody asked
	// for: the standard window stands.
	outside, outsideDeadline := ComputeQuestionScheduleWithin(posted, 0)
	if outside != defaultNotify || outsideDeadline != defaultDeadline {
		t.Fatal("an out-of-range window did not fall back to the default")
	}
}

// A set the store can seal is not automatically a set a person can read: the
// reception has to be able to measure what it is about to post, from the
// questions alone, before any record exists.
func TestAQuestionSetTooLongToPostIsMeasurableBeforeItIsSealed(t *testing.T) {
	var questions []map[string]any
	if err := json.Unmarshal([]byte(tenQuestionSet(10)), &questions); err != nil {
		t.Fatal(err)
	}
	for index := range questions {
		questions[index]["question"] = strings.Repeat("あ", 600)
		questions[index]["why_blocking"] = strings.Repeat("い", 600)
	}
	encoded, err := json.Marshal(questions)
	if err != nil {
		t.Fatal(err)
	}
	size, err := RenderedQuestionCommentBytes(string(encoded))
	if err != nil {
		t.Fatal(err)
	}
	if size <= MaxTrackerCommentBytes {
		t.Fatalf("ten questions of 600 characters each measure %d bytes, which the limit %d would accept", size, MaxTrackerCommentBytes)
	}
}
