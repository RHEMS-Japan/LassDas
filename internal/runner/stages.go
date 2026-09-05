package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
)

// Run drives the whole ticket path and returns the outcome for the terminal
// logic.
func (p *Pipeline) Run(ctx context.Context) (Outcome, error) {
	prep, outcome, err := p.pretrip(ctx)
	if err != nil || outcome.Code != "" {
		return outcome, err
	}
	repoRoot, baseRoot, baseSHA := prep.repoRoot, prep.baseRoot, prep.baseSHA

	// ---- model: readiness gate, then at most three finite stages ----
	outcome, err = p.modelStage(ctx, repoRoot, baseRoot, baseSHA)
	if err != nil || outcome.Code != "" || outcome.QuestionDecisionPath != "" {
		return outcome, err
	}

	// ---- validation ----
	if failed, err := p.validationStage(ctx, outcome.Stage, runnerReviewFiles()); err != nil {
		return Outcome{Code: "internal_failed"}, err
	} else if failed {
		return Outcome{Code: hook.TerminalValidationFailed}, nil
	}

	// ---- delivery ----
	return p.deliveryStage(ctx, outcome.Stage, runnerReviewFiles())
}

// ChainPrep is what the cards orchestration needs from a prepared run.
type ChainPrep struct {
	RepoRoot string
	BaseRoot string
	BaseSHA  string
}

// PrepareChainRun readies a delivery for the cards orchestration: the whole
// pre-implementation half of a run — workspace preparation, intake, source
// binding, the readiness gate — exactly as the runner mode executes it,
// stopping where the implement stage would start. A non-empty outcome code
// (or a question decision path) means the run must not reach a chain.
func (p *Pipeline) PrepareChainRun(ctx context.Context) (ChainPrep, Outcome, error) {
	prep, outcome, err := p.pretrip(ctx)
	if err != nil || outcome.Code != "" {
		return ChainPrep{}, outcome, err
	}
	outcome, err = p.readinessGate(ctx)
	if err != nil || outcome.Code != "" || outcome.QuestionDecisionPath != "" {
		return ChainPrep{}, outcome, err
	}
	return ChainPrep{RepoRoot: prep.repoRoot, BaseRoot: prep.baseRoot, BaseSHA: prep.baseSHA}, Outcome{}, nil
}

// pretripResult is what the pre-model half of a run leaves behind: the
// prepared workspace paths and the sealed base revision every later stage
// binds to.
type pretripResult struct {
	repoRoot string
	baseRoot string
	baseSHA  string
}

// pretrip is the pre-model half of a run — workspace preparation, intake,
// source binding, workspace shaping — shared verbatim by the runner mode
// (Run) and the cards orchestration (PrepareChainRun). A non-empty outcome
// code means the run stops here.
func (p *Pipeline) pretrip(ctx context.Context) (pretripResult, Outcome, error) {
	if err := p.Prepare(); err != nil {
		return pretripResult{}, Outcome{Code: "internal_failed"}, err
	}
	// ---- intake (workflow: read-ticket, read-contract) ----
	if code, err := p.worker(ctx, "read-ticket", []string{
		"read-ticket", "--config", p.Config.ConsumerConfigPath, "--tool-sha", p.Config.Identity.EngineSHA,
		"--envelope", p.path("ticket-envelope.json"),
		"--clarification-out", p.path("clarification.json"),
		"--out", p.path("raw-ticket.json"),
	}); err != nil || code != 0 {
		return pretripResult{}, Outcome{Code: "internal_failed"}, err
	}
	if code, err := p.worker(ctx, "read-contract", []string{
		"read-contract", "--config", p.Config.ConsumerConfigPath, "--tool-sha", p.Config.Identity.EngineSHA,
		"--raw", p.path("raw-ticket.json"), "--out", p.path("intake.json"),
	}); err != nil || code != 0 {
		return pretripResult{}, Outcome{Code: "internal_failed"}, err
	}
	gaps, err := p.readJSONField("intake.json", "gaps")
	if err != nil {
		return pretripResult{}, Outcome{Code: "internal_failed"}, err
	}
	if gaps != "" && gaps != "[]" && gaps != "null" {
		// The workflow's words (report step): an intake that still has open
		// questions can only be missing the destination; until the
		// ask-and-resume path is wired end to end for it, stop honestly —
		// the requester hears that a person will follow up, instead of the
		// run dying unreported. Same honest terminal here; intake gaps are
		// not the readiness question format and are never posted as one.
		return pretripResult{}, Outcome{Code: hook.TerminalClarificationRequired}, nil
	}

	// ---- source (build-draft, locate/derive, baseline, snapshot) ----
	code, err := p.worker(ctx, "build-draft", []string{
		"build-draft", "--config", p.Config.ConsumerConfigPath, "--tool-sha", p.Config.Identity.EngineSHA,
		"--raw", p.path("raw-ticket.json"), "--intake", p.path("intake.json"),
		"--out", p.path("ticket-draft.json"),
	})
	if err != nil {
		return pretripResult{}, Outcome{Code: "internal_failed"}, err
	}
	switch code {
	case 0:
	case 2:
		return pretripResult{}, Outcome{Code: hook.TerminalInputRejected, ParseRejected: true}, nil
	default:
		return pretripResult{}, Outcome{Code: "internal_failed"}, nil
	}
	if err := p.resolveConsumer(); err != nil {
		return pretripResult{}, Outcome{Code: "internal_failed"}, err
	}

	repoRoot := p.path("target-repo")
	if err := p.cloneTargetTo(ctx, repoRoot); err != nil {
		return pretripResult{}, Outcome{Code: "internal_failed"}, err
	}
	if code, err := p.controller(ctx, "baseline", []string{
		"baseline", "--config", p.Config.ConsumerConfigPath,
		"--draft", p.path("ticket-draft.json"), "--out", p.path("baseline.json"),
	}); err != nil || code != 0 {
		return pretripResult{}, Outcome{Code: "internal_failed"}, err
	}
	baseSHA, err := p.readJSONField("baseline.json", "baseline", "Integration", "SHA")
	if err != nil || len(baseSHA) != 40 {
		return pretripResult{}, Outcome{Code: "internal_failed"}, fmt.Errorf("baseline sha invalid: %q (%v)", baseSHA, err)
	}
	if code, err := p.gitIn(ctx, repoRoot, "checkout", "--detach", baseSHA); err != nil || code != 0 {
		return pretripResult{}, Outcome{Code: "internal_failed"}, err
	}

	absent, err := p.readJSONField("ticket-draft.json", "absent_text")
	if err != nil {
		return pretripResult{}, Outcome{Code: "internal_failed"}, err
	}
	if absent != "" {
		if code, err := p.worker(ctx, "locate-target", []string{
			"locate-target", "--config", p.Config.ConsumerConfigPath, "--tool-sha", p.Config.Identity.EngineSHA,
			"--draft", p.path("ticket-draft.json"), "--repo-root", repoRoot,
			"--out", p.path("readiness-ticket.json"),
		}); err != nil || code != 0 {
			return pretripResult{}, Outcome{Code: "internal_failed"}, err
		}
	} else {
		if code, err := p.worker(ctx, "list-candidates", []string{
			"list-candidates", "--config", p.Config.ConsumerConfigPath, "--tool-sha", p.Config.Identity.EngineSHA,
			"--draft", p.path("ticket-draft.json"), "--repo-root", repoRoot, "--base-sha", baseSHA,
			"--out", p.path("candidate-listing.json"),
		}); err != nil || code != 0 {
			return pretripResult{}, Outcome{Code: "internal_failed"}, err
		}
		// A derive rejection ended as internal_failed under the workflow
		// (only build-draft fed the parse outcome its report read); the
		// same requester-facing code is kept here.
		if code, err := p.worker(ctx, "derive-contract", []string{
			"derive-contract", "--config", p.Config.ConsumerConfigPath, "--tool-sha", p.Config.Identity.EngineSHA,
			"--draft", p.path("ticket-draft.json"), "--listing", p.path("candidate-listing.json"),
			"--derivation-out", p.path("derivation.json"),
			"--out", p.path("readiness-ticket.json"),
		}, p.modelKeyEnv()...); err != nil || code != 0 {
			p.noteReceptionCutoff("契約の導出")
			return pretripResult{}, Outcome{Code: "internal_failed"}, err
		}
	}
	if code, err := p.worker(ctx, "snapshot", []string{
		"snapshot", "--config", p.Config.ConsumerConfigPath, "--tool-sha", p.Config.Identity.EngineSHA,
		"--ticket", p.path("readiness-ticket.json"), "--repo-root", repoRoot, "--base-sha", baseSHA,
		"--out", p.path("readiness-source.json"),
	}); err != nil || code != 0 {
		return pretripResult{}, Outcome{Code: "internal_failed"}, err
	}

	// ---- model workspace shaping (workflow: rebuild from the sealed tar) ----
	baseRoot := p.path("target-base")
	if err := p.shapeModelWorkspace(ctx, repoRoot, baseRoot); err != nil {
		return pretripResult{}, Outcome{Code: "internal_failed"}, err
	}
	if err := p.writeAgentConfigs(); err != nil {
		return pretripResult{}, Outcome{Code: "internal_failed"}, err
	}

	return pretripResult{repoRoot: repoRoot, baseRoot: baseRoot, baseSHA: baseSHA}, Outcome{}, nil
}

// cloneTargetTo clones the consumer repository. The token never appears in
// the URL, the command line or the stored git config: it travels through a
// one-shot GIT_ASKPASS helper, so nothing an agent can later read from the
// workspace (or /proc) carries a destination credential.
func (p *Pipeline) cloneTargetTo(ctx context.Context, destination string) error {
	if err := os.RemoveAll(destination); err != nil {
		return err
	}
	if p.cloneTarget != nil {
		return p.cloneTarget(ctx, destination)
	}
	repository, err := p.readJSONField("ticket-draft.json", "repository")
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, "git", "clone", "--quiet",
		"https://x-access-token@github.com/"+repository+".git", destination)
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
	}
	command.WaitDelay = 20 * time.Second
	if token := p.TargetToken; token != "" {
		askpass, cleanup, err := askpassHelper()
		if err != nil {
			return err
		}
		defer cleanup()
		// The token rides in the git child's environment only — never the
		// runner's own, so later stage subprocesses cannot inherit it.
		command.Env = append(os.Environ(),
			"GIT_ASKPASS="+askpass, "GIT_TERMINAL_PROMPT=0", "LASSDAS_CLONE_TOKEN="+token)
	}
	return command.Run()
}

// askpassHelper writes a private one-shot GIT_ASKPASS script that answers
// with the token from the git process's own environment. It lives outside
// the workspace and is removed as soon as the clone returns.
func askpassHelper() (string, func(), error) {
	directory, err := os.MkdirTemp("", "askpass-")
	if err != nil {
		return "", nil, err
	}
	script := filepath.Join(directory, "askpass")
	body := "#!/bin/sh\nprintf '%s' \"$LASSDAS_CLONE_TOKEN\"\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		_ = os.RemoveAll(directory)
		return "", nil, err
	}
	return script, func() { _ = os.RemoveAll(directory) }, nil
}

// shapeModelWorkspace is the workflow's "rebuild the target working copy
// from the sealed archive": the agents work on a synthetic single-commit
// history with no remote and no credential, and what a change started from
// is read from a separate copy no agent is pointed at, so it cannot be
// rewritten by the run it bounds.
func (p *Pipeline) shapeModelWorkspace(ctx context.Context, repoRoot, baseRoot string) error {
	if err := os.RemoveAll(baseRoot); err != nil {
		return err
	}
	if err := copyTree(repoRoot, baseRoot); err != nil {
		return err
	}
	if err := makeReadOnly(baseRoot); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(repoRoot, ".git")); err != nil {
		return err
	}
	for _, arguments := range [][]string{
		{"init", "-q"},
		{"add", "-A"},
		{"-c", "user.name=automation", "-c", "user.email=automation@invalid", "commit", "-qm", "base"},
	} {
		if code, err := p.gitIn(ctx, repoRoot, arguments...); err != nil || code != 0 {
			return fmt.Errorf("workspace git reshape failed at %v (%v)", arguments, err)
		}
	}
	return nil
}

// copyTree copies the working tree, skipping .git (the workflow's tar never
// carried one).
func copyTree(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if relative == ".git" || strings.HasPrefix(relative, ".git"+string(os.PathSeparator)) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(destination, relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			return os.MkdirAll(target, 0o755)
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		default:
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(target, raw, info.Mode().Perm())
		}
	})
}

func makeReadOnly(root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		return os.Chmod(path, info.Mode().Perm()&^0o222)
	})
}

// writeAgentConfigs is the transcription of the workflow's two agent setup
// steps: the ticket-tracker MCP file the implementing agent may use
// (written where the consumer contract's relative --mcp-config path finds
// it: the parent of the agent's working copy), and the reviewing agent's
// provider file under $HOME (it reads its endpoint from its own file, not
// the environment; the credential stays in the env var the file names).
func (p *Pipeline) writeAgentConfigs() error {
	mcp := `{
  "mcpServers": {
    "backlog": {
      "command": "npx",
      "args": ["-y", "backlog-mcp-server@0.15.1"],
      "env": {
        "BACKLOG_DOMAIN": "${BACKLOG_DOMAIN}",
        "BACKLOG_API_KEY": "${BACKLOG_API_KEY}"
      }
    }
  }
}
`
	if err := os.WriteFile(p.path("agent-mcp.json"), []byte(mcp), 0o600); err != nil {
		return err
	}

	raw, err := readWorkspaceFile(p.Config.ConsumerConfigPath, maxWorkspaceReadBytes)
	if err != nil {
		return err
	}
	var consumer struct {
		Models struct {
			Reviewers []struct {
				ID      string `json:"id"`
				BaseURL string `json:"base_url"`
				Model   string `json:"model"`
				Effort  string `json:"effort"`
			} `json:"reviewers"`
		} `json:"models"`
		Agents struct {
			Reviewer struct {
				SecretEnv map[string]string `json:"secret_env"`
			} `json:"reviewer"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(raw, &consumer); err != nil {
		return err
	}
	// The workflow's jq 'keys[0]' was deterministic; Go map iteration is
	// not — sort so a two-entry secret_env cannot flip the provider file
	// between runs.
	keyNames := make([]string, 0, len(consumer.Agents.Reviewer.SecretEnv))
	for name := range consumer.Agents.Reviewer.SecretEnv {
		keyNames = append(keyNames, name)
	}
	sort.Strings(keyNames)
	keyEnv := ""
	if len(keyNames) > 0 {
		keyEnv = keyNames[0]
	}
	for _, reviewer := range consumer.Models.Reviewers {
		if reviewer.ID != "codex-adversarial" {
			continue
		}
		if reviewer.BaseURL == "" || reviewer.Model == "" || reviewer.Effort == "" || keyEnv == "" {
			return fmt.Errorf("consumer config names codex-adversarial without base_url/model/effort/secret_env")
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
			return err
		}
		configTOML := fmt.Sprintf(`model_provider = "gateway"
model = %q
model_reasoning_effort = %q

[model_providers.gateway]
name = "Consumer gateway"
base_url = %q
env_key = %q
wire_api = "responses"
`, reviewer.Model, reviewer.Effort, reviewer.BaseURL, keyEnv)
		return os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte(configTOML), 0o600)
	}
	// The runner mode hardwires this reviewer, so a configuration without
	// it must fail before any model spend (the workflow's jq -er did). The
	// cards mode derives every judge from configuration and has no provider
	// file to write — but a previous run's stale one must not stay in force
	// on the persistent pod $HOME, so it is removed instead of left behind.
	if p.Config.OrchestrationCards() {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		if err := os.Remove(filepath.Join(home, ".codex", "config.toml")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return errors.New("consumer config defines no codex-adversarial reviewer")
}

func (p *Pipeline) gitIn(ctx context.Context, dir string, arguments ...string) (int, error) {
	code, err := p.step(ctx, "git "+arguments[0], append([]string{"git", "-C", dir}, arguments...))
	return code, err
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
func (p *Pipeline) modelStage(ctx context.Context, repoRoot, baseRoot, baseSHA string) (Outcome, error) {
	if outcome, err := p.readinessGate(ctx); err != nil || outcome.Code != "" || outcome.QuestionDecisionPath != "" {
		return outcome, err
	}
	return p.implementRounds(ctx, repoRoot, baseRoot, baseSHA)
}

// readinessGate is the pre-generation gate — up to three assess/check
// attempts and the decision — shared verbatim by the runner mode and the
// cards orchestration. An empty outcome means ready: implementation may
// start.
func (p *Pipeline) readinessGate(ctx context.Context) (Outcome, error) {
	historyDir := p.path("history")
	readinessDir := historyDir + "/readiness"
	if err := os.MkdirAll(readinessDir, 0o755); err != nil {
		return Outcome{Code: hook.TerminalInternalFailed}, err
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
			p.noteReceptionCutoff("受付の判定")
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
			p.noteReceptionCutoff("受付の確認")
			return Outcome{Code: hook.TerminalModelFailed}, err
		}
		readinessArgs = append(readinessArgs, "--assessment", assessment, "--check", check)
		verdict, err := p.readJSONField(relPath(p.Workspace, check), "verdict")
		if err != nil {
			return Outcome{Code: hook.TerminalModelFailed}, err
		}
		// The workflow's jq -er 'select(pass|fail)' hard-failed the step on
		// anything else; a malformed verdict is a model failure, not "try
		// again".
		if verdict != "pass" && verdict != "fail" {
			return Outcome{Code: hook.TerminalModelFailed}, nil
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
		p.noteReceptionCutoff("受付の判断")
		return Outcome{Code: hook.TerminalModelFailed}, err
	}
	readinessOutcome, err := p.readJSONField(relPath(p.Workspace, decision), "outcome")
	if err != nil {
		return Outcome{Code: hook.TerminalModelFailed}, err
	}
	switch readinessOutcome {
	case "ready":
		return Outcome{}, nil
	case "clarification_required":
		return Outcome{Code: hook.TerminalClarificationRequired, QuestionDecisionPath: decision}, nil
	case "reject":
		return Outcome{Code: hook.TerminalReadinessRejected}, nil
	case "unresolved":
		return Outcome{Code: hook.TerminalReadinessUnresolved}, nil
	default:
		// The workflow's jq select() hard-failed on a malformed outcome; a
		// decision file this pipeline cannot read is a model failure, not a
		// legitimate readiness stop.
		return Outcome{Code: hook.TerminalModelFailed}, nil
	}
}

// implementRounds is the runner mode's in-process sequencing of the finite
// implement/review/decide rounds. The cards orchestration replaces exactly
// this function with the stage-card chain.
func (p *Pipeline) implementRounds(ctx context.Context, repoRoot, baseRoot, baseSHA string) (Outcome, error) {
	historyDir := p.path("history")
	// Implementation: at most three implement/review/decide stages.
	for stage := 1; stage <= 3; stage++ {
		stageDir := fmt.Sprintf("%s/stage-%d", historyDir, stage)
		if err := os.MkdirAll(stageDir, 0o755); err != nil {
			return Outcome{Code: hook.TerminalInternalFailed}, err
		}
		implementArgs := []string{
			"implement", "--config", p.Config.ConsumerConfigPath, "--tool-sha", p.Config.Identity.EngineSHA,
			"--draft", p.path("ticket-draft.json"), "--repo-root", repoRoot,
			"--base-root", baseRoot, "--base-sha", baseSHA,
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
				return Outcome{Code: hook.TerminalInternalFailed}, err
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
