package attendant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
)

// The investigating designer's shapes end and fail in their own ways
// (docs/INVESTIGATING_DESIGNER.md §4.4, §5, §7). As everywhere in the
// attendant, the card's state is only the alarm; the sealed records in the
// run directory are the classification.

// defaultDesignMaxRounds is the design doc's default for design_max_rounds.
const defaultDesignMaxRounds = 3

// consumerDesignMaxRounds reads the destination's design round limit
// leniently: absent means the default.
func consumerDesignMaxRounds(consumerConfigPath string) int {
	raw, err := os.ReadFile(consumerConfigPath)
	if err != nil {
		return defaultDesignMaxRounds
	}
	var parsed struct {
		DesignMaxRounds int `json:"design_max_rounds"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || parsed.DesignMaxRounds < 1 || parsed.DesignMaxRounds > 10 {
		return defaultDesignMaxRounds
	}
	return parsed.DesignMaxRounds
}

func designRoundDir(runDir string, round int) string {
	return filepath.Join(runDir, "history", fmt.Sprintf("design-%d", round))
}

// reportInvestigated ends an investigation-only delivery: the sealed report
// exists and (when the consumer asked for it) passed the evidence review.
// The report's text reaches the ticket through the terminal report; the
// measurements travel with it as attachments (see the hook).
func reportInvestigated(
	ctx context.Context,
	config runtime.Config,
	services *runtime.Services,
	envelope hook.DispatchEnvelope,
	run state.RunOverview,
	view chainView,
	logger Logger,
) error {
	runDir := runDirectory(config, run.DeliveryID)
	round := view.designRound
	if round < 1 {
		round = 1
	}
	if _, err := os.Stat(filepath.Join(designRoundDir(runDir, round), "investigation.json")); err != nil {
		return errors.New("investigation card is done but no sealed report exists")
	}
	repository, err := readField(runDir, "ticket-draft.json", "repository")
	if err != nil {
		repository = ""
	}
	terminal := runner.NewTerminal(config, services, envelope, chainOwnerRunID(run.DeliveryID), runDir, logger)
	if err := terminal.Report(ctx, hook.TerminalInvestigated, runner.Outcome{Code: hook.TerminalInvestigated, Stage: round}, repository); err != nil {
		return err
	}
	logger.Info("investigation delivered", "run", run.RunID, "design_round", round)
	return nil
}

// handleDesignChainFailure classifies a failed card of the investigating
// designer's stages, and the objection an applier raises against its
// design. It returns handled=false for cards the original classification
// owns.
func handleDesignChainFailure(
	ctx context.Context,
	config runtime.Config,
	services *runtime.Services,
	hermes *runtime.Hermes,
	envelope hook.DispatchEnvelope,
	run state.RunOverview,
	view chainView,
	plan runtime.ChainPlan,
	stageName string,
	logger Logger,
) (bool, error) {
	if plan.Shape == runtime.ShapeImplement {
		return false, nil
	}
	runDir := runDirectory(config, run.DeliveryID)
	roundDir := designRoundDir(runDir, view.designRound)
	var code hook.TerminalCode
	switch stageName {
	case runtime.StageInvestigate:
		if _, err := os.Stat(filepath.Join(roundDir, "incomplete.json")); err == nil {
			code = hook.TerminalInvestigationIncomplete
		} else {
			code = hook.TerminalModelFailed
		}
	case runtime.StageDesignReviewA, runtime.StageDesignReviewB:
		code = hook.TerminalModelFailed
	case runtime.StageDesignDecide:
		outcome, err := readField(runDir, fmt.Sprintf("history/design-%d/decision.json", view.designRound), "outcome")
		switch {
		case err != nil:
			code = hook.TerminalModelFailed
		case outcome == "revise":
			return true, nextDesignRound(ctx, hermes, config, run, view, plan, "design review asked for a revision", logger)
		case outcome == "nonconverged" && plan.Shape == runtime.ShapeInvestigation:
			code = hook.TerminalInvestigationNonconverged
		case outcome == "nonconverged":
			code = hook.TerminalDesignNonconverged
		default:
			// Approved and still failed: the applier's instruction could not
			// be rendered, or the card died after sealing — the machinery's own.
			code = hook.TerminalInternalFailed
		}
	case runtime.StageReviewA:
		// The card that seals the applier's work is where an objection
		// surfaces: the applier left revise-design.json and the seal turned
		// it into a sealed design-objection.json instead of a candidate.
		if objected, err := designObjectionRecorded(runDir, view.round); err == nil && objected {
			return true, nextDesignRound(ctx, hermes, config, run, view, plan, "the applier objected to the design", logger)
		}
		return false, nil
	default:
		return false, nil
	}
	terminal := runner.NewTerminal(config, services, envelope, chainOwnerRunID(run.DeliveryID), runDir, logger)
	repository, err := readField(runDir, "ticket-draft.json", "repository")
	if err != nil {
		repository = ""
	}
	if err := terminal.Report(ctx, code, runner.Outcome{Code: code}, repository); err != nil {
		return true, err
	}
	logger.Info("chain terminalized", "run", run.RunID, "stage", stageName, "code", string(code))
	return true, archiveChain(ctx, hermes, view.all)
}

// designObjectionRecorded reports whether the implementation round's seal
// recorded an applier objection instead of a candidate.
func designObjectionRecorded(runDir string, implementRound int) (bool, error) {
	for _, dir := range []string{fmt.Sprintf("history/stage-%d", implementRound), fmt.Sprintf("history/stage-%d/objection", implementRound)} {
		if _, err := os.Stat(filepath.Join(runDir, dir, "design-objection.json")); err == nil {
			return true, nil
		}
	}
	return false, nil
}

// nextDesignRound retires the cards that are not done and creates the next
// design round; the implementation counter advances only so the fresh apply
// card gets a key of its own — the round budget (max_stages) is counted from
// sealed decisions, which an objection or a design revision never adds to.
func nextDesignRound(
	ctx context.Context,
	hermes *runtime.Hermes,
	config runtime.Config,
	run state.RunOverview,
	view chainView,
	plan runtime.ChainPlan,
	why string,
	logger Logger,
) error {
	limit := consumerDesignMaxRounds(config.ConsumerConfigPath)
	if view.designRound >= limit {
		// The decide verb converts a last-round revise into nonconverged; an
		// objection at the limit has no verb to do that, so the attendant
		// ends the run honestly here.
		return errors.New("design round limit reached: " + why)
	}
	for _, task := range view.all {
		if task.Status == "done" {
			continue
		}
		if err := hermes.Archive(ctx, task.ID); err != nil {
			return err
		}
	}
	// Cards that are done but sit below the retired ones in the parent chain
	// (the apply card whose objection brought us here) must not be reused:
	// their keys carry the implementation round, so the next round's tail is
	// keyed one higher. Design cards are keyed by the new design round.
	rounds := runtime.ChainRounds{Design: view.designRound + 1, Implement: view.round}
	if plan.Shape == runtime.ShapeDesign {
		if _, done := view.cards[runtime.StageApply]; done && view.cards[runtime.StageApply].Status == "done" {
			rounds.Implement = view.round + 1
		}
		if rounds.Implement < 1 {
			rounds.Implement = 1
		}
		for _, task := range view.cards {
			if task.Status == "done" && task.ID != view.cards[runtime.StageApply].ID {
				// A done tail card below a retired one would be re-gated by
				// the kanban as satisfied; archive it so the new chain is whole.
				if err := hermes.Archive(ctx, task.ID); err != nil {
					return err
				}
			}
		}
	}
	terminalCard, err := runtime.EnsureChainFor(ctx, hermes, config.Chain, plan, nil, run.DeliveryID, run.RunID, run.Summary, rounds)
	if err != nil {
		return err
	}
	logger.Info("design round regenerated", "run", run.RunID, "why", why, "design_round", rounds.Design, "implement_round", rounds.Implement, "terminal_card", terminalCard)
	return nil
}
