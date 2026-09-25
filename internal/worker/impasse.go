package worker

import (
	"errors"
	"fmt"
)

// The question a deadlocked round used to put to its requester is gone.
//
// A round whose reviews and implementation each held a defensible position
// was turned into a multiple-choice question and the delivery stopped until
// somebody answered it. Overnight that is the same as not finishing, so the
// engine rules on the deadlock itself now (arbitrate.go). Questions live at
// reception only, and what is left here is the shape every asked question —
// reception's — is still held to.

// validateClarificationQuestions holds every asked question set to the same
// contract: sequential ids, a known dimension, bounded prose and two to four
// lettered choices. Readiness and impasse questions share it so the comment
// rendering and the answer intake never meet a shape they do not know.
func validateClarificationQuestions(questions []ReadinessQuestion) error {
	for index, question := range questions {
		if question.ID != fmt.Sprintf("Q%d", index+1) {
			return fmt.Errorf("question ids must be sequential (question %d is %q, want %q)", index+1, boundedHead(question.ID, 32), fmt.Sprintf("Q%d", index+1))
		}
		switch question.Dimension {
		case "user_visible_behavior", "acceptance_criterion", "preapproved_scope_choice", "safety_or_data":
		default:
			return errors.New("question dimension is invalid")
		}
		if problem := plainTextProblem(question.Question, 2000); problem != "" {
			return fmt.Errorf("question %s text %s", question.ID, problem)
		}
		if problem := plainTextProblem(question.WhyBlocking, 2000); problem != "" {
			return fmt.Errorf("question %s why_blocking %s", question.ID, problem)
		}
		// However many choices a question offers is how many it offers. A
		// question with one was refused and asked for again; the requester
		// would have read it fine.
		for choiceIndex, choice := range question.Choices {
			if choice.ID != string(rune('a'+choiceIndex)) {
				return fmt.Errorf("question %s choice %d id must be %q", question.ID, choiceIndex+1, string(rune('a'+choiceIndex)))
			}
			if problem := plainTextProblem(choice.Label, 800); problem != "" {
				return fmt.Errorf("question %s choice %s label %s", question.ID, choice.ID, problem)
			}
			if problem := plainTextProblem(choice.Effect, 1200); problem != "" {
				return fmt.Errorf("question %s choice %s effect %s", question.ID, choice.ID, problem)
			}
		}
	}
	return nil
}
