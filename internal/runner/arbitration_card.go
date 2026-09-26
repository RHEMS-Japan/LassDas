package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"automation.internal/ticket-ingress/internal/worker"
)

// arbitrationFailure keeps an unfinished ruling distinct from a completed
// judgment that the change needs revision. The latter starts implementation;
// the former must recover this card without paying for the same change again.
type arbitrationFailure struct{ cause error }

func (f *arbitrationFailure) Error() string { return "arbitration is unfinished: " + f.cause.Error() }
func (f *arbitrationFailure) Unwrap() error { return f.cause }

// Arbitration remains bounded, but its clock now belongs to the validation
// card, not the attendant's tick. Stops and other deliveries do not wait for it.
const arbitrationTimeout = 2 * time.Minute

func (p *Pipeline) arbitrateRefusedRound(ctx context.Context, reviewers []string, round int) (*worker.Ruling, error) {
	ruling, err := ReadRuling(p.Workspace, round)
	if err != nil {
		return nil, &arbitrationFailure{err}
	}
	if ruling != nil || !RoundsStagnated(p.Workspace, reviewers, round, ConsumerStagnationRounds(p.Config.ConsumerConfigPath)) {
		return ruling, nil
	}
	ctx, cancel := context.WithTimeout(ctx, arbitrationTimeout)
	defer cancel()
	ruling, err = p.Arbitrate(ctx, round)
	if err != nil {
		return nil, &arbitrationFailure{err}
	}
	return ruling, nil
}

// RulingApplied distinguishes a decision counted under an overruling from
// the earlier revise decision left by an interrupted card. The worker still
// re-derives and validates the complete decision before validation/publication.
func RulingApplied(runDir string, round int, ruling *worker.Ruling) (bool, error) {
	path := filepath.Join(runDir, "history", fmt.Sprintf("stage-%d", round), "decision.json")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	var decision worker.StageDecision
	if err := worker.ReadJSONFile(path, worker.MaxArtifactJSONBytes, &decision); err != nil {
		return false, err
	}
	return ruling != nil && ruling.RulingSHA256 != "" && decision.Ruling != nil && decision.Ruling.RulingSHA256 == ruling.RulingSHA256, nil
}

func (p *Pipeline) decideChainRound(ctx context.Context, round int, reviewers []string, recount bool) error {
	stageDir := p.path(fmt.Sprintf("history/stage-%d", round))
	if recount {
		if err := DropDecision(p.Workspace, round); err != nil {
			return err
		}
	}
	if _, err := os.Stat(filepath.Join(stageDir, "decision.json")); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	args := []string{
		"decide", "--config", p.Config.ConsumerConfigPath, "--tool-sha", p.Config.Identity.EngineSHA,
		"--ticket", stageDir + "/ticket.json", "--source", stageDir + "/source.json", "--candidate", stageDir + "/candidate.json",
	}
	for _, reviewer := range reviewers {
		args = append(args, "--review", filepath.Join(stageDir, reviewer+".json"))
	}
	ruling, err := ReadRuling(p.Workspace, round)
	if err != nil {
		return err
	}
	if ruling != nil && ruling.Ruling == worker.RulingOverruleReviewer {
		args = append(args, "--ruling", RulingFile(p.Workspace, round))
	}
	args = append(args, "--out", stageDir+"/decision.json")
	if err := p.runVerb(ctx, "decide", args); err != nil {
		return fmt.Errorf("the round could not be decided: %w", err)
	}
	return nil
}
