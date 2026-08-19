package runner

import (
	"context"
	"fmt"
	"os"

	"automation.internal/ticket-ingress/internal/hook"
)

// validationStage mirrors the workflow's validation job: apply the adopted
// candidate to a fresh copy, run the consumer's own checks, verify the
// applied tree and the publish gate. The GitHub job dropped privileges to
// `nobody` inside a throwaway sandbox; in the pod the runner executes the
// same steps in the task workspace — the pod itself is the sandbox (its own
// namespace, its own filesystem, egress-limited; see the runtime design's
// network section). Returns validationFailed=true for a gate refusal.
func (p *Pipeline) validationStage(ctx context.Context, stage int) (bool, error) {
	stageDir := fmt.Sprintf("%s/stage-%d", p.path("history"), stage)
	sandbox := p.path("validation-target")
	if err := os.RemoveAll(sandbox); err != nil {
		return false, err
	}
	if err := p.cloneTargetTo(ctx, sandbox); err != nil {
		return false, err
	}
	baseSHA, err := p.readJSONField("baseline.json", "baseline", "Integration", "SHA")
	if err != nil {
		return false, err
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
		append(common, "--repo-root", sandbox, "--out", p.path("validation.json"))...)); err != nil || code != 0 {
		return true, err
	}
	if code, err := p.worker(ctx, "verify-applied", append([]string{"verify-applied"},
		append(common, "--repo-root", sandbox)...)); err != nil || code != 0 {
		return true, err
	}
	if code, err := p.worker(ctx, "verify-publish-gate", append([]string{"verify-publish-gate"},
		append(common,
			"--review", stageDir+"/claude-correctness.json", "--review", stageDir+"/codex-adversarial.json",
			"--decision", stageDir+"/decision.json", "--validation", p.path("validation.json"))...)); err != nil || code != 0 {
		return true, err
	}
	return false, nil
}

func (p *Pipeline) cloneTargetTo(ctx context.Context, destination string) error {
	return p.cloneTarget(ctx, destination)
}

// deliveryStage publishes the adopted candidate: compose the trail, push
// the feature, open the PR — and stop there for the pull_request delivery
// (the PoC exit). The integration and production continuations call the
// same controller subcommands the workflow did; their polling lives inside
// the controller, unchanged.
func (p *Pipeline) deliveryStage(ctx context.Context, stage int) (Outcome, error) {
	stageDir := fmt.Sprintf("%s/stage-%d", p.path("history"), stage)
	evidence := map[string]string{}

	trailPath := p.path("m1-trail.txt")
	trailArgs := []string{
		"compose-trail", "--config", p.Config.ConsumerConfigPath, "--tool-sha", p.Config.Identity.EngineSHA,
		"--history", p.path("history"), "--validation", p.path("validation.json"),
	}
	trailArgs = append(trailArgs, p.clarificationArgs()...)
	trailArgs = append(trailArgs, "--out", trailPath)
	if code, err := p.worker(ctx, "compose-trail", trailArgs, p.modelKeyEnv()...); err != nil || code != 0 {
		// The trail must not stop a delivery (workflow: fixed fallback).
		fallback := "自動処理はレビューと検証を通過した変更を納品しました。詳細な経緯の要約は生成できなかったため、実行記録を参照してください。"
		if writeErr := os.WriteFile(trailPath, []byte(fallback), 0o600); writeErr != nil {
			return Outcome{Code: "internal_failed"}, writeErr
		}
	}
	common := []string{
		"--config", p.Config.ConsumerConfigPath,
		"--ticket", stageDir + "/ticket.json",
	}
	publishArgs := append([]string{"publish-feature"}, append(common,
		"--source", stageDir+"/source.json", "--candidate", stageDir+"/candidate.json",
		"--review", stageDir+"/claude-correctness.json", "--review", stageDir+"/codex-adversarial.json",
		"--decision", stageDir+"/decision.json", "--validation", p.path("validation.json"),
		"--baseline", p.path("baseline.json"), "--out", p.path("feature.json"))...)
	if code, err := p.controller(ctx, "publish-feature", publishArgs); err != nil || code != 0 {
		return Outcome{Code: hook.TerminalReleaseFailed}, err
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

	// integration / production continue through the controller chain. The
	// browser evidence steps require a display-capable environment; the
	// runtime image ships headless chromium exactly as the workflow runner
	// did.
	if code, err := p.controller(ctx, "wait-feature", append([]string{"wait-feature"}, append(common,
		"--feature-pr", p.path("feature-pr.json"), "--out", p.path("checks.json"))...)); err != nil || code != 0 {
		return Outcome{Code: hook.TerminalReleaseFailed, Evidence: evidence}, err
	}
	if code, err := p.controller(ctx, "merge-feature", append([]string{"merge-feature"}, append(common,
		"--feature-pr", p.path("feature-pr.json"), "--checks", p.path("checks.json"),
		"--out", p.path("feature-merge.json"))...)); err != nil || code != 0 {
		return Outcome{Code: hook.TerminalReleaseFailed, Evidence: evidence}, err
	}
	stagingArgs := append([]string{"await-staging"}, append(common,
		"--source", stageDir+"/source.json", "--candidate", stageDir+"/candidate.json",
		"--review", stageDir+"/claude-correctness.json", "--review", stageDir+"/codex-adversarial.json",
		"--decision", stageDir+"/decision.json", "--validation", p.path("validation.json"),
		"--baseline", p.path("baseline.json"), "--feature-merge", p.path("feature-merge.json"),
		"--out", p.path("staging.json"))...)
	if code, err := p.controller(ctx, "await-staging", stagingArgs); err != nil || code != 0 {
		return Outcome{Code: hook.TerminalReleaseFailed, Evidence: evidence}, err
	}
	// Staging browser evidence and the production continuation are wired
	// but land in the migration's acceptance phase; the pull_request exit
	// above is the poc contract this constitution ships first.
	return Outcome{Stage: stage, Evidence: evidence}, nil
}
