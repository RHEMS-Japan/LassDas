package main

import (
	"regexp"
	"strings"

	"automation.internal/ticket-ingress/internal/hook"
)

// The console's one write is bounded by this gate: the allowed answerer may
// post, to the newest open question, only lines that question itself printed.
// Question identity comes from the engine's own comment marker on the final
// line (hook.ExtractCommentMarker) - never from marker-shaped text elsewhere
// in a body, which requesters and models can both produce. The engine
// re-validates everything on intake, so a gate mistake here can annoy the
// operator with a refused or wasted comment but can never widen what the
// engine accepts.

// answerGateError pairs an operator-facing reason with the HTTP status the
// handler sends for it.
type answerGateError struct {
	Status int
	Reason string
}

func (e *answerGateError) Error() string { return e.Reason }

// printedAnswerLinePattern is the shape of one copy-paste answer line the
// question comment prints under every choice (hook.QuestionCommentContent:
// "回答 <tag> <questionID>:<choiceID>").
var printedAnswerLinePattern = regexp.MustCompile(`^回答 (C[0-9]+) ([^ :]+):([^ :]+)$`)

// answerOrCancelPattern recognizes a requester comment that opens as an
// answer or a cancel. It mirrors the engine's candidate patterns
// (internal/hook/answer_intake.go) with the same hand-typed rescues; being
// slightly wider or narrower than the engine only changes when the console
// says "already posted", never what the engine adopts.
var answerOrCancelPattern = regexp.MustCompile(`^(回答|中止)[ \t]*[Cc][0-9]+`)

var questionTagShape = regexp.MustCompile(`^C[0-9]+$`)

// markerParts splits a marker returned by hook.ExtractCommentMarker into its
// kind and kind-specific qualifiers. An empty marker yields an empty kind.
func markerParts(marker string) (string, []string) {
	trimmed := strings.TrimSuffix(strings.TrimPrefix(marker, "["), "]")
	parts := strings.Split(trimmed, ":")
	if len(parts) < 4 {
		return "", nil
	}
	return parts[2], parts[4:]
}

func firstContentLine(body string) string {
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func answerOrCancelCandidate(body string) bool {
	return answerOrCancelPattern.MatchString(firstContentLine(body))
}

// evaluateAnswerGate decides whether the requested lines may be posted as the
// answer to the question comment. It re-reads everything from the comment
// stream: the browser's claim is only a pointer.
func evaluateAnswerGate(comments []rawComment, questionCommentID int64, lines []string, answererID int64) *answerGateError {
	if len(lines) == 0 || len(lines) > hook.MaxClarificationQuestions {
		return &answerGateError{Status: 400, Reason: "between one and three answer lines"}
	}
	var question *rawComment
	for index := range comments {
		if comments[index].ID == questionCommentID {
			question = &comments[index]
		}
	}
	if question == nil {
		return &answerGateError{Status: 409, Reason: "that comment is not on this ticket"}
	}
	kind, qualifiers := markerParts(hook.ExtractCommentMarker(question.Content))
	if kind != "question" || len(qualifiers) == 0 || !questionTagShape.MatchString(qualifiers[0]) {
		return &answerGateError{Status: 409, Reason: "that comment is not a question"}
	}
	tag := qualifiers[0]

	// Only lines the question itself printed may travel, one per question,
	// and every question must get its line: the engine treats a partial
	// answer as a shortfall and demands a complete re-post, so letting one
	// through would only waste the requester a round-trip.
	printed := map[string]bool{}
	printedQuestions := map[string]bool{}
	for _, raw := range strings.Split(question.Content, "\n") {
		line := strings.TrimSpace(raw)
		if parts := printedAnswerLinePattern.FindStringSubmatch(line); parts != nil {
			printed[line] = true
			if parts[1] == tag {
				printedQuestions[strings.ToLower(parts[2])] = true
			}
		}
	}
	answeredQuestions := map[string]bool{}
	totalBytes := 0
	for _, line := range lines {
		parts := printedAnswerLinePattern.FindStringSubmatch(line)
		if parts == nil || !printed[line] {
			return &answerGateError{Status: 409, Reason: "an answer line is not printed in the question"}
		}
		if parts[1] != tag {
			return &answerGateError{Status: 409, Reason: "an answer line belongs to a different question round"}
		}
		if answeredQuestions[strings.ToLower(parts[2])] {
			return &answerGateError{Status: 400, Reason: "two answer lines address the same question"}
		}
		answeredQuestions[strings.ToLower(parts[2])] = true
		totalBytes += len(line) + 1
	}
	if len(answeredQuestions) != len(printedQuestions) {
		return &answerGateError{Status: 400, Reason: "every question needs its answer line in one post"}
	}
	if totalBytes > hook.MaxAnswerBodyBytes {
		return &answerGateError{Status: 400, Reason: "the answer comment would be too large"}
	}

	// Anything after the question that closes it - a newer question, the
	// engine's receipt, or the answerer's own pending answer or cancel -
	// makes this post either misdirected or a duplicate. A pending answer
	// stops being pending when the engine replies that it could not use it
	// (format guidance or a shortfall): that reply is an explicit request
	// to post again, and the panel is exactly the tool for that.
	answerPending := false
	for _, comment := range comments {
		if comment.ID <= question.ID {
			continue
		}
		laterKind, laterQualifiers := markerParts(hook.ExtractCommentMarker(comment.Content))
		switch laterKind {
		case "question":
			return &answerGateError{Status: 409, Reason: "a newer question exists - answer that one"}
		case "answer-receipt":
			if len(laterQualifiers) > 0 && laterQualifiers[0] == tag {
				return &answerGateError{Status: 409, Reason: "this question is already answered"}
			}
		case "answer-guidance", "answer-shortfall":
			if len(laterQualifiers) > 0 && laterQualifiers[0] == tag {
				answerPending = false
			}
		}
		if laterKind == "" && comment.UserID == answererID && answerOrCancelCandidate(comment.Content) {
			answerPending = true
		}
	}
	if answerPending {
		return &answerGateError{Status: 409, Reason: "an answer or cancel is already posted - the automation is picking it up"}
	}
	return nil
}
