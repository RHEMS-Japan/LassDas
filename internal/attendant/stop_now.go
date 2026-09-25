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

// stopRequestFile is where a claimed run records that its requester asked
// it to stop. It lives on the volume beside the run's other records, so a
// pod replaced between reading the stop and ending the run comes back
// knowing the run was stopped rather than resuming the work.
const stopRequestFile = "stop-requested.json"

// stopRequestSchemaVersion is this record's shape.
const stopRequestSchemaVersion = 1

// maxStopRequestBytes bounds the read. The record is four short fields;
// anything larger is not one of ours.
const maxStopRequestBytes = 16 * 1024

// stopRequestRecord is the run's own account of the stop: when this engine
// first saw it, whether the requester has been answered, and which step —
// if any — the ending is waiting on.
type stopRequestRecord struct {
	SchemaVersion int       `json:"schema_version"`
	At            time.Time `json:"at"`
	// Acknowledged says the one-line answer is on the ticket. Kept here as
	// well as on the ticket because the ticket read that would prove it is
	// the same listing that finds the stop, and a tick whose listing failed
	// must not post a second answer on the strength of not having looked.
	Acknowledged bool `json:"acknowledged"`
	// Waiting names the delivery phase whose merge is in flight, which the
	// run lets finish before it reports. Empty when nothing had to finish.
	Waiting string `json:"waiting,omitempty"`
}

func readStopRequest(runDir string) stopRequestRecord {
	encoded, err := os.ReadFile(filepath.Join(runDir, stopRequestFile))
	if err != nil || len(encoded) > maxStopRequestBytes {
		return stopRequestRecord{}
	}
	var record stopRequestRecord
	if json.Unmarshal(encoded, &record) != nil || record.SchemaVersion != stopRequestSchemaVersion {
		return stopRequestRecord{}
	}
	return record
}

// writeStopRequest seals the record. Best-effort like every other record
// this package writes: a volume that refuses the write must not be the
// reason a requester who asked to stop is not obeyed. What it costs is one
// repeated acknowledgement after a pod restart, which the ticket's own
// marker then catches.
func writeStopRequest(runDir string, record stopRequestRecord, logger Logger) {
	record.SchemaVersion = stopRequestSchemaVersion
	encoded, err := json.Marshal(record)
	if err == nil {
		err = os.WriteFile(filepath.Join(runDir, stopRequestFile), encoded, 0o600)
	}
	if err != nil {
		logger.Error("the stop request could not be recorded; the run is stopped anyway",
			"error", err.Error())
	}
}

// honourStopNow is the first thing a claimed run's tick does. It reports
// whether it handled the run, in which case nothing else in the tick may
// touch it: no healing, no report, no dispatch.
//
// The ticket is listed once and answers two questions — has the requester
// asked to stop, and is this engine's answer to that already posted — so
// the read costs one request per claimed run per pass, which is what the
// round boundaries were already costing whenever they were reached.
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
	comments, err := services.Backlog.ListComments(ctx, run.IssueID, 0)
	if err != nil {
		logger.Error("the stop could not be read this tick; the delivery goes on and is asked again next tick",
			"run", run.RunID, "error", err.Error())
		return false, nil
	}
	if !containsStopComment(comments, config.Tracker.AllowedCreatorID) {
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

	record := readStopRequest(runDir)
	if record.At.IsZero() {
		record.At = time.Now().UTC()
	}
	record.Waiting = mergeCardInFlight(runDir, run.DeliveryID, view.board)
	// Written before the ticket is answered and before anything is retired:
	// of the three, this is the one that survives the process.
	writeStopRequest(runDir, record, logger)

	if !record.Acknowledged && acknowledgeStop(ctx, services.Backlog, run, comments, logger) {
		record.Acknowledged = true
		writeStopRequest(runDir, record, logger)
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
// It is not throttled. The queued population is whatever the holds are
// holding, and the delay a throttle would buy is exactly the delay this
// exists to remove; if the tracker read ever needs rationing, it belongs on
// the budget hold's own ten-minute clock rather than on the stop.
//
// An unreadable listing is not a stop: the holds decide this pass and the
// question is asked again on the next one.
func queuedStopRequested(
	ctx context.Context,
	config runtime.Config,
	services *runtime.Services,
	run state.RunOverview,
	logger Logger,
) bool {
	if services == nil || services.Backlog == nil {
		return false
	}
	stopped, err := stopRequested(ctx, services.Backlog, config.Tracker.AllowedCreatorID, run.IssueID)
	if err != nil {
		logger.Error("the stop could not be read for a queued delivery; the holds decide this tick",
			"run", run.RunID, "error", err.Error())
		return false
	}
	if stopped {
		logger.Info("a queued delivery was stopped by its requester; it is ended ahead of every hold on the intake",
			"run", run.RunID)
	}
	return stopped
}
