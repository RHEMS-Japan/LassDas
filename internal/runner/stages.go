package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"automation.internal/ticket-ingress/internal/hook"
)

// Run drives the whole ticket path and returns the outcome for the terminal
// logic. Deliberate divergence from the workflow, called out once here: the
// YAML's report job could never see intake's "questions" outcome (its needs
// list omitted intake, so the context read as empty and the branch was
// dead); this transcription restores the intended behaviour — an intake
// that finds gaps asks instead of failing.
func (p *Pipeline) Run(ctx context.Context) (Outcome, error) {
	if err := p.WriteEnvelope(); err != nil {
		return Outcome{}, err
	}
	// ---- intake (workflow: read-ticket, read-contract) ----
	if code, err := p.worker(ctx, "read-ticket", []string{
		"read-ticket", "--config", p.Config.ConsumerConfigPath, "--tool-sha", p.Config.Identity.EngineSHA,
		"--envelope", p.path("ticket-envelope.json"),
		"--clarification-out", p.path("clarification.json"),
		"--out", p.path("raw-ticket.json"),
	}); err != nil || code != 0 {
		return Outcome{Code: "internal_failed"}, err
	}
	if code, err := p.worker(ctx, "read-contract", []string{
		"read-contract", "--config", p.Config.ConsumerConfigPath, "--tool-sha", p.Config.Identity.EngineSHA,
		"--raw", p.path("raw-ticket.json"), "--out", p.path("intake.json"),
	}); err != nil || code != 0 {
		return Outcome{Code: "internal_failed"}, err
	}
	gaps, err := p.readJSONField("intake.json", "gaps")
	if err != nil {
		return Outcome{Code: "internal_failed"}, err
	}
	if gaps != "" && gaps != "[]" && gaps != "null" {
		// Intake found gaps: ask. The readiness decision file doubles as
		// the question payload exactly as the workflow's question path
		// consumed readiness/decision.json — here intake.json carries the
		// gap questions and the questioner path accepts it.
		return Outcome{Code: hook.TerminalClarificationRequired, QuestionDecisionPath: p.path("intake.json")}, nil
	}

	// ---- source (build-draft, locate/derive, baseline, snapshot) ----
	code, err := p.worker(ctx, "build-draft", []string{
		"build-draft", "--config", p.Config.ConsumerConfigPath, "--tool-sha", p.Config.Identity.EngineSHA,
		"--raw", p.path("raw-ticket.json"), "--intake", p.path("intake.json"),
		"--out", p.path("ticket-draft.json"),
	})
	if err != nil {
		return Outcome{Code: "internal_failed"}, err
	}
	switch code {
	case 0:
	case 2:
		return Outcome{Code: hook.TerminalInputRejected, ParseRejected: true}, nil
	default:
		return Outcome{Code: "internal_failed"}, nil
	}
	if err := p.resolveConsumer(); err != nil {
		return Outcome{Code: "internal_failed"}, err
	}

	repoRoot := p.path("target-repo")
	if err := p.cloneTarget(ctx, repoRoot); err != nil {
		return Outcome{Code: "internal_failed"}, err
	}
	if code, err := p.controller(ctx, "baseline", []string{
		"baseline", "--config", p.Config.ConsumerConfigPath,
		"--draft", p.path("ticket-draft.json"), "--out", p.path("baseline.json"),
	}); err != nil || code != 0 {
		return Outcome{Code: "internal_failed"}, err
	}
	baseSHA, err := p.readJSONField("baseline.json", "baseline", "Integration", "SHA")
	if err != nil || len(baseSHA) != 40 {
		return Outcome{Code: "internal_failed"}, fmt.Errorf("baseline sha invalid: %q (%v)", baseSHA, err)
	}
	if code, err := p.gitIn(ctx, repoRoot, "checkout", "--detach", baseSHA); err != nil || code != 0 {
		return Outcome{Code: "internal_failed"}, err
	}

	absent, err := p.readJSONField("ticket-draft.json", "absent_text")
	if err != nil {
		return Outcome{Code: "internal_failed"}, err
	}
	if absent != "" {
		if code, err := p.worker(ctx, "locate-target", []string{
			"locate-target", "--config", p.Config.ConsumerConfigPath, "--tool-sha", p.Config.Identity.EngineSHA,
			"--draft", p.path("ticket-draft.json"), "--repo-root", repoRoot,
			"--out", p.path("readiness-ticket.json"),
		}); err != nil || code != 0 {
			return Outcome{Code: "internal_failed"}, err
		}
	} else {
		if code, err := p.worker(ctx, "list-candidates", []string{
			"list-candidates", "--config", p.Config.ConsumerConfigPath, "--tool-sha", p.Config.Identity.EngineSHA,
			"--draft", p.path("ticket-draft.json"), "--repo-root", repoRoot, "--base-sha", baseSHA,
			"--out", p.path("candidate-listing.json"),
		}); err != nil || code != 0 {
			return Outcome{Code: "internal_failed"}, err
		}
		code, err := p.worker(ctx, "derive-contract", []string{
			"derive-contract", "--config", p.Config.ConsumerConfigPath, "--tool-sha", p.Config.Identity.EngineSHA,
			"--draft", p.path("ticket-draft.json"), "--listing", p.path("candidate-listing.json"),
			"--derivation-out", p.path("derivation.json"),
			"--out", p.path("readiness-ticket.json"),
		}, p.modelKeyEnv()...)
		if err != nil {
			return Outcome{Code: "internal_failed"}, err
		}
		switch code {
		case 0:
		case 2:
			return Outcome{Code: hook.TerminalInputRejected, ParseRejected: true}, nil
		default:
			return Outcome{Code: "internal_failed"}, nil
		}
	}
	if code, err := p.worker(ctx, "snapshot", []string{
		"snapshot", "--config", p.Config.ConsumerConfigPath, "--tool-sha", p.Config.Identity.EngineSHA,
		"--ticket", p.path("readiness-ticket.json"), "--repo-root", repoRoot, "--base-sha", baseSHA,
		"--out", p.path("readiness-source.json"),
	}); err != nil || code != 0 {
		return Outcome{Code: "internal_failed"}, err
	}

	// ---- model: readiness gate, then at most three finite stages ----
	outcome, err := p.modelStage(ctx, repoRoot, baseSHA)
	if err != nil || outcome.Code != "" || outcome.QuestionDecisionPath != "" {
		return outcome, err
	}

	// ---- validation ----
	if failed, err := p.validationStage(ctx, outcome.Stage); err != nil {
		return Outcome{Code: "internal_failed"}, err
	} else if failed {
		return Outcome{Code: hook.TerminalValidationFailed}, nil
	}

	// ---- delivery ----
	return p.deliveryStage(ctx, outcome.Stage)
}

// cloneTarget prepares the consumer working copy the way the workflow's
// source job checked it out.
func (p *Pipeline) cloneTarget(ctx context.Context, repoRoot string) error {
	if _, err := os.Stat(repoRoot); err == nil {
		if err := os.RemoveAll(repoRoot); err != nil {
			return err
		}
	}
	repository, err := p.readJSONField("ticket-draft.json", "repository")
	if err != nil {
		return err
	}
	url := "https://github.com/" + repository + ".git"
	if token := os.Getenv("TARGET_GITHUB_TOKEN"); token != "" {
		url = "https://x-access-token:" + token + "@github.com/" + repository + ".git"
	}
	command := exec.CommandContext(ctx, "git", "clone", "--quiet", url, repoRoot)
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	return command.Run()
}

func (p *Pipeline) gitIn(ctx context.Context, dir string, arguments ...string) (int, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, arguments...)...)
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	if err := command.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode(), nil
		}
		return -1, err
	}
	return 0, nil
}

func (p *Pipeline) modelKeyEnv() []string {
	return []string{
		"MODEL_API_KEY_IMPLEMENTER=" + os.Getenv("MODEL_API_KEY_IMPLEMENTER"),
		"MODEL_API_KEY_REVIEWER=" + os.Getenv("MODEL_API_KEY_REVIEWER"),
	}
}

// clarificationArgs appends --clarification when the claimed envelope
// carried adopted answers.
func (p *Pipeline) clarificationArgs() []string {
	if p.exists("clarification.json") {
		return []string{"--clarification", p.path("clarification.json")}
	}
	return nil
}

// modelStage mirrors the workflow's single "Gate readiness, then run at
// most three finite model stages" step.
func (p *Pipeline) modelStage(ctx context.Context, repoRoot, baseSHA string) (Outcome, error) {
	historyDir := p.path("history")
	readinessDir := historyDir + "/readiness"
	if err := os.MkdirAll(readinessDir, 0o755); err != nil {
		return Outcome{}, err
	}
	// Readiness: up to three assess/check attempts (MaxReadinessAttempts).
	readinessArgs := []string{}
	for attempt := 1; attempt <= 3; attempt++ {
		assessment := fmt.Sprintf("%s/assessment-%d.json", readinessDir, attempt)
		check := fmt.Sprintf("%s/check-%d.json", readinessDir, attempt)
		assessArgs := []string{
			"assess-readiness", "--config", p.Config.ConsumerConfigPath, "--tool-sha", p.Config.Identity.EngineSHA,
			"--ticket", p.path("readiness-ticket.json"), "--source", p.path("readiness-source.json"),
			"--knowledge-root", p.Config.KnowledgeRoot, "--attempt", strconv.Itoa(attempt),
		}
		if attempt > 1 {
			assessArgs = append(assessArgs,
				"--previous-assessment", fmt.Sprintf("%s/assessment-%d.json", readinessDir, attempt-1),
				"--previous-check", fmt.Sprintf("%s/check-%d.json", readinessDir, attempt-1),
			)
		}
		assessArgs = append(assessArgs, p.clarificationArgs()...)
		assessArgs = append(assessArgs, "--out", assessment)
		if code, err := p.worker(ctx, "assess-readiness", assessArgs, p.modelKeyEnv()...); err != nil || code != 0 {
			return Outcome{Code: hook.TerminalModelFailed}, err
		}
		checkArgs := []string{
			"check-readiness", "--config", p.Config.ConsumerConfigPath, "--tool-sha", p.Config.Identity.EngineSHA,
			"--ticket", p.path("readiness-ticket.json"), "--source", p.path("readiness-source.json"),
			"--knowledge-root", p.Config.KnowledgeRoot, "--assessment", assessment,
		}
		checkArgs = append(checkArgs, p.clarificationArgs()...)
		checkArgs = append(checkArgs, "--out", check)
		if code, err := p.worker(ctx, "check-readiness", checkArgs, p.modelKeyEnv()...); err != nil || code != 0 {
			return Outcome{Code: hook.TerminalModelFailed}, err
		}
		readinessArgs = append(readinessArgs, "--assessment", assessment, "--check", check)
		verdict, err := p.readJSONField(relPath(p.Workspace, check), "verdict")
		if err != nil {
			return Outcome{Code: hook.TerminalModelFailed}, err
		}
		if verdict == "pass" || attempt == 3 {
			break
		}
	}
	decision := readinessDir + "/decision.json"
	decideArgs := append([]string{
		"decide-readiness", "--config", p.Config.ConsumerConfigPath, "--tool-sha", p.Config.Identity.EngineSHA,
		"--ticket", p.path("readiness-ticket.json"), "--source", p.path("readiness-source.json"),
	}, readinessArgs...)
	decideArgs = append(decideArgs, "--out", decision)
	if code, err := p.worker(ctx, "decide-readiness", decideArgs, p.modelKeyEnv()...); err != nil || code != 0 {
		return Outcome{Code: hook.TerminalModelFailed}, err
	}
	readinessOutcome, err := p.readJSONField(relPath(p.Workspace, decision), "outcome")
	if err != nil {
		return Outcome{Code: hook.TerminalModelFailed}, err
	}
	switch readinessOutcome {
	case "ready":
	case "clarification_required":
		return Outcome{Code: hook.TerminalClarificationRequired, QuestionDecisionPath: decision}, nil
	case "reject":
		return Outcome{Code: hook.TerminalReadinessRejected}, nil
	default:
		return Outcome{Code: hook.TerminalReadinessUnresolved}, nil
	}

	// Implementation: at most three implement/review/decide stages.
	for stage := 1; stage <= 3; stage++ {
		stageDir := fmt.Sprintf("%s/stage-%d", historyDir, stage)
		if err := os.MkdirAll(stageDir, 0o755); err != nil {
			return Outcome{}, err
		}
		implementArgs := []string{
			"implement", "--config", p.Config.ConsumerConfigPath, "--tool-sha", p.Config.Identity.EngineSHA,
			"--draft", p.path("ticket-draft.json"), "--repo-root", repoRoot,
			"--base-root", repoRoot, "--base-sha", baseSHA,
			"--knowledge-root", p.Config.KnowledgeRoot, "--stage", strconv.Itoa(stage),
		}
		if stage > 1 {
			previous := fmt.Sprintf("%s/stage-%d", historyDir, stage-1)
			implementArgs = append(implementArgs,
				"--previous-findings", previous+"/claude-correctness.json",
				"--previous-findings", previous+"/codex-adversarial.json",
			)
		}
		implementArgs = append(implementArgs, p.clarificationArgs()...)
		implementArgs = append(implementArgs,
			"--run-out", stageDir+"/implement-run.json",
			"--ticket-out", stageDir+"/ticket.json",
			"--source-out", stageDir+"/source.json",
			"--out", stageDir+"/candidate.json",
		)
		if code, err := p.worker(ctx, "implement", implementArgs, p.modelKeyEnv()...); err != nil || code != 0 {
			return Outcome{Code: hook.TerminalModelFailed}, err
		}
		reviewArgs := []string{
			"review", "--config", p.Config.ConsumerConfigPath, "--tool-sha", p.Config.Identity.EngineSHA,
			"--ticket", stageDir + "/ticket.json", "--source", stageDir + "/source.json",
			"--candidate", stageDir + "/candidate.json",
		}
		reviewArgs = append(reviewArgs, p.clarificationArgs()...)
		reviewArgs = append(reviewArgs, "--reviewer", "claude-correctness", "--out", stageDir+"/claude-correctness.json")
		claudeCode, err := p.worker(ctx, "review", reviewArgs, p.modelKeyEnv()...)
		if err != nil {
			return Outcome{Code: hook.TerminalModelFailed}, err
		}
		agentArgs := []string{
			"agent-review", "--config", p.Config.ConsumerConfigPath, "--tool-sha", p.Config.Identity.EngineSHA,
			"--ticket", stageDir + "/ticket.json", "--source", stageDir + "/source.json",
			"--candidate", stageDir + "/candidate.json", "--repo-root", repoRoot, "--base-sha", baseSHA,
			"--knowledge-root", p.Config.KnowledgeRoot,
		}
		agentArgs = append(agentArgs, p.clarificationArgs()...)
		agentArgs = append(agentArgs, "--reviewer", "codex-adversarial",
			"--run-out", stageDir+"/review-run.json", "--out", stageDir+"/codex-adversarial.json")
		codexCode, err := p.worker(ctx, "agent-review", agentArgs, p.modelKeyEnv()...)
		if err != nil {
			return Outcome{Code: hook.TerminalModelFailed}, err
		}
		if claudeCode != 0 || codexCode != 0 {
			return Outcome{Code: hook.TerminalModelFailed}, nil
		}
		if code, err := p.worker(ctx, "decide", []string{
			"decide", "--config", p.Config.ConsumerConfigPath, "--tool-sha", p.Config.Identity.EngineSHA,
			"--ticket", stageDir + "/ticket.json", "--source", stageDir + "/source.json",
			"--candidate", stageDir + "/candidate.json",
			"--review", stageDir + "/claude-correctness.json", "--review", stageDir + "/codex-adversarial.json",
			"--out", stageDir + "/decision.json",
		}); err != nil || code != 0 {
			return Outcome{Code: hook.TerminalModelFailed}, err
		}
		stageOutcome, err := p.readJSONField(relPath(p.Workspace, stageDir+"/decision.json"), "outcome")
		if err != nil {
			return Outcome{Code: hook.TerminalModelFailed}, err
		}
		switch stageOutcome {
		case "converged":
			return Outcome{Stage: stage}, nil
		case "revise":
			if stage == 3 {
				return Outcome{Code: hook.TerminalModelFailed}, nil
			}
		case "nonconverged":
			if stage != 3 {
				return Outcome{Code: hook.TerminalModelFailed}, nil
			}
			questionArgs := []string{
				"impasse-question", "--config", p.Config.ConsumerConfigPath, "--tool-sha", p.Config.Identity.EngineSHA,
				"--ticket", stageDir + "/ticket.json", "--source", stageDir + "/source.json",
				"--candidate", stageDir + "/candidate.json",
				"--review", stageDir + "/claude-correctness.json", "--review", stageDir + "/codex-adversarial.json",
			}
			questionArgs = append(questionArgs, p.clarificationArgs()...)
			questionDecision := historyDir + "/question/decision.json"
			if err := os.MkdirAll(historyDir+"/question", 0o755); err != nil {
				return Outcome{}, err
			}
			questionArgs = append(questionArgs, "--out", questionDecision)
			code, err := p.worker(ctx, "impasse-question", questionArgs, p.modelKeyEnv()...)
			if err == nil && code == 0 {
				impasseOutcome, readErr := p.readJSONField(relPath(p.Workspace, questionDecision), "outcome")
				if readErr == nil && impasseOutcome == "clarification_required" {
					return Outcome{Code: hook.TerminalClarificationRequired, QuestionDecisionPath: questionDecision}, nil
				}
			}
			return Outcome{Code: hook.TerminalNonconverged}, nil
		default:
			return Outcome{Code: hook.TerminalModelFailed}, nil
		}
	}
	return Outcome{Code: hook.TerminalModelFailed}, nil
}

func relPath(base, full string) string {
	rel, err := filepath.Rel(base, full)
	if err != nil {
		return full
	}
	return rel
}
