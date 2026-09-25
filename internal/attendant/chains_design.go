package attendant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/probe"
	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
	"automation.internal/ticket-ingress/internal/worker"
	"automation.internal/ticket-ingress/internal/worker/investigate"
	"time"
)

// The investigating designer's shapes end and fail in their own ways
// (docs/INVESTIGATING_DESIGNER.md §4.4, §5, §7). As everywhere in the
// attendant, the card's state is only the alarm; the sealed records in the
// run directory are the classification.

// errDesignRoundLimit says the design rounds are spent; the run ends as
// nonconverged instead of starting another round.
//
// Only the ceiling reaches it now. design_max_rounds used to stop a design
// here after three rounds, which is how a design whose judges kept objecting
// ended the delivery; rounds are no longer counted out, so what is left is
// the highest round number a record may carry at all.
var errDesignRoundLimit = errors.New("design round limit reached")

// designReturnCause says who sent the delivery back to the designer. The two
// answers end the run as different things, and a requester reads the
// difference: their design reviews never agreed, or their design reviews
// agreed and the change written from it was judged to need a different plan.
// Before this was carried, both ended as design_nonconverged and a requester
// whose three design rounds all passed was told the design reviews had not
// converged (live 2026-09-17).
type designReturnCause int

const (
	// designReviewsDisagreed: the design's own judges never agreed.
	designReviewsDisagreed designReturnCause = iota
	// designCalledWrongLater: the design was agreed, and the applier or a
	// review of the written change said the plan itself was wrong.
	designCalledWrongLater
)

// terminalCode is how the run ends when this cause meets a spent round
// budget. An investigation carries no implementation, so only its own
// reviews can disagree and only the one code can arise.
func (c designReturnCause) terminalCode(shape runtime.ChainShape) hook.TerminalCode {
	if shape == runtime.ShapeInvestigation {
		return hook.TerminalInvestigationNonconverged
	}
	if c == designCalledWrongLater {
		return hook.TerminalDesignRoundsSpent
	}
	return hook.TerminalDesignNonconverged
}

// consumerDesignMaxRounds is how far the design rounds may count.
//
// It is the artifact ceiling, not a budget. A destination declaring
// design_max_rounds still loads and still means what it says about the
// records it seals; what it no longer does is end a delivery whose design
// reviews have not agreed by that round.
func consumerDesignMaxRounds(string) int {
	return worker.StageCeiling
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
	if !investigationCommentPosted(ctx, services, run, round) {
		// The report must reach the ticket before the run says it did;
		// the next tick posts it (postDesignComments) and ends the run then.
		logger.Info("investigation report not yet on the ticket; ending deferred", "run", run.RunID)
		return nil
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
	var evidence map[string]string
	switch stageName {
	case runtime.StageInvestigate:
		if _, err := os.Stat(filepath.Join(roundDir, "incomplete.json")); err == nil {
			code = hook.TerminalInvestigationIncomplete
			evidence = incompleteEvidence(runDir, view.designRound)
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
			return true, nextDesignRoundOrEnd(ctx, config, services, hermes, envelope, run, view, plan, designReviewsDisagreed, "design review asked for a revision", logger)
		case outcome == "nonconverged" && plan.Shape == runtime.ShapeInvestigation:
			code = hook.TerminalInvestigationNonconverged
		case outcome == "nonconverged":
			// A record this engine can no longer seal. The decide verb used
			// to rewrite the final round's revise as nonconverged, and this
			// is where that ended the delivery — after putting the
			// disagreement to the requester as a question, who then had to
			// answer it before anything moved again. Rounds are not counted
			// out any more, so nothing produces this outcome; an upgrade can
			// still find one sealed by the engine that was running an hour
			// ago, and it is reported as what it says it is.
			code = hook.TerminalDesignNonconverged
		default:
			// Approved and still failed: the applier's instruction could not
			// be rendered, or the card died after sealing — the machinery's own.
			code = hook.TerminalInternalFailed
		}
	case runtime.StageApply, runtime.StageReviewA:
		// An objection surfaces on one of two cards: the apply card itself,
		// when the applier left revise-design.json at the root of its
		// working copy and the run-instruction command sealed it into the
		// design round's objection.json (issue #103); or the card that seals
		// the applier's work, when the file was left in the run directory.
		// Either way the design round's record is what says so.
		if objected, err := designObjectionRecorded(runDir, view.designRound); err == nil && objected {
			return true, nextDesignRoundOrEnd(ctx, config, services, hermes, envelope, run, view, plan, designCalledWrongLater, "the applier objected to the design", logger)
		}
		return false, nil
	default:
		return false, nil
	}
	// The same three endings the implementation side hands to the ladder:
	// a model that would not answer, and the machinery's own breakdown
	// after an approved design. Neither was a decision about the request.
	// The endings this keeps are the ones that are — a design its judges
	// could not agree on, a report the investigator could not complete.
	if ladderOwns(code) {
		verdict, err := climbLadder(ctx, newClimb(config, services, hermes, envelope, run, view, plan, stageName, logger))
		switch {
		case err != nil:
			return true, err
		case verdict == ladderHandled:
			return true, nil
		case verdict == ladderStopped:
			code, evidence = hook.TerminalCancelled, nil
		}
	}
	terminal := runner.NewTerminal(config, services, envelope, chainOwnerRunID(run.DeliveryID), runDir, logger)
	repository, err := readField(runDir, "ticket-draft.json", "repository")
	if err != nil {
		repository = ""
	}
	if code == hook.TerminalModelFailed {
		// Nothing else fills evidence on this code: the incomplete arm is
		// the only other writer and it ends as a different code.
		evidence = failedStepEvidence(config, runDir, stageName, view.designRound)
	}
	if err := terminal.Report(ctx, code, runner.Outcome{Code: code, Evidence: evidence}, repository); err != nil {
		return true, err
	}
	logger.Info("chain terminalized", "run", run.RunID, "stage", stageName, "code", string(code))
	return true, archiveChain(ctx, hermes, view.all)
}

// designObjectionRecorded reports whether the seal recorded an applier's
// objection to the given design round's design (the runner writes it as
// history/design-<N>/objection.json, so both sides agree on the round).
func designObjectionRecorded(runDir string, designRound int) (bool, error) {
	if designRound < 1 {
		return false, nil
	}
	if _, err := os.Stat(filepath.Join(designRoundDir(runDir, designRound), "objection.json")); err == nil {
		return true, nil
	}
	return false, nil
}

// unreadableReviewsStopReason is what a requester is told when the run ends
// because its sealed reviews could not be read. It names no file and no
// error text: those are the operator's, and the requester's question is
// only whether their ticket is at fault.
func unreadableReviewsStopReason(round int) string {
	return fmt.Sprintf("%d 巡目のレビュー結果を読めなかったため、自動処理を止めました。"+
		"依頼の内容とは別のところで止まっています。運用担当者が記録を確認します。", round)
}

// unreadableReviewsOutcome is how a run ends when its sealed reviews could
// not be read: the code and the sentence together, because the code decides
// which comment the requester reads and whether the failure counts toward
// the hold on new work.
//
// Only one thing still reaches it: a delivery with no reviewer configured
// at all, or one whose configuration cannot be read. There is no seat to
// ask again there, and nothing to change. A record that names its seat
// goes to climbUnreadableReview instead.
func unreadableReviewsOutcome(round int) (hook.TerminalCode, string) {
	return hook.TerminalInternalFailed, unreadableReviewsStopReason(round)
}

// unreadableReview names the seat whose sealed review could not be read.
//
// The seat is the whole point of carrying it: without a name this was one
// undifferentiated breakdown and the delivery ended on it. With a name it
// is one seat that did not leave a usable answer, which is a thing the
// ladder knows what to do about.
type unreadableReview struct {
	reviewer string
	err      error
}

func (u *unreadableReview) Error() string {
	return fmt.Sprintf("sealed review %s could not be read: %v", u.reviewer, u.err)
}

func (u *unreadableReview) Unwrap() error { return u.err }

// climbUnreadableReview turns a review that cannot be read into the failure
// it is — that seat's — and climbs the ladder for it.
//
// A sealed review missing or unparseable is the seat having left no usable
// answer. The decide verb read every one of them before it sealed the
// round's decision, so a record that will not read now went after that; the
// decision is therefore no longer derivable from its own evidence, and the
// round has, in the only sense that matters, not been decided. So the
// unreadable record and the decision built on it are dropped, the seat is
// moved to its next candidate, and the review is asked again for the same
// round. Nothing about the request has been decided by any of this, which
// is why it used to be the wrong thing for the delivery to end on.
//
// The hands are the ordinary ones for a model that would not answer: the
// seat's candidates in order, then the instruction rebuilt shorter, then
// the wait. A configured limit on attempts still ends the delivery, and the
// caller reports it exactly as it did before the ladder existed.
func climbUnreadableReview(
	ctx context.Context,
	config runtime.Config,
	services *runtime.Services,
	hermes *runtime.Hermes,
	envelope hook.DispatchEnvelope,
	run state.RunOverview,
	view chainView,
	plan runtime.ChainPlan,
	reviewer string,
	logger Logger,
) (ladderVerdict, error) {
	stage, known := reviewStageOf(config.ConsumerConfigPath, reviewer)
	if !known {
		return ladderSpent, nil
	}
	runDir := runDirectory(config, run.DeliveryID)
	round := view.round
	// The card's own account of its failure, written here rather than by
	// the card: the card exited zero and sealed a review, and what is wrong
	// with that review was only discovered afterwards, by a reader of it.
	// The class is what the ladder acts on, and this is a model that left
	// nothing usable behind — the same class the card would have sealed had
	// the answer been unusable while it still held it.
	failure := runner.StageFailure{
		DeliveryID: run.DeliveryID, ToolSHA: config.Identity.EngineSHA,
		Stage: stage, Round: round, Class: runner.FailureClassModel,
		Error: "the sealed review of " + reviewer + " could not be read", FailedAt: time.Now().UTC(),
	}
	if err := runner.SealStageFailureRecord(runDir, failure); err != nil {
		logger.Error("the unreadable review could not be recorded as the seat's failure",
			"run", run.RunID, "stage", stage, "round", round, "error", err.Error())
		return ladderSpent, nil
	}
	if err := runner.DropReviewAndDecision(runDir, reviewer, round); err != nil {
		logger.Error("the unreadable review could not be cleared for another attempt",
			"run", run.RunID, "stage", stage, "round", round, "error", err.Error())
		return ladderSpent, nil
	}
	logger.Info("a review that could not be read is the seat's own failure; the seat is asked again",
		"run", run.RunID, "stage", stage, "round", round, "seat", reviewer)
	return climbLadder(ctx, newClimb(config, services, hermes, envelope, run, view, plan, stage, logger))
}

// reviewStageOf is the card one configured reviewer runs as. The chain runs
// exactly two review cards, in the order the reviewers are configured.
func reviewStageOf(consumerConfigPath, reviewer string) (string, bool) {
	reviewers, err := consumerReviewerIDs(consumerConfigPath)
	if err != nil {
		return "", false
	}
	for index, configured := range reviewers {
		if configured != reviewer {
			continue
		}
		if index == 1 {
			return runtime.StageReviewB, true
		}
		return runtime.StageReviewA, true
	}
	return "", false
}

// designWrongForRound answers, for one implementation round, whether the
// delivery goes back to the designer. An error means the reviews could not
// be read at all, which is not an answer to that question: it is a reason to
// stop. It is separate from the attendant's own plumbing so the decision can
// be measured without a board or a tracker (review of #123).
func designWrongForRound(runDir, consumerConfigPath string, implementRound int) (bool, error) {
	reviewers, err := consumerReviewerIDs(consumerConfigPath)
	if err != nil {
		return false, fmt.Errorf("the configured reviewers could not be read: %w", err)
	}
	return reviewsFlagDesignWrong(runDir, implementRound, reviewers)
}

// reviewsFlagDesignWrong reports whether any sealed review of the
// implementation round carries the finding code design-wrong: the reviewer
// judged that the design itself does not hold, which sends the delivery back
// to the designer rather than to another implementation round.
//
// Every configured reviewer's record must be there and readable. Reaching
// this point proves they were: the round only gets here on a sealed revise
// decision, and the decide verb refuses to seal one unless it read every
// review it was given (measured in the review of #123). So a record that is
// now missing was deleted after that, which is the same corruption as one
// that will not parse — a dangling symlink reaches this as "missing", and a
// renamed reviewer id reaches it as "missing" for a record that is right
// there. Answering false for any of them inverted the decision silently.
//
// None of them is a judgement about the design, so none of them returns one:
// the error says the reviews could not be read, and the caller ends the run
// with that reason rather than spending the delivery's remaining design
// rounds re-reading the same broken record (the implement round does not
// advance across a design round, so it would be re-read every time).
func reviewsFlagDesignWrong(runDir string, implementRound int, reviewers []string) (bool, error) {
	if len(reviewers) == 0 {
		return false, errors.New("no reviewer is configured, so no sealed review was read")
	}
	designWrong := false
	for _, reviewer := range reviewers {
		path := filepath.Join(runDir, "history", fmt.Sprintf("stage-%d", implementRound), reviewer+".json")
		raw, err := os.ReadFile(path)
		if err != nil {
			return false, &unreadableReview{reviewer: reviewer, err: err}
		}
		var review struct {
			Findings []struct {
				Code string `json:"code"`
			} `json:"findings"`
		}
		if err := json.Unmarshal(raw, &review); err != nil {
			return false, &unreadableReview{reviewer: reviewer, err: err}
		}
		for _, finding := range review.Findings {
			if finding.Code == "design-wrong" {
				// Not returned yet: a record after this one may be
				// unreadable, and answering here would leave that unread and
				// unrecorded — the same silent no this closes, in a window
				// the order of the configured reviewers decides (review of
				// #123). Reading them all makes the answer the same whatever
				// that order is.
				designWrong = true
			}
		}
	}
	return designWrong, nil
}

// regenerateDesignBackedRound starts the next implementation round of a
// design-backed delivery: the applier gets the approved design's instruction
// again (with the reviewers' findings riding in the run directory), never the
// original implementer's.
func regenerateDesignBackedRound(ctx context.Context, hermes *runtime.Hermes, config runtime.Config, run state.RunOverview, view chainView, plan runtime.ChainPlan, logger Logger) error {
	for _, task := range view.all {
		if task.Status == "done" {
			continue
		}
		if err := hermes.Archive(ctx, task.ID); err != nil {
			return err
		}
	}
	pipeline := &runner.Pipeline{Config: config, Workspace: runDirectory(config, run.DeliveryID), Logger: logger}
	_, round := pipeline.ApprovedDesign()
	if round < 1 {
		return errors.New("design-backed round has no approved design to re-apply")
	}
	if err := pipeline.RenderApplyInstruction(ctx, round); err != nil {
		return err
	}
	rounds := runtime.ChainRounds{Design: view.designRound, Implement: view.round + 1}
	terminalCard, err := runtime.EnsureChainFor(ctx, hermes, config.Chain, plan, nil, run.DeliveryID, run.RunID, run.Summary, rounds)
	if err != nil {
		return err
	}
	logger.Info("design-backed round regenerated", "run", run.RunID, "implement_round", rounds.Implement, "terminal_card", terminalCard)
	return nil
}

// incompleteEvidence reads why the investigation round sealed nothing and
// the last objection the contract raised, from the round's incomplete.json,
// for the terminal report. A record that cannot be read leaves the report
// without a reason (the fixed text then speaks of the budget, as before);
// the run still ends.
func incompleteEvidence(runDir string, designRound int) map[string]string {
	evidence := map[string]string{}
	round := designRound
	if round < 1 || !incompleteRecordExists(runDir, round) {
		// The board may no longer name the round (a resubmission after the
		// cards were archived, or a view built without them): the newest
		// round that left a record is the one that ended the run.
		round = latestIncompleteRound(runDir)
	}
	if round < 1 {
		return evidence
	}
	name := fmt.Sprintf("history/design-%d/incomplete.json", round)
	if reason, err := readField(runDir, name, "reason"); err == nil && reason != "" {
		evidence["incomplete_reason"] = reason
	}
	if objection, err := readField(runDir, name, "last_refused_objection"); err == nil && objection != "" {
		evidence["incomplete_objection"] = objection
	}
	return evidence
}

func incompleteRecordExists(runDir string, round int) bool {
	info, err := os.Stat(filepath.Join(runDir, "history", fmt.Sprintf("design-%d", round), "incomplete.json"))
	return err == nil && info.Mode().IsRegular()
}

// latestIncompleteRound is the highest design round under history/ that
// left an incomplete.json, 0 when none did.
func latestIncompleteRound(runDir string) int {
	entries, err := os.ReadDir(filepath.Join(runDir, "history"))
	if err != nil {
		return 0
	}
	latest := 0
	for _, entry := range entries {
		var round int
		if _, err := fmt.Sscanf(entry.Name(), "design-%d", &round); err != nil || round <= latest || entry.Name() != fmt.Sprintf("design-%d", round) {
			continue
		}
		if incompleteRecordExists(runDir, round) {
			latest = round
		}
	}
	return latest
}

// nextDesignRoundOrEnd starts the next design round, or — when the rounds
// are spent — ends the run honestly with the shape's nonconverged code and
// retires every card, so the run never sits claimed with a failing card.
func nextDesignRoundOrEnd(
	ctx context.Context,
	config runtime.Config,
	services *runtime.Services,
	hermes *runtime.Hermes,
	envelope hook.DispatchEnvelope,
	run state.RunOverview,
	view chainView,
	plan runtime.ChainPlan,
	cause designReturnCause,
	why string,
	logger Logger,
) error {
	runDir := runDirectory(config, run.DeliveryID)
	// A design-round boundary honours 「停止」 like the implementation-round
	// boundary does: no next round starts after the requester asked to stop.
	if services != nil && services.Backlog != nil {
		if stopped, err := stopRequested(ctx, services.Backlog, config.Tracker.AllowedCreatorID, envelope.Snapshot.IssueID); err != nil {
			logger.Error("stop check before the next design round unreadable; proceeding", "run", run.RunID, "error", err.Error())
		} else if stopped {
			repository, readErr := readField(runDir, "ticket-draft.json", "repository")
			if readErr != nil {
				repository = ""
			}
			terminal := runner.NewTerminal(config, services, envelope, chainOwnerRunID(run.DeliveryID), runDir, logger)
			if err := terminal.Report(ctx, hook.TerminalCancelled, runner.Outcome{Code: hook.TerminalCancelled}, repository); err != nil {
				return err
			}
			return archiveChain(ctx, hermes, view.all)
		}
	}
	err := nextDesignRound(ctx, hermes, config, run, view, plan, why, logger)
	if !errors.Is(err, errDesignRoundLimit) {
		return err
	}
	// The designer cannot be asked again. When the delivery still has
	// implementation rounds, that is not the end of it: the design was
	// approved, the change exists, and the reviewers who objected to the
	// plan are the ones who will read the next attempt. Writing it again
	// under the design it has is the only work left, and it is work that
	// finishes - measured live, a delivery with an approved design, a
	// written change and objecting reviewers died here with nothing
	// delivered (完遂率を最優先、発注者指示 2026-09-17).
	// Only where there is written work to revise. The applier's objection
	// arrives before it writes anything - it refused to carry out the
	// design - so asking it again under the same design gets the same
	// refusal; that delivery has nothing left to try.
	_, candidateSealed := os.Stat(filepath.Join(runDir, fmt.Sprintf("history/stage-%d/candidate.json", view.round)))
	if cause == designCalledWrongLater && plan.Shape == runtime.ShapeDesign && candidateSealed == nil {
		if limit, limitErr := consumerRoundLimit(config.ConsumerConfigPath); limitErr == nil && (limit == 0 || view.round < limit) {
			logger.Info("design rounds spent; the change is written again under the design it has",
				"run", run.RunID, "round", view.round+1, "of", limit, "why", why)
			return regenerateDesignBackedRound(ctx, hermes, config, run, view, plan, logger)
		}
	}
	code := cause.terminalCode(plan.Shape)
	repository, readErr := readField(runDir, "ticket-draft.json", "repository")
	if readErr != nil {
		repository = ""
	}
	terminal := runner.NewTerminal(config, services, envelope, chainOwnerRunID(run.DeliveryID), runDir, logger)
	if err := terminal.Report(ctx, code, runner.Outcome{Code: code}, repository); err != nil {
		return err
	}
	logger.Info("design rounds spent; run ended", "run", run.RunID, "why", why, "code", string(code))
	return archiveChain(ctx, hermes, view.all)
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
		// objection at the limit has no verb to do that, so the caller ends
		// the run honestly on this signal.
		return fmt.Errorf("%w: %s", errDesignRoundLimit, why)
	}
	// Every card of the newest implementation round goes, done ones
	// included: the apply card that objected must not stay done below a new
	// design (the kanban would treat it as satisfied and dispatch the tail),
	// and archiving frees its key, so the implementation round number does
	// not move — which keeps the runner's own count (decisions sealed) and
	// the board's count the same. Design cards of the old round stay; the
	// new round is keyed one higher.
	for _, task := range view.all {
		if _, stage, _, ok := runtime.ParseChainCardKey(task.IdempotencyKey); ok && runtime.IsDesignStage(stage) && task.Status == "done" {
			continue
		}
		if err := hermes.Archive(ctx, task.ID); err != nil {
			return err
		}
	}
	rounds := runtime.ChainRounds{Design: view.designRound + 1, Implement: view.round}
	if plan.Shape == runtime.ShapeDesign && rounds.Implement < 1 {
		rounds.Implement = 1
	}
	terminalCard, err := runtime.EnsureChainFor(ctx, hermes, config.Chain, plan, nil, run.DeliveryID, run.RunID, run.Summary, rounds)
	if err != nil {
		return err
	}
	logger.Info("design round regenerated", "run", run.RunID, "why", why, "design_round", rounds.Design, "implement_round", rounds.Implement, "terminal_card", terminalCard)
	return nil
}

// maxCommentAttachments is the tracker's per-comment attachment budget: the
// measurements index, the raw outputs the report cites, and — when the
// report is too long for one comment — the report itself all come out of it
// (the tracker takes ten attachments per comment;
// docs/INVESTIGATING_DESIGNER.md §4.4).
const maxCommentAttachments = 10

// maxMeasurementAttachmentBytes is the per-file cap of §4.4 (256 KiB).
const maxMeasurementAttachmentBytes = 256 * 1024

// measurementsIndex renders the sealed measurements without their outputs,
// one JSON line each, for the requester's attachment.
func measurementsIndex(measurements []probe.Measurement) []byte {
	var b bytes.Buffer
	for _, measurement := range measurements {
		measurement.Output = ""
		encoded, err := json.Marshal(measurement)
		if err != nil {
			continue
		}
		b.Write(encoded)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// postDesignComments shows the requester what the round produced: the
// investigation report (measurements attached) once it is sealed, and the
// design's summary once the design reviews approved it. Both are posted at
// most once per run; the comment state in the ledger keeps them so.
func postDesignComments(ctx context.Context, config runtime.Config, services *runtime.Services, run state.RunOverview, view chainView, plan runtime.ChainPlan, logger Logger) {
	if plan.Shape == runtime.ShapeImplement || view.designRound < 1 || services.Tick == nil {
		return
	}
	runDir := runDirectory(config, run.DeliveryID)
	roundDir := designRoundDir(runDir, view.designRound)
	investigation, err := investigate.ReadInvestigation(filepath.Join(roundDir, "investigation.json"))
	if err != nil {
		return
	}
	qualifier := fmt.Sprintf("d%d", view.designRound)
	// The report is shown once the round's reviews stand behind it: the
	// evidence review for an investigation-only request (unless the
	// consumer turned it off), the design decision for a design request.
	reviewed := plan.Shape == runtime.ShapeInvestigation && !plan.ReviewInvestigation
	if !reviewed {
		outcome, err := readField(runDir, fmt.Sprintf("history/design-%d/decision.json", view.designRound), "outcome")
		reviewed = err == nil && outcome == "approved"
	}
	if !reviewed {
		return
	}
	posted, err := services.Tick.RunCommentPosted(ctx, run.RunID, hook.RunCommentInvestigation, qualifier)
	if err != nil {
		logger.Error("investigation comment state unreadable", "run", run.RunID, "error", err.Error())
		return
	}
	if !posted {
		facts := investigationFacts(investigation, plan.Shape == runtime.ShapeInvestigation, 0, 0)
		attachments, slots := []int64(nil), maxCommentAttachments
		// A report longer than one comment travels whole as an attachment,
		// uploaded before the measurements so the per-comment attachment
		// budget cannot be spent before the report itself has a place: the
		// report is what the requester asked for, the measurements back it.
		if overflow := hook.InvestigationReportOverflow(run.RunID, facts); len(overflow) > 0 && services.Backlog != nil {
			id, err := services.Backlog.UploadAttachment(ctx, hook.InvestigationReportFilename, overflow)
			if err != nil {
				logger.Error("investigation report not attached; the comment names the run record instead", "run", run.RunID, "error", err.Error())
			} else {
				attachments = append(attachments, id)
				facts.ReportAttached = true
				slots--
			}
		}
		measured, omitted := uploadMeasurements(ctx, services, runDir, investigation, slots, logger)
		attachments = append(attachments, measured...)
		facts.AttachedCount, facts.AttachmentsOmitted = len(attachments), omitted
		if !services.Tick.PostInvestigationComment(ctx, run.RunID, run.DeliveryID, qualifier, hook.InvestigationCommentContent(run.RunID, facts), attachments) {
			logger.Error("investigation report not posted; run continues", "run", run.RunID)
		}
	}
	if plan.Shape != runtime.ShapeDesign {
		return
	}
	design, err := investigate.ReadDesign(filepath.Join(roundDir, "design.json"))
	if err != nil || !design.DigestMatches() {
		return
	}
	facts := hook.DesignFacts{Round: design.Round, Cause: design.Cause, Approach: design.Approach, Files: design.FilePaths(),
		Verification: design.VerificationSummary(), BlastRadius: design.BlastRadius, NotDoing: design.NotDoing}
	if !services.Tick.PostDesignComment(ctx, run.RunID, run.DeliveryID, qualifier, hook.DesignCommentContent(run.RunID, facts)) {
		logger.Error("design summary not posted; run continues", "run", run.RunID)
	}
}

// investigationCommentPosted reports whether the round's report reached the
// ticket; an investigation-only delivery does not end before it did.
func investigationCommentPosted(ctx context.Context, services *runtime.Services, run state.RunOverview, designRound int) bool {
	if services.Tick == nil {
		return false
	}
	posted, err := services.Tick.RunCommentPosted(ctx, run.RunID, hook.RunCommentInvestigation, fmt.Sprintf("d%d", designRound))
	return err == nil && posted
}

func investigationFacts(investigation investigate.Investigation, endsHere bool, attached, omitted int) hook.InvestigationFacts {
	facts := hook.InvestigationFacts{Round: investigation.Round, Questions: investigation.Questions, Unknowns: investigation.Unknowns,
		Next: investigation.Next, MeasurementsCount: investigation.MeasurementsCount, AttachedCount: attached, AttachmentsOmitted: omitted, EndsHere: endsHere}
	for _, finding := range investigation.Findings {
		facts.Findings = append(facts.Findings, hook.InvestigationFindingFact{Claim: finding.Claim, Measured: finding.Confidence == investigate.ConfidenceMeasured, Evidence: finding.Evidence})
	}
	return facts
}

// uploadMeasurements attaches the measurements file and the raw outputs the
// report cites, re-scanning each for secret shapes before it leaves the
// pod. Uploads that fail are skipped and counted; the comment still posts.
// slots is how many files this may attach in all, the index included: the
// caller spends the same per-comment budget on the report when the report
// itself was too long for the comment.
func uploadMeasurements(ctx context.Context, services *runtime.Services, runDir string, investigation investigate.Investigation, slots int, logger Logger) ([]int64, int) {
	if services.Backlog == nil || slots <= 0 {
		return nil, 0
	}
	path := filepath.Join(runDir, "measurements.jsonl")
	measurements, err := probe.ReadPrefix(path, investigation.MeasurementsCount)
	if err != nil {
		logger.Error("measurements not attached: the sealed prefix does not verify", "error", err.Error())
		return nil, 0
	}
	var ids []int64
	// The attached measurements file is the index: every line without its
	// output (ids, probes, arguments, times, fingerprints, refusals). The
	// outputs the report cites travel as their own files; the full file
	// stays in the run directory. This keeps the index under the per-file
	// cap however large the outputs were.
	if index := measurementsIndex(measurements); len(index) > 0 {
		if kind, found := probe.SecretShaped(string(index), nil); found {
			logger.Error("measurements index not attached: it carries a secret shape", "kind", kind)
		} else if len(index) > maxMeasurementAttachmentBytes {
			logger.Error("measurements index not attached: larger than the per-file cap", "bytes", len(index))
		} else if id, err := services.Backlog.UploadAttachment(ctx, "measurements-index.jsonl", index); err == nil {
			ids = append(ids, id)
		}
	}
	cited := investigation.MeasuredEvidence()
	omitted := 0
	for _, measurement := range measurements {
		if measurement.Refused || measurement.Output == "" || !cited[measurement.ID] {
			continue
		}
		if len(ids) >= slots {
			omitted++
			continue
		}
		if len(measurement.Output) > maxMeasurementAttachmentBytes {
			// The design's per-file cap (§4.4); the full output stays in the
			// run directory.
			omitted++
			continue
		}
		if kind, found := probe.SecretShaped(measurement.Output, nil); found {
			logger.Error("measurement not attached: it carries a secret shape", "id", measurement.ID, "kind", kind)
			omitted++
			continue
		}
		id, err := services.Backlog.UploadAttachment(ctx, "measurement-"+measurement.ID+".txt", []byte(measurement.Output))
		if err != nil {
			omitted++
			continue
		}
		ids = append(ids, id)
	}
	return ids, omitted
}
