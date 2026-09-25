package hook

import (
	"strconv"
	"strings"
	"testing"
)

// openRoundRecord is a question record for the round given, so a test can
// put a comment written for one round in front of another.
func openRoundRecord(revision int) QuestionRecord {
	record := intakeTestRecord(intakeTwoQuestionSet)
	record.QuestionRevision = revision
	if revision > 1 {
		record.ClarificationSHA256 = strings.Repeat("a", 64)
	}
	return record
}

// What round a comment was written for is this package's to decide, not the
// reading's: the reading is shown the questions, never the round, so an
// answer to an earlier attempt's question reads exactly like an answer to
// this one. Taking it resumes a run with a decision the requester made about
// something else (live 2026-09-25).
func TestAnAnswerNamingAnotherRoundIsNotTheOpenQuestionsAnswer(t *testing.T) {
	answered := AnswerReading{Kind: AnswerReadingAnswer, Answers: map[string]string{"Q1": "a"}}
	stale := intakeComment(101, "回答 C1 Q1:a")

	decision, err := EvaluateAnswerIntake(intakeInput(openRoundRecord(2), read(stale, answered)))
	if err != nil {
		t.Fatalf("EvaluateAnswerIntake() error = %v", err)
	}
	if decision.Adopted != nil {
		t.Fatalf("an answer written for C1 was adopted while C2 was open: %+v", decision.Adopted)
	}

	// The same words, with the round they name open, are the answer.
	decision, err = EvaluateAnswerIntake(intakeInput(openRoundRecord(1), read(stale, answered)))
	if err != nil {
		t.Fatalf("EvaluateAnswerIntake() error = %v", err)
	}
	if decision.Adopted == nil || decision.Adopted.CommentID != 101 {
		t.Fatalf("the answer to the open round was not adopted: %+v", decision.Adopted)
	}

	// A comment that names no round at all is left to the reading, which is
	// how a person who answers in their own words is heard.
	plain := intakeComment(102, "b でお願いします")
	decision, err = EvaluateAnswerIntake(intakeInput(openRoundRecord(2), read(plain, answered)))
	if err != nil {
		t.Fatalf("EvaluateAnswerIntake() error = %v", err)
	}
	if decision.Adopted == nil {
		t.Fatal("an answer in the requester's own words was dropped for naming no round")
	}
}

// Ids put comments in order; they do not say when anything was written. A
// comment that existed before the question did cannot be its answer, however
// it is numbered.
func TestACommentOlderThanTheQuestionIsNotItsAnswer(t *testing.T) {
	answered := AnswerReading{Kind: AnswerReadingAnswer, Answers: map[string]string{"Q1": "a"}}
	comment := intakeComment(101, "Q1: a")
	comment.PostedAt = 3000

	input := intakeInput(openRoundRecord(1), read(comment, answered))
	input.QuestionPostedAt = 3500
	decision, err := EvaluateAnswerIntake(input)
	if err != nil {
		t.Fatalf("EvaluateAnswerIntake() error = %v", err)
	}
	if decision.Adopted != nil {
		t.Fatalf("a comment written before the question was adopted as its answer: %+v", decision.Adopted)
	}

	input.QuestionPostedAt = 2500
	decision, err = EvaluateAnswerIntake(input)
	if err != nil {
		t.Fatalf("EvaluateAnswerIntake() error = %v", err)
	}
	if decision.Adopted == nil {
		t.Fatal("a comment written after the question was not adopted")
	}
}

// The cancellation the question printed ends the run here, with no reading
// at all, and it beats an answer in the same snapshot.
func TestThePrescribedCancelIsReadByTheEngine(t *testing.T) {
	record := openRoundRecord(1)
	cancel := intakeComment(101, "中止 C1")
	decision, err := EvaluateAnswerIntake(intakeInput(record))
	if err != nil {
		t.Fatalf("EvaluateAnswerIntake() error = %v", err)
	}
	if decision.Cancel != nil {
		t.Fatal("an empty snapshot cancelled the run")
	}
	// No reading is supplied for the comment at all.
	input := intakeInput(record)
	input.Comments = append(input.Comments, cancel)
	decision, err = EvaluateAnswerIntake(input)
	if err != nil {
		t.Fatalf("EvaluateAnswerIntake() error = %v", err)
	}
	if decision.Cancel == nil || decision.Cancel.CommentID != 101 {
		t.Fatalf("the prescribed cancellation was not honoured: %+v", decision)
	}

	// Written in full-width characters, as a Japanese keyboard produces it.
	wide := intakeComment(102, "中止Ｃ１")
	input = intakeInput(record)
	input.Comments = append(input.Comments, wide)
	decision, err = EvaluateAnswerIntake(input)
	if err != nil {
		t.Fatalf("EvaluateAnswerIntake() error = %v", err)
	}
	if decision.Cancel == nil {
		t.Fatal("a full-width cancellation was not honoured")
	}

	// Naming another round, it is not this question's cancellation.
	input = intakeInput(openRoundRecord(2))
	input.Comments = append(input.Comments, cancel)
	decision, err = EvaluateAnswerIntake(input)
	if err != nil {
		t.Fatalf("EvaluateAnswerIntake() error = %v", err)
	}
	if decision.Cancel != nil {
		t.Fatalf("a cancellation for C1 ended a run waiting on C2: %+v", decision.Cancel)
	}
}

// One rule for what a stop is, wherever it is read from.
func TestAStopIsTheWholeFirstLineAndNothingElse(t *testing.T) {
	for body, want := range map[string]bool{
		"停止":            true,
		"\n  停止  \n":    true,
		"停止\nこの方針は違います": true,
		"レビュー後に停止も検討します": false,
		"方針は良いです\n停止":    false,
		"停止してください":       false,
		"":               false,
		"中止 C1":          false,
	} {
		if got := IsStopComment(body); got != want {
			t.Fatalf("IsStopComment(%q) = %v, want %v", body, got, want)
		}
	}
}

// What a wait has read of a thread is remembered against the question it
// read it for. A second question on the same run has a thread of its own to
// read once - including the comments written while the first round was
// being answered, which the first read never saw.
func TestARememberedScanBelongsToOneQuestion(t *testing.T) {
	service := &QuestionTickService{}
	service.rememberHistoryScan("run-42", 100, 5000)

	if postedAt, scanned := service.historyScanned("run-42", 100); !scanned || postedAt != 5000 {
		t.Fatalf("the scan for the question it was taken for reads back as (%d, %v)", postedAt, scanned)
	}
	if _, scanned := service.historyScanned("run-42", 200); scanned {
		t.Fatal("a second question inherited what was read for the first")
	}
	if _, scanned := service.historyScanned("run-43", 100); scanned {
		t.Fatal("another run inherited what this one read")
	}
	service.forgetHistoryScan("run-42")
	if _, scanned := service.historyScanned("run-42", 100); scanned {
		t.Fatal("a wait that ended is still remembered")
	}
}

// Forgetting is only ever a re-read, so the map is allowed to give up
// everything rather than grow without a bound.
func TestRememberedScansAreBounded(t *testing.T) {
	service := &QuestionTickService{}
	for index := 0; index <= maxRememberedScans; index++ {
		service.rememberHistoryScan("run-"+strconv.Itoa(index), 100, 5000)
	}
	if len(service.scanned) > maxRememberedScans {
		t.Fatalf("remembered scans = %d, over the bound %d", len(service.scanned), maxRememberedScans)
	}
	if _, scanned := service.historyScanned("run-"+strconv.Itoa(maxRememberedScans), 100); !scanned {
		t.Fatal("the newest scan was the one dropped")
	}
}
