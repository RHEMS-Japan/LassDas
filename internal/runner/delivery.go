package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"automation.internal/ticket-ingress/internal/hook"
)

// runnerReviewFiles are the runner mode's fixed review artifact names,
// frozen with that rail (issue #12 resolves in the cards mode, which
// derives the names from configuration and passes them in).
func runnerReviewFiles() []string {
	return []string{"claude-correctness.json", "codex-adversarial.json"}
}

// reviewFileArgs renders --review flags for the named artifacts of a stage.
func reviewFileArgs(stageDir string, names []string) []string {
	arguments := make([]string, 0, 2*len(names))
	for _, name := range names {
		arguments = append(arguments, "--review", stageDir+"/"+name)
	}
	return arguments
}

// validationStage mirrors the workflow's validation job: apply the adopted
// candidate to a fresh copy, run the consumer's own checks, verify the
// applied tree and the publish gate. The GitHub job dropped privileges to
// `nobody` inside a throwaway sandbox; in the pod the runner executes the
// same steps in the task workspace — the pod itself is the sandbox (its own
// namespace, its own filesystem, egress-limited; see the runtime design's
// network section). Returns validationFailed=true for a gate refusal.
func (p *Pipeline) validationStage(ctx context.Context, stage int, reviewFiles []string) (bool, error) {
	return p.validationStageAt(ctx, stage, reviewFiles, "")
}

// validationStageAt pins the validation checkout to baseSHA when given: the
// publish retry validates the same candidate on a freshly advanced
// integration base. An empty baseSHA reads the run's recorded baseline.
func (p *Pipeline) validationStageAt(ctx context.Context, stage int, reviewFiles []string, baseSHA string) (bool, error) {
	stageDir := fmt.Sprintf("%s/stage-%d", p.path("history"), stage)
	sandbox := p.path("validation-target")
	if err := os.RemoveAll(sandbox); err != nil {
		return false, err
	}
	if err := p.cloneTargetTo(ctx, sandbox); err != nil {
		return false, err
	}
	if baseSHA == "" {
		recorded, err := p.readJSONField("baseline.json", "baseline", "Integration", "SHA")
		if err != nil {
			return false, err
		}
		baseSHA = recorded
	}
	if code, err := p.gitIn(ctx, sandbox, "checkout", "--detach", baseSHA); err != nil || code != 0 {
		return false, fmt.Errorf("validation checkout failed (%v)", err)
	}
	common := []string{
		"--config", p.Config.ConsumerConfigPath, "--tool-sha", p.Config.Identity.EngineSHA,
		"--ticket", stageDir + "/ticket.json", "--source", stageDir + "/source.json",
		"--candidate", stageDir + "/candidate.json",
	}
	if code, err := p.worker(ctx, "apply", append([]string{"apply"}, append(common, "--repo-root", sandbox)...)); err != nil || code != 0 {
		return true, err
	}
	if code, err := p.worker(ctx, "run-validation", append([]string{"run-validation"},
		append(common, "--repo-root", sandbox, "--checkout-sha", baseSHA,
			"--out", p.path("validation.json"))...)); err != nil || code != 0 {
		return true, err
	}
	if code, err := p.worker(ctx, "verify-applied", append([]string{"verify-applied"},
		append(common, "--repo-root", sandbox)...)); err != nil || code != 0 {
		return true, err
	}
	gateArgs := append(common, reviewFileArgs(stageDir, reviewFiles)...)
	gateArgs = append(gateArgs, "--decision", stageDir+"/decision.json", "--validation", p.path("validation.json"))
	if design := p.approvedDesignPath(); design != "" {
		gateArgs = append(gateArgs, "--design", design, "--design-decision", filepath.Join(filepath.Dir(design), "decision.json"))
	}
	if code, err := p.worker(ctx, "verify-publish-gate", append([]string{"verify-publish-gate"}, gateArgs...)); err != nil || code != 0 {
		return true, err
	}
	return false, nil
}

// EnsureTrail leaves the trail file the terminal report attaches, composing
// it from the model history when this run has not composed one yet. The
// delivery path composes before publishing; a failure terminal that ran at
// least one implementation round carries the same round-by-round record —
// the 608 and 610 post-mortems had to read the dying workspace by hand
// because nothing rendered the history onto the ticket (#10). A composition
// failure writes the fixed fallback line instead: the trail must never stop
// a report, and the only error out of here is the file being unwritable.
//
// Only a trail this run wrote is trusted, tracked in-process — a file that
// merely exists could have been left by anyone: the model agents run with
// the workspace parent writable, and ../m1-trail.txt sits outside the
// observed repository, so a pre-placed file would otherwise ride into the
// report and the pull request unverified (buddy-review finding). The flag
// still keeps the legitimate reuse: a post-publish failure report attaches
// the trail the delivery composed moments earlier in this same process.
func (p *Pipeline) EnsureTrail(ctx context.Context) error {
	if p.trailWritten {
		return nil
	}
	trailPath := p.path("m1-trail.txt")
	// Clear whatever squatted on the path: the composer's write is exclusive,
	// and a leftover file would turn an honest trail into the fallback line.
	if err := os.Remove(trailPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	trailArgs := []string{
		"compose-trail", "--config", p.Config.ConsumerConfigPath, "--tool-sha", p.Config.Identity.EngineSHA,
		"--history", p.path("history"), "--validation", p.path("validation.json"),
	}
	trailArgs = append(trailArgs, p.clarificationArgs()...)
	if design := p.approvedDesignPath(); design != "" {
		trailArgs = append(trailArgs, "--design", design)
	}
	trailArgs = append(trailArgs, "--out", trailPath)
	if code, err := p.worker(ctx, "compose-trail", trailArgs, p.modelKeyEnv()...); err != nil || code != 0 {
		// The workflow wrote this exact fixed line on compose failure, and it
		// asserts nothing about what was or was not delivered.
		fallback := "証跡の自動生成に失敗したため、この実行の詳細は実行履歴を参照してください。\n"
		if err := os.WriteFile(trailPath, []byte(fallback), 0o600); err != nil {
			return err
		}
	}
	p.trailWritten = true
	return nil
}

// AttemptedImplementation reports whether this run sealed at least the start
// of one implementation round — the condition under which a failure report
// has a history worth rendering. Prepare must have run: without its
// workspace clearing, a stage directory could be an earlier dispatch's
// leftover, and this run must not render someone else's history as its own.
func (p *Pipeline) AttemptedImplementation() bool {
	return p.prepared && p.exists("history/stage-1")
}

// deliveryStage publishes the adopted candidate: compose the trail, push
// the feature, open the PR — and stop there for the pull_request delivery
// (the PoC exit). The integration and production continuations call the
// same controller subcommands the workflow did; their polling lives inside
// the controller, unchanged.
func (p *Pipeline) deliveryStage(ctx context.Context, stage int, reviewFiles []string) (Outcome, error) {
	stageDir := fmt.Sprintf("%s/stage-%d", p.path("history"), stage)
	evidence := map[string]string{}

	trailPath := p.path("m1-trail.txt")
	if err := p.EnsureTrail(ctx); err != nil {
		return Outcome{Code: hook.TerminalInternalFailed}, err
	}
	common := []string{
		"--config", p.Config.ConsumerConfigPath,
		"--ticket", stageDir + "/ticket.json",
	}
	if outcome, err := p.publishWithBaseAdvance(ctx, stage, reviewFiles, stageDir, common); err != nil || outcome.Code != "" {
		return outcome, err
	}
	if code, err := p.controller(ctx, "create-feature-pr", append([]string{"create-feature-pr"}, append(common,
		"--feature", p.path("feature.json"), "--trail", trailPath,
		"--out", p.path("feature-pr.json"))...)); err != nil || code != 0 {
		return Outcome{Code: hook.TerminalReleaseFailed}, err
	}
	if url, err := p.readJSONField("feature-pr.json", "payload", "pull_request", "HTMLURL"); err == nil && url != "" {
		evidence["pull_request_url"] = url
	}
	if p.delivery == "pull_request" {
		return Outcome{Stage: stage, Evidence: evidence}, nil
	}
	return p.deliveryUnsupported(stage, evidence)
}

const (
	// publishAttempts bounds the base-advance retries: the initial publish
	// plus two catch-ups. On a repository other work merges into every few
	// minutes, more attempts would just keep chasing the branch.
	publishAttempts = 3
	// publishFailureFile is where the controller leaves the machine-readable
	// refusal reason; only integration_base_changed is worth a retry.
	publishFailureFile = "publish-failure.json"
	// advancedBaselineFile is the freshly snapshotted baseline a retry
	// publishes against. baseline.json stays untouched: the source snapshot
	// keeps chaining to the base the candidate was recorded on.
	advancedBaselineFile = "baseline-advanced.json"
)

// publishWithBaseAdvance runs the publish-feature step, and when the only
// refusal is "the integration branch advanced mid-run", it snapshots the new
// base, re-runs the deterministic validation on it, and publishes once more.
// A candidate whose touched files also moved upstream still refuses (the
// controller's per-file blob checks), and every other refusal stays final.
func (p *Pipeline) publishWithBaseAdvance(ctx context.Context, stage int, reviewFiles []string, stageDir string, common []string) (Outcome, error) {
	baselinePath := p.path("baseline.json")
	sourceBase := ""
	for attempt := 1; ; attempt++ {
		if err := os.Remove(p.path(publishFailureFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return Outcome{Code: hook.TerminalInternalFailed}, err
		}
		publishArgs := append([]string{"publish-feature"}, common...)
		publishArgs = append(publishArgs, "--source", stageDir+"/source.json", "--candidate", stageDir+"/candidate.json")
		publishArgs = append(publishArgs, reviewFileArgs(stageDir, reviewFiles)...)
		publishArgs = append(publishArgs,
			"--decision", stageDir+"/decision.json", "--validation", p.path("validation.json"),
			"--baseline", baselinePath, "--failure-out", p.path(publishFailureFile),
			"--out", p.path("feature.json"))
		if sourceBase != "" {
			publishArgs = append(publishArgs, "--source-base", sourceBase)
		}
		code, err := p.controller(ctx, "publish-feature", publishArgs)
		if err == nil && code == 0 {
			return Outcome{}, nil
		}
		invariantCode := p.readPublishInvariant()
		if invariantCode != "integration_base_changed" || attempt >= publishAttempts {
			switch {
			case sourceBase != "" && invariantCode == "source_blob_changed":
				p.writeStopReason("公開の中断理由: 実行中に統合先ブランチが進み、変更対象のファイルが統合先でも書き換えられていた（競合）ため、安全のため公開を中止しました。")
			case invariantCode == "integration_base_changed":
				p.writeStopReason("公開の中断理由: 実行中に統合先ブランチが進み続け、追随の上限に達したため公開を中止しました。")
			}
			return Outcome{Code: hook.TerminalReleaseFailed}, err
		}
		// The base advanced mid-run: snapshot it fresh, re-validate the same
		// candidate on it, and try once more. The original base the source
		// snapshot chains to is pinned on the first advance and never moves.
		if sourceBase == "" {
			original, readErr := p.readJSONField("baseline.json", "baseline", "Integration", "SHA")
			if readErr != nil {
				return Outcome{Code: hook.TerminalReleaseFailed}, readErr
			}
			sourceBase = original
		}
		if err := os.Remove(p.path(advancedBaselineFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return Outcome{Code: hook.TerminalReleaseFailed}, err
		}
		if code, err := p.controller(ctx, "baseline", []string{
			"baseline", "--config", p.Config.ConsumerConfigPath,
			"--draft", p.path("ticket-draft.json"), "--out", p.path(advancedBaselineFile),
		}); err != nil || code != 0 {
			return Outcome{Code: hook.TerminalReleaseFailed}, errors.New("advanced baseline snapshot failed")
		}
		advancedSHA, err := p.readJSONField(advancedBaselineFile, "baseline", "Integration", "SHA")
		if err != nil || len(advancedSHA) != 40 {
			return Outcome{Code: hook.TerminalReleaseFailed}, errors.New("advanced baseline sha invalid")
		}
		if err := os.Remove(p.path("validation.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return Outcome{Code: hook.TerminalReleaseFailed}, err
		}
		if failed, err := p.validationStageAt(ctx, stage, reviewFiles, advancedSHA); err != nil || failed {
			p.writeStopReason("公開の中断理由: 実行中に統合先ブランチが進み、新しい統合先の上での検証が通らなかったため公開を中止しました。")
			return Outcome{Code: hook.TerminalReleaseFailed}, errors.New("revalidation on the advanced base failed")
		}
		baselinePath = p.path(advancedBaselineFile)
	}
}

// readPublishInvariant reads the controller's machine-readable refusal
// reason; anything unreadable is "no reason", which the caller treats as
// final.
func (p *Pipeline) readPublishInvariant() string {
	encoded, err := os.ReadFile(p.path(publishFailureFile))
	if err != nil || len(encoded) > 4096 {
		return ""
	}
	var failure struct {
		Invariant string `json:"invariant"`
	}
	if json.Unmarshal(encoded, &failure) != nil {
		return ""
	}
	return failure.Invariant
}

// deliveryStopReasonFile carries the requester-facing reason a delivery
// stopped, across processes: the publish card's runner writes it, and the
// attendant — which recomposes the trail in its own process before the
// failure report — folds it in with AttachDeliveryStopReason.
const deliveryStopReasonFile = "delivery-stop-reason.txt"

// writeStopReason records why the delivery stopped. Best-effort by design:
// a missing reason only makes the report less specific, never blocks it.
func (p *Pipeline) writeStopReason(reason string) {
	_ = os.WriteFile(p.path(deliveryStopReasonFile), []byte(reason), 0o600)
}

// AttachDeliveryStopReason appends the recorded stop reason (if any) to the
// trail this pipeline composed. Call it after EnsureTrail: the attendant's
// trail recomposition would otherwise discard a note the publish card's own
// process had appended.
func (p *Pipeline) AttachDeliveryStopReason() {
	encoded, err := os.ReadFile(p.path(deliveryStopReasonFile))
	if err != nil || len(encoded) == 0 || len(encoded) > 1024 {
		return
	}
	p.appendTrailNote(string(encoded))
}

// appendTrailNote adds one requester-facing line to the composed trail so a
// failure report says why the delivery stopped. Best-effort by design — the
// trail must never block the report that carries it — and it never grows the
// trail past the report's size bound.
func (p *Pipeline) appendTrailNote(note string) {
	if !p.trailWritten {
		return
	}
	info, err := os.Stat(p.path("m1-trail.txt"))
	if err != nil || info.Size()+int64(len(note))+2 > int64(hook.MaxTerminalTrailBytes) {
		return
	}
	file, err := os.OpenFile(p.path("m1-trail.txt"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.WriteString("\n" + note + "\n")
}

func (p *Pipeline) deliveryUnsupported(stage int, evidence map[string]string) (Outcome, error) {
	// resolveConsumer refuses integration/production consumers before any
	// work, because their success reports need browser evidence steps this
	// runtime does not carry yet; reaching here with one is a bug.
	return Outcome{Code: hook.TerminalInternalFailed, Evidence: evidence},
		fmt.Errorf("delivery %q reached the delivery stage but is not shipped in the pod runtime", p.delivery)
}
