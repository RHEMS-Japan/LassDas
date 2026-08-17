package hook

import "context"

// WorkDispatcher wakes the worker as soon as a ticket is queued, instead of
// leaving it to the schedule (measured at five minutes nominal and twenty in
// bad weather). Dispatching is best-effort by design: the envelope is already
// durable when this runs, and the schedule remains as the safety net, so a
// failure here is logged and the webhook still gets its acceptance.
type WorkDispatcher interface {
	DispatchWork(ctx context.Context) error
}

// UseDispatcher enables instant wake-up. Passing nil keeps the schedule-only
// behaviour; the service never fails a request over a dispatcher.
func (s *Service) UseDispatcher(dispatcher WorkDispatcher) {
	s.dispatcher = dispatcher
}

func (s *Service) dispatchWork(ctx context.Context, deliveryID string) {
	if s.dispatcher == nil {
		return
	}
	if err := s.dispatcher.DispatchWork(ctx); err != nil {
		// The dispatcher's error strings are static prose plus HTTP status
		// codes and never carry a credential, so the reason may be logged:
		// without it an expired key would degrade to schedule-only silently.
		s.logger.Warn("work dispatch failed", "code", "dispatch_failed", "delivery_id", deliveryID, "reason", err.Error())
		return
	}
	s.logger.Info("work dispatched", "code", "dispatch_ok", "delivery_id", deliveryID)
}
