package worker

import (
	"errors"
	"fmt"

	"automation.internal/ticket-ingress/internal/hook"
)

// The reception is the one place a run may ask the requester anything. It
// asks every open point in a single comment and then never asks again, so
// what a destination gets to choose is how much may be asked there: nothing
// at all, only what nobody could decide for them, or anything the four
// dimensions admit. The rest of the run has no question in it, and a setting
// here is what decides whether a ticket can stop on a person overnight.

const (
	// QuestionsNone asks nothing. Every open point is settled by the
	// reception itself and recorded as an assumption, so no run of this
	// destination ever waits on an answer.
	QuestionsNone = "none"
	// QuestionsMinimal asks only the points no defensible default settles —
	// the ones whose answer changes what the requester gets and that only
	// they can give.
	QuestionsMinimal = "minimal"
	// QuestionsNormal is the older behaviour: any unresolved point falling
	// in one of the four dimensions may become a question.
	QuestionsNormal = "normal"

	// DefaultQuestionMaxItems is how many questions one reception set holds
	// when the destination names no number. It is not three because the set
	// is now the whole conversation: what is not asked here is decided
	// without the requester.
	DefaultQuestionMaxItems = 10
	// DefaultQuestionMaxRounds is how often one run may put questions to the
	// requester. Once — the reception asks, the answers come back, and every
	// stage after that decides for itself.
	DefaultQuestionMaxRounds = 1
	// DefaultAssumptionMaxItems is how many settled points one assessment
	// may record. A reception that asks less records more, and a record cut
	// short is a decision the requester never sees.
	DefaultAssumptionMaxItems = 64
	// AssumptionItemCeiling bounds what a destination may raise that to.
	AssumptionItemCeiling = 256
)

// QuestionItemCeiling is the highest number of questions one set can hold
// whatever the destination asks for: the answer grammar and the sealed store
// number questions up to this and no further.
const QuestionItemCeiling = hook.MaxClarificationQuestions

// QuestionsMode is how much this destination lets the reception ask.
func (c Config) QuestionsMode() string {
	switch c.Questions {
	case QuestionsNone, QuestionsMinimal, QuestionsNormal:
		return c.Questions
	}
	return QuestionsMinimal
}

// QuestionItems is how many questions one reception set may hold.
func (c Config) QuestionItems() int {
	if c.QuestionMaxItems == 0 {
		return DefaultQuestionMaxItems
	}
	return c.QuestionMaxItems
}

// QuestionRounds is how many times one run may put questions to the
// requester.
func (c Config) QuestionRounds() int {
	if c.QuestionMaxRounds == 0 {
		return DefaultQuestionMaxRounds
	}
	return c.QuestionMaxRounds
}

// AnswerWeekdays is how many weekdays a requester has to answer.
func (c Config) AnswerWeekdays() int {
	if c.QuestionDeadlineWeekdays == 0 {
		return hook.DefaultQuestionDeadlineWeekdays
	}
	return c.QuestionDeadlineWeekdays
}

// AssumptionItems is how many settled points one assessment may record.
func (c Config) AssumptionItems() int {
	if c.AssumptionMaxItems == 0 {
		return DefaultAssumptionMaxItems
	}
	return c.AssumptionMaxItems
}

func (c Config) validateReception() error {
	switch c.Questions {
	case "", QuestionsNone, QuestionsMinimal, QuestionsNormal:
	default:
		return fmt.Errorf("questions must be %q, %q or %q", QuestionsNone, QuestionsMinimal, QuestionsNormal)
	}
	if c.QuestionMaxItems < 0 || c.QuestionMaxItems > QuestionItemCeiling {
		return fmt.Errorf("question_max_items must be between 1 and %d", QuestionItemCeiling)
	}
	// More than one round is not a longer conversation, it is the waiting
	// the reception exists to end: the second round can only be asked after
	// the first was answered, which is another night.
	if c.QuestionMaxRounds < 0 || c.QuestionMaxRounds > hook.MaxClarificationRounds {
		return fmt.Errorf("question_max_rounds must be between 1 and %d", hook.MaxClarificationRounds)
	}
	if c.QuestionDeadlineWeekdays != 0 &&
		(c.QuestionDeadlineWeekdays < hook.MinQuestionDeadlineWeekdays || c.QuestionDeadlineWeekdays > hook.MaxQuestionDeadlineWeekdays) {
		return fmt.Errorf("question_deadline_weekdays must be between %d and %d",
			hook.MinQuestionDeadlineWeekdays, hook.MaxQuestionDeadlineWeekdays)
	}
	if c.AssumptionMaxItems < 0 || c.AssumptionMaxItems > AssumptionItemCeiling {
		return fmt.Errorf("assumption_max_items must be between 1 and %d", AssumptionItemCeiling)
	}
	return nil
}

// askingPolicy is what the reception may ask on this particular run: the
// mode the destination set, and the budget left once the rounds already
// spent are counted. A budget of zero is not an error — it is the instruction
// to decide the point instead of asking it.
type askingPolicy struct {
	Mode          string
	MaxItems      int
	MaxAssumption int
	RoundsSpent   int
	RoundsAllowed int
}

// askingPolicyFor reads the policy for one assessment. clarification is the
// answers this run already has; its revision counts the round it is about to
// be, so the rounds already spent are one fewer.
func askingPolicyFor(config Config, clarification *ClarificationContext) askingPolicy {
	policy := askingPolicy{
		Mode:          config.QuestionsMode(),
		MaxItems:      config.QuestionItems(),
		MaxAssumption: config.AssumptionItems(),
		RoundsAllowed: config.QuestionRounds(),
	}
	if clarification != nil && clarification.Revision > 1 {
		policy.RoundsSpent = clarification.Revision - 1
	}
	if policy.Mode == QuestionsNone || policy.RoundsSpent >= policy.RoundsAllowed {
		policy.MaxItems = 0
	}
	return policy
}

// MayAsk reports whether this run is still allowed to put a question to the
// requester at all.
func (p askingPolicy) MayAsk() bool { return p.MaxItems > 0 }

// applyTo is the coercion that makes the policy true of the model's answer,
// in the same spirit as the design judgment: the reception's output is bent
// onto the side the destination asked for, never objected to. A destination
// that asked for no questions, or a run that has already used its one round,
// gets the questions folded into the record of what was decided instead —
// which is what the requester reads in the plan notice — rather than a
// refused assessment and a run that ends on a policy it could have obeyed.
func (p askingPolicy) applyTo(output ModelReadinessOutput) ModelReadinessOutput {
	if p.MayAsk() || output.Decision != ReadinessOutcomeClarification {
		return output
	}
	for _, question := range output.Questions {
		if len(output.Assumptions) >= p.MaxAssumption {
			break
		}
		output.Assumptions = append(output.Assumptions, ReadinessAssumption{
			Kind:      AssumptionDefensibleDefault,
			Statement: assumptionStatementFor(question),
			Evidence:  assumptionEvidenceFor(question, p),
		})
	}
	output.Decision = ReadinessOutcomeReady
	output.Questions = nil
	return output
}

// assumptionStatementFor says what was settled, in the words the question
// was going to be asked in: the point, and the choice taken for it.
func assumptionStatementFor(question ReadinessQuestion) string {
	statement := question.Question
	if len(question.Choices) > 0 {
		statement += " → " + question.Choices[0].Label
	}
	return collapseSpaces(boundedHead(statement, 2000))
}

// assumptionEvidenceFor says why nobody was asked. The requester reads this
// in the plan notice, so it names the reason in their terms, not the setting.
func assumptionEvidenceFor(question ReadinessQuestion, policy askingPolicy) string {
	reason := "この納品先では確認を行わない設定のため、受付で判断しました。"
	if policy.RoundsSpent >= policy.RoundsAllowed {
		reason = "確認は 1 回だけのため、追加の確認は行わず受付で判断しました。"
	}
	if why := collapseSpaces(question.WhyBlocking); why != "" {
		reason += " " + why
	}
	return collapseSpaces(boundedHead(reason, 2000))
}

// refuse is the budget side of the policy: what the destination allows, as
// opposed to what the protocol can carry. It runs once, where the model's
// answer is taken, and never again on a sealed record - an operator who
// lowers a number later must not make yesterday's correct records
// unreadable.
func (p askingPolicy) refuse(output ModelReadinessOutput) error {
	if len(output.Questions) > p.MaxItems {
		return fmt.Errorf("readiness asked %d questions, limit %d", len(output.Questions), p.MaxItems)
	}
	if len(output.Assumptions) > p.MaxAssumption {
		return fmt.Errorf("readiness recorded %d assumptions, limit %d", len(output.Assumptions), p.MaxAssumption)
	}
	return questionSetFitsComment(output.Questions)
}

// questionSetFitsComment holds one question set to the comment that has to
// carry it. The refusal names the size and the limit because it goes back to
// the model, which is the only thing that can shorten the set — the sealed
// store would refuse the same set far later, at the posting, where nothing
// can be done about it any more.
func questionSetFitsComment(questions []ReadinessQuestion) error {
	if len(questions) == 0 {
		return nil
	}
	encoded, err := marshalPrompt(questions)
	if err != nil {
		return errors.New("readiness questions could not be encoded")
	}
	if len(encoded) > hook.MaxQuestionSetBytes {
		return fmt.Errorf("readiness questions are %d bytes, limit %d", len(encoded), hook.MaxQuestionSetBytes)
	}
	size, err := hook.RenderedQuestionCommentBytes(encoded)
	if err != nil {
		return errors.New("readiness questions could not be rendered as a comment")
	}
	if size > hook.MaxTrackerCommentBytes {
		return fmt.Errorf("the comment for these %d questions is %d bytes, limit %d: ask fewer or shorten them",
			len(questions), size, hook.MaxTrackerCommentBytes)
	}
	return nil
}
