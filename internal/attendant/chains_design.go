package attendant

import (
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
	"automation.internal/ticket-ingress/internal/worker/investigate"
)

// The investigating designer's shapes end and fail in their own ways
// (docs/INVESTIGATING_DESIGNER.md §4.4, §5, §7). As everywhere in the
// attendant, the card's state is only the alarm; the sealed records in the
// run directory are the classification.

// defaultDesignMaxRounds is the design doc's default for design_max_rounds.
const defaultDesignMaxRounds = 3

// errDesignRoundLimit says the design rounds are spent; the run ends as
// nonconverged instead of starting another round.
var errDesignRoundLimit = errors.New("design round limit reached")

// consumerDesignMaxRounds reads the destination's design round limit
// leniently: absent means the default.
func consumerDesignMaxRounds(consumerConfigPath string) int {
	raw, err := os.ReadFile(consumerConfigPath)
	if err != nil {
		return defaultDesignMaxRounds
	}
	var parsed struct {
		DesignMaxRounds int `json:"design_max_rounds"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || parsed.DesignMaxRounds < 1 || parsed.DesignMaxRounds > 10 {
		return defaultDesignMaxRounds
	}
	return parsed.DesignMaxRounds
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
	switch stageName {
	case runtime.StageInvestigate:
		if _, err := os.Stat(filepath.Join(roundDir, "incomplete.json")); err == nil {
			code = hook.TerminalInvestigationIncomplete
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
			return true, nextDesignRoundOrEnd(ctx, config, services, hermes, envelope, run, view, plan, "design review asked for a revision", logger)
		case outcome == "nonconverged" && plan.Shape == runtime.ShapeInvestigation:
			code = hook.TerminalInvestigationNonconverged
		case outcome == "nonconverged":
			code = hook.TerminalDesignNonconverged
		default:
			// Approved and still failed: the applier's instruction could not
			// be rendered, or the card died after sealing — the machinery's own.
			code = hook.TerminalInternalFailed
		}
	case runtime.StageReviewA:
		// The card that seals the applier's work is where an objection
		// surfaces: the applier left revise-design.json and the seal turned
		// it into a sealed design-objection.json instead of a candidate.
		if objected, err := designObjectionRecorded(runDir, view.designRound); err == nil && objected {
			return true, nextDesignRoundOrEnd(ctx, config, services, hermes, envelope, run, view, plan, "the applier objected to the design", logger)
		}
		return false, nil
	default:
		return false, nil
	}
	terminal := runner.NewTerminal(config, services, envelope, chainOwnerRunID(run.DeliveryID), runDir, logger)
	repository, err := readField(runDir, "ticket-draft.json", "repository")
	if err != nil {
		repository = ""
	}
	if err := terminal.Report(ctx, code, runner.Outcome{Code: code}, repository); err != nil {
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

// reviewsFlagDesignWrong reports whether any sealed review of the
// implementation round carries the finding code design-wrong: the reviewer
// judged that the design itself does not hold, which sends the delivery back
// to the designer rather than to another implementation round.
func reviewsFlagDesignWrong(runDir string, implementRound int, reviewers []string) bool {
	for _, reviewer := range reviewers {
		raw, err := os.ReadFile(filepath.Join(runDir, "history", fmt.Sprintf("stage-%d", implementRound), reviewer+".json"))
		if err != nil {
			continue
		}
		var review struct {
			Findings []struct {
				Code string `json:"code"`
			} `json:"findings"`
		}
		if json.Unmarshal(raw, &review) != nil {
			continue
		}
		for _, finding := range review.Findings {
			if finding.Code == "design-wrong" {
				return true
			}
		}
	}
	return false
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
	why string,
	logger Logger,
) error {
	err := nextDesignRound(ctx, hermes, config, run, view, plan, why, logger)
	if !errors.Is(err, errDesignRoundLimit) {
		return err
	}
	code := hook.TerminalDesignNonconverged
	if plan.Shape == runtime.ShapeInvestigation {
		code = hook.TerminalInvestigationNonconverged
	}
	runDir := runDirectory(config, run.DeliveryID)
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

// maxAttachedMeasurements is the per-comment attachment budget: the
// measurements file plus this many raw outputs (the tracker takes ten
// attachments per comment; docs/INVESTIGATING_DESIGNER.md §4.4).
const maxAttachedMeasurements = 9

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
		attachments, omitted := uploadMeasurements(ctx, services, runDir, investigation, logger)
		facts := investigationFacts(investigation, plan.Shape == runtime.ShapeInvestigation, len(attachments), omitted)
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
func uploadMeasurements(ctx context.Context, services *runtime.Services, runDir string, investigation investigate.Investigation, logger Logger) ([]int64, int) {
	if services.Backlog == nil {
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
		} else if id, err := services.Backlog.UploadAttachment(ctx, "measurements.jsonl", index); err == nil {
			ids = append(ids, id)
		}
	}
	cited := investigation.MeasuredEvidence()
	attached, omitted := 0, 0
	for _, measurement := range measurements {
		if measurement.Refused || measurement.Output == "" || !cited[measurement.ID] {
			continue
		}
		if attached >= maxAttachedMeasurements {
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
		attached++
	}
	return ids, omitted
}
