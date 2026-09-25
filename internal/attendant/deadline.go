package attendant

import (
	"strconv"
	"time"

	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
)

// The end of a run that cannot get past a failure.
//
// The ladder is built so that nothing ends a delivery: each kind of failure
// has a remedy, the remedies are played one at a time, and when they are
// spent the stage waits and is dispatched again with the waits growing. That
// is the right shape for every failure that clears. For one that does not —
// a key nobody raises, a provider that stays down, an agent that hands the
// work back with the same sentence every time — it is a delivery that climbs
// all night and all the next day, posts one notice saying it is still going,
// and never reports anything at all.
//
// So a run has a wall. When it is reached while the run is stuck, the
// delivery stops climbing and writes down where it got to: what it kept
// meeting, how many attempts and remedies it spent on it, what landed, and
// what a person would have to supply or fix. That is a worse morning than a
// finished delivery and a far better one than no comment.
//
// Two things it deliberately is not. It is not a timeout on the work: a run
// that is making progress is never touched by it, because it is only ever
// asked at the two places a run can be stuck — climbing the ladder, and
// answering a round the implementing agent handed back. And it is not an
// internal failure: nothing broke, the engine ran out of the time it was
// given.

// runDeadlinePassed says whether this run has used up the time it was given,
// and how long it has been going.
//
// The clock starts at the claim. What it therefore excludes is the one wait
// this engine still has with a person in it: a reception question hands the
// row back to the queue with its claim cleared, so the hours or days a
// requester takes to answer belong to no claim, and the claim that follows
// the answer starts the clock again from nothing. A delivery is measured on
// the time it spent working, never on the time it spent waiting to be told
// what to work on.
//
// A run with no claim time on the row cannot be measured and is left alone.
// That is a ledger row from before this field, or a caller that built an
// overview by hand; ending such a run on a clock that reads zero would
// report every one of them as out of time the moment it met a failure.
func runDeadlinePassed(config runtime.Config, run state.RunOverview, now time.Time) (spent time.Duration, passed bool) {
	if run.ClaimedAt <= 0 {
		return 0, false
	}
	claimed := time.UnixMilli(run.ClaimedAt).UTC()
	spent = now.Sub(claimed)
	if spent < 0 {
		// A claim in the future: a clock that moved backwards under the
		// pod. Not an ending — the next tick reads it again.
		return 0, false
	}
	return spent, spent >= config.Chain.RunDeadline()
}

// deadlineEvidence is what the report of a delivery that ran out of time
// carries: the step it was on when the clock went, how long it was given,
// and what already landed.
//
// What landed is read from the records the cards sealed rather than from
// anything this tick believes, for the same reason the finished delivery's
// report reads them: a comment that names a staging screen has to be
// pointing at one that exists. A run whose destination cannot be read
// carries no evidence at all, because a report that shows a pull request
// has to say which repository it is in.
func deadlineEvidence(config runtime.Config, runDir, repository, stageName string, round int) map[string]string {
	evidence := map[string]string{}
	for key, value := range failedStepEvidence(config, runDir, stageName, round) {
		evidence[key] = value
	}
	// The reason for a model failure is bound to the model_failed ending by
	// the report's own shape check, and this ending is not that one. Left
	// in, it would fail the check — and a report that fails the check never
	// reaches the requester, which is the whole thing this ending exists to
	// prevent.
	delete(evidence, "model_failure_reason")
	evidence["deadline_hours"] = strconv.Itoa(int(config.Chain.RunDeadline().Hours()))
	if repository == "" {
		return evidence
	}
	if url, err := readField(runDir, "feature-pr.json", "payload", "pull_request", "HTMLURL"); err == nil && url != "" {
		evidence["pull_request_url"] = url
	}
	for file, key := range map[string]string{
		runner.DeliverStagingReportFile:    "staging_evidence_url",
		runner.DeliverProductionReportFile: "production_evidence_url",
	} {
		report, found, err := runner.ReadDeliverReport(runDir, file)
		if err == nil && found && report.Verdict == "pass" && report.TargetURL != "" {
			evidence[key] = report.TargetURL
		}
	}
	// No depth is carried. The report binds that field to an ending that
	// finished or was stopped and refuses it on any other, so how far this
	// delivery got is told in the prose the run composes instead.
	return evidence
}
