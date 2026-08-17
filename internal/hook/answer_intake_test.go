package hook

import (
	"strings"
	"testing"
)

const intakeTwoQuestionSet = `[{"id":"Q1","dimension":"user_visible_behavior","question":"並び順は?","why_blocking":"表示が変わる","choices":[{"id":"a","label":"新着順","effect":"新しい順"},{"id":"b","label":"名前順","effect":"五十音順"}]},{"id":"Q2","dimension":"data_lifecycle","question":"既存データは?","why_blocking":"移行が変わる","choices":[{"id":"a","label":"残す","effect":"全件保持"},{"id":"c","label":"消す","effect":"初期化"}]}]`

func intakeTestRecord(questionsJSON string) QuestionRecord {
	record := questionTestRecord()
	record.QuestionsJSON = questionsJSON
	record.QuestionsSHA256 = TerminalReportDigest([]byte(questionsJSON))
	return record
}

func intakeTestInput(record QuestionRecord, comments ...BacklogComment) AnswerIntakeInput {
	return AnswerIntakeInput{
		Question:          record,
		QuestionCommentID: 100,
		AnswererID:        terminalTestConfig().AllowedCreatorID,
		Comments:          comments,
	}
}

func intakeComment(commentID int64, body string) BacklogComment {
	return BacklogComment{CommentID: commentID, UserID: terminalTestConfig().AllowedCreatorID, Body: body, PostedAt: 3500}
}

func TestAnswerIntakeAdoptsTheCopyPasteLine(t *testing.T) {
	record := intakeTestRecord(questionTestSetJSON)
	comment := intakeComment(101, "回答 C1 Q1:a")
	decision, err := EvaluateAnswerIntake(intakeTestInput(record, comment))
	if err != nil {
		t.Fatalf("EvaluateAnswerIntake() error = %v", err)
	}
	if decision.Adopted == nil || decision.Cancel != nil || len(decision.Replies) != 0 {
		t.Fatalf("decision = %+v, want adoption only", decision)
	}
	if decision.Adopted.CommentID != 101 || decision.Adopted.PostedAt != 3500 {
		t.Fatalf("adopted binding = %+v", decision.Adopted)
	}
	if decision.Adopted.AnswersJSON != `{"Q1":"a"}` {
		t.Fatalf("answers = %s", decision.Adopted.AnswersJSON)
	}
	if decision.Adopted.BodySHA256 != TerminalReportDigest([]byte(comment.Body)) {
		t.Fatal("body digest is not bound to the raw comment body")
	}
}

func TestAnswerIntakeAcceptsBlockPasteAndTypedRescues(t *testing.T) {
	record := intakeTestRecord(intakeTwoQuestionSet)
	for _, run := range []struct {
		name string
		body string
	}{
		{name: "block form", body: "回答 C1\nQ1: a\nQ2: c"},
		{name: "pasted per-choice lines", body: "回答 C1 Q1:a\n回答 C1 Q2:c"},
		{name: "mixed paste then typed", body: "回答 C1 Q1:a\nQ2: c"},
		{name: "full-width colon and space", body: "回答　C1\nQ1：a\nQ2：c"},
		{name: "lowercase q and uppercase choice", body: "回答 C1\nq1: A\nq2: C"},
		{name: "lines out of numeric order", body: "回答 C1\nQ2: c\nQ1: a"},
		{name: "windows line endings", body: "回答 C1\r\nQ1: a\r\nQ2: c"},
		{name: "missing space before the marker", body: "回答C1 Q1:a\n回答C1 Q2:c"},
		{name: "lowercase revision marker", body: "回答 c1\nQ1: a\nQ2: c"},
	} {
		t.Run(run.name, func(t *testing.T) {
			decision, err := EvaluateAnswerIntake(intakeTestInput(record, intakeComment(101, run.body)))
			if err != nil {
				t.Fatalf("EvaluateAnswerIntake() error = %v", err)
			}
			if decision.Adopted == nil {
				t.Fatalf("decision = %+v, want adoption", decision)
			}
			if decision.Adopted.AnswersJSON != `{"Q1":"a","Q2":"c"}` {
				t.Fatalf("answers = %s", decision.Adopted.AnswersJSON)
			}
		})
	}
}

func TestAnswerIntakeIgnoresCommentsOutsideTheContract(t *testing.T) {
	record := intakeTestRecord(questionTestSetJSON)
	answerer := terminalTestConfig().AllowedCreatorID
	decision, err := EvaluateAnswerIntake(intakeTestInput(record,
		BacklogComment{CommentID: 101, UserID: answerer + 7, Body: "回答 C1 Q1:a", PostedAt: 3500},
		BacklogComment{CommentID: 99, UserID: answerer, Body: "回答 C1 Q1:a", PostedAt: 3500},
		BacklogComment{CommentID: 102, UserID: answerer, Body: "回答 C1 Q1:a", PostedAt: 4000},
		BacklogComment{CommentID: 103, UserID: answerer, Body: "承知しました、確認します", PostedAt: 3500},
		BacklogComment{CommentID: 104, UserID: answerer, Body: "中止 C2", PostedAt: 3500},
	))
	if err != nil {
		t.Fatalf("EvaluateAnswerIntake() error = %v", err)
	}
	if decision.Adopted != nil || decision.Cancel != nil || len(decision.Replies) != 0 {
		t.Fatalf("decision = %+v, want empty", decision)
	}
}

func TestAnswerIntakeAdoptsTheHighestCompleteAnswer(t *testing.T) {
	record := intakeTestRecord(questionTestSetJSON)
	decision, err := EvaluateAnswerIntake(intakeTestInput(record,
		intakeComment(101, "回答 C1 Q1:a"),
		intakeComment(105, "回答 C1 Q1:b"),
	))
	if err != nil {
		t.Fatalf("EvaluateAnswerIntake() error = %v", err)
	}
	if decision.Adopted == nil || decision.Adopted.CommentID != 105 || decision.Adopted.AnswersJSON != `{"Q1":"b"}` {
		t.Fatalf("decision = %+v, want the later answer adopted", decision.Adopted)
	}
}

func TestAnswerIntakePrefersTheEarliestCancelOverAnyAnswer(t *testing.T) {
	record := intakeTestRecord(questionTestSetJSON)
	body := "中止 C1"
	decision, err := EvaluateAnswerIntake(intakeTestInput(record,
		intakeComment(101, "回答 C1 Q1:a"),
		intakeComment(102, body),
		intakeComment(103, "中止　C1"),
	))
	if err != nil {
		t.Fatalf("EvaluateAnswerIntake() error = %v", err)
	}
	if decision.Cancel == nil || decision.Adopted != nil || len(decision.Replies) != 0 {
		t.Fatalf("decision = %+v, want cancel only", decision)
	}
	if decision.Cancel.CommentID != 102 || decision.Cancel.BodySHA256 != TerminalReportDigest([]byte(body)) {
		t.Fatalf("cancel binding = %+v", decision.Cancel)
	}
}

func TestAnswerIntakeReadsCancelFromTheFirstLineOnly(t *testing.T) {
	record := intakeTestRecord(questionTestSetJSON)
	// A stop with trailing politeness is still a stop; converting an
	// expressed stop into a resume would be the worse failure.
	polite, err := EvaluateAnswerIntake(intakeTestInput(record,
		intakeComment(101, "回答 C1 Q1:a"),
		intakeComment(102, "中止 C1\nお手数ですがよろしくお願いします"),
	))
	if err != nil {
		t.Fatalf("EvaluateAnswerIntake() error = %v", err)
	}
	if polite.Cancel == nil || polite.Cancel.CommentID != 102 || polite.Adopted != nil {
		t.Fatalf("decision = %+v, want the polite cancel to win", polite)
	}
	if compact, err := EvaluateAnswerIntake(intakeTestInput(record, intakeComment(103, "中止C1"))); err != nil || compact.Cancel == nil {
		t.Fatalf("decision = %+v, err = %v, want the unspaced cancel accepted", compact, err)
	}
	// A 中止 line buried below an answer body is not a cancel; the comment is
	// an uninterpretable answer and gets guidance.
	buried, err := EvaluateAnswerIntake(intakeTestInput(record, intakeComment(104, "回答 C1\nQ1: a\n中止 C1")))
	if err != nil {
		t.Fatalf("EvaluateAnswerIntake() error = %v", err)
	}
	if buried.Cancel != nil || buried.Adopted != nil ||
		len(buried.Replies) != 1 || buried.Replies[0].Kind != AnswerReplyGuidance {
		t.Fatalf("decision = %+v, want guidance only", buried)
	}
}

func TestAnswerIntakeRepliesGuidanceOncePerRevision(t *testing.T) {
	record := intakeTestRecord(questionTestSetJSON)
	for _, run := range []struct {
		name string
		body string
	}{
		{name: "unknown question", body: "回答 C1 Q9:a"},
		{name: "unknown choice", body: "回答 C1 Q1:z"},
		{name: "duplicate question", body: "回答 C1\nQ1: a\nQ1: b"},
		{name: "extra prose", body: "回答 C1\nQ1: a\nよろしくお願いします"},
		{name: "wrong revision header", body: "回答 C2\nQ1: a"},
		// Blank padding keeps the grammar itself valid, so only the byte bound
		// rejects this one.
		{name: "oversize body", body: "回答 C1 Q1:a" + strings.Repeat("\n", MaxAnswerBodyBytes)},
		// A near-miss marker must never be silently dropped: with no reaction
		// at all the requester would wait for the next renotification.
		{name: "near-miss marker with prose", body: "回答C1です。新着順でお願いします"},
	} {
		t.Run(run.name, func(t *testing.T) {
			decision, err := EvaluateAnswerIntake(intakeTestInput(record, intakeComment(101, run.body)))
			if err != nil {
				t.Fatalf("EvaluateAnswerIntake() error = %v", err)
			}
			if decision.Adopted != nil || len(decision.Replies) != 1 || decision.Replies[0].Kind != AnswerReplyGuidance || decision.Replies[0].CommentID != 101 {
				t.Fatalf("decision = %+v, want one guidance reply", decision)
			}
		})
	}

	// Two uninterpretable comments in one snapshot get a single guidance, and
	// none is repeated once it was sent or the triggering comment was handled.
	twoInvalid := intakeTestInput(record, intakeComment(101, "回答 C1 Q9:a"), intakeComment(102, "回答 C1 Q1:z"))
	decision, err := EvaluateAnswerIntake(twoInvalid)
	if err != nil {
		t.Fatalf("EvaluateAnswerIntake() error = %v", err)
	}
	if len(decision.Replies) != 1 || decision.Replies[0].CommentID != 101 {
		t.Fatalf("decision = %+v, want a single guidance for the first comment", decision)
	}
	sent := twoInvalid
	sent.GuidanceSent = true
	if decision, err := EvaluateAnswerIntake(sent); err != nil || len(decision.Replies) != 0 {
		t.Fatalf("decision = %+v, err = %v, want no reply after guidance was sent", decision, err)
	}
	handled := twoInvalid
	handled.HandledCommentIDs = map[int64]bool{101: true}
	if decision, err := EvaluateAnswerIntake(handled); err != nil || len(decision.Replies) != 0 {
		t.Fatalf("decision = %+v, err = %v, want no second guidance after the first was handled", decision, err)
	}
}

func TestAnswerIntakeReturnsShortfallPerIncompleteAnswer(t *testing.T) {
	record := intakeTestRecord(intakeTwoQuestionSet)
	decision, err := EvaluateAnswerIntake(intakeTestInput(record,
		intakeComment(101, "回答 C1"),
		intakeComment(102, "回答 C1 Q2:c"),
	))
	if err != nil {
		t.Fatalf("EvaluateAnswerIntake() error = %v", err)
	}
	if decision.Adopted != nil || len(decision.Replies) != 2 {
		t.Fatalf("decision = %+v, want two shortfall replies", decision)
	}
	first, second := decision.Replies[0], decision.Replies[1]
	if first.CommentID != 101 || first.Kind != AnswerReplyShortfall ||
		len(first.MissingQuestionIDs) != 2 || first.MissingQuestionIDs[0] != "Q1" || first.MissingQuestionIDs[1] != "Q2" {
		t.Fatalf("first reply = %+v", first)
	}
	if second.CommentID != 102 || second.Kind != AnswerReplyShortfall ||
		len(second.MissingQuestionIDs) != 1 || second.MissingQuestionIDs[0] != "Q1" {
		t.Fatalf("second reply = %+v", second)
	}

	// A handled shortfall is not re-sent; a complete answer in the same
	// snapshot supersedes every reply.
	handled := intakeTestInput(record, intakeComment(101, "回答 C1"), intakeComment(102, "回答 C1 Q2:c"))
	handled.HandledCommentIDs = map[int64]bool{101: true}
	if decision, err := EvaluateAnswerIntake(handled); err != nil || len(decision.Replies) != 1 || decision.Replies[0].CommentID != 102 {
		t.Fatalf("decision = %+v, err = %v, want only the unhandled shortfall", decision, err)
	}
	completed, err := EvaluateAnswerIntake(intakeTestInput(record,
		intakeComment(101, "回答 C1"),
		intakeComment(103, "回答 C1\nQ1: a\nQ2: c"),
	))
	if err != nil {
		t.Fatalf("EvaluateAnswerIntake() error = %v", err)
	}
	if completed.Adopted == nil || completed.Adopted.CommentID != 103 || len(completed.Replies) != 0 {
		t.Fatalf("decision = %+v, want adoption without replies", completed)
	}
}

func TestAnswerIntakeFailsClosedOnBrokenInput(t *testing.T) {
	record := intakeTestRecord(questionTestSetJSON)
	if _, err := EvaluateAnswerIntake(intakeTestInput(record, intakeComment(101, "回答 C1 Q1:a"), intakeComment(101, "回答 C1 Q1:b"))); err == nil {
		t.Fatal("duplicate comment ids were accepted")
	}
	for _, run := range []struct {
		name string
		set  string
	}{
		{name: "single choice", set: `[{"id":"Q1","choices":[{"id":"a"}]}]`},
		{name: "duplicate question ids", set: `[{"id":"Q1","choices":[{"id":"a"},{"id":"b"}]},{"id":"q1","choices":[{"id":"a"},{"id":"b"}]}]`},
		{name: "duplicate choice ids", set: `[{"id":"Q1","choices":[{"id":"a"},{"id":"A"}]}]`},
		{name: "missing question id", set: `[{"choices":[{"id":"a"},{"id":"b"}]}]`},
	} {
		t.Run(run.name, func(t *testing.T) {
			if _, err := EvaluateAnswerIntake(intakeTestInput(intakeTestRecord(run.set), intakeComment(101, "回答 C1 Q1:a"))); err == nil {
				t.Fatal("broken question set was accepted")
			}
		})
	}
	zeroBinding := intakeTestInput(record, intakeComment(101, "回答 C1 Q1:a"))
	zeroBinding.QuestionCommentID = 0
	if _, err := EvaluateAnswerIntake(zeroBinding); err == nil {
		t.Fatal("missing question comment binding was accepted")
	}
}

func TestAdoptedAnswersSealIntoAClarificationRound(t *testing.T) {
	record := intakeTestRecord(questionTestSetJSON)
	decision, err := EvaluateAnswerIntake(intakeTestInput(record, intakeComment(601, "回答 C1 Q1:a")))
	if err != nil || decision.Adopted == nil {
		t.Fatalf("EvaluateAnswerIntake() = %+v, err = %v", decision, err)
	}
	question := questionTestRecord()
	question.QuestionsJSON = record.QuestionsJSON
	question.QuestionsSHA256 = record.QuestionsSHA256
	encodedQuestion, err := MarshalQuestionRecord(question)
	if err != nil {
		t.Fatalf("MarshalQuestionRecord() error = %v", err)
	}
	clarification := clarificationTestRecord(t)
	clarification.Rounds = []ClarificationRound{{
		QuestionRecordJSON:   string(encodedQuestion),
		QuestionRecordSHA256: TerminalReportDigest(encodedQuestion),
		QuestionCommentID:    100,
		AnswerCommentID:      decision.Adopted.CommentID,
		AnswererID:           terminalTestConfig().AllowedCreatorID,
		AnswerPostedAt:       decision.Adopted.PostedAt,
		AnswerBodySHA256:     decision.Adopted.BodySHA256,
		AnswersJSON:          decision.Adopted.AnswersJSON,
		AnswersSHA256:        TerminalReportDigest([]byte(decision.Adopted.AnswersJSON)),
	}}
	if err := clarification.ValidateRoute(terminalTestConfig()); err != nil {
		t.Fatalf("adopted answer does not seal into a clarification round: %v", err)
	}
}
