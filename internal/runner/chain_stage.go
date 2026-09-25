package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/worker"
)

// The cards orchestration runs each pipeline stage as its own kanban card;
// the card's per-profile worker.command invokes this binary with
// `chain-stage --stage <name>`, operating on the shared run directory the
// attendant prepared. No stage touches the ledger — claims, questions and
// terminal reports stay with the attendant — and a non-zero exit is the
// card's honest failure: the kanban blocks the card with no retry, and the
// attendant reads the sealed artifacts, not the exit, to decide what the
// failure means.
//
// The round a stage belongs to is derived from the artifacts, never passed
// in: worker.command is fixed per profile, so nothing per-card could carry
// it. A round's directory is created by the seal step and completed by the
// decide step, which makes "the first stage directory without a decision"
// the current round for every stage that runs after implement.

// chainReviewers reads the configured identities and enforces the chain
// plan's two-reviewer shape: only two review profiles exist, so a consumer
// with more judges than that still runs two cards.
func chainReviewers(consumerConfigPath string) ([]string, error) {
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
	identifiers := make([]string, 0, len(parsed.Models.Reviewers))
	for _, reviewer := range parsed.Models.Reviewers {
		if reviewer.ID == "" {
			return nil, errors.New("consumer config reviewer id missing")
		}
		identifiers = append(identifiers, reviewer.ID)
	}
	// The chain runs exactly two review cards. A third configured reviewer
	// would make every decide refuse its review set — it demands one sealed
	// review per configured reviewer — and the run would fail on every
	// round. Refused here, which the attendant hits during preparation,
	// before any card exists (buddy-review finding M-1).
	if len(identifiers) != 2 {
		return nil, errors.New("the cards orchestration runs exactly two reviewers")
	}
	return identifiers, nil
}

// chainImplementer reads the implementer seat's id, the same lenient way
// the reviewer ids are read: the file's other sections are the worker's
// business and validated there, and what is needed here is a name.
func chainImplementer(consumerConfigPath string) (string, error) {
	raw, err := os.ReadFile(consumerConfigPath)
	if err != nil {
		return "", errors.New("consumer config unreadable")
	}
	var parsed struct {
		Models struct {
			Implementer struct {
				ID string `json:"id"`
			} `json:"implementer"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || parsed.Models.Implementer.ID == "" {
		return "", errors.New("consumer config implementer id missing")
	}
	return parsed.Models.Implementer.ID, nil
}

// currentRound is the first round whose decision has not been sealed yet.
// Rounds are complete exactly when their decision.json exists; the count of
// decided rounds plus one is where the seal and the reviews work.
func (p *Pipeline) currentRound() int {
	round := 1
	for ; ; round++ {
		if _, err := os.Stat(p.path(fmt.Sprintf("history/stage-%d/decision.json", round))); err != nil {
			return round
		}
	}
}

// latestCandidateRound is the newest round holding a sealed candidate — the
// round the validate and publish stages belong to. Unlike currentRound it
// does not move when the decision lands: the kanban re-dispatches a
// timed-out direct command once, and a validate card that already sealed
// its decision must resume its own round, not read an empty next one
// (buddy-review finding M-2 — the drift misreported a converged round as a
// validation failure).
func (p *Pipeline) latestCandidateRound() int {
	round := 0
	for next := 1; ; next++ {
		if _, err := os.Stat(p.path(fmt.Sprintf("history/stage-%d/candidate.json", next))); err != nil {
			return round
		}
		round = next
	}
}

// RenderImplementInstruction writes the implement card's instruction for
// one round into the run directory (INSTRUCTION.md). The kernel authors the
// prompt even though the kanban launches the agent; earlier rounds'
// objections ride in with it.
func (p *Pipeline) RenderImplementInstruction(ctx context.Context, round int) error {
	reviewers, err := chainReviewers(p.Config.ConsumerConfigPath)
	if err != nil {
		return err
	}
	args := []string{
		"implement-instruction", "--config", p.Config.ConsumerConfigPath, "--tool-sha", p.Config.Identity.EngineSHA,
		"--repo-root", p.path("target-repo"),
		"--draft", p.path("ticket-draft.json"),
	}
	if round > 1 {
		previous := fmt.Sprintf("%s/stage-%d", p.path("history"), round-1)
		for _, reviewer := range reviewers {
			args = append(args, "--previous-findings", fmt.Sprintf("%s/%s.json", previous, reviewer))
		}
		// A round the judges passed and the deterministic validation refused
		// left what was printed; this round exists to answer it. Passed only
		// when the record is there and reads back whole: most rounds are
		// repeated over an objection instead and sealed no such record, and
		// the command refuses a path it cannot read rather than rendering an
		// instruction that quietly lost the one thing it is being run for.
		if _, sealed := ReadValidationFailure(p.Workspace, round-1); sealed {
			args = append(args, "--validation-failure", ValidationFailureFile(p.Workspace, round-1))
		}
		// The previous round had stopped moving and the engine ruled that
		// the change really was short of what the ticket asks. What it
		// ruled is the reason this round exists, so it rides in — and
		// only for that ruling: an overruling took objections out of the
		// previous round's count and has nothing to tell this one.
		if ruling, err := ReadRuling(p.Workspace, round-1); err == nil && ruling != nil &&
			ruling.Ruling == worker.RulingInstructImplementer {
			args = append(args, "--ruling", RulingFile(p.Workspace, round-1))
		}
	}
	// This round has been handed back before, and what the engine decided
	// in the agent's place is the reason it is being rendered again. Keyed
	// on this round rather than the one before it: a return does not start
	// a new round, it restates the one that was returned — including the
	// first, which has no previous round at all.
	if returned, err := ReadReturns(p.Workspace, round); err == nil && returned.Latest() != nil {
		args = append(args, "--returned", ReturnRecordFile(p.Workspace, round))
	}
	args = append(args, p.clarificationArgs()...)
	// The implementer's seat has one launch, so the ladder's remedy for an
	// implementer that will not answer is the instruction rather than the
	// occupant: rebuilt shorter, once, before the delivery starts waiting.
	if implementer, err := chainImplementer(p.Config.ConsumerConfigPath); err == nil {
		args = append(args, p.seatArguments(runtime.StageImplement, implementer, round)...)
	}
	args = append(args, "--out", p.path("INSTRUCTION.md"))
	if err := p.runVerb(ctx, "implement-instruction", args); err != nil {
		return fmt.Errorf("implement instruction could not be rendered: %w", err)
	}
	return nil
}

// RunChainStage executes one stage card's work. The workspace is the shared
// run directory; the caller (cmd/runner in chain-stage mode) has already
// stripped the destination credential from the environment and hands it in
// explicitly for the publish stage alone.
//
// A card that fails seals what kind of failure it was before it returns. The
// exit code is all that survives this process, and it says only that the
// stage stopped — the difference between a model that would not answer, a
// full volume and a missing binary lives in the record or nowhere.
func (p *Pipeline) RunChainStage(ctx context.Context, stage string) error {
	err := p.runChainStage(ctx, stage)
	if err != nil {
		p.SealStageFailure(stage, err)
	}
	return err
}

func (p *Pipeline) runChainStage(ctx context.Context, stage string) error {
	if err := p.resolveConsumer(); err != nil {
		return err
	}
	baseSHA, err := p.readJSONField("baseline.json", "baseline", "Integration", "SHA")
	if err != nil || len(baseSHA) != 40 {
		return fmt.Errorf("baseline sha invalid: %q (%v)", baseSHA, err)
	}
	reviewers, err := chainReviewers(p.Config.ConsumerConfigPath)
	if err != nil {
		return err
	}
	repoRoot := p.path("target-repo")
	baseRoot := p.path("target-base")
	switch stage {
	case runtime.StageInvestigate:
		return p.chainInvestigate(ctx, repoRoot, baseSHA)
	case runtime.StageDesignReviewA:
		return p.chainDesignReview(ctx, reviewers, 0, repoRoot, baseSHA)
	case runtime.StageDesignReviewB:
		return p.chainDesignReview(ctx, reviewers, 1, repoRoot, baseSHA)
	case runtime.StageDesignDecide:
		return p.chainDesignDecide(ctx, reviewers)
	case runtime.StageReviewA:
		return p.chainSealAndReview(ctx, reviewers, 0, repoRoot, baseRoot, baseSHA)
	case runtime.StageReviewB:
		return p.chainReview(ctx, reviewers, 1, repoRoot, baseSHA)
	case runtime.StageValidate:
		return p.chainValidate(ctx, reviewers)
	case runtime.StagePublish:
		return p.chainPublish(ctx, reviewers)
	case runtime.StageImplement:
		return p.chainRunInstruction(ctx, "implementer", repoRoot, baseSHA)
	case runtime.StageApply:
		return p.chainRunInstruction(ctx, "applier", repoRoot, baseSHA)
	default:
		return fmt.Errorf("chain stage %q is not runnable as a command", stage)
	}
}

// chainRunInstruction runs the implementer or the applier on the
// instruction the attendant rendered (INSTRUCTION.md). The card is a direct
// command like the reviews, so the agent starts through the worker and its
// launcher under the agent user instead of as the kanban's native worker
// under the engine's user (issue #23); the seal of what it left stays with
// the first review card, as before. The run record goes to the round the
// seal will complete.
func (p *Pipeline) chainRunInstruction(ctx context.Context, role, repoRoot, baseSHA string) error {
	instruction := p.path("INSTRUCTION.md")
	if _, err := os.Stat(instruction); err != nil {
		return errors.New("no instruction to run")
	}
	round := p.currentRound()
	stageDir := fmt.Sprintf("%s/stage-%d", p.path("history"), round)
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		return err
	}
	record := fmt.Sprintf("%s/%s-run.json", stageDir, role)
	// A re-dispatched card writes its own record; the record is
	// exclusive-create, so the earlier attempt's must go first.
	if err := os.Remove(record); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	args := []string{
		"run-instruction", "--config", p.Config.ConsumerConfigPath, "--tool-sha", p.Config.Identity.EngineSHA,
		"--draft", p.path("ticket-draft.json"), "--role", role, "--instruction", instruction,
		"--repo-root", repoRoot, "--base-sha", baseSHA, "--stage", strconv.Itoa(round),
		"--knowledge-root", p.Config.KnowledgeRoot, "--out", record,
	}
	if role == "applier" {
		// The applier may stop instead of editing: it writes its objection at
		// the root of its working copy (the only place it can write), and the
		// command seals that into the design round's record — so the card
		// needs the design the objection is against and where the record
		// goes (issue #103).
		design, designRound, err := p.requiredDesign()
		if err != nil {
			return err
		}
		if design != "" {
			args = append(args, "--design", design, "--objection-out", p.designObjectionPath(designRound))
			args = append(args, "--investigation", filepath.Join(filepath.Dir(design), "investigation.json"), "--measurements", p.path("measurements.jsonl"))
		}
	}
	if err := p.runVerb(ctx, "run-instruction", args); err != nil {
		return fmt.Errorf("the %s did not finish: %w", role, err)
	}
	return nil
}

// chainSealAndReview seals what the implement card's agent left in the
// working copy, then runs the first configured reviewer on it. Sealing lives
// here rather than on its own card because the kanban has no
// completion hook on a native worker; the first judge's card is the first
// deterministic moment after the implementer finished.
func (p *Pipeline) chainSealAndReview(ctx context.Context, reviewers []string, index int, repoRoot, baseRoot, baseSHA string) error {
	round := p.currentRound()
	stageDir := fmt.Sprintf("%s/stage-%d", p.path("history"), round)
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(stageDir + "/candidate.json"); err != nil {
		// A re-dispatched card may find a half-written attempt: the seal's
		// outputs are exclusive-create, so its own leftovers must go before
		// it can run again.
		for _, partial := range []string{"/implement-run.json", "/ticket.json", "/source.json"} {
			if err := os.Remove(stageDir + partial); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		sealArgs := []string{
			"seal-candidate", "--config", p.Config.ConsumerConfigPath, "--tool-sha", p.Config.Identity.EngineSHA,
			"--draft", p.path("ticket-draft.json"), "--repo-root", repoRoot,
			"--base-root", baseRoot, "--base-sha", baseSHA, "--stage", strconv.Itoa(round),
			"--run-out", stageDir + "/implement-run.json",
			"--ticket-out", stageDir + "/ticket.json",
			"--source-out", stageDir + "/source.json",
			"--out", stageDir + "/candidate.json",
		}
		design, designRound, err := p.requiredDesign()
		if err != nil {
			return err
		}
		reportRun := stageDir + "/implementer-run.json"
		if design != "" {
			// A design-backed round: the seal holds the applier to the design
			// and turns an objection into the design round's record instead.
			reportRun = stageDir + "/applier-run.json"
			sealArgs = append(sealArgs, "--design", design,
				"--objection", p.path("revise-design.json"), "--objection-out", p.designObjectionPath(designRound))
		}
		sealArgs = append(sealArgs, "--report-run", reportRun)
		if err := p.runVerb(ctx, "seal-candidate", sealArgs); err != nil {
			return fmt.Errorf("the implemented change could not be sealed: %w", err)
		}
	}
	return p.chainReviewSealed(ctx, reviewers, index, repoRoot, baseSHA, round)
}

// chainReview runs one configured reviewer against the round's sealed
// candidate. It refuses to run before the seal exists: a judge with nothing
// sealed to judge is a chain ordering violation, not a review.
func (p *Pipeline) chainReview(ctx context.Context, reviewers []string, index int, repoRoot, baseSHA string) error {
	round := p.currentRound()
	if _, err := os.Stat(p.path(fmt.Sprintf("history/stage-%d/candidate.json", round))); err != nil {
		return errors.New("no sealed candidate to review")
	}
	return p.chainReviewSealed(ctx, reviewers, index, repoRoot, baseSHA, round)
}

func (p *Pipeline) chainReviewSealed(ctx context.Context, reviewers []string, index int, repoRoot, baseSHA string, round int) error {
	stageDir := fmt.Sprintf("%s/stage-%d", p.path("history"), round)
	reviewer := reviewers[index]
	if _, err := os.Lstat(fmt.Sprintf("%s/%s.json", stageDir, reviewer)); err == nil {
		// A re-dispatched card finds its own sealed review: nothing left to
		// do, and redoing it would double the judge's spend.
		return nil
	}
	// A half-written attempt leaves the run record without the review; the
	// exclusive-create outputs need their own leftovers gone first.
	if err := os.Remove(fmt.Sprintf("%s/%s-run.json", stageDir, reviewer)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	reviewArgs := []string{
		"agent-review", "--config", p.Config.ConsumerConfigPath, "--tool-sha", p.Config.Identity.EngineSHA,
		"--ticket", stageDir + "/ticket.json", "--source", stageDir + "/source.json",
		"--candidate", stageDir + "/candidate.json", "--repo-root", repoRoot, "--base-sha", baseSHA,
		"--knowledge-root", p.Config.KnowledgeRoot,
	}
	if round > 1 {
		previous := fmt.Sprintf("%s/stage-%d", p.path("history"), round-1)
		for _, earlier := range reviewers {
			reviewArgs = append(reviewArgs, "--previous-findings", fmt.Sprintf("%s/%s.json", previous, earlier))
		}
	}
	reviewArgs = append(reviewArgs, p.clarificationArgs()...)
	design, _, err := p.requiredDesign()
	if err != nil {
		return err
	}
	if design != "" {
		reviewArgs = append(reviewArgs, "--design-md", filepath.Join(filepath.Dir(design), "DESIGN.md"))
		reviewArgs = append(reviewArgs, "--design", design, "--investigation", filepath.Join(filepath.Dir(design), "investigation.json"), "--measurements", p.path("measurements.jsonl"))
	}
	// Where this seat is sitting now, and whether its instruction has been
	// rebuilt. Both are the ladder's decisions about an earlier attempt at
	// this same round, and both are empty for a card nothing has failed at.
	reviewArgs = append(reviewArgs, p.seatArguments(reviewStage(index), reviewer, round)...)
	reviewArgs = append(reviewArgs, "--reviewer", reviewer,
		"--run-out", fmt.Sprintf("%s/%s-run.json", stageDir, reviewer),
		"--out", fmt.Sprintf("%s/%s.json", stageDir, reviewer))
	if err := p.runVerb(ctx, "agent-review", reviewArgs, p.modelKeyEnv()...); err != nil {
		return fmt.Errorf("review by %s did not finish: %w", reviewer, err)
	}
	return nil
}

// chainValidate tallies the round — every configured reviewer's sealed
// verdict, counted under the engine's ruling when the round had to be ruled
// on — and, when the round converged, runs the deterministic validation. A
// revise outcome exits non-zero after sealing the decision: this card's work
// (carrying a converged change) genuinely cannot proceed, and the attendant
// reads the decision to decide what happens to the round.
func (p *Pipeline) chainValidate(ctx context.Context, reviewers []string) error {
	round := p.latestCandidateRound()
	if round == 0 {
		return errors.New("no sealed candidate to validate")
	}
	stageDir := fmt.Sprintf("%s/stage-%d", p.path("history"), round)
	reviewFlags := make([]string, 0, 2*len(reviewers))
	for _, reviewer := range reviewers {
		reviewFlags = append(reviewFlags, "--review", fmt.Sprintf("%s/%s.json", stageDir, reviewer))
	}
	// A re-dispatched card resumes behind its own sealed decision instead
	// of deciding twice (the output is exclusive-create, and the round it
	// belongs to must not drift).
	if _, err := os.Stat(stageDir + "/decision.json"); err != nil {
		decideArgs := append([]string{
			"decide", "--config", p.Config.ConsumerConfigPath, "--tool-sha", p.Config.Identity.EngineSHA,
			"--ticket", stageDir + "/ticket.json", "--source", stageDir + "/source.json",
			"--candidate", stageDir + "/candidate.json",
		}, reviewFlags...)
		// This round was ruled on and is being decided again. The ruling
		// says which objections the ticket does not require, and the
		// verdict is counted without them; a round nobody ruled on has no
		// such file and is counted exactly as it always was.
		ruling, err := ReadRuling(p.Workspace, round)
		if err != nil {
			return err
		}
		if ruling != nil && ruling.Ruling == worker.RulingOverruleReviewer {
			decideArgs = append(decideArgs, "--ruling", RulingFile(p.Workspace, round))
		}
		decideArgs = append(decideArgs, "--out", stageDir+"/decision.json")
		if err := p.runVerb(ctx, "decide", decideArgs); err != nil {
			return fmt.Errorf("the round could not be decided: %w", err)
		}
	}
	outcome, err := p.readJSONField(fmt.Sprintf("history/stage-%d/decision.json", round), "outcome")
	if err != nil {
		return errors.New("the decision could not be read back")
	}
	switch outcome {
	case "converged":
	case "revise":
		return errors.New("the round was sent back for revision")
	default:
		return fmt.Errorf("the decision outcome %q is not one this chain knows", outcome)
	}
	// A resumed attempt redoes the validation from scratch — its outputs
	// are exclusive-create and the sandbox is rebuilt anyway; minutes of
	// deterministic work beat a stuck leftover.
	if err := os.Remove(p.path("validation.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if refusal, err := p.validationStage(ctx, round, chainReviewFiles(reviewers)); err != nil {
		return err
	} else if refusal != nil {
		// The round is refused, not the delivery. What the validation printed
		// is sealed into this round so the next round's instruction can carry
		// it: the judges passed this change and the destination's own commands
		// did not, and the only party that can close that gap is the agent
		// that wrote it, told what was printed. Without this the output lived
		// in the pod's log alone and the run ended on it.
		p.SealValidationFailure(round, refusal.step, refusal.output)
		// The sentinel, not a fresh sentence: this is the one failure the
		// validate card owns, and the class turns on having taken this branch.
		return ErrValidationRejected
	}
	return nil
}

// chainReviewFiles are the configuration-derived review artifact names
// shared by both modes: one per configured reviewer, named by its identity.
func chainReviewFiles(reviewers []string) []string {
	names := make([]string, 0, len(reviewers))
	for _, reviewer := range reviewers {
		names = append(names, reviewer+".json")
	}
	return names
}

// ChainOutcomeFile is where the publish stage leaves what the attendant's
// terminal report needs: the adopted round and the delivery evidence.
const ChainOutcomeFile = "m1-chain-outcome.json"

// ChainOutcome is the publish stage's sealed handoff to the attendant.
type ChainOutcome struct {
	Stage    int               `json:"stage"`
	Evidence map[string]string `json:"evidence"`
}

// chainPublish delivers the adopted round and persists the outcome for the
// attendant, which owns the report.
func (p *Pipeline) chainPublish(ctx context.Context, reviewers []string) error {
	round := p.latestCandidateRound()
	if round < 1 {
		return errors.New("no decided round to publish")
	}
	outcome, err := p.readJSONField(fmt.Sprintf("history/stage-%d/decision.json", round), "outcome")
	if err != nil || outcome != "converged" {
		return errors.New("the last decided round did not converge")
	}
	delivered, err := p.deliveryStage(ctx, round, chainReviewFiles(reviewers))
	// The delivery's own code is read before the error, and it wins. A refusal
	// by the destination and a breakdown of the machinery both arrive here as
	// one non-zero exit; collapsing the code into a sentence — or dropping it
	// entirely, as the error arm did — left the two indistinguishable from
	// outside this process. It rides the error now, into the round's record.
	if delivered.Code != "" {
		return &deliveryRefusal{code: delivered.Code, err: err}
	}
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(ChainOutcome{Stage: round, Evidence: delivered.Evidence})
	if err != nil {
		return err
	}
	return os.WriteFile(p.path(ChainOutcomeFile), encoded, 0o600)
}
