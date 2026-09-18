package worker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// maxAnswerReadingBytes bounds the reading that comes back. A reading names
// question and choice identifiers and nothing else, so it is small.
const maxAnswerReadingBytes = 1 << 14

// AnswerReading is what a requester's comment means, read against the
// questions that were actually asked.
//
// Whether a comment is an answer is a judgement, not a measurement. It used
// to be made by five regular expressions over the comment's first line, so a
// requester who wrote anything but 「回答 C1 Q1:a」 had not answered as far as
// the automation was concerned - and the patterns were widened one near-miss
// at a time (a bare choice symbol, then full-width characters). A model that
// is already reading the repository to find defects can read a sentence.
type AnswerReading struct {
	// Kind is answer, cancel, or unrelated. Unrelated is a comment that says
	// nothing about the questions: it is left alone, not answered back at.
	Kind string `json:"kind"`
	// Answers maps a question id to the choice id the comment picks.
	Answers map[string]string `json:"answers"`
	// NotNeeded and Unanswered are what the reading noticed, and they travel
	// to the role as context. Nothing in the engine gates on them: a role
	// that needs more than it was given asks again.
	NotNeeded  []string `json:"not_needed"`
	Unanswered []string `json:"unanswered"`
	// Reason is one sentence for the record, in the requester's language.
	Reason string `json:"reason"`
}

// IsAnswer reports whether the reading says the comment was an answer. The
// engine asks nothing else of it - not whether the choices exist, not
// whether every question was covered. The answers go to the role that asked, with the
// questions it asked, and a role that still cannot proceed asks again - the
// same way it asked the first time. Deciding completeness in code is what
// left a live delivery waiting forever on a question whose own words made it
// conditional (2026-09-17).
func (r AnswerReading) IsAnswer() bool {
	return r.Kind == AnswerReadingAnswer && len(r.Answers) > 0
}

const (
	AnswerReadingAnswer    = "answer"
	AnswerReadingCancel    = "cancel"
	AnswerReadingUnrelated = "unrelated"
)

func answerReadingSystemPrompt() string {
	return strings.Join([]string{
		"You read one comment a requester left on a ticket and say what it means for the questions that were asked.",
		"You are not judging the answer's wisdom. You are reading which choice the person picked.",
		"Rules:",
		`- kind is "answer" when the comment picks at least one choice, "cancel" when the person is calling the work off, "unrelated" otherwise.`,
		"- answers maps a question id to a choice id. Use only ids that appear in the questions you are given.",
		"- A person may write the choice's words instead of its letter, may answer out of order, may answer in any language, and may write nothing but a single letter. All of these are answers.",
		"- not_needed is for a question whose own words make it conditional on another answer (\"only if you chose b\") when the condition did not happen.",
		"- unanswered is for a question that still genuinely needs an answer. Both lists are notes for whoever asked; leave them empty if there is nothing to note.",
		"- Do not withhold an answer because other questions are unanswered. Report what the comment picked.",
		"- If the comment is someone thinking aloud and not deciding, that is unrelated, not an answer. Do not guess a choice the person did not make.",
		"- reason is one sentence in the language the comment is written in.",
	}, "\n")
}

func answerReadingSchema() string {
	return `{"type":"object","additionalProperties":false,` +
		`"required":["kind","answers","reason"],` +
		`"properties":{` +
		`"kind":{"type":"string","enum":["answer","cancel","unrelated"]},` +
		`"answers":{"type":"object","additionalProperties":{"type":"string"}},` +
		`"not_needed":{"type":"array","items":{"type":"string"}},` +
		`"unanswered":{"type":"array","items":{"type":"string"}},` +
		`"reason":{"type":"string","maxLength":600}}}`
}

// ReadAnswer asks the model what one comment means. The questions are handed
// over as they were sealed, so the reading is bound to what was actually
// asked and not to a paraphrase of it.
func (i *ModelInvoker) ReadAnswer(ctx context.Context, endpoint ModelEndpoint, questionsJSON, body string) (AnswerReading, InvocationUsage, error) {
	prompt, err := json.Marshal(struct {
		Questions json.RawMessage `json:"questions"`
		Comment   string          `json:"comment"`
	}{Questions: json.RawMessage(questionsJSON), Comment: body})
	if err != nil {
		return AnswerReading{}, InvocationUsage{}, errors.New("answer reading prompt could not be built")
	}
	var reading AnswerReading
	usage, err := i.converseJSON(ctx, endpoint, answerReadingSystemPrompt(), string(prompt), answerReadingSchema(), maxAnswerReadingBytes,
		func(answer []byte, _ InvocationUsage) error {
			var parsed AnswerReading
			if err := json.Unmarshal(answer, &parsed); err != nil {
				return errors.New("the reading is not the shape that was asked for")
			}
			reading = parsed
			return nil
		})
	if err != nil {
		return AnswerReading{}, InvocationUsage{}, err
	}
	return reading, usage, nil
}
