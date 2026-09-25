package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"automation.internal/ticket-ingress/internal/worker"
)

// Where a ruling lives, and how one is made.
//
// A round the engine had to rule on keeps the ruling beside its own
// candidate, reviews and decision, on the volume that outlives the pod. Two
// readers want it: the decide verb of the same round, which counts the
// verdict again without the objections the ruling set aside, and the next
// round's instruction, which carries what the ruling told the implementer.
// Both find it by round number, the way every other record of a round is
// found.

// RulingFile is where one round's ruling is sealed.
func RulingFile(runDir string, round int) string {
	return filepath.Join(runDir, "history", fmt.Sprintf("stage-%d", round), worker.RulingFileName)
}

// ReadRuling reads back the ruling made about one round. A round nobody had
// to rule on — nearly all of them — reads as no ruling and no error.
//
// A file that is there and will not read is an error rather than an absence.
// Reading it as absent would count a round under objections the engine had
// already set aside, which is the round being decided wrongly rather than
// not being decided.
func ReadRuling(runDir string, round int) (*worker.Ruling, error) {
	if round < 1 {
		return nil, nil
	}
	return worker.ReadRulingFile(RulingFile(runDir, round))
}

// Arbitrate rules on a round that has stopped moving, and reports what was
// ruled. A round that already carries a ruling is not ruled on again: the
// first ruling stands, and a second call is the caller looking at the same
// deadlock one tick later.
//
// The reviews are the ones the round actually sealed, by seat, in the order
// the seats are configured — the same set the decide verb read, so the
// ruling is bound to exactly what was counted.
func (p *Pipeline) Arbitrate(ctx context.Context, round int) (*worker.Ruling, error) {
	if round < 1 {
		return nil, errors.New("a ruling names a round this chain does not have")
	}
	if existing, err := ReadRuling(p.Workspace, round); err != nil || existing != nil {
		return existing, err
	}
	reviewers, err := chainReviewers(p.Config.ConsumerConfigPath)
	if err != nil {
		return nil, err
	}
	stageDir := fmt.Sprintf("%s/stage-%d", p.path("history"), round)
	args := []string{
		"arbitrate", "--config", p.Config.ConsumerConfigPath, "--tool-sha", p.Config.Identity.EngineSHA,
		"--ticket", stageDir + "/ticket.json", "--source", stageDir + "/source.json",
		"--candidate", stageDir + "/candidate.json",
	}
	for _, reviewer := range reviewers {
		review := filepath.Join(stageDir, reviewer+".json")
		if _, err := os.Stat(review); err != nil {
			return nil, fmt.Errorf("the sealed review by %s is not there to rule on", reviewer)
		}
		args = append(args, "--review", review)
	}
	args = append(args, p.clarificationArgs()...)
	args = append(args, "--out", RulingFile(p.Workspace, round))
	if err := p.runVerb(ctx, "arbitrate", args, p.modelKeyEnv()...); err != nil {
		return nil, fmt.Errorf("the deadlock could not be ruled on: %w", err)
	}
	ruling, err := ReadRuling(p.Workspace, round)
	if err != nil {
		return nil, err
	}
	if ruling == nil {
		return nil, errors.New("the arbiter wrote no ruling")
	}
	return ruling, nil
}

// DropDecision clears one round's decision so it can be decided again under
// a ruling.
//
// Only the decision. The candidate and the reviews are exactly what they
// were and are what the round is being decided from; the round is being
// counted again, not done again. The decision has to go because the sealed
// one was counted without the ruling, and the round a validate card works on
// is the newest one carrying a candidate — a round still holding its old
// decision would have the card seal nothing and read the old answer back.
func DropDecision(runDir string, round int) error {
	if round < 1 {
		return errors.New("a decision to drop names a round this chain does not have")
	}
	path := filepath.Join(runDir, "history", fmt.Sprintf("stage-%d", round), "decision.json")
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
