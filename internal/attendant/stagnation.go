package attendant

import (
	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
	"automation.internal/ticket-ingress/internal/worker"
	"context"
	"encoding/json"
	"errors"
	"os"
)

func stagnated(runDir string, reviewers []string, round, repeat int) bool {
	return runner.RoundsStagnated(runDir, reviewers, round, repeat)
}

// ruleOnStagnation schedules the existing validation card. It never spends
// a model turn in the attendant: the same tick must remain available for
// stops and other deliveries while arbitration runs in the card.
// A stop noticed during dispatch is returned separately so a round limit
// cannot replace that cancellation with a failure ending.
func ruleOnStagnation(ctx context.Context, config runtime.Config, services *runtime.Services,
	hermes *runtime.Hermes, envelope hook.DispatchEnvelope, run state.RunOverview,
	view chainView, logger Logger) (handled, stopped bool, err error) {
	runDir := runDirectory(config, run.DeliveryID)
	existing, err := runner.ReadRuling(runDir, view.round)
	if err != nil {
		return true, false, err
	}
	if existing != nil {
		if existing.Ruling != worker.RulingOverruleReviewer {
			return false, false, nil
		}
		applied, err := runner.RulingApplied(runDir, view.round, existing)
		if err != nil {
			return true, false, err
		}
		if applied {
			return false, false, nil
		}
	} else {
		reviewers, err := consumerReviewerIDs(config.ConsumerConfigPath)
		if err != nil {
			return true, false, err
		}
		if !stagnated(runDir, reviewers, view.round, runner.ConsumerStagnationRounds(config.ConsumerConfigPath)) {
			return false, false, nil
		}
	}
	plan, err := chainPlanFor(config, runDir, run, logger)
	if err != nil {
		return true, false, err
	}
	logger.Info("the rounds have stopped moving; the validation card will rule on it",
		"run", run.RunID, "round", view.round)
	verdict, err := dispatchAgain(ctx, newClimb(config, services, hermes, envelope, run, view, plan, runtime.StageValidate, logger))
	return verdict == ladderHandled, verdict == ladderStopped, err
}

// consumerRoundLimit reads the destination's round limit: the number of
// implementation rounds an operator is willing to pay for, or zero for the
// unbounded default.
//
// Unbounded is the answer for a configuration that does not mention it,
// which is every configuration written before rounds stopped being counted.
// An unreadable configuration is an error rather than a default: the rest of
// this tick reads the same file, and guessing here would hide that.
func consumerRoundLimit(consumerConfigPath string) (int, error) {
	raw, err := os.ReadFile(consumerConfigPath)
	if err != nil {
		return 0, errors.New("consumer config unreadable")
	}
	var parsed struct {
		MaxRounds int `json:"max_rounds"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || parsed.MaxRounds < 0 || parsed.MaxRounds > worker.StageCeiling {
		return 0, errors.New("consumer config max_rounds invalid")
	}
	return parsed.MaxRounds, nil
}
