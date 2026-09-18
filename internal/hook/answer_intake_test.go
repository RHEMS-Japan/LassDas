package hook

import (
	"testing"
)

const intakeTwoQuestionSet = `[{"id":"Q1","dimension":"user_visible_behavior","question":"並び順は?","why_blocking":"表示が変わる","choices":[{"id":"a","label":"新着順","effect":"新しい順"},{"id":"b","label":"名前順","effect":"五十音順"}]},{"id":"Q2","dimension":"data_lifecycle","question":"既存データは?","why_blocking":"移行が変わる","choices":[{"id":"a","label":"残す","effect":"全件保持"},{"id":"c","label":"消す","effect":"初期化"}]}]`

func intakeTestRecord(questionsJSON string) QuestionRecord {
	record := questionTestRecord()
	record.QuestionsJSON = questionsJSON
	record.QuestionsSHA256 = TerminalReportDigest([]byte(questionsJSON))
	return record
}

func intakeComment(commentID int64, body string) BacklogComment {
	return BacklogComment{CommentID: commentID, UserID: terminalTestConfig().AllowedCreatorID, Body: body, PostedAt: 3500}
}

// intakeInput pairs each comment with what the reading made of it, keyed the
// way the tick keys them.
func intakeInput(record QuestionRecord, pairs ...struct {
	comment BacklogComment
	reading AnswerReading
}) AnswerIntakeInput {
	input := AnswerIntakeInput{
		Question:          record,
		QuestionCommentID: 100,
		AnswererID:        terminalTestConfig().AllowedCreatorID,
		HandledCommentIDs: map[int64]bool{},
		Readings:          map[int64]AnswerReading{},
	}
	for _, pair := range pairs {
		input.Comments = append(input.Comments, pair.comment)
		input.Readings[pair.comment.CommentID] = pair.reading
	}
	return input
}

func read(comment BacklogComment, reading AnswerReading) struct {
	comment BacklogComment
	reading AnswerReading
} {
	return struct {
		comment BacklogComment
		reading AnswerReading
	}{comment: comment, reading: reading}
}

// What a person wrote is not what decides: the reading is. A comment that no
// pattern would have matched is an answer when the reading says it is.
func TestAnAnswerIsWhateverTheReadingSaysIsOne(t *testing.T) {
	record := intakeTestRecord(intakeTwoQuestionSet)
	comment := intakeComment(101, "a でいきます。展開のことは書かないでください。")
	decision, err := EvaluateAnswerIntake(intakeInput(record,
		read(comment, AnswerReading{Kind: AnswerReadingAnswer, Answers: map[string]string{"Q1": "a"}, NotNeeded: []string{"Q2"}})))
	if err != nil {
		t.Fatalf("EvaluateAnswerIntake() error = %v", err)
	}
	if decision.Adopted == nil || decision.Cancel != nil {
		t.Fatalf("decision = %+v, want the answer adopted", decision)
	}
	if decision.Adopted.CommentID != 101 || decision.Adopted.AnswersJSON != `{"Q1":"a"}` {
		t.Fatalf("adopted = %+v", decision.Adopted)
	}
}

// The delivery that stalled on 2026-09-17: a second question whose own words
// made it conditional on the first was never going to be answered, and the
// engine waited for it forever. Completeness is the asking role's judgement,
// so an answer that leaves a question open is adopted all the same.
func TestAnAnswerThatLeavesAQuestionOpenIsStillAdopted(t *testing.T) {
	record := intakeTestRecord(intakeTwoQuestionSet)
	comment := intakeComment(101, "Q1 は a で")
	decision, err := EvaluateAnswerIntake(intakeInput(record,
		read(comment, AnswerReading{Kind: AnswerReadingAnswer, Answers: map[string]string{"Q1": "a"}, Unanswered: []string{"Q2"}})))
	if err != nil {
		t.Fatalf("EvaluateAnswerIntake() error = %v", err)
	}
	if decision.Adopted == nil {
		t.Fatalf("an answer with one question still open was not adopted: %+v", decision)
	}
}

// A comment the reading calls unrelated is left alone. The automation used to
// answer back with the format the person should have used; that courtesy
// could fail, and when it did it stopped the delivery.
func TestACommentAboutSomethingElseIsLeftAlone(t *testing.T) {
	record := intakeTestRecord(intakeTwoQuestionSet)
	decision, err := EvaluateAnswerIntake(intakeInput(record,
		read(intakeComment(101, "ありがとうございます、確認します"), AnswerReading{Kind: AnswerReadingUnrelated})))
	if err != nil {
		t.Fatalf("EvaluateAnswerIntake() error = %v", err)
	}
	if decision.Adopted != nil || decision.Cancel != nil {
		t.Fatalf("decision = %+v, want nothing to happen", decision)
	}
}

// A stop wins over an answer, and the earliest one is the evidence.
func TestAStopWinsOverAnyAnswer(t *testing.T) {
	record := intakeTestRecord(intakeTwoQuestionSet)
	decision, err := EvaluateAnswerIntake(intakeInput(record,
		read(intakeComment(101, "やっぱりやめます"), AnswerReading{Kind: AnswerReadingCancel}),
		read(intakeComment(102, "やっぱり a で"), AnswerReading{Kind: AnswerReadingAnswer, Answers: map[string]string{"Q1": "a"}}),
	))
	if err != nil {
		t.Fatalf("EvaluateAnswerIntake() error = %v", err)
	}
	if decision.Cancel == nil || decision.Cancel.CommentID != 101 || decision.Adopted != nil {
		t.Fatalf("decision = %+v, want the earliest stop", decision)
	}
}

// The last answer the requester wrote is the one that counts.
func TestTheLastAnswerWins(t *testing.T) {
	record := intakeTestRecord(intakeTwoQuestionSet)
	decision, err := EvaluateAnswerIntake(intakeInput(record,
		read(intakeComment(101, "a で"), AnswerReading{Kind: AnswerReadingAnswer, Answers: map[string]string{"Q1": "a"}}),
		read(intakeComment(102, "やっぱり b で"), AnswerReading{Kind: AnswerReadingAnswer, Answers: map[string]string{"Q1": "b"}}),
	))
	if err != nil {
		t.Fatalf("EvaluateAnswerIntake() error = %v", err)
	}
	if decision.Adopted == nil || decision.Adopted.CommentID != 102 || decision.Adopted.AnswersJSON != `{"Q1":"b"}` {
		t.Fatalf("adopted = %+v, want the later answer", decision.Adopted)
	}
}

// A comment with no reading is one the model could not be asked about yet.
// It is skipped, never treated as an answer and never discarded: the next
// tick asks again.
func TestACommentWithNoReadingIsSkippedNotDiscarded(t *testing.T) {
	record := intakeTestRecord(intakeTwoQuestionSet)
	input := intakeInput(record)
	input.Comments = append(input.Comments, intakeComment(101, "a で"))
	decision, err := EvaluateAnswerIntake(input)
	if err != nil {
		t.Fatalf("EvaluateAnswerIntake() error = %v", err)
	}
	if decision.Adopted != nil || decision.Cancel != nil {
		t.Fatalf("decision = %+v, want the comment left for the next tick", decision)
	}
}

// Which comments are in scope is still routing, not reading: another
// person's comment, one posted before the question, and one posted after the
// deadline all stay out, whatever a reading would have said.
func TestOnlyTheAnswererInsideTheDeadlineTakesPart(t *testing.T) {
	record := intakeTestRecord(intakeTwoQuestionSet)
	answer := AnswerReading{Kind: AnswerReadingAnswer, Answers: map[string]string{"Q1": "a"}}

	someoneElse := intakeComment(101, "a で")
	someoneElse.UserID = terminalTestConfig().AllowedCreatorID + 1
	before := intakeComment(99, "a で")
	late := intakeComment(103, "a で")
	late.PostedAt = record.AnswerDeadlineAt

	decision, err := EvaluateAnswerIntake(intakeInput(record,
		read(someoneElse, answer), read(before, answer), read(late, answer)))
	if err != nil {
		t.Fatalf("EvaluateAnswerIntake() error = %v", err)
	}
	if decision.Adopted != nil || decision.Cancel != nil {
		t.Fatalf("decision = %+v, want none of them to count", decision)
	}
}

// A comment with no text says nothing about the questions. Reading one fails,
// and a failed reading is kept for the next tick, so a single empty comment
// on the ticket was retried every minute for as long as the question stayed
// open (live 2026-09-18).
func TestAnEmptyCommentIsNotSomethingToRead(t *testing.T) {
	record := intakeTestRecord(intakeTwoQuestionSet)
	blank := intakeComment(101, "   \n\t ")
	decision, err := EvaluateAnswerIntake(intakeInput(record,
		read(blank, AnswerReading{Kind: AnswerReadingUnrelated})))
	if err != nil {
		t.Fatalf("EvaluateAnswerIntake() error = %v", err)
	}
	if decision.Adopted != nil || decision.Cancel != nil {
		t.Fatalf("decision = %+v, want nothing", decision)
	}
}
