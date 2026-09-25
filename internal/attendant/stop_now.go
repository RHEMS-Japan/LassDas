package attendant

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
)

// The stop, honoured inside one tick.
//
// 「停止」 was read in six places, and every one of them sat at a boundary:
// before a round was created, before a card was dispatched again, before
// the merge, before the question. A delivery between two boundaries — a
// card running, which is where a delivery spends nearly all of its wall
// clock — read nothing at all. Measured on this engine: with an
// implementing, reviewing, validating, checking, merging or promoting card
// in flight, three passes of the chain loop left the run claimed, the
// ticket without a word on it, and the requester watching a step they had
// asked to stop.
//
// So the stop is read first instead, on every pass, for every claimed run,
// before anything looks at what the cards are doing. What follows is one
// sequence: write down that the run was stopped, answer the ticket, stop
// dispatching, retire the cards, and end the run as cancelled carrying
// whatever had actually landed.
//
// Retiring a card does not kill the process behind it. It stops this engine
// from reading the result, which for every card but the two that merge is
// the same thing as stopping it: a result nobody reads is a result nobody
// acts on, and the branch, the review and the validation output it might
// still write go nowhere. The two that merge are different and are handled
// below.

// stopReadFile is where a run records its reading of the ticket for a
// 「停止」: when it last looked, and what it found. It lives on the volume
// beside the run's other records, so a pod replaced between reading the
// stop and ending the run comes back knowing the run was stopped rather
// than resuming the work — and so a restart does not reset the interval
// below and start listing the tracker from scratch.
//
// A fresh attempt never inherits it: the preparation empties the run
// directory before anything else (internal/runner/runner.go's Prepare), so
// a stop belongs to the attempt that read it and to no other.
const stopReadFile = "stop-read.json"

// stopReadSchemaVersion is this record's shape.
const stopReadSchemaVersion = 1

// maxStopReadBytes bounds the read. The record is four short fields;
// anything larger is not one of ours.
const maxStopReadBytes = 16 * 1024

// tickStopReadInterval is how often one run's tick may list its ticket for
// a 「停止」.
//
// Unthrottled, this cost one listing per run per pass of a loop that passes
// every ten seconds — six a minute for every delivery at once, each paging
// through a long ticket — which is the cost the ladder's own waiting stages
// already refused to pay (stopReadInterval, ladder.go). Half that interval
// is used here rather than the ladder's full minute, because this read is
// the one a requester's stop waits on wherever the delivery happens to be:
// at a fifth of the requests it still honours a stop inside the minute the
// contract promises (docs/OPERATING.md).
//
// It is deliberately not the interval the reads that spend money use. Those
// are unthrottled and stay so: a dispatch, a round and a merge each read the
// ticket afresh, because that read is the last thing between a requester who
// asked to stop and money being spent.
const tickStopReadInterval = 30 * time.Second

// stopReadRecord is the run's own account of the stop: when the ticket was
// last listed for one, when the stop was first seen, whether the requester
// has been answered, and which step — if any — the ending is waiting on.
type stopReadRecord struct {
	SchemaVersion int `json:"schema_version"`
	// LastReadAt is when the ticket was last listed for a stop by the tick,
	// which is what the interval above is measured from.
	LastReadAt time.Time `json:"last_read_at"`
	// RequestedAt is when the stop was first seen. Zero means the requester
	// has not asked this run to stop as far as any read has shown.
	RequestedAt time.Time `json:"requested_at,omitempty"`
	// Acknowledged says the one-line answer is on the ticket. Kept here as
	// well as on the ticket because the ticket read that would prove it is
	// the same listing that finds the stop, and a pass that did not list
	// must not post a second answer on the strength of not having looked.
	Acknowledged bool `json:"acknowledged,omitempty"`
	// Waiting names the delivery phase whose merge is in flight, which the
	// run lets finish before it reports. Empty when nothing had to finish.
	Waiting string `json:"waiting,omitempty"`
}

// stopped reports whether a read has shown the requester asking this run to
// stop. Once it has, no later read can take it back: a comment the tracker
// stops returning is a comment that was written.
func (r stopReadRecord) stopped() bool { return !r.RequestedAt.IsZero() }

// dueForStopRead says whether the ticket may be listed this pass.
func (r stopReadRecord) dueForStopRead(now time.Time) bool {
	return r.LastReadAt.IsZero() || !now.Before(r.LastReadAt.Add(tickStopReadInterval))
}

func readStopRead(runDir string) stopReadRecord {
	encoded, err := os.ReadFile(filepath.Join(runDir, stopReadFile))
	if err != nil || len(encoded) > maxStopReadBytes {
		return stopReadRecord{}
	}
	var record stopReadRecord
	if json.Unmarshal(encoded, &record) != nil || record.SchemaVersion != stopReadSchemaVersion {
		return stopReadRecord{}
	}
	return record
}

// writeStopRead seals the record. Best-effort like every other record this
// package writes: a volume that refuses the write must not be the reason a
// requester who asked to stop is not obeyed. What it costs is a ticket
// listed once a pass instead of once an interval, and one repeated
// acknowledgement after a pod restart, which the ticket's own marker then
// catches.
//
// The directory is made first because a queued run reads its ticket for a
// stop before anything else has had reason to make one.
func writeStopRead(runDir string, record stopReadRecord, logger Logger) {
	record.SchemaVersion = stopReadSchemaVersion
	// 0711 like every other writer of the run directory: the agent user
	// must be able to enter it later (docs/RUNTIME_POD.md).
	err := os.MkdirAll(runDir, 0o711)
	if err == nil {
		var encoded []byte
		if encoded, err = json.Marshal(record); err == nil {
			err = os.WriteFile(filepath.Join(runDir, stopReadFile), encoded, 0o600)
		}
	}
	if err != nil {
		logger.Error("the stop reading could not be recorded; the run is stopped anyway",
			"error", err.Error())
	}
}

// refreshStopRead brings the record up to date from the ticket, at most
// once per interval, and hands back the listing it used — nil when it did
// not list, which is every pass inside the interval and every pass of a run
// that already knows both things a listing could tell it.
//
// A listing that fails still counts against the interval. Asking a tracker
// that is refusing once every ten seconds, for every delivery at once, is
// the shape that turns one outage into a second one; the stop is delayed by
// the interval instead, and the reads that gate spending are unaffected.
func refreshStopRead(
	ctx context.Context,
	config runtime.Config,
	backlog commentLister,
	run state.RunOverview,
	record *stopReadRecord,
	runDir string,
	now time.Time,
	logger Logger,
) []hook.BacklogComment {
	if record.stopped() && record.Acknowledged {
		return nil
	}
	if !record.dueForStopRead(now) {
		return nil
	}
	record.LastReadAt = now
	comments, err := backlog.ListComments(ctx, run.IssueID, 0)
	if err != nil {
		logger.Error("the stop could not be read this pass; the delivery goes on and is asked again after the interval",
			"run", run.RunID, "error", err.Error())
		writeStopRead(runDir, *record, logger)
		return nil
	}
	if !record.stopped() && containsStopComment(comments, config.Tracker.AllowedCreatorID) {
		record.RequestedAt = now
	}
	writeStopRead(runDir, *record, logger)
	return comments
}

// honourStopNow is the first thing a claimed run's tick does. It reports
// whether it handled the run, in which case nothing else in the tick may
// touch it: no healing, no report, no dispatch.
//
// The ticket is listed at most once per tickStopReadInterval, and only
// while something is still to be learnt from it: whether the requester has
// asked to stop, and, once they have, whether this engine's own answer is
// already posted. A run that knows both never lists it again, however many
// passes the ending takes — so a delivery waiting for a merge to finish
// costs the tracker nothing at all.
//
// An unreadable listing is not a stop. The engine's whole purpose is to
// keep going, and the reads that gate spending money are still fail-closed
// where they were: nothing is dispatched, no round is created and no merge
// happens past a listing this engine could not read. So a tracker outage
// delays the stop rather than either ignoring it or freezing the delivery.
func honourStopNow(
	ctx context.Context,
	config runtime.Config,
	services *runtime.Services,
	hermes *runtime.Hermes,
	run state.RunOverview,
	view chainView,
	runDir string,
	logger Logger,
) (bool, error) {
	if services == nil || services.Backlog == nil {
		return false, nil
	}
	record := readStopRead(runDir)
	comments := refreshStopRead(ctx, config, services.Backlog, run, &record, runDir, time.Now().UTC(), logger)
	if !record.stopped() {
		return false, nil
	}
	// The ending is a terminal report and a terminal report needs the run's
	// sealed envelope. Without one there is nothing to report with, so the
	// tick goes on to the recovery it would have done anyway — which
	// rebuilds the directory from the ledger's own copy, and the stop is
	// honoured on the pass after that.
	envelope, err := readEnvelope(runDir, run.DeliveryID)
	if err != nil {
		logger.Error("the requester's stop cannot be reported without the run's sealed envelope; the recovery runs first",
			"run", run.RunID, "error", err.Error())
		return false, nil
	}

	record.Waiting = mergeCardInFlight(runDir, run.DeliveryID, view.board)
	// Written before the ticket is answered and before anything is retired:
	// of the three, this is the one that survives the process.
	writeStopRead(runDir, record, logger)

	// Answered from the listing the stop was found in. A pass that did not
	// list has nothing to check its own marker against, so it leaves the
	// answer to the pass after the interval rather than posting a second
	// one blind.
	if !record.Acknowledged && comments != nil &&
		acknowledgeStop(ctx, services.Backlog, run, comments, logger) {
		record.Acknowledged = true
		writeStopRead(runDir, record, logger)
	}

	if record.Waiting != "" {
		// Nothing is dispatched and nothing is retired. The step finishes
		// or fails on its own, and the pass after that ends the run with
		// the records it sealed.
		logger.Info("the requester's stop is recorded; the step that merges is left to finish before the run ends",
			"run", run.RunID, "stage", record.Waiting)
		return true, nil
	}
	return true, endStoppedRun(ctx, config, services, hermes, envelope, run, view, runDir, logger)
}

// acknowledgeStop puts the one-line answer on the ticket and says whether
// it is there. The listing it is given is the one the stop was found in, so
// the marker check costs nothing: this engine's own answer is a comment on
// the same ticket, and finding it is what keeps a run whose record was lost
// from answering twice.
func acknowledgeStop(
	ctx context.Context,
	backlog operatorConfirmationSource,
	run state.RunOverview,
	comments []hook.BacklogComment,
	logger Logger,
) bool {
	if _, posted := commentIDWithMarker(comments, hook.StopAcknowledgedMarker(run.RunID)); posted {
		return true
	}
	if _, err := backlog.AddComment(ctx, run.IssueID, hook.StopAcknowledgedContent(run.RunID)); err != nil {
		logger.Error("the stop acknowledgement could not be posted; it is tried again next tick",
			"run", run.RunID, "error", err.Error())
		return false
	}
	logger.Info("the requester's stop was acknowledged on the ticket", "run", run.RunID)
	return true
}

// mergeCardInFlight names the delivery phase this run must let finish
// before it reports, or "" when there is none.
//
// Two phases change branches the destination keeps: the one that merges the
// pull request and deploys to staging, and the one that merges the release
// branch and promotes to production. Their merges land whether or not
// anything reads the result, so a run that reported while one was running
// would name a depth that changed a second later — the requester would be
// told a pull request was all that existed, and a deployment would start
// after they had been told it. Nothing else has that property: a card that
// implements, reviews, validates, publishes or waits for the CI leaves work
// this engine is free to drop on the floor.
//
// A card still waiting its turn has merged nothing, so it is retired like
// any other. Anything that is not plainly finished or plainly not started
// is treated as running, because the expensive mistake here is the one that
// reports over a merge.
func mergeCardInFlight(runDir, deliveryID string, board []runtime.BoardTask) string {
	cards := deliverCards(runDir, deliveryID, board)
	for _, stage := range []string{deliverStageIntegrate, deliverStagePromote} {
		card := cards[stage]
		if card == nil || deliverCardStopped(card) || card.Status == "todo" {
			continue
		}
		return stage
	}
	return ""
}

// endStoppedRun ends a stopped run as cancelled, carrying what actually
// landed, and retires every card the delivery had.
//
// What it may claim is decided exactly where a stop after a landing already
// decided it (deliver_depth.go): read the records the cards sealed, name a
// pull request or a staging landing when the records show one, claim
// nothing when they do not, and never undo what has already been deployed.
//
// The one thing added here is the depth. Turning those records into a claim
// needs the destination's configured depth, which the delivery seals just
// before it issues its first card — and a stop arriving before that has
// none, so a run whose pull request exists would report having reached
// nowhere. It is decided here in that case, from the same settings and by
// the same function the delivery itself uses, and only once the publish
// step has sealed its outcome: before that there is genuinely nothing to
// name.
func endStoppedRun(
	ctx context.Context,
	config runtime.Config,
	services *runtime.Services,
	hermes *runtime.Hermes,
	envelope hook.DispatchEnvelope,
	run state.RunOverview,
	view chainView,
	runDir string,
	logger Logger,
) error {
	if _, sealed := readDepthRecord(runDir); !sealed && deliverFileExists(runDir, runner.ChainOutcomeFile) {
		if decided, err := planDeliveryDepth(config, run, runDir); err != nil {
			logger.Error("the delivery depth could not be decided for a stopped run; the report claims only what it can prove",
				"run", run.RunID, "error", err.Error())
		} else {
			sealDepthRecord(runDir, decided, logger)
		}
	}
	return endDeliveryEarly(ctx, config, services, hermes, envelope, run, view, runDir, hook.TerminalCancelled, logger)
}

// queuedStopRequested reads 「停止」 for a run that has not been claimed.
//
// It is read before the operator's intake pause and before the budget and
// session holds, because none of those is a reason to keep a request its
// requester has withdrawn. Under a pause the run waited for an operator to
// resume intake; under a budget hold it waited for a limit to be raised,
// re-probing every ten minutes; in both cases a requester who had written
// 「停止」 watched their own request sit there. Nothing the holds are
// waiting for changes what a withdrawn request is worth.
//
// It is on the same interval as the claimed read and shares its record, so
// a hundred tickets queued behind a paused intake cost the tracker two
// listings a minute each rather than six every ten seconds.
//
// An unreadable listing is not a stop: the holds decide this pass and the
// question is asked again after the interval.
func queuedStopRequested(
	ctx context.Context,
	config runtime.Config,
	services *runtime.Services,
	run state.RunOverview,
	runDir string,
	logger Logger,
) bool {
	if services == nil || services.Backlog == nil {
		return false
	}
	record := readStopRead(runDir)
	if !record.stopped() {
		refreshStopRead(ctx, config, services.Backlog, run, &record, runDir, time.Now().UTC(), logger)
		if !record.stopped() {
			return false
		}
		logger.Info("a queued delivery was stopped by its requester; it is ended ahead of every hold on the intake",
			"run", run.RunID)
	}
	return true
}
