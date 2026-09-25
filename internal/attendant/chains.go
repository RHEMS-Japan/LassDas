// Package attendant drives the cards orchestration: one resident process
// that claims queued runs, prepares their shared run directory, keeps each
// delivery's stage-card chain aligned with reality, and owns every ledger
// transition — questions and terminal reports included. The stage cards
// themselves never touch the ledger; their whole contract is artifacts and
// exit codes, which is what keeps a failed stage a fact the attendant
// reads, never a state a worker claims.
package attendant

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
	"automation.internal/ticket-ingress/internal/worker"
)

// Logger is the narrow logging surface the sync needs; it is also exactly
// the logger shape the runner pipeline takes.
type Logger interface {
	Info(string, ...any)
	Error(string, ...any)
}

// failedCardStatuses are the card states the attendant terminalizes.
// Measured against the fork's status vocabulary (buddy review): timeouts,
// spawn failures, crashes and breaker trips are events and run outcomes,
// not statuses — every one of them lands the card in "blocked" (below the
// retry threshold the kanban re-dispatches once first, which is why the
// stages resume idempotently). Detection leads immediately to a sealed
// terminal report on the tracker and the chain's retirement; the board is
// never the failure record. A card an operator moved to "triage" is a
// deliberate human lane and is left waiting.
var failedCardStatuses = map[string]bool{
	"blocked": true,
}

// chainOwnerRunID is the attendant's per-delivery claim identity. The
// ledger seals an owner into every claim and the terminal report must
// present the same one, possibly many ticks and process restarts later, so
// the identity is derived, stateless, from the delivery id itself.
func chainOwnerRunID(deliveryID string) int64 {
	digest := sha256.Sum256([]byte(deliveryID))
	value := int64(binary.BigEndian.Uint64(digest[:8]) &^ (1 << 63)) // #nosec G115 -- top bit cleared; non-negative by construction.
	if value == 0 {
		value = 1
	}
	return value
}

// SyncChains is the cards orchestration's per-tick protocol: queued runs
// are claimed, prepared and given their first round; claimed runs have
// their chain healed, their finished publish reported, and their failures
// classified from artifacts into regeneration, a question, or an honest
// terminal; a run whose terminal report stayed pending has the same report
// submitted again until it completes.
func SyncChains(ctx context.Context, config runtime.Config, services *runtime.Services, hermes *runtime.Hermes, logger Logger) error {
	runs, err := services.Store.ScanRuns(ctx)
	if err != nil {
		return err
	}
	if len(runs) == 0 {
		return nil
	}
	tasks, err := hermes.ListBoardTasks(ctx)
	if err != nil {
		return err
	}
	for _, run := range runs {
		view := chainViewFor(tasks, run.DeliveryID)
		switch run.State {
		case "queued":
			// The operator's pause keeps a queued run queued; claimed runs
			// below keep going either way.
			if holdQueuedRun(ctx, config, services.Backlog, run, runDirectory(config, run.DeliveryID), logger) {
				continue
			}
			if err := startQueuedRun(ctx, config, services, hermes, run, view, logger); err != nil {
				logger.Error("chain start failed", "run", run.RunID, "error", err.Error())
			}
		case "claimed":
			if err := advanceClaimedRun(ctx, config, services, hermes, run, view, logger); err != nil {
				logger.Error("chain advance failed", "run", run.RunID, "error", err.Error())
			}
		case "terminal_report_pending":
			// The terminal report was begun but never completed: the comment
			// may be on the ticket, the completion was lost to a transient
			// store failure, and the quick retries met the run's own lease.
			// A question-sealed run's ending belongs to the reception tick;
			// every other one is re-submitted as it was — never healed,
			// requeued or reclassified, which would rerun or contradict it.
			if run.QuestionSealed {
				continue
			}
			if err := resubmitPendingTerminal(ctx, config, services, hermes, run, view, logger); err != nil {
				logger.Error("pending terminal report not completed", "run", run.RunID, "error", err.Error())
			}
		case "terminal":
			// An answer after a question's deadline is answered, not acted on.
			noticeLateAnswer(ctx, config, services.Backlog, run, runDirectory(config, run.DeliveryID), logger)
			// The run itself is closed; what may remain is the debug
			// role's post-merge observation, or the v2 delivery
			// continuation (config makes the two mutually exclusive).
			if err := syncE2E(ctx, config, services, hermes, run, tasks, logger); err != nil {
				logger.Error("e2e sync failed", "run", run.RunID, "error", err.Error())
			}
			recordFeatureMerge(ctx, config, run, runDirectory(config, run.DeliveryID), logger)
			if err := syncDeliver(ctx, config, services, hermes, run, tasks, logger); err != nil {
				logger.Error("deliver sync failed", "run", run.RunID, "error", err.Error())
			}
		default:
			// awaiting_answer and question_report_pending belong to the
			// reception tick's own machinery.
		}
	}
	return nil
}

// chainView is one delivery's chain as the board tells it: the current
// (highest) round's cards by stage, and every living chain card of any
// round for retirement sweeps.
// chainView is what the board says about one delivery: the newest
// implementation round and its cards, and — for the investigating
// designer's shapes — the newest design round and its cards, which count
// separately (`:d<N>` keys) because a design can restart without an
// implementation having happened.
type chainView struct {
	round int
	cards map[string]runtime.BoardTask
	// designRound is the newest design round on the board (0 when none).
	designRound int
	designCards map[string]runtime.BoardTask
	all         []runtime.BoardTask
	// board is the whole listing this tick read. The delivery's cards — the
	// ones that merge, wait for the workflow and observe a screen — are
	// keyed outside the chain's namespace, so they are not in any field
	// above and the delivery hand-off needs the listing itself.
	board []runtime.BoardTask
}

func chainViewFor(tasks []runtime.BoardTask, deliveryID string) chainView {
	view := chainView{cards: map[string]runtime.BoardTask{}, designCards: map[string]runtime.BoardTask{}, board: tasks}
	rounds := map[int]map[string]runtime.BoardTask{}
	designRounds := map[int]map[string]runtime.BoardTask{}
	for _, task := range tasks {
		delivery, stage, round, ok := runtime.ParseChainCardKey(task.IdempotencyKey)
		if !ok || delivery != deliveryID || task.Status == "archived" {
			continue
		}
		view.all = append(view.all, task)
		if runtime.IsDesignStage(stage) {
			if designRounds[round] == nil {
				designRounds[round] = map[string]runtime.BoardTask{}
			}
			designRounds[round][stage] = task
			if round > view.designRound {
				view.designRound = round
			}
			continue
		}
		if rounds[round] == nil {
			rounds[round] = map[string]runtime.BoardTask{}
		}
		rounds[round][stage] = task
		if round > view.round {
			view.round = round
		}
	}
	if view.round > 0 {
		view.cards = rounds[view.round]
	}
	if view.designRound > 0 {
		view.designCards = designRounds[view.designRound]
	}
	return view
}

// hasChain reports whether the board carries any live card of the delivery.
func (v chainView) hasChain() bool { return v.round > 0 || v.designRound > 0 }

// rounds is the pair EnsureChainFor keys new cards by.
func (v chainView) rounds() runtime.ChainRounds {
	return runtime.ChainRounds{Design: v.designRound, Implement: v.round}
}

// card returns the newest round's card of a stage, whichever counter it lives in.
func (v chainView) card(stage string) (runtime.BoardTask, bool) {
	if runtime.IsDesignStage(stage) {
		task, ok := v.designCards[stage]
		return task, ok
	}
	task, ok := v.cards[stage]
	return task, ok
}

func (v chainView) existingKeys(deliveryID string) map[string]runtime.BoardTask {
	existing := make(map[string]runtime.BoardTask, len(v.cards)+len(v.designCards))
	for stage, task := range v.cards {
		existing[runtime.ChainCardKey(deliveryID, stage, v.round)] = task
	}
	for stage, task := range v.designCards {
		existing[runtime.ChainCardKey(deliveryID, stage, v.designRound)] = task
	}
	return existing
}

func runDirectory(config runtime.Config, deliveryID string) string {
	return runtime.RunDirectory(config.Chain, deliveryID)
}

func readTargetToken(config runtime.Config) (string, error) {
	raw, err := os.ReadFile(config.Chain.TargetTokenPath)
	if err != nil {
		return "", errors.New("target token unreadable")
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", errors.New("target token file is empty")
	}
	return token, nil
}

// stoppedWhileWorking is the opportunistic stop read between finishing a
// piece of work and acting on it. Unlike the fail-closed read before the
// claim, an unreadable listing here proceeds and says so: the work is
// already spent, and refusing to act on it would re-run the whole gate on
// every retry. Proceeding is safe in a way it was not before — a question
// asked in spite of a stop is ended by the answer wait, which reads the
// stop itself. onFailure names, in the log, what proceeding means here.
func stoppedWhileWorking(ctx context.Context, backlog commentLister, allowedCreatorID, issueID int64, runID, onFailure string, logger Logger) bool {
	stopped, err := stopRequested(ctx, backlog, allowedCreatorID, issueID)
	if err != nil {
		logger.Error("stop re-check unreadable; "+onFailure, "run", runID, "error", err.Error())
		return false
	}
	return stopped
}

// startQueuedRun claims one queued run by name, prepares its run directory
// from scratch and creates the first round — or ends the run honestly when
// preparation itself decides it (an intake rejection, a readiness stop, a
// readiness question). A queued run with leftover chain cards is a resumed
// or crash-recovered one; the old chain leaves dispatch entirely before the
// fresh attempt, whose preparation also clears the directory.
func startQueuedRun(
	ctx context.Context,
	config runtime.Config,
	services *runtime.Services,
	hermes *runtime.Hermes,
	run state.RunOverview,
	view chainView,
	logger Logger,
) error {
	for _, task := range view.all {
		if err := hermes.Archive(ctx, task.ID); err != nil {
			return fmt.Errorf("stale chain card %s: %w", task.ID, err)
		}
	}
	runDir := runDirectory(config, run.DeliveryID)
	if err := os.MkdirAll(runDir, 0o711); err != nil {
		return err
	}
	now := time.Now().UTC()
	// A budget or session hold throttles its own retry: the run stays
	// queued and unclaimed until the interval since the refusal has passed.
	if budgetHeldRecently(runDir, now) || sessionHeldRecently(runDir, now) {
		return nil
	}
	envelope, disposition, err := services.Store.Pull(ctx, hook.PullClaimRequest{
		SpaceKey:            config.Tracker.SpaceKey,
		ProjectID:           config.Tracker.ProjectID,
		ProjectKey:          config.Tracker.ProjectKey,
		AllowedCreatorID:    config.Tracker.AllowedCreatorID,
		AllowedActivityType: config.Tracker.AllowedActivityType,
		RunID:               run.RunID,
		Target:              config.Target(),
		Owner:               config.Owner(chainOwnerRunID(run.DeliveryID)),
		IssuedAt:            now,
		ClaimedAt:           now,
		ClockSkew:           2 * time.Minute,
	})
	if err != nil {
		return fmt.Errorf("pull failed: %w", err)
	}
	if disposition != hook.PullAcquired {
		logger.Info("queued run not claimable this tick", "run", run.RunID, "disposition", string(disposition))
		return nil
	}
	// A stop request already on the ticket cancels the run before any model
	// work is spent. The check sits before the intake pipeline on purpose:
	// when the comment listing fails here, the retried tick has lost nothing,
	// whereas a failure after the readiness gate would re-run the whole
	// assessment on every retry. A stop arriving later is honoured at the
	// next round boundary.
	stopped, err := stopRequested(ctx, services.Backlog, config.Tracker.AllowedCreatorID, envelope.Snapshot.IssueID)
	if err != nil {
		return fmt.Errorf("stop check before intake: %w", err)
	}
	if stopped {
		terminal := runner.NewTerminal(config, services, envelope, chainOwnerRunID(run.DeliveryID), runDir, logger)
		return terminal.Report(ctx, hook.TerminalCancelled, runner.Outcome{Code: hook.TerminalCancelled}, "")
	}
	// Money before work: a key out of budget holds the run here. The claim
	// is left to the next tick's recovery (a claim with no chain goes back
	// to the queue) and the hold file throttles the next probe.
	if checkBudgets(ctx, config, services.Backlog, run, runDir, envelope.Snapshot.IssueID, os.Getenv, budgetProbeClient, logger) {
		return nil
	}
	// The observation browser's way in before the work: a destination the
	// browser cannot sign in to would end the run as an unjudged screen.
	// The login that lands renews the jar for the observations to come.
	if checkSessions(ctx, config, services.Backlog, run, runDir, envelope.Snapshot.IssueID, liveSessionRenewer(os.Getenv), logger) {
		return nil
	}
	token, err := readTargetToken(config)
	if err != nil {
		return err
	}
	pipeline := &runner.Pipeline{
		Config: config, Services: services, Envelope: envelope,
		Workspace: runDir, TargetToken: token, Logger: logger,
	}
	_, outcome, runErr := pipeline.PrepareChainRun(ctx)
	terminal := runner.NewTerminal(config, services, envelope, chainOwnerRunID(run.DeliveryID), runDir, logger)
	if outcome.QuestionDecisionPath != "" {
		// The gate takes minutes, and a stop written while it ran is a stop.
		// Without this the question went out to a requester who had written
		// 「停止」 eight seconds earlier, and the ticket then sat in the
		// answer wait (live 2026-09-25).
		if stoppedWhileWorking(ctx, services.Backlog, config.Tracker.AllowedCreatorID,
			envelope.Snapshot.IssueID, run.RunID, "asking anyway", logger) {
			repository, repositoryErr := readField(runDir, "ticket-draft.json", "repository")
			if repositoryErr != nil {
				repository = ""
			}
			return terminal.Report(ctx, hook.TerminalCancelled, runner.Outcome{Code: hook.TerminalCancelled}, repository)
		}
		return terminal.AskQuestion(ctx, outcome.QuestionDecisionPath)
	}
	if runErr != nil || outcome.Code != "" {
		if runErr != nil {
			logger.Error("chain preparation error", "run", run.RunID, "error", runErr.Error())
		}
		if outcome.Code == "" {
			outcome.Code = hook.TerminalInternalFailed
		}

		return terminal.Report(ctx, outcome.Code, outcome, pipeline.Repository())
	}
	// Readiness passed. Post the implementation-plan notice — a notice, not
	// a gate: a failed post is logged and the run continues.
	if posted, reason := services.Tick.PostPlanComment(ctx, envelope.DeliveryID,
		hook.PlanCommentContent(envelope.Snapshot.RunID, loadPlanFacts(runDir))); !posted {
		logger.Error("plan notice not posted; run continues", "run", run.RunID, "reason", reason)
	}
	// One opportunistic re-check before the first cards, for a stop request
	// that arrived while the gate was running. Best-effort on purpose: a
	// listing failure here must not send the whole gate into a retry loop,
	// so it logs and proceeds — the fail-closed checks are the claim-time
	// one above and every round boundary after this.
	if stopped, stopErr := stopRequested(ctx, services.Backlog, config.Tracker.AllowedCreatorID, envelope.Snapshot.IssueID); stopErr != nil {
		logger.Error("stop re-check unreadable; proceeding", "run", run.RunID, "error", stopErr.Error())
	} else if stopped {
		repository, repositoryErr := readField(runDir, "ticket-draft.json", "repository")
		if repositoryErr != nil {
			repository = ""
		}
		return terminal.Report(ctx, hook.TerminalCancelled, runner.Outcome{Code: hook.TerminalCancelled}, repository)
	}
	plan, err := chainPlanFor(config, runDir, run, logger)
	if err != nil {
		// Fail closed: a request the decision routed to the investigating
		// designer must not be handed to the implementer instead.
		logger.Error("chain shape unavailable; ending honestly", "run", run.RunID, "error", err.Error())
		return terminal.Report(ctx, hook.TerminalInternalFailed, runner.Outcome{Code: hook.TerminalInternalFailed}, pipeline.Repository())
	}
	if plan.Shape == runtime.ShapeImplement {
		if err := pipeline.RenderImplementInstruction(ctx, 1); err != nil {
			return err
		}
	}
	rounds := runtime.ChainRounds{Design: 1, Implement: 1}
	terminalCard, err := runtime.EnsureChainFor(ctx, hermes, config.Chain, plan, nil, run.DeliveryID, run.RunID, run.Summary, rounds)
	if err != nil {
		return err
	}
	logger.Info("chain created", "run", run.RunID, "shape", string(plan.Shape), "round", 1, "terminal_card", terminalCard)
	return nil
}

// chainPlanFor reads the shape the readiness decision asks for. It fails
// closed: a decision that cannot be read, or a shape whose profiles the pod
// does not have, is an error the caller ends the run with — the decision
// said "investigate" or "design first", and running the implementer instead
// would silently do something else with the requester's ticket.
func chainPlanFor(config runtime.Config, runDir string, run state.RunOverview, logger Logger) (runtime.ChainPlan, error) {
	plan, err := runner.ChainPlanFromDecision(runDir, config.ConsumerConfigPath)
	if err != nil {
		return runtime.ChainPlan{}, fmt.Errorf("readiness decision gives no chain shape: %w", err)
	}
	if plan.Shape != runtime.ShapeImplement && !config.Chain.Profiles.DesignEnabled() {
		logger.Error("the decision asks for the investigating designer but the pod has no design profiles", "run", run.RunID, "shape", string(plan.Shape))
		return runtime.ChainPlan{}, errors.New("the pod has no profiles for the investigating designer's cards")
	}
	if plan.Shape == runtime.ShapeDesign {
		// The design shape ends in the applier's card, which runs the
		// consumer's agents.applier launch; without one the run would pay
		// for the investigation, the design and two judges and then die
		// at the apply card. Refused here, before any card exists.
		hasApplier, err := consumerHasApplier(config.ConsumerConfigPath)
		if err != nil {
			return runtime.ChainPlan{}, err
		}
		if !hasApplier {
			logger.Error("the decision asks for a design but the consumer configures no applier launch (agents.applier)", "run", run.RunID)
			return runtime.ChainPlan{}, errors.New("the design shape needs the applier's launch (agents.applier)")
		}
	}
	return plan, nil
}

// consumerHasApplier reads, leniently, whether the consumer configuration
// gives the applier a launch.
func consumerHasApplier(consumerConfigPath string) (bool, error) {
	raw, err := os.ReadFile(consumerConfigPath)
	if err != nil {
		return false, errors.New("consumer config unreadable")
	}
	var parsed struct {
		Agents struct {
			Applier *struct {
				Command string `json:"command"`
			} `json:"applier"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return false, errors.New("consumer config invalid")
	}
	return parsed.Agents.Applier != nil && parsed.Agents.Applier.Command != "", nil
}

// advanceClaimedRun keeps one in-flight chain honest: heal missing cards,
// report a finished publish, and classify any failed card from the sealed
// artifacts — a revise regenerates the next round, an impasse question goes
// to the requester, everything else becomes an honest terminal. A claim
// with no chain at all is a preparation that died between claiming and
// creating cards; the run goes back to the queue and restarts cleanly.
// engineChangedUnderRun answers whether this delivery's records were
// written by an engine other than the one running now, and names it.
func engineChangedUnderRun(config runtime.Config, runDir string) (string, bool) {
	sha, err := readField(runDir, "ticket-draft.json", "tool_sha")
	if err != nil || sha == "" || config.Identity.EngineSHA == "" {
		return "", false
	}
	return sha, sha != config.Identity.EngineSHA
}

func advanceClaimedRun(
	ctx context.Context,
	config runtime.Config,
	services *runtime.Services,
	hermes *runtime.Hermes,
	run state.RunOverview,
	view chainView,
	logger Logger,
) error {
	if !view.hasChain() {
		logger.Info("claimed run has no chain; requeueing", "run", run.RunID)
		return services.Store.RecoverLostClaim(ctx, run.Key, run.ClaimedAt, time.Now().UTC())
	}
	runDir := runDirectory(config, run.DeliveryID)
	// Every stage refuses a draft written by a different engine, because a
	// record has to re-derive under the engine that reads it. So a
	// delivery that was in flight when the engine was updated could not
	// take another step - and the failed card was healed and dispatched
	// again, every minute, for ever. Measured live: a delivery sat in
	// "工程の復旧処理中" for thirty-one minutes after an upgrade, saying
	// "ticket draft could not be read" each time.
	//
	// It starts again instead. The work so far is redone under the engine
	// that is running, which costs a few minutes; the alternative is a
	// delivery that never ends and never reports (完遂率を最優先、発注者
	// 指示 2026-09-17).
	if wrote, changed := engineChangedUnderRun(config, runDir); changed {
		logger.Info("the engine changed under this delivery; starting it again",
			"run", run.RunID, "wrote", wrote, "running", config.Identity.EngineSHA)
		for _, task := range view.all {
			if err := hermes.Archive(ctx, task.ID); err != nil {
				return err
			}
		}
		return services.Store.RecoverLostClaim(ctx, run.Key, run.ClaimedAt, time.Now().UTC())
	}
	envelope, err := readEnvelope(runDir, run.DeliveryID)
	if err != nil {
		// Without the sealed envelope nothing can be reported; a fresh
		// attempt rebuilds the directory from the ledger's own copy.
		logger.Error("run envelope unreadable; requeueing", "run", run.RunID, "error", err.Error())
		for _, task := range view.all {
			if archiveErr := hermes.Archive(ctx, task.ID); archiveErr != nil {
				return archiveErr
			}
		}
		return services.Store.RecoverLostClaim(ctx, run.Key, run.ClaimedAt, time.Now().UTC())
	}
	plan, err := chainPlanFor(config, runDir, run, logger)
	if err != nil {
		logger.Error("chain shape unavailable for a claimed run; ending honestly", "run", run.RunID, "error", err.Error())
		terminal := runner.NewTerminal(config, services, envelope, chainOwnerRunID(run.DeliveryID), runDir, logger)
		repository, readErr := readField(runDir, "ticket-draft.json", "repository")
		if readErr != nil {
			repository = ""
		}
		if err := terminal.Report(ctx, hook.TerminalInternalFailed, runner.Outcome{Code: hook.TerminalInternalFailed}, repository); err != nil {
			return err
		}
		return archiveChain(ctx, hermes, view.all)
	}
	// An objection transition interrupted between "archive the round" and
	// "create the next design round" leaves the done design cards and an
	// objection record with no implementation cards; healing from here would
	// re-apply the objected design. Resume the transition instead.
	if plan.Shape == runtime.ShapeDesign && view.round == 0 && view.designRound > 0 {
		if objected, err := designObjectionRecorded(runDir, view.designRound); err == nil && objected {
			if _, decided := readField(runDir, fmt.Sprintf("history/design-%d/decision.json", view.designRound+1), "outcome"); decided != nil {
				logger.Info("resuming an interrupted objection transition", "run", run.RunID, "design_round", view.designRound)
				return nextDesignRoundOrEnd(ctx, config, services, hermes, envelope, run, view, plan, designCalledWrongLater, "the applier objected to the design (resumed)", logger)
			}
		}
	}
	rounds := view.rounds()
	if rounds.Design == 0 {
		rounds.Design = 1
	}
	if rounds.Implement == 0 {
		rounds.Implement = 1
	}
	if _, err := runtime.EnsureChainFor(ctx, hermes, config.Chain, plan, view.existingKeys(run.DeliveryID), run.DeliveryID, run.RunID, run.Summary, rounds); err != nil {
		return err
	}
	// An investigation-only delivery honours 「停止」 before its report is
	// posted: the stop is read here, at the one place the report leaves the
	// pod, and the run ends as cancelled with nothing posted.
	if plan.Shape == runtime.ShapeInvestigation && services.Backlog != nil {
		if stopped, err := stopRequested(ctx, services.Backlog, config.Tracker.AllowedCreatorID, envelope.Snapshot.IssueID); err != nil {
			logger.Error("stop check before the investigation report unreadable; proceeding", "run", run.RunID, "error", err.Error())
		} else if stopped {
			terminal := runner.NewTerminal(config, services, envelope, chainOwnerRunID(run.DeliveryID), runDir, logger)
			repository, readErr := readField(runDir, "ticket-draft.json", "repository")
			if readErr != nil {
				repository = ""
			}
			if err := terminal.Report(ctx, hook.TerminalCancelled, runner.Outcome{Code: hook.TerminalCancelled}, repository); err != nil {
				return err
			}
			return archiveChain(ctx, hermes, view.all)
		}
	}
	postDesignComments(ctx, config, services, run, view, plan, logger)
	stages := runtime.ChainStagesFor(config.Chain, plan)
	last := stages[len(stages)-1]
	if task, ok := view.card(last.Name); ok && task.Status == "done" {
		if plan.Shape == runtime.ShapeInvestigation {
			return reportInvestigated(ctx, config, services, envelope, run, view, logger)
		}
		// The change is published. How much further it travels is the
		// destination's own setting, and the delivery carries it there
		// before anything calls it a success (deliver_depth.go).
		return completeDelivery(ctx, config, services, hermes, envelope, run, view, plan, logger)
	}
	for _, stage := range stages {
		task, ok := view.card(stage.Name)
		if !ok || !failedCardStatuses[task.Status] {
			continue
		}
		// The cards mode never parks a card for a question, so a
		// needs_input block is as terminal as any other — logged with its
		// kind so an unexpected one is diagnosable from the record.
		//
		// The class is the card's own account of what kind of thing went
		// wrong. It decides nothing here — the classification below is
		// unchanged — but it is what tells a disk that filled from a model
		// that would not answer, which the card's exit code never could.
		logger.Info("chain card failed", "run", run.RunID, "stage", stage.Name,
			"status", task.Status, "block_kind", task.BlockKind,
			"class", failedCardClass(runDir, stage.Name, view))
		if handled, err := handleDesignChainFailure(ctx, config, services, hermes, envelope, run, view, plan, stage.Name, logger); handled {
			return err
		}
		return handleChainFailure(ctx, config, services, hermes, envelope, run, view, stage.Name, logger)
	}
	return nil
}

// failedCardClass names what the failed card said went wrong, for the line
// this tick logs. "none" means the card sealed nothing to read: one dispatched
// by an older engine, or one that died before it could write.
func failedCardClass(runDir, stageName string, view chainView) string {
	round := view.round
	if runtime.IsDesignStage(stageName) {
		round = view.designRound
	}
	if failure, ok := runner.ReadStageFailure(runDir, stageName, round); ok {
		return string(failure.Class)
	}
	return "none"
}

// pendingTerminalAction says what the tick does with a run whose terminal
// report stayed pending: resubmit the report it already decided, or leave it
// to a person when the pieces the report is built from are gone.
type pendingTerminalAction string

const (
	pendingTerminalResubmit      pendingTerminalAction = "resubmit"
	pendingTerminalNeedsOperator pendingTerminalAction = "needs_operator"
)

// classifyPendingTerminal keeps the re-submit honest. The code was decided
// when the report was first begun and sits in the run row; only two reports
// are rebuilt from the chain cards and the artifacts they left — a success
// (its stage and evidence) and an investigated ending (its sealed report and
// attachments) — so those need the cards, and everything else (cancelled at
// claim, rejected or failed in preparation, a chain that could not be
// derived, a failed model stage) is re-submitted from the row alone. Nothing
// is ever requeued: the claimed-run path would rerun the whole delivery.
func classifyPendingTerminal(run state.RunOverview, view chainView) (pendingTerminalAction, string) {
	if run.TerminalCode == "" || !hook.TerminalCode(run.TerminalCode).Valid() {
		return pendingTerminalNeedsOperator, "the pending report carries no terminal code"
	}
	switch hook.TerminalCode(run.TerminalCode) {
	case hook.TerminalSuccess, hook.TerminalInvestigated:
		if !view.hasChain() {
			return pendingTerminalNeedsOperator, "the chain cards are gone; a " + run.TerminalCode + " report is rebuilt from them"
		}
	}
	return pendingTerminalResubmit, ""
}

// resubmitPendingTerminal submits the same terminal report again. The store
// re-acquires a pending report whose digest matches once its lease expired,
// the report service finds the posted comment by its marker, and the
// completion lands; the cards are then archived as they would have been.
// Nothing here heals cards, checks for a stop, or requeues: the outcome was
// decided when the report was first begun (live 2026-09-05: a run whose
// completion failed stayed pending for over an hour with nothing driving it).
// A report from before any card existed has no cards to archive and no run
// directory artifacts to lean on; its envelope comes from the ledger's copy.
func resubmitPendingTerminal(
	ctx context.Context,
	config runtime.Config,
	services *runtime.Services,
	hermes *runtime.Hermes,
	run state.RunOverview,
	view chainView,
	logger Logger,
) error {
	action, reason := classifyPendingTerminal(run, view)
	if action != pendingTerminalResubmit {
		logger.Error("pending terminal report needs an operator", "run", run.RunID, "code", run.TerminalCode, "reason", reason)
		return nil
	}
	runDir := runDirectory(config, run.DeliveryID)
	envelope, err := pendingEnvelope(runDir, run)
	if err != nil {
		logger.Error("pending terminal report needs an operator", "run", run.RunID, "code", run.TerminalCode, "reason", "run envelope unreadable: "+err.Error())
		return nil
	}
	code := hook.TerminalCode(run.TerminalCode)
	// An incomplete end carries its reason and last objection again: the
	// first submission's comment may be the one that failed (MINOR on #93).
	var evidence map[string]string
	switch code {
	case hook.TerminalInvestigationIncomplete:
		evidence = incompleteEvidence(runDir, view.designRound)
	case hook.TerminalModelFailed:
		// The same hazard, one code along: the step this run ended on is
		// not in the run row, and the board that named it is gone by now.
		// The first attempt wrote it down for exactly this.
		evidence = runner.RecordedFailedStep(runDir)
	}
	switch code {
	case hook.TerminalSuccess:
		return reportChainSuccess(ctx, config, services, envelope, run, logger)
	case hook.TerminalInvestigated:
		return reportInvestigated(ctx, config, services, envelope, run, view, logger)
	}
	terminal := runner.NewTerminal(config, services, envelope, chainOwnerRunID(run.DeliveryID), runDir, logger)
	// What a rebuilt report carries can depend on the repository it names:
	// a stop that reached staging cites a commit in that repository, and
	// all of it is sealed into the digest this re-submission has to
	// reproduce. Each candidate below is rebuilt with its own.
	evidenceFor := func(repository string) map[string]string {
		if code == hook.TerminalCancelled {
			return stoppedDeliveryEvidence(runDir, repository, code)
		}
		return evidence
	}
	repository, err := pendingRepository(ctx, terminal, runDir, run, code, evidenceFor)
	if err != nil {
		logger.Error("pending terminal report needs an operator", "run", run.RunID, "code", run.TerminalCode, "reason", err.Error())
		return nil
	}
	if err := terminal.Report(ctx, code, runner.Outcome{Code: code, Evidence: evidenceFor(repository)}, repository); err != nil {
		return err
	}
	logger.Info("pending terminal report completed", "run", run.RunID, "code", string(code))
	return archiveChain(ctx, hermes, view.all)
}

// pendingRepository picks the repository the pending report was begun with.
// The first report named the consumer repository only once the run had
// resolved it (Pipeline.Repository), and "" before that — while the run
// directory may hold a ticket draft naming the repository either way (the
// draft is written before the consumer is resolved, and a directory can
// carry a previous attempt's draft). The store seals the repository into
// the report digest, so sending the other one would be refused as a
// conflict on every tick — the same silence this path exists to end. Both
// candidates are rebuilt and the one that reproduces the row's digest is
// sent; neither matching is left to a person.
func pendingRepository(ctx context.Context, terminal *runner.Terminal, runDir string, run state.RunOverview, code hook.TerminalCode, evidenceFor func(string) map[string]string) (string, error) {
	if run.TerminalReportSHA256 == "" {
		return "", errors.New("the pending report carries no digest")
	}
	candidates := []string{""}
	if drafted, err := readField(runDir, "ticket-draft.json", "repository"); err == nil && drafted != "" {
		candidates = []string{drafted, ""}
	}
	for _, candidate := range candidates {
		digest, err := terminal.ReportDigest(ctx, code, runner.Outcome{Code: code, Evidence: evidenceFor(candidate)}, candidate)
		if err != nil {
			if candidate != "" {
				// The draft's value could not be shaped into a report, so
				// the first attempt cannot have sent it either; the other
				// candidate decides.
				continue
			}
			return "", err
		}
		if digest == run.TerminalReportSHA256 {
			return candidate, nil
		}
	}
	return "", errors.New("no rebuilt report reproduces the pending report's digest")
}

func reportChainSuccess(
	ctx context.Context,
	config runtime.Config,
	services *runtime.Services,
	envelope hook.DispatchEnvelope,
	run state.RunOverview,
	logger Logger,
) error {
	runDir := runDirectory(config, run.DeliveryID)
	raw, err := os.ReadFile(filepath.Join(runDir, runner.ChainOutcomeFile))
	if err != nil {
		return errors.New("chain outcome artifact unreadable")
	}
	var outcome runner.ChainOutcome
	if err := json.Unmarshal(raw, &outcome); err != nil || outcome.Stage < 1 {
		return errors.New("chain outcome artifact invalid")
	}
	repository, err := readField(runDir, "ticket-draft.json", "repository")
	if err != nil {
		return errors.New("run repository unreadable")
	}
	evidence := outcome.Evidence
	reached := ""
	// What the delivery actually reached, read back from the records its
	// cards sealed (deliver_depth.go). Only a delivery whose depth this
	// engine decided has one: a report begun by an engine from before the
	// depth moved into the run carries the evidence it carried then, which
	// is what its re-submission has to reproduce byte for byte.
	if depth, sealed := readDepthRecord(runDir); sealed {
		reached, evidence, _ = deliveryOutcome(runDir, repository, depth, evidence)
	}
	terminal := runner.NewTerminal(config, services, envelope, chainOwnerRunID(run.DeliveryID), runDir, logger)
	if err := terminal.Report(ctx, hook.TerminalSuccess, runner.Outcome{Stage: outcome.Stage, Evidence: evidence}, repository); err != nil {
		return err
	}
	logger.Info("chain delivered", "run", run.RunID, "round", outcome.Stage, "reached", reached)
	return nil
}

func handleChainFailure(
	ctx context.Context,
	config runtime.Config,
	services *runtime.Services,
	hermes *runtime.Hermes,
	envelope hook.DispatchEnvelope,
	run state.RunOverview,
	view chainView,
	stageName string,
	logger Logger,
) error {
	runDir := runDirectory(config, run.DeliveryID)
	// stopReason, when set, is the requester-facing sentence for a run this
	// process is ending itself; the report below folds it into the trail.
	stopReason := ""
	terminal := runner.NewTerminal(config, services, envelope, chainOwnerRunID(run.DeliveryID), runDir, logger)
	action, code := classifyChainFailure(stageName, func() (string, error) {
		return readField(runDir, fmt.Sprintf("history/stage-%d/decision.json", view.round), "outcome")
	}, func() bool {
		return worker.RoundReturnedWork(filepath.Join(runDir, "history"), view.round)
	})
	// What the tick decided, beside the card it found. The card is only
	// where the chain stopped moving: a validate card blocked because the
	// round was sent back reads as "the failure is validate" unless the
	// decision that sent it back is in the record too (live 2026-09-17).
	//
	// The code is deliberately not on this line. Two of the three actions
	// usually do not report it - regenerate starts another round, and
	// answering a returned round starts the same one again - and the value
	// classifyChainFailure carries alongside them is what a failure of that
	// would be reported as. Logged here it named model_failed for the very
	// run that ends as design_rounds_spent, which is the misreading this
	// line exists to prevent. The code the run does end with is on the
	// "chain terminalized" line, which is written after the report is
	// accepted rather than before it (review of #201).
	logger.Info("chain failure classified", "run", run.RunID, "stage", stageName, "action", action.String())
	// Three of the endings this could choose were never decisions about the
	// request: something broke, and the delivery was over. The ladder takes
	// those instead — it reads what the card said went wrong, changes
	// something, and dispatches the stage again — and this is the only way
	// out of it that reports anything: the requester's own stop, or a limit
	// on the attempts that an operator deliberately configured.
	if action == actionReport && ladderOwns(code) {
		// The shape says which cards the chain has, so the ladder cannot
		// rebuild anything without it. A delivery that reaches this tick has
		// one — the tick refuses to advance a claimed run whose shape it
		// cannot read, well before here — so this arm is the honest answer
		// to something that should not happen rather than a path with a
		// remedy: report as the classification decided, and say why the
		// ladder did not get its turn.
		plan, planErr := chainPlanFor(config, runDir, run, logger)
		if planErr != nil {
			logger.Error("the chain shape could not be read; the failed card is reported rather than climbed",
				"run", run.RunID, "stage", stageName, "error", planErr.Error())
		} else {
			verdict, err := climbLadder(ctx, newClimb(config, services, hermes, envelope, run, view, plan, stageName, logger))
			switch {
			case err != nil:
				return err
			case verdict == ladderHandled:
				return nil
			case verdict == ladderStopped:
				code = hook.TerminalCancelled
			}
		}
	}
	switch action {
	case actionAnswerReturn:
		verdict, answerErr := answerReturnedWork(ctx, config, services, hermes, envelope, run, view, logger)
		if answerErr != nil {
			return answerErr
		}
		switch verdict {
		case returnRelaunched:
			return nil
		case returnStopped:
			code = hook.TerminalCancelled
		}
	case actionRegenerate:
		limit, limitErr := consumerRoundLimit(config.ConsumerConfigPath)
		if limitErr != nil {
			return limitErr
		}
		stopped, stopErr := stopRequested(ctx, services.Backlog, config.Tracker.AllowedCreatorID, envelope.Snapshot.IssueID)
		if stopErr != nil {
			return fmt.Errorf("stop check before round %d: %w", view.round+1, stopErr)
		}
		switch {
		case stopped:
			// The requester asked the run to stop: finished cards stay
			// finished, no next round is created, and the run ends honestly.
			code = hook.TerminalCancelled
		case view.round >= worker.StageCeiling:
			// The highest round number any record may carry. Past it the
			// instruction would still be rendered and the implementer would
			// still run, and then the seal would refuse the round as one no
			// record can name — a failure the ladder classes as the model's
			// and re-dispatches, for ever, having paid for the agent every
			// time round. The design side stops itself the same way, on
			// errDesignRoundLimit.
			//
			// Nothing reaches this by converging or by being ruled on. A
			// delivery that gets here has been round fifty times.
			logger.Error("the delivery reached the highest round any record can carry",
				"run", run.RunID, "round", view.round, "ceiling", worker.StageCeiling)
		case limit > 0 && view.round >= limit:
			// An operator said how many rounds they were willing to pay for
			// and this is the last of them. The code the classification
			// carried stands, and it is the one place any of these codes is
			// still produced. Nothing reaches here by default: a
			// configuration that names no limit is unbounded, and a delivery
			// that stops moving is ruled on below rather than counted out.
		default:
			// Why the round advanced, when it advanced over a refused
			// validation rather than over an objection. Both arrive here as
			// one action, and only the sealed record tells them apart: the
			// line above says a round was regenerated and says nothing about
			// a validation that had already been found to fail. The digest is
			// the field worth logging — two rounds carrying the same one are
			// a run repeating itself, and that is readable from the log
			// without opening the run directory.
			if failure, sealed := runner.ReadValidationFailure(runDir, view.round); sealed {
				logger.Info("the deterministic validation refused the round; the next round is told what it printed",
					"run", run.RunID, "round", view.round, "step", failure.Step, "output_sha256", failure.OutputSHA256)
			}
			// Before another round is paid for, whether this one did
			// anything. A round that objected to exactly what the round
			// before it objected to, or that wrote exactly the same bytes,
			// is not going to be answered by starting another one just like
			// it; the engine rules on it instead. Ruling may put this same
			// round back to work without the objections the ticket does not
			// require, and then this tick is done.
			//
			// Asked before the chain's shape is read, and not conditional on
			// it: what the rounds did is readable from the records whatever
			// shape the delivery has, and a shape that will not read must
			// not be the reason a deadlock goes on being paid for.
			ruled, ruleErr := ruleOnStagnation(ctx, config, services, hermes, envelope, run, view, logger)
			if ruleErr != nil {
				return ruleErr
			}
			if ruled {
				return nil
			}
			plan, planErr := chainPlanFor(config, runDir, run, logger)
			if planErr != nil || plan.Shape != runtime.ShapeDesign {
				stopped, err := regenerateRound(ctx, services, hermes, config, envelope, run, view, logger)
				if err != nil {
					return err
				}
				if !stopped {
					return nil
				}
				// The requester asked to stop while the deadlock was being
				// ruled on. The stop read at the top of this arm was minutes
				// ago and a model call happened in between.
				code = hook.TerminalCancelled
				break
			}
			// A design-backed delivery: a reviewer who found the design
			// itself wrong sends the run back to the designer; anything
			// else is another application of the same design.
			designWrong, readErr := designWrongForRound(runDir, config.ConsumerConfigPath, view.round)
			switch {
			case readErr != nil:
				// Not an answer about the design: one of the round's sealed
				// reviews could not be read, so nothing here knows whether
				// the design was called wrong.
				//
				// A record that names its seat is that seat's own failure to
				// leave a usable answer, and the ladder asks it again from
				// somewhere else. Only a delivery with no seat to ask — none
				// configured, or a configuration that will not read — still
				// ends here, because there is nothing left to change.
				logger.Error("the sealed reviews could not be read",
					"delivery_id", run.DeliveryID, "round", view.round, "error", readErr.Error())
				var unreadable *unreadableReview
				verdict := ladderSpent
				if errors.As(readErr, &unreadable) {
					climbed, climbErr := climbUnreadableReview(ctx, config, services, hermes, envelope, run, view, plan, unreadable.reviewer, logger)
					if climbErr != nil {
						return climbErr
					}
					if climbed == ladderHandled {
						return nil
					}
					verdict = climbed
				}
				if verdict == ladderStopped {
					code = hook.TerminalCancelled
				} else {
					code, stopReason = unreadableReviewsOutcome(view.round)
				}
			case designWrong:
				logger.Info("a review found the design itself wrong; the delivery goes back to the designer",
					"run", run.RunID, "round", view.round, "design_round", view.designRound)
				// At the design-round limit this ends the run as rounds
				// spent instead of returning the limit error every tick.
				return nextDesignRoundOrEnd(ctx, config, services, hermes, envelope, run, view, plan, designCalledWrongLater, "a review found the design itself wrong", logger)
			default:
				stopped, err := regenerateDesignBackedRound(ctx, services, hermes, config, envelope, run, view, plan, logger)
				if err != nil {
					return err
				}
				if !stopped {
					return nil
				}
				code = hook.TerminalCancelled
			}
		}
	}
	// The failure report carries the same round record a delivery would
	// have (#10); composition failure never blocks the report.
	if _, err := os.Stat(filepath.Join(runDir, "history", "stage-1")); err == nil {
		pipeline := &runner.Pipeline{Config: config, Workspace: runDir, Logger: logger}
		if stopReason != "" {
			pipeline.WriteStopReason(stopReason)
		}
		// A round that sealed no candidate is rendered from the
		// implementing agent's report, which cannot say which card then
		// blocked; the step's requester-facing name is this tick's to give.
		pipeline.NoteBlockedStep(failedStepFor(runDir, stageName, view.round))
		_ = pipeline.EnsureTrail(ctx)
		// The publish card records why a delivery stopped in its own
		// process; the recomposed trail would silently drop it otherwise.
		pipeline.AttachDeliveryStopReason()
	}
	repository, err := readField(runDir, "ticket-draft.json", "repository")
	if err != nil {
		repository = ""
	}
	var evidence map[string]string
	if code == hook.TerminalModelFailed {
		evidence = failedStepEvidence(config, runDir, stageName, view.round)

	}
	if err := terminal.Report(ctx, code, runner.Outcome{Code: code, Evidence: evidence}, repository); err != nil {
		return err
	}
	logger.Info("chain terminalized", "run", run.RunID, "stage", stageName, "code", string(code))
	return archiveChain(ctx, hermes, view.all)
}

// failureAction is what a failed card's artifacts call for.
type failureAction int

const (
	// actionReport ends the run with the accompanying terminal code.
	actionReport failureAction = iota
	// actionRegenerate retires the round's remnant and starts the next.
	actionRegenerate
	// actionAnswerReturn answers a round whose agent handed the work back
	// and starts that same round again.
	actionAnswerReturn
)

// String names the action for the record. The type is an int, so a plain
// conversion would log one unprintable rune. A fourth action added without
// a name here says so rather than borrowing one (review of #201).
func (a failureAction) String() string {
	switch a {
	case actionReport:
		return "report"
	case actionRegenerate:
		return "regenerate"
	case actionAnswerReturn:
		return "answer_return"
	default:
		return "unknown"
	}
}

// classifyChainFailure reads a failed card into an action. The card's state
// is only the alarm; the sealed artifacts are the classification: a decided
// revise goes on to the engine's own answer, a decided converge that still
// failed means the deterministic validation refused, and anything undecided
// is the machinery's own death.
func classifyChainFailure(stageName string, decision func() (string, error), returned func() bool) (failureAction, hook.TerminalCode) {
	switch stageName {
	case runtime.StagePublish:
		return actionReport, hook.TerminalReleaseFailed
	case runtime.StageImplement:
		// The implementer is told to change nothing and say why when it
		// cannot carry the request out. What it wrote is an answer, not a
		// breakdown — and it is not an ending either. The engine decides
		// what the report asked about, records what it decided, and starts
		// the same round again; nothing is handed to the requester.
		//
		// The code is what a failure of that answering would be reported
		// as, and it names the machinery rather than the agent: on this
		// path the agent answered, and the only way the delivery stops here
		// is the engine failing to act on it. A card that blocked for any
		// other reason left no such record and takes the arm below.
		if returned() {
			return actionAnswerReturn, hook.TerminalInternalFailed
		}
		return actionReport, hook.TerminalModelFailed
	case runtime.StageValidate:
		outcome, err := decision()
		if err != nil {
			return actionReport, hook.TerminalModelFailed
		}
		switch outcome {
		case "revise":
			// The code travels with the action rather than being a
			// placeholder. A regenerate reports nothing on the ordinary
			// path, but an operator who configured a round limit ends the
			// run out of this same arm, and there the code is what the
			// requester is told: the reviews did not agree within the
			// number of rounds that operator was willing to pay for. Said
			// as a model failure it would claim the AI had not answered,
			// and on this path both seats answered.
			return actionRegenerate, hook.TerminalNonconverged
		case "nonconverged":
			// A record this engine can no longer seal: the decide verb used
			// to rewrite a final-round revise as nonconverged, and that is
			// what ended a delivery on a count. An upgrade can still find
			// one sealed an hour ago, and it is reported as what it says.
			return actionReport, hook.TerminalNonconverged
		case "converged":
			// The judges passed the round and the deterministic validation
			// refused it. That used to end the delivery: the run reported
			// that validation had failed and stopped, leaving the build
			// output in the pod's log for a person to find and carry back.
			// The validate card now seals what was printed, and the next
			// round's instruction carries it, so the round is repeated with
			// the one thing it was missing instead of being abandoned.
			//
			// As in the revise arm, the code is what an operator's own
			// round limit reports out of this path: the validation refusing
			// the change, not the AI failing to answer. Without such a limit
			// the next round carries what the commands printed and this code
			// is produced nowhere.
			return actionRegenerate, hook.TerminalValidationFailed
		default:
			return actionReport, hook.TerminalModelFailed
		}
	default:
		return actionReport, hook.TerminalModelFailed
	}
}

// regenerateRound retires the failed round's undone remnant and creates the
// next round's chain on the same run directory: the implementer continues
// from the tree it already changed, told what the judges objected to.
// roundBoundaryStop reads 「停止」 at a round boundary. A tracker this
// deployment does not have cannot be asked, and a delivery whose requester
// cannot be heard goes on rather than stopping on a reader that is missing.
func roundBoundaryStop(ctx context.Context, services *runtime.Services, config runtime.Config, envelope hook.DispatchEnvelope) (bool, error) {
	if services == nil || services.Backlog == nil {
		return false, nil
	}
	stopped, err := stopRequested(ctx, services.Backlog, config.Tracker.AllowedCreatorID, envelope.Snapshot.IssueID)
	if err != nil {
		return false, fmt.Errorf("stop check before the next round: %w", err)
	}
	return stopped, nil
}

func regenerateRound(
	ctx context.Context,
	services *runtime.Services,
	hermes *runtime.Hermes,
	config runtime.Config,
	envelope hook.DispatchEnvelope,
	run state.RunOverview,
	view chainView,
	logger Logger,
) (bool, error) {
	// 「停止」 once more, here rather than only at the top of the tick.
	// Between the two there can be a model call — the engine ruling on a
	// deadlock takes up to two minutes — and this is the last thing between
	// a requester who has asked the delivery to stop and a round that
	// starts spending again. Reported to the caller, which ends the run,
	// because only the caller has the report to end it with.
	if stopped, err := roundBoundaryStop(ctx, services, config, envelope); err != nil || stopped {
		return stopped, err
	}
	for _, task := range view.all {
		if task.Status == "done" {
			continue
		}
		if err := hermes.Archive(ctx, task.ID); err != nil {
			return false, err
		}
	}
	pipeline := &runner.Pipeline{Config: config, Workspace: runDirectory(config, run.DeliveryID), Logger: logger}
	if err := pipeline.RenderImplementInstruction(ctx, view.round+1); err != nil {
		return false, err
	}
	terminalCard, err := runtime.EnsureChain(ctx, hermes, config.Chain, nil, run.DeliveryID, run.RunID, run.Summary, view.round+1)
	if err != nil {
		return false, err
	}
	logger.Info("round regenerated", "run", run.RunID, "round", view.round+1, "terminal_card", terminalCard)
	return false, nil
}

func archiveChain(ctx context.Context, hermes *runtime.Hermes, tasks []runtime.BoardTask) error {
	for _, task := range tasks {
		if err := hermes.Archive(ctx, task.ID); err != nil {
			return err
		}
	}
	return nil
}

// readEnvelope reloads the sealed dispatch envelope the preparation left in
// the run directory, and refuses one that names another delivery.
func readEnvelope(runDir, deliveryID string) (hook.DispatchEnvelope, error) {
	raw, err := os.ReadFile(filepath.Join(runDir, "ticket-envelope.json"))
	if err != nil {
		return hook.DispatchEnvelope{}, errors.New("envelope unreadable")
	}
	var envelope hook.DispatchEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return hook.DispatchEnvelope{}, errors.New("envelope invalid")
	}
	if envelope.DeliveryID != deliveryID {
		return hook.DispatchEnvelope{}, errors.New("envelope names another delivery")
	}
	return envelope, nil
}

// pendingEnvelope reads the envelope a pending terminal report is rebuilt
// under: the run directory's copy when it is there and readable, otherwise
// the ledger's own copy of the sealed envelope (the row keeps it from the
// dispatch on, and the store re-checks the report against it). Both are
// checked to name this delivery.
func pendingEnvelope(runDir string, run state.RunOverview) (hook.DispatchEnvelope, error) {
	envelope, err := readEnvelope(runDir, run.DeliveryID)
	if err == nil {
		return withClarification(envelope, run), nil
	}
	if run.EnvelopeJSON == "" {
		return hook.DispatchEnvelope{}, err
	}
	var stored hook.DispatchEnvelope
	if json.Unmarshal([]byte(run.EnvelopeJSON), &stored) != nil {
		return hook.DispatchEnvelope{}, errors.New("ledger envelope invalid")
	}
	if stored.DeliveryID != run.DeliveryID {
		return hook.DispatchEnvelope{}, errors.New("ledger envelope names another delivery")
	}
	return withClarification(stored, run), nil
}

// withClarification attaches the row's clarification record to an envelope
// that lacks one, the way Pull hands a resumed run its envelope: the
// ledger keeps the record beside the envelope, and the terminal that ends
// the run preserves the adopted answers from it.
func withClarification(envelope hook.DispatchEnvelope, run state.RunOverview) hook.DispatchEnvelope {
	if envelope.ClarificationJSON == "" && run.ClarificationJSON != "" {
		envelope.ClarificationJSON = run.ClarificationJSON
	}
	return envelope
}

// maxWorkspaceFieldBytes bounds every artifact read here; the files are
// small sealed JSON records.
const maxWorkspaceFieldBytes = 1 << 20

// readField walks one string field out of a JSON artifact under the run
// directory.
func readField(runDir, name string, keys ...string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(runDir, name))
	if err != nil {
		return "", err
	}
	if len(raw) > maxWorkspaceFieldBytes {
		return "", errors.New("artifact too large")
	}
	var current any
	if err := json.Unmarshal(raw, &current); err != nil {
		return "", err
	}
	for _, key := range keys {
		object, ok := current.(map[string]any)
		if !ok {
			return "", errors.New("artifact field path invalid")
		}
		current = object[key]
	}
	value, ok := current.(string)
	if !ok {
		return "", errors.New("artifact field is not a string")
	}
	return value, nil
}

// consumerReviewerIDs reads the configured reviewer ids leniently, the way
// the runner names the sealed review files.
func consumerReviewerIDs(consumerConfigPath string) ([]string, error) {
	raw, err := os.ReadFile(consumerConfigPath)
	if err != nil {
		return nil, errors.New("consumer config unreadable")
	}
	var parsed struct {
		Models struct {
			Reviewers []struct {
				ID string `json:"id"`
			} `json:"reviewers"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, errors.New("consumer config invalid")
	}
	ids := make([]string, 0, len(parsed.Models.Reviewers))
	for _, reviewer := range parsed.Models.Reviewers {
		if reviewer.ID != "" {
			ids = append(ids, reviewer.ID)
		}
	}
	return ids, nil
}
