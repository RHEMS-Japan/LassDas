package runner

import (
	"automation.internal/ticket-ingress/internal/worker"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// A delivery repeating itself is the one thing rounds alone cannot fix.
//
// Rounds are cheap to start and there is no longer a count that stops them,
// so a round that objects to exactly what the round before it objected to,
// or that produces exactly the change the round before it produced, would go
// on being started for as long as the provider's key holds out. Nothing in
// that loop is a decision about the request. The engine has to make one.
//
// Three signals say the delivery has stopped moving, and each is deliberately
// an exact match rather than a resemblance:
//
//	S1  the objections did not move. The same seats raised the same codes
//	    about the same paths. A round that fixed two of three findings is a
//	    strict subset, not an equality, and is a delivery converging — which
//	    must not be interrupted.
//	S2  the change did not move. The round wrote the same bytes to the same
//	    paths. For a round the judges passed and the destination's own
//	    commands refused, the same bytes paired with the same refusal.
//	S3  a finding disappeared and returned, or the change returned to bytes
//	    already refused. Fixing one objection by undoing the fix for another
//	    is not progress. Other findings may have moved in the meantime, so
//	    requiring the entire set to recur would miss this oscillation.
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
	if candidate.Stage != round || len(candidate.Files) == 0 {
		return roundSignature{}, false
	}
	signature := roundSignature{findings: make(map[findingKey]struct{}, 8), candidate: candidateFingerprint(candidate)}
	for _, reviewer := range reviewers {
		var review worker.Review
		if err := worker.ReadJSONFile(filepath.Join(stageDir, reviewer+".json"), worker.MaxReviewJSONBytes, &review); err != nil {
			return roundSignature{}, false
		}
		if review.Stage != round || review.ReviewerID != reviewer || (review.Verdict != "pass" && review.Verdict != "revise") {
			return roundSignature{}, false
		}
		if review.Verdict != "revise" {
			continue
		}
		for _, finding := range review.Findings {
			signature.findings[findingKey{reviewer: reviewer, code: finding.Code, path: finding.Path}] = struct{}{}
		}
	}
	if failure, sealed := ReadValidationFailure(runDir, round); sealed {
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

// recurrence counts returns separated by at least one observed absence.
// Continuous presence does not count: an objection left standing while other
// objections are being fixed is not evidence of an oscillation.
type recurrence struct {
	absent  bool
	returns int
}

func (r *recurrence) observe(present bool) int {
	if !present {
		r.absent = true
	} else if r.absent {
		r.returns++
		r.absent = false
	}
	return r.returns
}

// RoundsStagnated reports consecutive unchanged rounds or a return to a resolved
// finding / rejected change. The setting counts repetitions of either
// signal. Every intervening round must be readable: absence of a record is
// not evidence that an objection was answered. The existing record ceiling
// also bounds this lookback; no model call or extra record is needed.
func RoundsStagnated(runDir string, reviewers []string, round, repeat int) bool {
	if repeat < 1 {
		repeat = worker.DefaultStagnationRepeatRounds
	}
	if round < repeat+1 || round > worker.StageCeiling {
		return false
	}
	current, readable := readRoundSignature(runDir, reviewers, round)
	if !readable {
		return false
	}
	findings := make(map[findingKey]*recurrence, len(current.findings))
	for key := range current.findings {
		findings[key] = &recurrence{}
	}
	var change recurrence
	newer := current
	consecutive, unchanged := true, 0
	for prior := round - 1; prior >= 1; prior-- {
		older, readable := readRoundSignature(runDir, reviewers, prior)
		if !readable {
			return false
		}
		consecutive = consecutive && newer.repeats(older)
		if consecutive {
			unchanged++
			if unchanged >= repeat {
				return true
			}
		}
		for key, seen := range findings {
			_, present := older.findings[key]
			if seen.observe(present) >= repeat {
				return true
			}
		}
		sameChange := current.candidate != "" && current.candidate == older.candidate && current.validation == older.validation
		if change.observe(sameChange) >= repeat {
			return true
		}
		newer = older
	}
	return false
}

// ConsumerStagnationRounds reads how many repetitions (unchanged rounds or
// returns to a resolved finding / rejected change) call for arbitration.
// An absent or unreadable value is
// the default, because a delivery that cannot read this setting is better
// ruled on early than never.
func ConsumerStagnationRounds(consumerConfigPath string) int {
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
