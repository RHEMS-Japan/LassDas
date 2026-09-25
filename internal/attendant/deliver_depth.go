package attendant

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
	"automation.internal/ticket-ingress/internal/worker"
)

// The delivery, driven from inside the run.
//
// The cards after the pull request used to run behind a delivery that had
// already been declared a success: the run reported that the change was
// proposed, went terminal, and only then did anything merge, deploy or get
// looked at. Whatever happened afterwards could no longer change what the
// ticket had been told, so a screen that never showed the change and a
// deployment that never started were footnotes to a success.
//
// They run inside the delivery now. The run stays claimed while the change
// is merged, the workflow is waited for, staging is observed, production is
// promoted and observed — and the success is reported at the end, carrying
// the evidence of wherever the delivery actually got to. Nothing between
// the pull request and production waits for a person unless the operator
// asked for a look (go_gate: required); a phase that did not do what it
// promised is a failure like any other, and the ladder takes it.

// deliverProgress is what one pass over a delivery's cards found.
type deliverProgress int

const (
	// deliverWorking: a card is running, or one was just issued, or the
	// ladder is waiting to dispatch one again. Nothing to report.
	deliverWorking deliverProgress = iota
	// deliverReached: the delivery is as deep as it is going to get, and
	// the success report can be written with the evidence of that depth.
	deliverReached
	// deliverStopped: the requester asked the delivery to stop.
	deliverStopped
	// deliverSpent: an operator configured a limit on the attempts and a
	// phase has reached it, so the delivery ends on the failure instead.
	deliverSpent
)

// The delivery cards, and the round their records are kept under. The names
// and the round come from the chain package so that the card which seals an
// account of its own failure and the tick which reads it cannot drift
// apart: they are the same two values on both sides.
const (
	deliverStageChecks    = runtime.DeliverStageChecks
	deliverStageIntegrate = runtime.DeliverStageIntegrate
	deliverStagePromote   = runtime.DeliverStagePromote
	deliverLadderRound    = runtime.DeliverRound
)

// deliverCardDied is what a delivery card that stopped without sealing
// anything is climbed as. A card that left no record says nothing about
// what went wrong, so it is the unnamed failure — which the ladder waits on
// and dispatches again, the right answer for a pod replaced mid-observation.
const deliverCardDied = "card_failed"

// deliverConfigured is the half of the old gate that still holds while a
// delivery is running: the cards are configured and this delivery was
// claimed after the operator's cut-off. The other half — that the run had
// already succeeded — is what this change removes, because the success now
// comes after these cards rather than before them.
//
// Fails closed on anything unparsable: turning the feature on must never
// reach back through deliveries claimed before it existed.
func deliverConfigured(chain runtime.ChainConfig, run state.RunOverview) bool {
	if !chain.Deliver.Enabled() {
		return false
	}
	enabledAfter, err := chain.Deliver.EnabledAfterTime()
	if err != nil || run.ClaimedAt <= 0 || run.ClaimedAt < enabledAfter.UnixMilli() {
		return false
	}
	return true
}

// deliverCards finds this delivery's live card for each phase. A phase the
// ladder has dispatched again carries its attempt in the key, so the card
// looked for is the one belonging to the attempt the record is on: the
// board keeps archived cards in its listing, and two cards sharing a key
// could not be told apart.
func deliverCards(runDir, deliveryID string, tasks []runtime.BoardTask) map[string]*runtime.BoardTask {
	cards := map[string]*runtime.BoardTask{}
	for _, stage := range []string{deliverStageChecks, deliverStageIntegrate, deliverStagePromote} {
		key := deliverCardKeyAt(deliveryID, stage, deliverAttempt(runDir, stage))
		for index := range tasks {
			if tasks[index].IdempotencyKey == key {
				cards[stage] = &tasks[index]
				break
			}
		}
	}
	return cards
}

// deliverAttempt is which attempt at a delivery phase is the current one:
// the first, plus one for every dispatch the ladder has asked for.
func deliverAttempt(runDir, stage string) int {
	return readLadderRecord(runDir, stage, deliverLadderRound).Attempts + 1
}

// advanceDelivery moves one claimed delivery's cards by at most one step,
// and says whether the configured depth has been reached.
//
// The deepest sealed record decides, so a tick arriving after two phases
// finished does not re-run the first. Every phase ends one of three ways:
// it did what it promised, it reached a stopping point that is legitimate,
// or it did neither and the ladder takes it.
func advanceDelivery(
	ctx context.Context,
	config runtime.Config,
	services *runtime.Services,
	hermes *runtime.Hermes,
	envelope hook.DispatchEnvelope,
	run state.RunOverview,
	view chainView,
	plan runtime.ChainPlan,
	depth depthPlan,
	tasks []runtime.BoardTask,
	logger Logger,
) (deliverProgress, error) {
	runDir := runDirectory(config, run.DeliveryID)
	if !deliverFileExists(runDir, "feature-pr.json") {
		// The publish card finished and sealed no pull request. There is
		// nothing to merge and nothing to observe, so this delivery is as
		// deep as it goes; the report gate refuses a success with no
		// evidence, which is the honest end for it.
		return deliverReached, nil
	}
	// Nothing writes the destination's configuration here, and this is the
	// seam where something one day will. Past the pull request is the one
	// window it could be written in — from here the cards re-verify their
	// sealed records under the digest the pull request recorded, so a
	// configuration that moved now could not make them unreadable and does
	// not restart this delivery (chains.go's exemption).
	//
	// What would be written is the staging digest-commit policy, and it is
	// not written because the engine cannot yet make it true. That policy
	// says which files the deployment's own commit modifies, with which
	// message and by whom, and the promotion holds the real commit to it
	// exactly (internal/githubapi/waits.go's digest comparison). Nothing
	// asks the implementer to make such a commit, so a policy written here
	// would describe a commit that never happens, and the destination would
	// stop short of production for ever afterwards. Writing it belongs with
	// the change that has the round build the digest-commit step itself:
	// the workflow committing the image digest into named files with that
	// exact prefix and actor, the policy naming THOSE files, the
	// instruction asking for it, and contents: write admitted by the
	// destination's policy for that step alone. Until then the setting is
	// reported by name as one this delivery did not apply.
	cards := deliverCards(runDir, run.DeliveryID, tasks)
	climb := func(stage, verdict string) (deliverProgress, error) {
		return climbDeliverPhase(ctx, config, services, hermes, envelope, run, view, plan, stage, verdict, cards, logger)
	}
	// Nothing is projected onto the board here. The projection reads the
	// delivery through classifyAfterTerminal, which has no terminal code to
	// read while the run is still claimed and calls that 「失敗で終了」 — so a
	// healthy production delivery would sit on the board as needing
	// attention from the tick that posted its staging report until the tick
	// that ended it, which is every hour the promotion takes. The end is
	// projected once the run has one, on the terminal side (syncDeliver).

	if report, err := readDeliverReport(runDir, runner.DeliverProductionReportFile); err == nil {
		if report.Verdict != "pass" {
			return climb(deliverStagePromote, report.Verdict)
		}
		posted, err := services.Tick.ReleaseReportPosted(ctx, run.RunID)
		if err != nil {
			return deliverWorking, err
		}
		if !posted {
			return deliverWorking, reportDeliverRelease(ctx, services, hermes, run, runDir, cards, logger)
		}
		return deliverReached, nil
	}
	if card := cards[deliverStagePromote]; card != nil {
		if deliverCardStopped(card) {
			return climb(deliverStagePromote, deliverCardDied)
		}
		return deliverWorking, nil
	}
	// A release report with no production record behind it is the Go wait
	// having ended without a promotion: the requester stopped it, or the
	// window expired. Either way the delivery goes no deeper.
	if posted, err := services.Tick.ReleaseReportPosted(ctx, run.RunID); err != nil {
		return deliverWorking, err
	} else if posted {
		return deliverReached, nil
	}

	if _, err := readDeliverReport(runDir, runner.DeliverStagingReportFile); err == nil {
		return advancePastStaging(ctx, config, services, hermes, run, runDir, depth, cards, climb, logger)
	}
	if card := cards[deliverStageIntegrate]; card != nil {
		if deliverCardStopped(card) {
			return climb(deliverStageIntegrate, deliverCardDied)
		}
		return deliverWorking, nil
	}
	if deliverFileExists(runDir, runner.DeliverChecksFile) {
		return issueIntegrateCard(ctx, config, services, hermes, run, runDir, cards, logger)
	}
	if card := cards[deliverStageChecks]; card != nil {
		if deliverCardStopped(card) {
			return climb(deliverStageChecks, deliverCardDied)
		}
		return deliverWorking, nil
	}
	return issueChecksCard(ctx, config, services, hermes, run, runDir, logger)
}

// advancePastStaging decides what a sealed staging record means for a
// delivery that is still running.
func advancePastStaging(
	ctx context.Context,
	config runtime.Config,
	services *runtime.Services,
	hermes *runtime.Hermes,
	run state.RunOverview,
	runDir string,
	depth depthPlan,
	cards map[string]*runtime.BoardTask,
	climb func(stage, verdict string) (deliverProgress, error),
	logger Logger,
) (deliverProgress, error) {
	report, err := readDeliverReport(runDir, runner.DeliverStagingReportFile)
	if err != nil {
		return deliverWorking, nil // unreadable this tick; read again next
	}
	// deploy_not_applicable is a stopping point rather than a failure: the
	// merge landed and the destination's own deployment covers nothing this
	// change touched, so nothing started and there is no screen to look at.
	if report.Verdict != "pass" && report.Verdict != "deploy_not_applicable" {
		return climb(stagingVerdictOwner(report.Verdict), report.Verdict)
	}
	posted, err := services.Tick.StagingReportPosted(ctx, run.RunID)
	if err != nil {
		return deliverWorking, err
	}
	if !posted {
		return deliverWorking, reportDeliverStaging(ctx, config, services, hermes, run, runDir, cards, logger)
	}
	if report.Verdict != "pass" || !depth.reachesProduction() || report.PromotionHold != "" {
		// Either this is where the destination stops, or the promotion gate
		// cannot move — the release branch carries changes staging does not,
		// the delta could not be read, the ticket promised nothing a screen
		// could be judged on — and a promotion nothing could fulfil is not
		// attempted.
		return deliverReached, nil
	}
	return advanceTowardsPromotion(ctx, config, services, hermes, run, runDir, logger)
}

// stagingVerdictOwner names the card that produced a staging record.
//
// The record is one file whichever phase wrote it, and the gate the CI wait
// ends on writes the same file as the merge and the observation do. Climbed
// against the wrong card, a red gate would spend the merge card's attempts
// and leave the wait card's own record untouched — so the wait would be
// repeated once more than the ladder had counted, every time.
func stagingVerdictOwner(verdict string) string {
	if verdict == "checks_failed" {
		return deliverStageChecks
	}
	return deliverStageIntegrate
}

// climbDeliverPhase hands one delivery phase that did not do what it
// promised to the ladder.
//
// It is the same ladder the chain's cards climb, with one thing of its own:
// a delivery card is not part of the chain, so it brings its own way of
// being built again — drop the records of the steps that have to happen a
// second time and retire the card, and the next tick issues a fresh one.
// Everything else is shared, deliberately: the record of what has been
// tried, the waits that grow, the single notice to the operator, and the
// stop that is read before anything is dispatched.
func climbDeliverPhase(
	ctx context.Context,
	config runtime.Config,
	services *runtime.Services,
	hermes *runtime.Hermes,
	envelope hook.DispatchEnvelope,
	run state.RunOverview,
	view chainView,
	plan runtime.ChainPlan,
	stage, verdict string,
	cards map[string]*runtime.BoardTask,
	logger Logger,
) (deliverProgress, error) {
	// The ladder keys its record by stage and round, and a delivery phase
	// has no round of its own: it is merged, deployed and observed once,
	// whatever round of implementation produced the change. Pinning the
	// view's round here is what makes the record the same one every tick
	// finds, whichever round the chain ended on.
	view.round = deliverLadderRound
	climb := newClimb(config, services, hermes, envelope, run, view, plan, stage, logger)
	climb.rebuild = func(ctx context.Context, climb ladderClimb) (ladderVerdict, error) {
		return rebuildDeliverCard(ctx, climb, verdict, cards)
	}
	logger.Info("a delivery phase did not do what it promised; it is climbed rather than reported",
		"run", run.RunID, "stage", stage, "verdict", verdict)
	climbed, err := climbLadder(ctx, climb)
	switch {
	case err != nil:
		return deliverWorking, err
	case climbed == ladderStopped:
		return deliverStopped, nil
	case climbed == ladderSpent:
		return deliverSpent, nil
	}
	return deliverWorking, nil
}

// rebuildDeliverCard clears the way for one more attempt at a delivery
// phase: the records of the steps that have to happen again are removed,
// and every card of this phase is retired so the next tick can issue a
// fresh one.
//
// Only the steps that have to happen again. The delivery verb resumes past
// every step whose record already exists, which is what makes a second
// attempt cheap — and what makes removing the wrong record expensive: a
// merge that landed would be attempted a second time and refused, and the
// delivery would be climbing a failure it had created itself.
func rebuildDeliverCard(ctx context.Context, climb ladderClimb, verdict string, cards map[string]*runtime.BoardTask) (ladderVerdict, error) {
	for _, name := range deliverRetryDrops(climb.stage, verdict) {
		if err := os.Remove(filepath.Join(climb.runDir, name)); err != nil && !os.IsNotExist(err) {
			return ladderHandled, err
		}
	}
	if err := sweepDeliverCards(ctx, climb.hermes, cards); err != nil {
		return ladderHandled, err
	}
	climb.logger.Info("the delivery phase's records were cleared for another attempt",
		"run", climb.run.RunID, "stage", climb.stage, "verdict", verdict)
	return ladderHandled, nil
}

// deliverRetryDrops names the records one more attempt at a phase has to
// produce again, chosen by what the phase said went wrong. The phase report
// always goes: it is the record that says the phase is over. What else goes
// is whatever the refusal was about, and nothing before it.
func deliverRetryDrops(stage, verdict string) []string {
	if stage == deliverStagePromote {
		drops := []string{runner.DeliverProductionReportFile, deliverFailureRecord(stage)}
		switch verdict {
		case "observe_failed", "observe_blocked":
			return append(drops, runner.DeliverProductionVisibleFile, runner.DeliverProductionShotFile)
		case "deploy_failed", "deploy_absent":
			return append(drops, runner.DeliverProductionFile, runner.DeliverProductionPlainFile)
		case "merge_failed", "merge_unverified", "promotion_failed":
			return append(drops, runner.DeliverPromotionFile, runner.DeliverPromotionMergeFile, runner.DeliverReflectionFile)
		}
		return drops
	}
	drops := []string{runner.DeliverStagingReportFile, deliverFailureRecord(stage)}
	if stage == deliverStageChecks {
		// The gate's own record is written only when it goes green, so
		// there is nothing of it to drop; what goes is the record that says
		// the phase is over.
		return drops
	}
	switch verdict {
	case "observe_failed", "observe_blocked":
		return append(drops, runner.DeliverStagingVisibleFile, runner.DeliverStagingShotFile)
	case "deploy_failed", "deploy_absent":
		return append(drops, runner.DeliverStagingProofFile, runner.DeliverStagingPlainProofFile)
	}
	return drops
}

// deliverFailureRecord is where a delivery card's account of its own
// failure lives, relative to the run directory.
//
// It goes with every other record a second attempt must not inherit. A
// delivery card has one round however many attempts it takes, so the
// account of the attempt before it sits at the same path — and left there,
// a card that filled the volume once and then met a red gate would be
// climbed as a volume that filled, and swept instead of waited on. The
// account is the attempt's, not the phase's.
func deliverFailureRecord(stage string) string {
	return runner.StageFailureFile("", stage, deliverLadderRound)
}

// commitSHAPattern is what a commit the report may cite looks like. A merge
// record that carries anything else carries no commit at all: the report
// gate binds the commit to a URL it builds itself, and a value that is not
// a commit would be refused there with nothing saying why.
var commitSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// deliveryOutcome reads how far the delivery actually got and the evidence
// for it, out of the records the cards sealed.
//
// Read from the records, never from what the engine believes it did. A
// depth whose records are not there carries no evidence for that depth, and
// the report gate then refuses the claim — which is exactly right: a
// success that says production was verified has to be able to show the page
// it was verified on.
func deliveryOutcome(runDir, repository string, depth depthPlan, evidence map[string]string) (string, map[string]string, string) {
	carried := map[string]string{}
	for key, value := range evidence {
		carried[key] = value
	}
	reached := string(worker.DeliverPullRequest)
	shortfall := depth.shortfallText()

	staging, stagingErr := readDeliverReport(runDir, runner.DeliverStagingReportFile)
	if stagingErr == nil && staging.Verdict == "pass" &&
		commitSHAPattern.MatchString(staging.MergedSHA) && staging.TargetURL != "" &&
		carried["pull_request_url"] != "" && repository != "" {
		reached = string(worker.DeliverIntegration)
		carried["commit_sha"] = staging.MergedSHA
		carried["commit_url"] = "https://github.com/" + repository + "/commit/" + staging.MergedSHA
		carried["staging_evidence_url"] = staging.TargetURL
		if hold := staging.PromotionHold; hold != "" && depth.reachesProduction() {
			shortfall = hold
		}
	} else if stagingErr == nil && staging.Verdict == "deploy_not_applicable" && depth.reachesIntegration() {
		shortfall = "変更した範囲がこの納品先の自動デプロイの対象外のため、staging への反映は行われていません。"
	}

	production, productionErr := readDeliverReport(runDir, runner.DeliverProductionReportFile)
	if reached == string(worker.DeliverIntegration) && productionErr == nil &&
		production.Verdict == "pass" && production.TargetURL != "" {
		reached = string(worker.DeliverProduction)
		carried["production_evidence_url"] = production.TargetURL
	}

	if reached == depth.Configured {
		shortfall = ""
	} else if hold := releasePathHold(runDir); hold != "" && depth.reachesProduction() &&
		reached == string(worker.DeliverIntegration) {
		// The path to production was checked before the promotion and did
		// not check out (deliver.go). That is a more exact answer than the
		// depth record's own line, which was written before the delivery
		// ran and only knows what the settings said then.
		shortfall = hold
	} else if shortfall == "" && depth.reachesProduction() && reached == string(worker.DeliverIntegration) {
		// The Go wait ended without a promotion: the operator asked for a
		// look before production moved and no look arrived in time.
		shortfall = "本番反映の前に運用担当者の確認を必要とする設定のため、確認が得られるまで staging までで止めています。"
	}
	carried["reached_delivery"] = reached
	if shortfall != "" {
		carried["delivery_shortfall"] = boundedShortfall(shortfall)
	}
	return reached, carried, shortfall
}

// boundedShortfall holds the line to what the report may carry, cutting on
// a rune boundary so what survives is still readable.
func boundedShortfall(line string) string {
	if len(line) <= hook.MaxDeliveryShortfallBytes {
		return line
	}
	cut := hook.MaxDeliveryShortfallBytes
	for cut > 0 && !utf8RuneStart(line[cut]) {
		cut--
	}
	return line[:cut]
}

// utf8RuneStart reports whether a byte begins a rune (a continuation byte
// is 10xxxxxx).
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// completeDelivery is what happens when the publish card is done: the
// change is proposed, and how much further it travels is decided here.
//
// A destination that stops at the proposal is reported the way it always
// was, from the same place, with the same evidence. A destination that asks
// for more keeps the run claimed while the delivery cards carry it, and the
// success is written at the end of that.
func completeDelivery(
	ctx context.Context,
	config runtime.Config,
	services *runtime.Services,
	hermes *runtime.Hermes,
	envelope hook.DispatchEnvelope,
	run state.RunOverview,
	view chainView,
	plan runtime.ChainPlan,
	logger Logger,
) error {
	runDir := runDirectory(config, run.DeliveryID)
	depth, sealed := readDepthRecord(runDir)
	if !sealed {
		decided, err := planDeliveryDepth(config, run, runDir)
		if err != nil {
			// The destination's depth cannot be read at all. The change is
			// published and nothing here can say where else it should go,
			// so the delivery ends on what it has — which is the proposal,
			// and the report says so rather than claiming anything more.
			logger.Error("the delivery depth could not be decided; the delivery ends at its pull request",
				"run", run.RunID, "error", err.Error())
			return reportChainSuccess(ctx, config, services, envelope, run, logger)
		}
		depth = sealDepthRecord(runDir, decided, logger)
		if depth.short() {
			logger.Info("the destination asks for a deeper delivery than this instance can carry",
				"run", run.RunID, "configured", depth.Configured, "reached", depth.Reached, "missing", depth.Missing)
		}
	}
	if !depth.reachesIntegration() {
		return reportChainSuccess(ctx, config, services, envelope, run, logger)
	}
	progress, err := advanceDelivery(ctx, config, services, hermes, envelope, run, view, plan, depth, view.board, logger)
	if err != nil {
		return err
	}
	switch progress {
	case deliverWorking:
		return nil
	case deliverStopped:
		return endDeliveryEarly(ctx, config, services, hermes, envelope, run, view, runDir, hook.TerminalCancelled, logger)
	case deliverSpent:
		// The one ending a failed delivery phase still has, and only for an
		// operator who asked for it: retry_max_attempts is zero by default,
		// which is no bound at all, and a phase is climbed for as long as it
		// takes. With a bound configured, the delivery ends the way it would
		// have before the ladder existed.
		return endDeliveryEarly(ctx, config, services, hermes, envelope, run, view, runDir, hook.TerminalReleaseFailed, logger)
	}
	return reportChainSuccess(ctx, config, services, envelope, run, logger)
}

// endDeliveryEarly closes a delivery that will not reach its depth, on the
// requester's stop or on a bound an operator configured, and retires both
// the chain and the delivery cards.
func endDeliveryEarly(
	ctx context.Context,
	config runtime.Config,
	services *runtime.Services,
	hermes *runtime.Hermes,
	envelope hook.DispatchEnvelope,
	run state.RunOverview,
	view chainView,
	runDir string,
	code hook.TerminalCode,
	logger Logger,
) error {
	repository, err := readField(runDir, "ticket-draft.json", "repository")
	if err != nil {
		repository = ""
	}
	evidence := stoppedDeliveryEvidence(runDir, repository, code)
	terminal := runner.NewTerminal(config, services, envelope, chainOwnerRunID(run.DeliveryID), runDir, logger)
	if err := terminal.Report(ctx, code, runner.Outcome{Code: code, Evidence: evidence}, repository); err != nil {
		return err
	}
	logger.Info("the delivery ended before its configured depth", "run", run.RunID, "code", string(code))
	if err := sweepDeliverCards(ctx, hermes, deliverCards(runDir, run.DeliveryID, view.board)); err != nil {
		return err
	}
	return archiveChain(ctx, hermes, view.all)
}

// stoppedDeliveryEvidence is what a stop has to say about what had already
// landed when it arrived.
//
// A stop used to mean nothing had happened: the delivery was over the
// moment its pull request existed, so the report claimed no links and the
// footer said production was untouched. A requester can now stop one whose
// change is merged, deployed and looked at — and telling them nothing moved
// would send them away from the environment they have to go and see. So the
// stop carries the depth it reached and that depth's evidence, read from
// the records the cards sealed.
//
// Nothing for an ending that is not a stop: the endings a bound on the
// attempts still produces are failures, and what they may carry is the
// failure reporting's own question.
func stoppedDeliveryEvidence(runDir, repository string, code hook.TerminalCode) map[string]string {
	if code != hook.TerminalCancelled || repository == "" {
		return nil
	}
	depth, sealed := readDepthRecord(runDir)
	if !sealed {
		return nil
	}
	outcome, err := readChainOutcome(runDir)
	if err != nil {
		return nil
	}
	_, evidence, _ := deliveryOutcome(runDir, repository, depth, outcome.Evidence)
	// The shortfall line explains a delivery that stopped because its
	// instance could not take it further. This one stopped because it was
	// told to, and the stop's own sentence says so; the line would read as
	// a second, wrong reason.
	delete(evidence, "delivery_shortfall")
	return evidence
}

// readChainOutcome reads what the publish card sealed: the adopted round
// and the pull request it opened.
func readChainOutcome(runDir string) (runner.ChainOutcome, error) {
	var outcome runner.ChainOutcome
	raw, err := os.ReadFile(filepath.Join(runDir, runner.ChainOutcomeFile))
	if err != nil {
		return outcome, err
	}
	if err := json.Unmarshal(raw, &outcome); err != nil || outcome.Stage < 1 {
		return runner.ChainOutcome{}, errors.New("chain outcome artifact invalid")
	}
	return outcome, nil
}
