package attendant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
)

// The ladder.
//
// A card that failed used to be the end of the delivery: the tick read the
// sealed artifacts, chose an ending, posted it, and retired the chain. The
// morning after a night's work was a ticket saying the model had not
// answered, with nothing done and a person needed to start it again.
//
// Nothing about that failure was a decision that the delivery could not be
// carried out. A volume that filled, a binary that was not on the image, a
// registry that answered 502, a provider that gave up: each of them has a
// remedy, and none of the remedies is a person. So the tick reads what kind
// of thing went wrong — the card wrote it down — plays a hand it has not
// played for that kind, and dispatches the stage again.
//
// Two rules hold the whole thing up. The same hand is never played twice,
// which is what keeps this from being the loop it replaces; and the record
// of which hands have been played lives in the run directory, on the
// volume, so a pod that is replaced mid-climb resumes where the ladder had
// got to rather than starting again from the top.
//
// Waiting is the last rung and never the first. It is what is left when
// there is nothing to change, and it is not an ending either: the waits
// grow, the stage is dispatched again after each of them, and the delivery
// resumes the moment whatever was refusing stops refusing.

// The rungs, top to bottom. A rung is a kind of remedy; the hands below are
// the individual things that remedy can do.
const (
	// rungReclaim makes room on the volume.
	rungReclaim = 0
	// rungSeat moves a role to another model and another provider, and,
	// when the seat has nowhere left to move, asks the same occupant a
	// shorter way. Both are seat.go's; both are this rung, because they
	// are the two answers to one thing — the model will not answer.
	rungSeat = 1
	// rungRoute reaches the same thing by another route.
	rungRoute = 2
	// rungTool gets a missing tool from somewhere else.
	rungTool = 3
	// rungRound carries a refused verification into the next round. The
	// validate card's own path owns it, and the ladder is never consulted
	// for that class.
	rungRound = 4
	// rungWait waits, tells the operator once, and keeps trying.
	rungWait = 5
)

// ladderSchemaVersion is this record's shape.
const ladderSchemaVersion = 1

// maxLadderRecordBytes bounds the read. The record is a handful of short
// fields and a list of hands; anything larger is not one of ours.
const maxLadderRecordBytes = 64 * 1024

// ladderRecord is what one stage of one round has already tried.
//
// It is keyed by stage and round rather than by failure class on purpose. A
// stage that fails for a full volume and then, with room made, for a
// missing binary is one stage being got working, and the hands it has
// already spent are spent whichever class asked for them.
type ladderRecord struct {
	SchemaVersion int    `json:"schema_version"`
	Stage         string `json:"stage"`
	Round         int    `json:"round"`
	// Attempts counts the dispatches this ladder has asked for. It does not
	// count what the card did on its own: the kanban re-dispatches a card
	// once below its own failure threshold, before the card settles into
	// blocked and anything here sees it.
	Attempts int `json:"attempts"`
	// LadderStep is the rung the last hand came from, or the waiting rung
	// once the hands are spent.
	LadderStep int `json:"ladder_step"`
	// Tried names the hands already played, in the order they were played.
	// The waiting rung is deliberately absent from it: waiting is the one
	// hand that is meant to be played again.
	Tried      []string  `json:"tried,omitempty"`
	LastAt     time.Time `json:"last_at"`
	LastReason string    `json:"last_reason,omitempty"`
	// Interruptions counts the times this card was stopped rather than
	// having failed. They are recorded and not counted: a rolling restart
	// says nothing about the model, the network or the volume, and the
	// stage is simply dispatched again.
	Interruptions int `json:"interruptions,omitempty"`
	// NoticedSteps are the rungs whose one notice is already on the ticket,
	// so a wait that lasts hours reads the ticket once rather than once a
	// tick.
	NoticedSteps []int `json:"noticed_steps,omitempty"`
	// LastStopReadAt is when the ticket was last read for a 「停止」 while
	// this stage was only waiting. The chain loop passes every few seconds,
	// and a stage can wait half an hour; without this the tracker would be
	// listed six times a minute per waiting card, all night, for every
	// delivery at once.
	LastStopReadAt time.Time `json:"last_stop_read_at"`
	// LoggedNoTracker records that this stage has already said, once, that
	// the delivery has no tracker to speak to. Without it a silent climb
	// would repeat the line every tick for as long as it lasted, which is
	// the shape an operator learns to scroll past.
	LoggedNoTracker bool `json:"logged_no_tracker,omitempty"`
}

// ladderHand is one thing the ladder can do about one kind of failure.
type ladderHand struct {
	step int
	name string
	// reclaim, when set, is work done before the stage is dispatched again.
	// A hand without one is the dispatch itself.
	reclaim func(ctx context.Context, climb ladderClimb) error
}

// ladderClimb is one failed card and everything the climb needs to do
// something about it. The tracker arrives as an interface rather than
// through the services, the way every other notice in this package takes
// it: what it does — read the ticket, put one comment on it — is measurable
// without a tracker, and the one thing worth measuring here is that it is
// done once.
type ladderClimb struct {
	config   runtime.Config
	services *runtime.Services
	tracker  operatorConfirmationSource
	hermes   *runtime.Hermes
	envelope hook.DispatchEnvelope
	run      state.RunOverview
	view     chainView
	plan     runtime.ChainPlan
	stage    string
	runDir   string
	logger   Logger
	// rebuild replaces the chain rebuild below for a card that is not part
	// of the chain. The delivery cards — the ones that merge, wait for the
	// workflow and observe a screen — are dispatched outside the chain's
	// namespace and have no stage after them to retire, so they bring their
	// own way of being built again. Nil means the chain's own.
	rebuild func(ctx context.Context, climb ladderClimb) (ladderVerdict, error)
}

// ladderHands are the hands for one kind of failure, in the order they are
// played. An empty list descends straight to the waiting rung.
//
// Each hand has to be a different hand. Dispatching the same stage against
// the same model with the same prompt is the loop this replaces, so a model
// that will not answer is moved to another seat, and when the seat has run
// out of occupants the instruction is rebuilt before anybody waits.
//
// The model hands depend on the delivery, not only on the class: which
// seats a role has is the consumer's to configure, and whether a candidate
// may be taken depends on where the other review seat is sitting at this
// moment. The rest are the same hands for every delivery.
func ladderHands(class runner.FailureClass, climb ladderClimb) []ladderHand {
	switch class {
	case runner.FailureClassModel:
		return modelHands(climb)
	case runner.FailureClassDisk:
		return []ladderHand{
			// Other deliveries' leavings before this delivery's own. The
			// finished runs' copies of the destination are worth gigabytes
			// each and nothing will ever read them again; the sandbox below
			// belongs to a delivery that is still running and costs it a
			// fresh clone.
			{step: rungReclaim, name: "reclaim:finished-runs", reclaim: reclaimFinishedRuns},
			{step: rungReclaim, name: "reclaim:own-sandbox", reclaim: reclaimOwnSandbox},
		}
	case runner.FailureClassNetwork:
		// One more attempt over a route made from scratch. A process that
		// starts again resolves the name again and opens new connections,
		// which is the whole remedy for a reset connection or a registry
		// that answered 502 once. Alternate sources for the destination and
		// mirrors for the toolchain are more hands for this rung, and go in
		// this list beside it.
		return []ladderHand{{step: rungRoute, name: "route:fresh-card"}}
	case runner.FailureClassTool:
		// The same, for a tool that was not there: the stage installs what
		// it needs at the top of its own run, so a dispatch from scratch is
		// an install from scratch. Other sources to install from are more
		// hands for this rung.
		return []ladderHand{{step: rungTool, name: "tool:fresh-card"}}
	default:
		// A key that has reached its limit — no seat could help, every one
		// of them is reached through that key — and a failure nobody could
		// name. Both wait.
		//
		// A refused verification is here too, and never arrives: it is the
		// validate card's own answer about the change, and the round it
		// belongs to carries what was printed into the next one instead of
		// ever reaching the ladder. Should a record leave it stranded here
		// anyway, waiting and dispatching the card again is harmless — the
		// card resumes behind its own sealed decision.
		return nil
	}
}

// ladderVerdict is what the ladder did with a failed card.
type ladderVerdict int

const (
	// ladderHandled: the stage was dispatched again, or the ladder is
	// waiting to dispatch it. Either way there is nothing to report.
	ladderHandled ladderVerdict = iota
	// ladderStopped: the requester asked the run to stop while the ladder
	// was climbing. The caller ends the run as cancelled.
	ladderStopped
	// ladderSpent: an operator configured a limit on the attempts and it
	// has been reached. The caller ends the run the way it would have
	// before the ladder existed.
	ladderSpent
)

// newClimb gathers one failed card's context. The tracker comes out of the
// services here, at the one place that has both, so that everything below
// takes it as the narrow thing it uses.
func newClimb(
	config runtime.Config,
	services *runtime.Services,
	hermes *runtime.Hermes,
	envelope hook.DispatchEnvelope,
	run state.RunOverview,
	view chainView,
	plan runtime.ChainPlan,
	stageName string,
	logger Logger,
) ladderClimb {
	climb := ladderClimb{
		config: config, services: services, hermes: hermes, envelope: envelope,
		run: run, view: view, plan: plan, stage: stageName,
		runDir: runDirectory(config, run.DeliveryID), logger: logger,
	}
	if services != nil && services.Backlog != nil {
		climb.tracker = services.Backlog
	}
	return climb
}

// climbLadder decides what happens to one failed card and does it.
//
// The class comes from the card's own sealed account of its failure. A card
// that sealed nothing — one dispatched by an older engine, one that died
// before it could write — is treated as a failure nobody could name, which
// waits and tries again. That is the right answer for an unreadable failure
// and a far better one than ending the delivery over it.
func climbLadder(ctx context.Context, climb ladderClimb) (ladderVerdict, error) {
	config, run, stageName, logger := climb.config, climb.run, climb.stage, climb.logger
	runDir := climb.runDir
	round := stageRound(climb.view, stageName)
	if round < 1 {
		return ladderSpent, nil
	}
	failure, sealed := runner.ReadStageFailure(runDir, stageName, round)
	class := runner.FailureClassUnknown
	if sealed {
		class = failure.Class
	}
	record := readLadderRecord(runDir, stageName, round)
	now := time.Now().UTC()

	// A delivery whose ticket cannot be reached at all. The ladder can
	// neither say that it is still going nor hear a stop, so it would climb
	// on in silence for as long as it took; said once per stage, an operator
	// can see which deliveries are in that position and why nothing arrives
	// on their tickets.
	if climb.tracker == nil && !record.LoggedNoTracker {
		logger.Error("no tracker is configured for this delivery: the ladder cannot tell the ticket it is still going, and cannot hear a stop",
			"run", run.RunID, "stage", stageName, "round", round)
		record.LoggedNoTracker = true
		writeLadderRecord(runDir, stageName, round, record, logger)
	}

	// A card that was stopped rather than one that failed. The pod was
	// being replaced; nothing was learnt about the model, the route or the
	// volume, so nothing is spent and the stage is simply dispatched again.
	// Counted as a model failure it would walk the delivery down the
	// remedies for one, a rung at a time, over a rolling restart.
	if sealed && failure.Interrupted {
		record.Interruptions++
		record.LastAt = now
		record.LastReason = "the card was interrupted"
		writeLadderRecord(runDir, stageName, round, record, logger)
		logger.Info("the card was interrupted rather than failed; the stage is dispatched again",
			"run", run.RunID, "stage", stageName, "round", round, "interruptions", record.Interruptions)
		return dispatchAgain(ctx, climb)
	}

	if limit := config.Chain.RetryMaxAttempts; limit > 0 && record.Attempts >= limit {
		logger.Info("the configured limit on attempts for this stage is reached; the delivery ends on the failure",
			"run", run.RunID, "stage", stageName, "round", round, "attempts", record.Attempts, "limit", limit)
		return ladderSpent, nil
	}

	hand, found := nextHand(class, climb, record.Tried)
	if !found {
		return waitRung(ctx, climb, class, record, now)
	}
	// The record is written before the hand is played, so a pod that stops
	// between the two comes back having spent the hand rather than about to
	// play it a second time.
	record.Attempts++
	record.LadderStep = hand.step
	record.Tried = append(record.Tried, hand.name)
	record.LastAt = now
	record.LastReason = string(class)
	writeLadderRecord(runDir, stageName, round, record, logger)
	if hand.reclaim != nil {
		if err := hand.reclaim(ctx, climb); err != nil {
			// The hand is spent either way. What it was trying to free may
			// have been freed by the sweep the reception runs anyway, and
			// the next hand is a different one; stopping here would leave
			// the delivery on a blocked card with nothing driving it.
			logger.Error("a ladder hand did not finish; the stage is dispatched anyway",
				"run", run.RunID, "stage", stageName, "hand", hand.name, "error", err.Error())
		}
	}
	logger.Info("a failed card is climbed rather than reported",
		"run", run.RunID, "stage", stageName, "round", round, "class", string(class),
		"ladder_step", hand.step, "hand", hand.name, "attempts", record.Attempts)
	return dispatchAgain(ctx, climb)
}

// waitRung is the bottom of the ladder: there is nothing left to change, so
// the stage is dispatched again after a wait that grows, for as long as it
// takes. The ticket is told once — for sharing, not for an answer — and the
// delivery resumes by itself the moment the attempt succeeds.
func waitRung(ctx context.Context, climb ladderClimb, class runner.FailureClass, record ladderRecord, now time.Time) (ladderVerdict, error) {
	config, run, stageName, logger := climb.config, climb.run, climb.stage, climb.logger
	round := record.Round
	entering := record.LadderStep != rungWait
	record.LadderStep = rungWait
	record.LastReason = string(class)
	if entering {
		// The wait starts now. Dispatching immediately on arriving here
		// would spend an attempt on the refusal that just happened, which
		// is the one moment it is certain to meet it again.
		record.LastAt = now
		// And the stop is read on the way in. A delivery that reaches the
		// waiting rung on the first tick after its card failed — a key at
		// its limit does — would otherwise not be asked again until the
		// first dispatch, which the longest waits put half an hour away.
		stopped, _ := stopAskedWhileWaiting(ctx, climb, &record, now)
		writeLadderRecord(climb.runDir, stageName, round, record, logger)
		if stopped {
			return ladderStopped, nil
		}
		record = noticeLadderWait(ctx, climb, class, record)
		writeLadderRecord(climb.runDir, stageName, round, record, logger)
		logger.Info("the ladder has nothing left to change; the stage waits and is dispatched again",
			"run", run.RunID, "stage", stageName, "round", round, "class", string(class),
			"wait", ladderWait(config.Chain, record).String())
		return ladderHandled, nil
	}
	if updated := noticeLadderWait(ctx, climb, class, record); updated.told(rungWait) != record.told(rungWait) {
		record = updated
		writeLadderRecord(climb.runDir, stageName, round, record, logger)
	}
	if now.Before(record.LastAt.Add(ladderWait(config.Chain, record))) {
		// Still waiting. This is the only thing the tick does for this
		// card, and it is throttled: the loop passes every few seconds and
		// the wait can be half an hour.
		if stopped, read := stopAskedWhileWaiting(ctx, climb, &record, now); read {
			writeLadderRecord(climb.runDir, stageName, round, record, logger)
			if stopped {
				return ladderStopped, nil
			}
		}
		return ladderHandled, nil
	}
	record.Attempts++
	record.LastAt = now
	writeLadderRecord(climb.runDir, stageName, round, record, logger)
	logger.Info("the wait is over; the stage is dispatched again",
		"run", run.RunID, "stage", stageName, "round", round, "class", string(class), "attempts", record.Attempts)
	return dispatchAgain(ctx, climb)
}

// stopAsked reports whether the requester has asked this run to stop.
//
// A tracker that cannot be read is not a stop, and neither is one that was
// never configured. The engine's whole purpose is to keep going, so an
// outage answers "no" and the question is asked again next tick.
func stopAsked(ctx context.Context, climb ladderClimb) bool {
	if climb.tracker == nil {
		return false
	}
	stopped, err := stopRequested(ctx, climb.tracker, climb.config.Tracker.AllowedCreatorID, climb.envelope.Snapshot.IssueID)
	if err != nil {
		climb.logger.Error("the stop check could not be read; the climb continues",
			"run", climb.run.RunID, "stage", climb.stage, "error", err.Error())
		return false
	}
	if stopped {
		climb.logger.Info("the requester asked the run to stop while the ladder was climbing",
			"run", climb.run.RunID, "stage", climb.stage)
	}
	return stopped
}

// stopReadInterval is how often a stage that is only waiting reads its
// ticket for a 「停止」.
//
// The chain loop passes every ten seconds, and the waits it passes over
// reach half an hour, so an unthrottled read would list the tracker six
// times a minute for every waiting card at once — a night of deliveries
// held behind one spent key would spend it on nothing else. A minute is
// the longest a requester should wait to be obeyed and the shortest that
// costs the tracker nothing to speak of.
const stopReadInterval = time.Minute

// stopAskedWhileWaiting reads the stop for a stage that is only waiting,
// at most once per stopReadInterval, and says whether it read at all so
// the caller knows whether the record is worth writing.
//
// The tick that actually dispatches does not come through here: it reads
// without a throttle, because that read is the last thing between a
// requester who asked to stop and a card that spends money.
func stopAskedWhileWaiting(ctx context.Context, climb ladderClimb, record *ladderRecord, now time.Time) (stopped, read bool) {
	if !record.LastStopReadAt.IsZero() && now.Before(record.LastStopReadAt.Add(stopReadInterval)) {
		return false, false
	}
	record.LastStopReadAt = now
	return stopAsked(ctx, climb), true
}

// ladderWait is how long before the next attempt: the first wait doubled
// once for each wait already spent, up to the longest.
//
// The attempts spent on the hands above do not lengthen it. They were
// different remedies, not repetitions, and starting the first wait at
// half an hour because two directories were swept would be the ladder
// punishing itself for having worked.
func ladderWait(chain runtime.ChainConfig, record ladderRecord) time.Duration {
	base, longest := chain.RetryBackoffBase(), chain.RetryBackoffMax()
	waits := record.Attempts - len(record.Tried)
	if waits < 0 {
		waits = 0
	}
	wait := base
	for range waits {
		if wait >= longest/2 {
			return longest
		}
		wait *= 2
	}
	if wait > longest {
		return longest
	}
	return wait
}

// nextHand is the first hand for this kind of failure that has not been
// played. No hand left means the waiting rung.
func nextHand(class runner.FailureClass, climb ladderClimb, tried []string) (ladderHand, bool) {
	for _, hand := range ladderHands(class, climb) {
		if !slices.Contains(tried, hand.name) {
			return hand, true
		}
	}
	return ladderHand{}, false
}

// dispatchAgain archives the failed stage and everything after it, then
// builds the missing cards back.
//
// Only from the failed stage onward. The stages before it finished, and
// their records — the sealed candidate, the sealed reviews — are what the
// stage being dispatched again reads; archiving them would throw away the
// round's work and pay for all of it a second time.
//
// Everything after it has to go, though, and that is not an optimisation.
// The kanban treats an archived parent as satisfied, so a stage rebuilt
// underneath a card that is still waiting would be handed straight to
// dispatch and run beside the chain's living remainder on the one run
// directory they share. Archiving is also the only thing that releases a
// card's idempotency key, so a stage cannot be built again until its old
// card is archived.
func dispatchAgain(ctx context.Context, climb ladderClimb) (ladderVerdict, error) {
	config, run, stageName, logger := climb.config, climb.run, climb.stage, climb.logger
	// 「停止」 is read here without a throttle. This is the last thing between
	// a requester who has asked the delivery to stop and a card that starts
	// spending again, and a dispatch is rare enough — once per hand, then
	// once per wait — that reading it every time costs nothing. A stage that
	// is only waiting reads it on its own slower clock.
	if stopAsked(ctx, climb) {
		return ladderStopped, nil
	}
	if climb.rebuild != nil {
		return climb.rebuild(ctx, climb)
	}
	stages := runtime.ChainStagesFor(config.Chain, climb.plan)
	from := -1
	for index, stage := range stages {
		if stage.Name == stageName {
			from = index
			break
		}
	}
	if from < 0 {
		return ladderSpent, fmt.Errorf("stage %s is not in this delivery's chain", stageName)
	}
	rounds := climb.view.rounds()
	if rounds.Design == 0 {
		rounds.Design = 1
	}
	if rounds.Implement == 0 {
		rounds.Implement = 1
	}
	existing := climb.view.existingKeys(run.DeliveryID)
	archived := make([]string, 0, len(stages)-from)
	for _, stage := range stages[from:] {
		key := runtime.ChainCardKey(run.DeliveryID, stage.Name, roundOf(rounds, stage.Name))
		task, live := existing[key]
		if !live {
			continue
		}
		if err := climb.hermes.Archive(ctx, task.ID); err != nil {
			return ladderHandled, err
		}
		delete(existing, key)
		archived = append(archived, stage.Name)
	}
	terminal, err := runtime.EnsureChainFor(ctx, climb.hermes, config.Chain, climb.plan, existing,
		run.DeliveryID, run.RunID, run.Summary, rounds)
	if err != nil {
		return ladderHandled, err
	}
	logger.Info("the failed stage and the ones after it were rebuilt",
		"run", run.RunID, "stage", stageName, "archived", archived, "terminal_card", terminal)
	return ladderHandled, nil
}

// roundOf is the round one stage counts in. Design stages keep their own
// count, because a design can be started again without an implementation
// having happened.
func roundOf(rounds runtime.ChainRounds, stageName string) int {
	if runtime.IsDesignStage(stageName) {
		return rounds.Design
	}
	return rounds.Implement
}

// stageRound is the round the board says this stage's card belongs to.
func stageRound(view chainView, stageName string) int {
	if runtime.IsDesignStage(stageName) {
		return view.designRound
	}
	return view.round
}

// ladderOwns reports whether an ending the old classification chose is one
// the ladder now takes instead.
//
// These three said the same thing in three ways: something broke and the
// delivery is over. None of them was ever a decision about the request, so
// no failed card is reported under them any more.
//
// That is the reporting path only, and two regenerating ones still end a
// delivery under two of these codes. A revise that meets the record ceiling,
// or an operator's own round limit, reports the code its classification
// carried, which for a revise is the reviews not having converged
// (chains.go, the revise arm of classifyChainFailure). And a design-backed
// round whose sealed review cannot be read reports the internal failure
// (chains_design.go, unreadableReviewsOutcome) in the one case left to it:
// a delivery with no seat to ask again — none configured, or a
// configuration that will not read. A record that names its seat is that
// seat's own failure to leave a usable answer and climbs this ladder like
// any other. Both reach the report through actionRegenerate, which is why
// neither passes this gate.
//
// The codes stay in the vocabulary either way: ledger rows and comments
// already posted name all three.
func ladderOwns(code hook.TerminalCode) bool {
	switch code {
	case hook.TerminalModelFailed, hook.TerminalInternalFailed, hook.TerminalReleaseFailed:
		return true
	default:
		return false
	}
}

// told reports whether the one notice for a rung is already on the ticket.
func (r ladderRecord) told(step int) bool { return slices.Contains(r.NoticedSteps, step) }

// noticeLadderWait puts one comment on the ticket saying the delivery is
// still going and what it is waiting on, and returns the record with that
// said.
//
// For sharing only. Nothing waits for a reply, nothing is asked of the
// requester, and the delivery is not held: the stage is dispatched again
// after every wait whether anybody reads this or not. Two things make it
// worth posting at all — a delivery that has been inside one stage for
// half an hour looks stopped from the outside, and a key that has reached
// its limit will not come back until a person raises it.
//
// Exactly once per run, per stage, per rung. The record says whether it has
// been said, so a wait lasting hours costs the tracker nothing after the
// first pass; the ticket itself is searched for the marker before posting,
// so a record lost with a pod does not produce a second comment.
func noticeLadderWait(ctx context.Context, climb ladderClimb, class runner.FailureClass, record ladderRecord) ladderRecord {
	config, run, stageName, logger := climb.config, climb.run, climb.stage, climb.logger
	if record.told(rungWait) || climb.tracker == nil {
		return record
	}
	// A key that has reached its limit is told at once, because no amount
	// of waiting raises it and every other seat is reached through the same
	// key. Everything else is given the attempts an operator allowed it
	// before anybody is told: most of what lands here clears itself.
	if class != runner.FailureClassCredit && record.Attempts < config.Chain.RetryNoticeAttemptsValue() {
		return record
	}
	content := hook.LadderWaitContent(run.RunID, stageName)
	if class == runner.FailureClassCredit {
		content = hook.KeyLimitReachedContent(run.RunID, stageName)
	}
	marker := hook.LadderNoticeMarker(run.RunID, stageName, hook.LadderWaitRung)
	comments, err := climb.tracker.ListComments(ctx, climb.envelope.Snapshot.IssueID, 0)
	if err != nil {
		logger.Error("ladder notice: comment listing failed", "run", run.RunID, "stage", stageName, "error", err.Error())
		return record
	}
	if _, posted := commentIDWithMarker(comments, marker); !posted {
		if _, err := climb.tracker.AddComment(ctx, climb.envelope.Snapshot.IssueID, content); err != nil {
			logger.Error("ladder notice: post failed", "run", run.RunID, "stage", stageName, "error", err.Error())
			return record
		}
		logger.Info("ladder notice: the ticket was told the delivery is still going",
			"run", run.RunID, "stage", stageName, "class", string(class))
	}
	record.NoticedSteps = append(record.NoticedSteps, rungWait)
	return record
}

// ladderRecordFile is where a stage's climb is written down: under the run
// directory, which is the persistent volume, so the record outlives the pod
// that made it and a delivery resumes its climb where it left off.
func ladderRecordFile(runDir, stage string, round int) string {
	return filepath.Join(runDir, "retry", fmt.Sprintf("%s-r%d.json", stage, round))
}

// readLadderRecord reads back what this stage has tried. A record that is
// missing, too large, unreadable, or bound to another stage or round reads
// as a climb that has not started: the ladder then begins at the top, which
// costs at most one hand replayed and never leaves a delivery stuck.
func readLadderRecord(runDir, stage string, round int) ladderRecord {
	fresh := ladderRecord{SchemaVersion: ladderSchemaVersion, Stage: stage, Round: round}
	encoded, err := os.ReadFile(ladderRecordFile(runDir, stage, round))
	if err != nil || len(encoded) > maxLadderRecordBytes {
		return fresh
	}
	var record ladderRecord
	if json.Unmarshal(encoded, &record) != nil {
		return fresh
	}
	if record.SchemaVersion != ladderSchemaVersion || record.Stage != stage || record.Round != round {
		return fresh
	}
	return record
}

// writeLadderRecord seals the climb so far, with the same manners the run
// directory's other attendant records are written with: the directory is
// traversable but not listable, the file is this user's alone, and a link
// left at the path must not carry the write somewhere else.
//
// A failure to write is logged and nothing else. The alternative is a
// delivery that stops because its bookkeeping stopped, and the worst a lost
// record costs is one hand played twice.
func writeLadderRecord(runDir, stage string, round int, record ladderRecord, logger Logger) {
	record.SchemaVersion = ladderSchemaVersion
	record.Stage = stage
	record.Round = round
	encoded, err := json.Marshal(record)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(ladderRecordFile(runDir, stage, round)), 0o711)
	}
	if err == nil {
		path := ladderRecordFile(runDir, stage, round)
		if err = os.Remove(path); err != nil && errors.Is(err, os.ErrNotExist) {
			err = nil
		}
		if err == nil {
			err = os.WriteFile(path, encoded, 0o600)
		}
	}
	if err != nil {
		logger.Error("the ladder record could not be written", "stage", stage, "round", round, "error", err.Error())
	}
}
