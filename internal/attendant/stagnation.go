package attendant

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
	"automation.internal/ticket-ingress/internal/worker"
)

// A delivery repeating itself is the one thing rounds alone cannot fix.
//
// Rounds are cheap to start and there is no longer a count that stops them,
// so a round that objects to exactly what the round before it objected to,
// or that produces exactly the change the round before it produced, would go
// on being started for as long as the provider's key holds out. Nothing in
// that loop is a decision about the request. The engine has to make one.
//
// Two signals say the delivery has stopped moving, and each is deliberately
// an exact match rather than a resemblance:
//
//	S1  the objections did not move. The same seats raised the same codes
//	    about the same paths. A round that fixed two of three findings is a
//	    strict subset, not an equality, and is a delivery converging — which
//	    must not be interrupted.
//	S2  the change did not move. The round wrote the same bytes to the same
//	    paths. For a round the judges passed and the destination's own
//	    commands refused, the same bytes paired with the same refusal.
//
// The objection's identity is (seat, code, path) and deliberately not the
// message: the same complaint is worded differently every time a model is
// asked for it, and an identity that moved with the wording would recognise
// nothing. The cost of leaving the message out is a seat that invents a new
// code for the same complaint every round, which is not recognised as
// stagnation and goes on costing money without stopping — money rather than
// a stalled delivery, which is the cheaper of the two failures.
//
// A round that cannot be read is not stagnation. Two rounds have to be
// compared to say they are the same, and a round missing a record says
// nothing about whether the delivery is moving; the delivery goes on, which
// is what an unreadable record was always least able to justify stopping.

// findingKey is one objection's identity across rounds.
type findingKey struct {
	reviewer string
	code     string
	path     string
}

// roundSignature is what one round did, in the terms the next round is
// compared against.
type roundSignature struct {
	// findings is every objection the round's seats left standing. Empty
	// for a round the seats passed.
	findings map[findingKey]struct{}
	// candidate is the fingerprint of the change: the paths and the digest
	// of what was written to each, in path order.
	//
	// The candidate's own sealed digest cannot be used. It covers the time
	// the candidate was generated and the model request that generated it,
	// so two rounds writing byte-identical files carry different digests by
	// construction — which is exactly the case this has to recognise.
	candidate string
	// validation is the digest of what the destination's own commands
	// printed when they refused the round, empty for a round that was never
	// validated. It rides with the candidate fingerprint: a round the
	// judges passed and the commands refused has no standing objections to
	// compare, and repeating the same change and getting the same refusal
	// is the shape stagnation takes there.
	validation string
}

// readRoundSignature reads one round. A round whose candidate or whose
// seats' reviews are missing or unreadable has no signature, which the
// caller reads as a round that says nothing about stagnation.
func readRoundSignature(runDir string, reviewers []string, round int) (roundSignature, bool) {
	if round < 1 || len(reviewers) == 0 {
		return roundSignature{}, false
	}
	stageDir := filepath.Join(runDir, "history", fmt.Sprintf("stage-%d", round))
	var candidate worker.Candidate
	if err := worker.ReadJSONFile(filepath.Join(stageDir, "candidate.json"), worker.MaxArtifactJSONBytes, &candidate); err != nil {
		return roundSignature{}, false
	}
	signature := roundSignature{findings: make(map[findingKey]struct{}, 8), candidate: candidateFingerprint(candidate)}
	for _, reviewer := range reviewers {
		var review worker.Review
		if err := worker.ReadJSONFile(filepath.Join(stageDir, reviewer+".json"), worker.MaxReviewJSONBytes, &review); err != nil {
			return roundSignature{}, false
		}
		if review.Verdict != "revise" {
			continue
		}
		for _, finding := range review.Findings {
			signature.findings[findingKey{reviewer: reviewer, code: finding.Code, path: finding.Path}] = struct{}{}
		}
	}
	if failure, sealed := runner.ReadValidationFailure(runDir, round); sealed {
		signature.validation = failure.OutputSHA256
	}
	return signature, true
}

// candidateFingerprint is the change itself: every path and the digest of
// what the round wrote there, in path order, folded into one digest.
func candidateFingerprint(candidate worker.Candidate) string {
	type fileDigest struct {
		path   string
		digest string
	}
	files := make([]fileDigest, 0, len(candidate.Files))
	for _, file := range candidate.Files {
		sum := sha256.Sum256([]byte(file.Content))
		files = append(files, fileDigest{path: file.Path, digest: hex.EncodeToString(sum[:])})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	hash := sha256.New()
	for _, file := range files {
		// Length-prefixed, so that two different path-and-content splits
		// cannot fold to the same fingerprint.
		fmt.Fprintf(hash, "%d:%s%d:%s", len(file.path), file.path, len(file.digest), file.digest)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// repeats reports whether one round did what the round before it did.
func (s roundSignature) repeats(previous roundSignature) bool {
	// S1: the objections did not move. An empty set is not a signal — a
	// round the seats passed objected to nothing, and two of those in a row
	// is a delivery that converged, not one that is stuck.
	if len(s.findings) > 0 && len(s.findings) == len(previous.findings) {
		same := true
		for key := range s.findings {
			if _, carried := previous.findings[key]; !carried {
				same = false
				break
			}
		}
		if same {
			return true
		}
	}
	// S2: the change did not move, and — where the destination's commands
	// had their say — they said the same thing about it.
	return s.candidate != "" && s.candidate == previous.candidate && s.validation == previous.validation
}

// stagnated reports whether the delivery has stopped moving at this round:
// the signal holding for repeat consecutive rounds, each read whole.
func stagnated(runDir string, reviewers []string, round, repeat int) bool {
	if repeat < 1 {
		repeat = worker.DefaultStagnationRepeatRounds
	}
	if round < repeat+1 {
		return false
	}
	newer, readable := readRoundSignature(runDir, reviewers, round)
	if !readable {
		return false
	}
	for step := 0; step < repeat; step++ {
		older, readable := readRoundSignature(runDir, reviewers, round-step-1)
		if !readable || !newer.repeats(older) {
			return false
		}
		newer = older
	}
	return true
}

// arbitrationTimeout bounds the one model call this makes inside the
// attendant's own loop. A deadlock is ruled on at most once per round, and
// an arbiter that cannot answer inside this leaves the round to go on to
// the next one rather than holding every other delivery's tick.
const arbitrationTimeout = 2 * time.Minute

// ruleOnStagnation rules on a round that has stopped moving, and reports
// whether the round was put back to work under that ruling.
//
// True means the round is being decided again without the objections the
// ticket does not require, and this tick is finished. False means the
// delivery goes on to the next round — because nothing was stagnant, because
// the ruling told the implementer what to satisfy, or because the ruling
// could not be made at all. Only the last of those is a disappointment, and
// even then another round is a better answer than stopping: the delivery
// keeps its own momentum and the ladder is still behind everything that
// breaks.
func ruleOnStagnation(
	ctx context.Context,
	config runtime.Config,
	services *runtime.Services,
	hermes *runtime.Hermes,
	envelope hook.DispatchEnvelope,
	run state.RunOverview,
	view chainView,
	logger Logger,
) (bool, error) {
	runDir := runDirectory(config, run.DeliveryID)
	reviewers, err := consumerReviewerIDs(config.ConsumerConfigPath)
	if err != nil {
		logger.Error("the deadlock could not be looked for; the delivery goes on to the next round",
			"run", run.RunID, "round", view.round, "error", err.Error())
		return false, nil
	}
	// A round already ruled on is not ruled on twice. The one case that
	// reaches here is an overruling that did not carry the round: some
	// objections stood, the round was decided as revise again, and the
	// answer to that is the next round rather than a second ruling on the
	// same evidence.
	existing, err := runner.ReadRuling(runDir, view.round)
	if err != nil {
		logger.Error("the round's ruling could not be read; the delivery goes on to the next round",
			"run", run.RunID, "round", view.round, "error", err.Error())
		return false, nil
	}
	if existing != nil {
		return false, nil
	}
	repeat := consumerStagnationRounds(config.ConsumerConfigPath)
	if !stagnated(runDir, reviewers, view.round, repeat) {
		return false, nil
	}
	logger.Info("the rounds have stopped moving; the engine rules on it",
		"run", run.RunID, "round", view.round, "repeat_rounds", repeat)
	ruleCtx, cancel := context.WithTimeout(ctx, arbitrationTimeout)
	defer cancel()
	pipeline := &runner.Pipeline{Config: config, Workspace: runDir, Logger: logger}
	ruling, err := pipeline.Arbitrate(ruleCtx, view.round)
	if err != nil {
		logger.Error("the deadlock could not be ruled on; the delivery goes on to the next round",
			"run", run.RunID, "round", view.round, "error", err.Error())
		return false, nil
	}
	if ruling.Ruling != worker.RulingOverruleReviewer {
		logger.Info("the change was ruled short of the request; the next round is told what to satisfy",
			"run", run.RunID, "round", view.round)
		return false, nil
	}
	// The objections the ticket does not require are set aside, so the round
	// is counted again. The decision sealed without the ruling has to go
	// first: the decide verb writes once, and the card dispatched below
	// would otherwise read the old answer straight back.
	if err := runner.DropDecision(runDir, view.round); err != nil {
		logger.Error("the ruled round could not be cleared to be decided again; the delivery goes on to the next round",
			"run", run.RunID, "round", view.round, "error", err.Error())
		return false, nil
	}
	// The shape is needed only here: rebuilding a card means knowing which
	// cards this delivery has. A shape that will not read leaves the ruling
	// sealed and the delivery going on to the next round, which is a worse
	// answer than deciding this one again and a much better one than
	// stopping.
	plan, err := chainPlanFor(config, runDir, run, logger)
	if err != nil {
		logger.Error("the chain shape could not be read; the ruled round goes on to the next one instead",
			"run", run.RunID, "round", view.round, "error", err.Error())
		return false, nil
	}
	logger.Info("the objections were ruled not required by the request; the round is decided again",
		"run", run.RunID, "round", view.round, "overruled", len(ruling.Overruled))
	verdict, err := dispatchAgain(ctx, newClimb(config, services, hermes, envelope, run, view, plan, runtime.StageValidate, logger))
	if err != nil {
		return false, err
	}
	// A requester who asked the delivery to stop while this was decided is
	// answered by the caller, which ends the run; the ruling stays sealed
	// and says what was decided before they asked.
	return verdict == ladderHandled, nil
}

// consumerStagnationRounds reads how many consecutive identical rounds the
// destination calls a deadlock, leniently: an absent or unreadable value is
// the default, because a delivery that cannot read this setting is better
// ruled on early than never.
func consumerStagnationRounds(consumerConfigPath string) int {
	raw, err := os.ReadFile(consumerConfigPath)
	if err != nil {
		return worker.DefaultStagnationRepeatRounds
	}
	var parsed struct {
		StagnationRepeatRounds int `json:"stagnation_repeat_rounds"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || parsed.StagnationRepeatRounds < 1 || parsed.StagnationRepeatRounds > 10 {
		return worker.DefaultStagnationRepeatRounds
	}
	return parsed.StagnationRepeatRounds
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
