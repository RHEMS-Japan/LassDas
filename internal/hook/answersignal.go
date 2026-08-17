package hook

import "context"

// commentActivityType is Backlog's activity type for an issue comment.
const commentActivityType = 3

// UseAnswerSignal turns an incoming comment webhook into an immediate
// question tick. Without it an answer sits until the schedule fires -
// nominally five minutes, measured at twenty in bad weather, and on
// 2026-08-12 a posted answer needed a hand-driven tick to be adopted at all.
// Passing nil keeps comments ignored exactly as before.
func (s *Service) UseAnswerSignal(tick QuestionTickProcessor) {
	s.answerTick = tick
}

// processAnswerSignal treats the comment purely as a wake-up call. The
// comment body is never read here: interpreting and adopting answers stays
// with the same clock logic the schedule drives, so this instant path adds
// no new input surface - that is its safety condition. The tick is
// idempotent, which also makes the automation's own posted comments
// harmless self-signals.
func (s *Service) processAnswerSignal(ctx context.Context, hint WebhookHint) Result {
	if hint.ProjectID != s.config.ProjectID || hint.ProjectKey != s.config.ProjectKey ||
		hint.CreatorID != s.config.AllowedCreatorID {
		return s.result(DecisionIgnored, "activity_not_allowed", hint, "", "")
	}
	tick := s.answerTick.ProcessQuestionTick(ctx, QuestionTickRequest{
		Protocol: QuestionTickProtocol, AutomationRunID: s.config.ExpectedRunID, IssuedAt: s.now().UTC(),
	})
	if tick.Code == "question_tick_resumed" {
		// The resumed envelope waits for a pull; the same wake-up that made
		// ticket intake take five seconds makes answer adoption take five.
		s.dispatchWork(ctx, tick.DeliveryID)
		return s.result(DecisionAccepted, "answer_signal_resumed", hint, "", tick.DeliveryID)
	}
	s.logger.Info("answer signal ticked", "code", tick.Code, "delivery_id", tick.DeliveryID)
	return s.result(DecisionAccepted, "answer_signal_ticked", hint, "", tick.DeliveryID)
}
