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
)

// The investigating designer's cards (docs/INVESTIGATING_DESIGNER.md §9).
// Design rounds live in history/design-<N>/: the investigate card seals
// investigation.json (and design.json + DESIGN.md in design mode) there; the
// design-review cards add one <reviewer>-design-review.json each; the
// design-decide card seals decision.json. A round whose investigate card
// ended without records leaves incomplete.json instead, which the attendant
// reads as the honest ending it is.

// ChainPlanFromDecision derives the chain's shape from the sealed readiness
// decision and the consumer's switches. It is the one place the mode is
// decided (§6: request_kind and needs_design, nothing else).
func ChainPlanFromDecision(runDir string, consumerConfigPath string) (runtime.ChainPlan, error) {
	decision := filepath.Join(runDir, "history", "readiness", "decision.json")
	raw, err := os.ReadFile(decision)
	if err != nil {
		return runtime.ChainPlan{}, errors.New("readiness decision unreadable")
	}
	var parsed struct {
		RequestKind string `json:"request_kind"`
		NeedsDesign *bool  `json:"needs_design"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return runtime.ChainPlan{}, errors.New("readiness decision invalid")
	}
	switch {
	case parsed.RequestKind == "investigation":
		review, err := consumerReviewsInvestigation(consumerConfigPath)
		if err != nil {
			return runtime.ChainPlan{}, err
		}
		return runtime.ChainPlan{Shape: runtime.ShapeInvestigation, ReviewInvestigation: review}, nil
	case parsed.NeedsDesign != nil && *parsed.NeedsDesign:
		return runtime.ChainPlan{Shape: runtime.ShapeDesign}, nil
	default:
		return runtime.ChainPlan{Shape: runtime.ShapeImplement}, nil
	}
}

// consumerReviewsInvestigation reads the destination switch leniently: an
// absent block or key means the review is on (§5).
func consumerReviewsInvestigation(consumerConfigPath string) (bool, error) {
	raw, err := os.ReadFile(consumerConfigPath)
	if err != nil {
		return false, errors.New("consumer config unreadable")
	}
	var parsed struct {
		Consumers []struct {
			Design *struct {
				ReviewInvestigation *bool `json:"review_investigation"`
			} `json:"design"`
		} `json:"consumers"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return false, errors.New("consumer config invalid")
	}
	for _, consumer := range parsed.Consumers {
		if consumer.Design != nil && consumer.Design.ReviewInvestigation != nil && !*consumer.Design.ReviewInvestigation {
			return false, nil
		}
	}
	return true, nil
}

// designRoundDir is the round's directory under history/.
func (p *Pipeline) designRoundDir(round int) string {
	return p.path(fmt.Sprintf("history/design-%d", round))
}

// currentDesignRound is the first design round without a sealed decision:
// the round the investigate and review cards belong to. A round that ended
// incomplete has no decision either and is not resumed; the attendant ends
// the run instead, so the first such round is always the current one.
func (p *Pipeline) currentDesignRound() int {
	round := 1
	for ; ; round++ {
		if _, err := os.Stat(filepath.Join(p.designRoundDir(round), "decision.json")); err != nil {
			return round
		}
	}
}

// LatestDesignRound is the newest round holding a sealed investigation, 0
// when none exists.
func (p *Pipeline) LatestDesignRound() int {
	round := 0
	for next := 1; ; next++ {
		if _, err := os.Stat(filepath.Join(p.designRoundDir(next), "investigation.json")); err != nil {
			return round
		}
		round = next
	}
}

// chainInvestigate runs the investigating designer for the current design
// round. Mode follows the plan: an investigation-only request seals the
// report alone; a design request continues into the design.
func (p *Pipeline) chainInvestigate(ctx context.Context, repoRoot, baseSHA string) error {
	plan, err := ChainPlanFromDecision(p.Workspace, p.Config.ConsumerConfigPath)
	if err != nil {
		return err
	}
	mode := "design"
	if plan.Shape == runtime.ShapeInvestigation {
		mode = "investigation"
	} else if plan.Shape != runtime.ShapeDesign {
		return errors.New("the readiness decision asks for no investigation")
	}
	round := p.currentDesignRound()
	roundDir := p.designRoundDir(round)
	if _, err := os.Stat(filepath.Join(roundDir, "investigation.json")); err == nil {
		// A re-dispatched card finds its own sealed records; the design
		// mode is complete only with the design.
		if mode == "investigation" {
			return nil
		}
		if _, err := os.Stat(filepath.Join(roundDir, "design.json")); err == nil {
			return nil
		}
	}
	if err := os.MkdirAll(roundDir, 0o755); err != nil {
		return err
	}
	args := []string{
		"investigate", "--config", p.Config.ConsumerConfigPath, "--tool-sha", p.Config.Identity.EngineSHA,
		"--draft", p.path("ticket-draft.json"), "--repo-root", repoRoot, "--base-sha", baseSHA,
		"--round", strconv.Itoa(round), "--mode", mode,
		"--measurements", p.path("measurements.jsonl"), "--out-dir", roundDir,
	}
	if round > 1 {
		args = append(args, "--previous-dir", p.designRoundDir(round-1))
	}
	if seed, state := observationSessionPaths(); seed != "" || state != "" {
		args = append(args, "--session-seed", seed, "--session-state", state)
	}
	if code, err := p.worker(ctx, "investigate", args, p.modelKeyEnv()...); err != nil || code != 0 {
		return errors.New("the investigation round did not seal its records")
	}
	return nil
}

// observationSessionPaths names the screen check's session files, which
// the http probes may read (never write). Both come from the pod's
// environment; empty when the consumer has no observation session.
func observationSessionPaths() (seed, state string) {
	return os.Getenv(E2ESessionFileEnvironment), os.Getenv(E2ESessionStateFileEnvironment)
}

// chainDesignReview runs one configured reviewer against the round's sealed
// design (or, for an investigation-only request, its report).
func (p *Pipeline) chainDesignReview(ctx context.Context, reviewers []string, index int, repoRoot, baseSHA string) error {
	round := p.currentDesignRound()
	roundDir := p.designRoundDir(round)
	if _, err := os.Stat(filepath.Join(roundDir, "investigation.json")); err != nil {
		return errors.New("no sealed investigation to review")
	}
	reviewer := reviewers[index]
	out := filepath.Join(roundDir, reviewer+"-design-review.json")
	if _, err := os.Stat(out); err == nil {
		return nil
	}
	if err := os.Remove(filepath.Join(roundDir, reviewer+"-design-review-run.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	lens := "A"
	if index == 1 {
		lens = "B"
	}
	args := []string{
		"agent-design-review", "--config", p.Config.ConsumerConfigPath, "--tool-sha", p.Config.Identity.EngineSHA,
		"--investigation", filepath.Join(roundDir, "investigation.json"),
		"--measurements", p.path("measurements.jsonl"), "--repo-root", repoRoot, "--base-sha", baseSHA,
		"--reviewer", reviewer, "--lens", lens,
	}
	if _, err := os.Stat(filepath.Join(roundDir, "design.json")); err == nil {
		args = append(args, "--design", filepath.Join(roundDir, "design.json"))
	}
	if round > 1 {
		previous := p.designRoundDir(round - 1)
		for _, earlier := range reviewers {
			if _, err := os.Stat(filepath.Join(previous, earlier+"-design-review.json")); err == nil {
				args = append(args, "--previous-findings", filepath.Join(previous, earlier+"-design-review.json"))
			}
		}
	}
	args = append(args, "--run-out", filepath.Join(roundDir, reviewer+"-design-review-run.json"), "--out", out)
	if code, err := p.worker(ctx, "agent-design-review", args, p.modelKeyEnv()...); err != nil || code != 0 {
		return fmt.Errorf("design review by %s did not finish", reviewer)
	}
	return nil
}

// chainDesignDecide seals the round's decision from the sealed reviews and
// fails the card for anything but approval, so the attendant reads the
// decision and acts on it (a revise starts the next design round; a
// nonconverged ends the run honestly).
func (p *Pipeline) chainDesignDecide(ctx context.Context, reviewers []string) error {
	round := p.currentDesignRound()
	roundDir := p.designRoundDir(round)
	if _, err := os.Stat(filepath.Join(roundDir, "investigation.json")); err != nil {
		return errors.New("no sealed investigation to decide on")
	}
	plan, err := ChainPlanFromDecision(p.Workspace, p.Config.ConsumerConfigPath)
	if err != nil {
		return err
	}
	expected := reviewers
	if plan.Shape == runtime.ShapeInvestigation {
		expected = reviewers[:1]
	}
	args := []string{
		"decide-design", "--config", p.Config.ConsumerConfigPath, "--tool-sha", p.Config.Identity.EngineSHA,
		"--investigation", filepath.Join(roundDir, "investigation.json"), "--round", strconv.Itoa(round),
		"--measurements", p.path("measurements.jsonl"),
	}
	if _, err := os.Stat(filepath.Join(roundDir, "design.json")); err == nil {
		args = append(args, "--design", filepath.Join(roundDir, "design.json"))
	}
	for _, reviewer := range expected {
		review := filepath.Join(roundDir, reviewer+"-design-review.json")
		if _, err := os.Stat(review); err != nil {
			return fmt.Errorf("design review by %s is missing", reviewer)
		}
		args = append(args, "--review", review)
	}
	decision := filepath.Join(roundDir, "decision.json")
	if _, err := os.Stat(decision); err != nil {
		args = append(args, "--out", decision)
		if code, err := p.worker(ctx, "decide-design", args); err != nil || code != 0 {
			return errors.New("the design round could not be decided")
		}
	}
	outcome, err := p.readJSONField(fmt.Sprintf("history/design-%d/decision.json", round), "outcome")
	if err != nil {
		return errors.New("the design decision could not be read back")
	}
	switch outcome {
	case "approved":
		if plan.Shape == runtime.ShapeDesign {
			return p.RenderApplyInstruction(ctx, round)
		}
		return nil
	case "revise":
		return errors.New("the design was sent back for revision")
	case "nonconverged":
		return errors.New("the design rounds did not converge")
	default:
		return fmt.Errorf("the design decision outcome %q is not one this chain knows", outcome)
	}
}

// RenderApplyInstruction writes the applier's INSTRUCTION.md for an approved
// design: the kernel's rendering of the design plus the rules of §7. The
// applier reads nothing else.
func (p *Pipeline) RenderApplyInstruction(_ context.Context, round int) error {
	design, err := os.ReadFile(filepath.Join(p.designRoundDir(round), "DESIGN.md"))
	if err != nil {
		return errors.New("the approved design's rendering is missing")
	}
	instruction := applyInstructionPreamble + string(design) + applyInstructionRules
	return os.WriteFile(p.path("INSTRUCTION.md"), []byte(instruction), 0o644)
}

const applyInstructionPreamble = `# Instruction

You apply an approved design. Do not decide; copy. The design below was
measured, written and reviewed before you started. Everything under it is
the complete list of what changes.

`

const applyInstructionRules = `
---

## Rules

- Change only the files the design lists, in the way it says. Any other change makes the seal refuse the result.
- Do not reopen the approach. If a step cannot be done as written, or you would have to touch another file, stop: write ` + "`revise-design.json`" + ` in the working directory with ` + "`{\"reason\": \"…\", \"section\": \"files|approach|verification|cause\"}`" + ` and finish without editing anything else. The design goes back to its author.
- Never add automation, CI/CD, release, credential, IAM, repository-governance or deployment machinery. Never claim to have run a command or observed a deployment.
- Do not commit; the seal reads the working tree.
`

// approvedDesignPath is the design the current implementation round applies:
// the newest design round whose decision approved it. Empty for a delivery
// without a design (the original chain), so every caller adds the design
// arguments only when there is one.
func (p *Pipeline) approvedDesignPath() string {
	path, _ := p.approvedDesign()
	return path
}

// approvedDesign returns the newest approved design and its round.
func (p *Pipeline) approvedDesign() (string, int) {
	for round := p.LatestDesignRound(); round >= 1; round-- {
		outcome, err := p.readJSONField(fmt.Sprintf("history/design-%d/decision.json", round), "outcome")
		if err != nil || outcome != "approved" {
			continue
		}
		design := filepath.Join(p.designRoundDir(round), "design.json")
		if _, err := os.Stat(design); err == nil {
			return design, round
		}
	}
	return "", 0
}

// ErrNoApprovedDesign is the fail-closed answer for a design-backed
// delivery whose approved design cannot be found: the seal, the reviews and
// the gate must not fall back to the original chain's rules by accident.
var ErrNoApprovedDesign = errors.New("a design-backed round has no approved design to hold the change to")

// requiredDesign returns the approved design when the delivery's plan is the
// design shape, "" when the plan is the original chain, and
// ErrNoApprovedDesign when the design shape has no approved design.
func (p *Pipeline) requiredDesign() (string, int, error) {
	plan, err := ChainPlanFromDecision(p.Workspace, p.Config.ConsumerConfigPath)
	if err != nil || plan.Shape != runtime.ShapeDesign {
		return "", 0, nil
	}
	design, round := p.approvedDesign()
	if design == "" {
		return "", 0, ErrNoApprovedDesign
	}
	return design, round, nil
}

// designObjectionPath is where the seal records an applier's objection to
// the design of one design round; the attendant reads the same path.
func (p *Pipeline) designObjectionPath(designRound int) string {
	return filepath.Join(p.designRoundDir(designRound), "objection.json")
}
