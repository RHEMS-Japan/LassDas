package worker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// Reading one comment is the shape a decision model is built for: what the
// comment is (an answer, a stop, or neither) and, for each question that was
// asked, which of its own choices the comment picks. Nothing in the reading
// is prose the requester sees - the one sentence it also carries is for the
// record, and a decision model does not write sentences.
//
// This runs on every comment in scope on every tick for as long as a
// question stays open, which is once a minute. It is the most repeated model
// call the reception makes.

// decisionKindQuestion is the id of the question that asks what the comment
// is. It cannot collide with a requester question's id, which the readiness
// author draws from Q1, Q2 and so on.
const decisionKindQuestion = "__kind"

// decisionNotAnswered is the choice every question carries so the comment
// can leave it alone. Without it the model must pick one of the real
// choices, and a comment that answered two of three questions would invent
// the third.
const decisionNotAnswered = "__not_answered"

// ReadAnswerByDecision reads one comment with a decision model. It returns
// the same reading the prose path returns, so the caller cannot tell which
// read it - except that this one carries no sentence.
//
// It is not a fallback for the prose path and does not become one silently:
// the caller decides which to use, and a failure here is returned, never
// swallowed.
func (c *SystemOneClient) ReadAnswerByDecision(ctx context.Context, endpoint ModelEndpoint, questionsJSON, body string) (AnswerReading, error) {
	questions, err := decodeSealedQuestions(questionsJSON)
	if err != nil {
		return AnswerReading{}, err
	}
	if strings.TrimSpace(body) == "" {
		return AnswerReading{}, safeModelLiteral("the comment is empty")
	}
	asked := map[string]SystemOneQuestion{
		decisionKindQuestion: {
			Kind: SystemOneChoice,
			Instructions: "What this comment is, read against the questions that were asked. " +
				"It is an answer when it picks at least one choice, in whatever words and in any language, " +
				"including a bare choice letter. It is a stop when the person is calling the work off. " +
				"Someone thinking aloud without deciding is neither.",
			Criteria: map[string]string{
				AnswerReadingAnswer:    "picks at least one of the choices below",
				AnswerReadingCancel:    "calls the work off",
				AnswerReadingUnrelated: "says nothing about the questions",
			},
		},
	}
	for _, question := range questions {
		options := map[string]string{decisionNotAnswered: "this comment does not answer this question"}
		for _, choice := range question.Choices {
			label := strings.TrimSpace(choice.Label)
			if choice.Effect != "" {
				label += " — " + choice.Effect
			}
			options[choice.ID] = label
		}
		asked[question.ID] = SystemOneQuestion{
			Kind:         SystemOneChoice,
			Instructions: question.Question,
			Criteria:     options,
		}
	}
	answers, err := c.Decide(ctx, endpoint, SystemOneRequest{
		Model: endpoint.Model, State: body, Questions: asked,
	})
	if err != nil {
		return AnswerReading{}, err
	}
	reading := AnswerReading{Answers: map[string]string{}}
	kind := answers[decisionKindQuestion]
	if kind.Choice == nil {
		return AnswerReading{}, safeModelLiteral("the reading does not say what the comment is")
	}
	reading.Kind = *kind.Choice
	for _, question := range questions {
		answer, ok := answers[question.ID]
		if !ok || answer.Choice == nil || *answer.Choice == decisionNotAnswered {
			reading.Unanswered = append(reading.Unanswered, question.ID)
			continue
		}
		reading.Answers[question.ID] = *answer.Choice
	}
	// A comment read as an answer that picked nothing is not an answer. The
	// adoption path already asks for at least one choice (IsAnswer), and
	// saying so here keeps the two readings the same shape.
	if reading.Kind == AnswerReadingAnswer && len(reading.Answers) == 0 {
		reading.Kind = AnswerReadingUnrelated
	}
	return reading, nil
}

// decodeSealedQuestions reads the questions as they were sealed. A reading
// bound to a paraphrase would answer a question nobody asked.
func decodeSealedQuestions(questionsJSON string) ([]ReadinessQuestion, error) {
	if strings.TrimSpace(questionsJSON) == "" {
		return nil, safeModelLiteral("no questions were handed over to read against")
	}
	var questions []ReadinessQuestion
	if err := json.Unmarshal([]byte(questionsJSON), &questions); err != nil {
		var wrapped struct {
			Questions []ReadinessQuestion `json:"questions"`
		}
		if json.Unmarshal([]byte(questionsJSON), &wrapped) != nil {
			return nil, safeModelLiteral("the sealed questions could not be read")
		}
		questions = wrapped.Questions
	}
	if len(questions) == 0 {
		return nil, safeModelLiteral("no questions were handed over to read against")
	}
	for _, question := range questions {
		if strings.TrimSpace(question.ID) == "" {
			return nil, safeModelLiteral("a sealed question has no id")
		}
		if question.ID == decisionKindQuestion {
			return nil, errors.New("a sealed question uses the reserved id " + decisionKindQuestion)
		}
		if len(question.Choices) == 0 {
			return nil, safeModelError("sealed question " + question.ID + " offers no choices")
		}
	}
	return questions, nil
}
