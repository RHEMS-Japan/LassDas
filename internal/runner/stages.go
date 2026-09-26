package runner

import (
	"context"
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
	"automation.internal/ticket-ingress/internal/worker"
)

// ChainPrep is what a prepared run hands to its chain of stage cards.
type ChainPrep struct {
	RepoRoot string
	BaseRoot string
	BaseSHA  string
}

// PrepareChainRun readies a delivery for its chain: the whole
// pre-implementation half of a run — workspace preparation, intake, source
// binding, the readiness gate — stopping where the implement stage would
// start. A non-empty outcome code
// (or a question decision path) means the run must not reach a chain.
//
// beforeReception runs between the two halves, and the seam is there for
// one reason: the reception is the only place this engine asks the
// requester anything, so everything a question could be about has to be
// known before it runs — while the destination's own repository is not on
// the volume until the first half has cloned it. A caller with nothing to
// do there passes nil. Its error is not the run's: what it prepares makes
// the reception better informed, and a reception that runs without it asks
// what it always asked.
func (p *Pipeline) PrepareChainRun(ctx context.Context, beforeReception func() error) (ChainPrep, Outcome, error) {
	prep, outcome, err := p.pretrip(ctx)
	if err != nil || outcome.Code != "" {
		return ChainPrep{}, outcome, err
	}
	if beforeReception != nil {
		if err := beforeReception(); err != nil {
			p.Logger.Error("the reception was prepared incompletely; it asks what it can",
				"error", err.Error())
		}
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
// source binding, workspace shaping. A non-empty outcome code means the run
// stops here.
func (p *Pipeline) pretrip(ctx context.Context) (pretripResult, Outcome, error) {
	if err := p.Prepare(); err != nil {
		return pretripResult{}, Outcome{Code: "internal_failed"}, err
	}
	// Each key's running total before this run spends anything. The report
	// subtracts it; without it a provider with no per-window billing
	// endpoint leaves the requester no cost at all.
	p.recordSpendBaseline(ctx)
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
		// The intake is a model turn too: its failure leaves the requester
		// a note and the run its detail, like the other reception stages.
		//
		// What no longer reaches here is a reader whose answers could not be
		// used: the step makes the reading itself and exits cleanly, saying
		// so in the record (reception_fallback.go). What is left is the step
		// failing to run at all — no key, no network, no room on the volume.
		p.noteReceptionCutoff(intakeStage)
		return pretripResult{}, Outcome{Code: "internal_failed"}, err
	}
	// A reading made without the model is told to the requester once, where
	// the plan notice and the closing comment both look.
	if fallback, err := p.readJSONField("intake.json", "fallback"); err == nil && fallback == "true" {
		p.recordReceptionFallback()
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

	// ---- source (build-draft, the reception contract, baseline, snapshot) ----
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
		// Nothing to search for, so nothing to name: the contract is completed
		// without files and the change decides its own. No model runs here.
		if code, err := p.worker(ctx, "reception-ticket", []string{
			"reception-ticket", "--config", p.Config.ConsumerConfigPath, "--tool-sha", p.Config.Identity.EngineSHA,
			"--draft", p.path("ticket-draft.json"),
			"--out", p.path("readiness-ticket.json"),
		}); err != nil || code != 0 {
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
		note := "実行エージェントの設定ファイルを用意できませんでした。運用担当者が設定と保存先の権限を確認します。"
		var setup *agentSetupError
		if errors.As(err, &setup) {
			note = setup.note
		}
		if writeErr := p.writeReceptionTrail(note + "\n成果物の実装・納品は始めていません。同じ設定のまま再実行しても解消しません。\n"); writeErr != nil {
			p.Logger.Error("agent setup failure trail not written", "error", writeErr.Error())
		}
		return pretripResult{}, Outcome{Code: hook.TerminalInternalFailed, Evidence: map[string]string{"failed_step": "実行エージェントの設定"}}, err
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
	// The agent reads this from its working copy (../agent-mcp.json) under
	// its own user; it names servers and the variables they read, no value.
	if err := os.WriteFile(p.path("agent-mcp.json"), []byte(mcp), 0o644); err != nil {
		return err
	}

	consumer, err := worker.LoadConfig(p.Config.ConsumerConfigPath)
	if err != nil {
		return &agentSetupError{note: "モデル・レビュー担当を含む実行設定を読み取れないか、設定の検査に合格しなかったため停止しました。運用担当者が実行設定を確認します。", cause: err}
	}
	codexCount := 0
	for _, reviewer := range consumer.Models.Reviewers {
		if filepath.Base(consumer.Agents.ReviewerAgentFor(reviewer.ID).Command) == "codex" && (len(consumer.Agents.ReviewerAgents) > 0 || reviewer.ID != "claude-correctness") {
			codexCount++
		}
	}
	if codexCount > 1 {
		return &agentSetupError{note: "複数の Codex レビュー担当が同じ設定ファイルを使う構成には対応していないため停止しました。運用担当者が担当ごとの設定の分離を確認します。", cause: errors.New("multiple Codex reviewer launches would share one provider file")}
	}
	for _, reviewer := range consumer.Models.Reviewers {
		agent := consumer.Agents.ReviewerAgentFor(reviewer.ID)
		if filepath.Base(agent.Command) != "codex" || (len(consumer.Agents.ReviewerAgents) == 0 && reviewer.ID == "claude-correctness") {
			continue
		}
		// Only a configured Codex CLI launch needs this provider file.
		// Hermes profiles and direct model calls carry their own settings.
		keyNames := make([]string, 0, len(agent.SecretEnv))
		for name := range agent.SecretEnv {
			keyNames = append(keyNames, name)
		}
		sort.Strings(keyNames)
		keyEnv := ""
		if len(keyNames) > 0 {
			keyEnv = keyNames[0]
		}
		if reviewer.BaseURL == "" || reviewer.Model == "" || reviewer.Effort == "" || keyEnv == "" {
			return &agentSetupError{note: "Codex レビュー担当の接続先・モデル・推論設定・鍵の参照先が揃っていないため停止しました。運用担当者がレビュー担当の設定を確認します。", cause: fmt.Errorf("Codex reviewer %s needs base_url/model/effort/secret_env", reviewer.ID)}
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
	// No Codex launch in either mode: discard a previous run's stale file.
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(home, ".codex", "config.toml")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// The private cause stays in the operator log. Only the explicit note is
// passed to the ticket; filesystem paths and credential values never are.
type agentSetupError struct {
	note  string
	cause error
}

func (e *agentSetupError) Error() string { return e.cause.Error() }
func (e *agentSetupError) Unwrap() error { return e.cause }

func configuredReviewerIDs(config worker.Config) []string {
	ids := make([]string, 0, len(config.Models.Reviewers))
	for _, reviewer := range config.Models.Reviewers {
		ids = append(ids, reviewer.ID)
	}
	return ids
}

func (p *Pipeline) gitIn(ctx context.Context, dir string, arguments ...string) (int, error) {
	code, err := p.step(ctx, "git "+arguments[0], append([]string{"git", "-C", dir}, arguments...))
	return code, err
}

func (p *Pipeline) modelKeyEnv() []string {
	return []string{
		"MODEL_API_KEY_IMPLEMENTER=" + os.Getenv("MODEL_API_KEY_IMPLEMENTER"),
		"MODEL_API_KEY_REVIEWER=" + os.Getenv("MODEL_API_KEY_REVIEWER"),
		// The reception's optional decision model reads its key here,
		// listed beside the other roles' keys so that the worker's key
		// variables can be read off one place. The list restricts nothing:
		// the worker inherits this process's environment, so a variable
		// this process was given reaches it whether or not it is named
		// here. What closes the workflow path to one name is the reusable
		// workflow's own secrets block, which passes only what it declares.
		"MODEL_API_KEY_DECISIONS=" + os.Getenv("MODEL_API_KEY_DECISIONS"),
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

// readinessGate is the pre-generation gate — up to three assess/check
// attempts and the decision. An empty outcome means ready: implementation
// may start.
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
		assessArgs = append(assessArgs, p.releasePathArgs()...)
		assessArgs = append(assessArgs, "--out", assessment)
		if code, err := p.worker(ctx, "assess-readiness", assessArgs, p.modelKeyEnv()...); err != nil || code != 0 {
			if outcome, decided := p.decideWithoutReaders(ctx, "受付の判定"); decided {
				return outcome, nil
			}
			p.noteReceptionCutoff("受付の判定")
			return receptionModelFailure("AI による受付の判定"), err
		}
		checkArgs := []string{
			"check-readiness", "--config", p.Config.ConsumerConfigPath, "--tool-sha", p.Config.Identity.EngineSHA,
			"--ticket", p.path("readiness-ticket.json"), "--source", p.path("readiness-source.json"),
			"--knowledge-root", p.Config.KnowledgeRoot, "--assessment", assessment,
		}
		checkArgs = append(checkArgs, p.clarificationArgs()...)
		checkArgs = append(checkArgs, p.releasePathArgs()...)
		checkArgs = append(checkArgs, "--out", check)
		if code, err := p.worker(ctx, "check-readiness", checkArgs, p.modelKeyEnv()...); err != nil || code != 0 {
			if outcome, decided := p.decideWithoutReaders(ctx, "受付の確認"); decided {
				return outcome, nil
			}
			p.noteReceptionCutoff("受付の確認")
			return receptionModelFailure("AI による受付の確認"), err
		}
		readinessArgs = append(readinessArgs, "--assessment", assessment, "--check", check)
		verdict, err := p.readJSONField(relPath(p.Workspace, check), "verdict")
		if err != nil {
			p.noteReceptionRecord("受付の確認")
			return receptionModelFailure("受付の確認の記録の読み取り"), err
		}
		// The workflow's jq -er 'select(pass|fail)' hard-failed the step on
		// anything else; a malformed verdict is a model failure, not "try
		// again".
		if verdict != "pass" && verdict != "fail" {
			p.noteReceptionRecord("受付の確認")
			return receptionModelFailure("受付の確認の記録の読み取り"), nil
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
		if outcome, decided := p.decideWithoutReaders(ctx, "受付の判定のまとめ"); decided {
			return outcome, nil
		}
		// A model verb that failed, so the note is the one that reads the
		// cause: the record note would say the record could not be read
		// under a headline saying the AI could not finish, and both land in
		// the same comment (review of #132).
		p.noteReceptionCutoff("受付の判定のまとめ")
		return receptionModelFailure("AI による受付の判定のまとめ"), err
	}
	readinessOutcome, err := p.readJSONField(relPath(p.Workspace, decision), "outcome")
	if err != nil {
		p.noteReceptionRecord("受付の判定のまとめ")
		return receptionModelFailure("受付の判定のまとめの記録の読み取り"), err
	}
	// A reader that refused the request and named nothing to ask about. The
	// request goes on as written, and the requester is told so once, where the
	// plan notice and the closing comment both look. Read before the outcome
	// below, because it is the outcome that says whether anybody is being
	// asked: a refusal that did draft questions is being asked about, and says
	// nothing more.
	if balked, err := p.readJSONField(relPath(p.Workspace, decision), "rejected_reading"); err == nil &&
		balked != "" && readinessOutcome == "ready" {
		p.recordReceptionBalked(balked)
	}
	switch readinessOutcome {
	case "ready":
		return Outcome{}, nil
	case "clarification_required":
		return Outcome{Code: hook.TerminalClarificationRequired, QuestionDecisionPath: decision}, nil
	case "reject":
		// No reader reaches here any more: a refusal over what a request says
		// is not an outcome this gate seals (internal/worker
		// sealedReceptionOutcome). The arm stays for a decision an older
		// engine sealed, and the comment it ends with now states a mechanical
		// reason and names the requester.
		return Outcome{Code: hook.TerminalReadinessRejected}, nil
	case "unresolved":
		return Outcome{Code: hook.TerminalReadinessUnresolved}, nil
	default:
		// The workflow's jq select() hard-failed on a malformed outcome; a
		// decision file this pipeline cannot read is a model failure, not a
		// legitimate readiness stop.
		p.noteReceptionRecord("受付の判定のまとめ")
		return receptionModelFailure("受付の判定のまとめの記録の読み取り"), nil
	}
}

func relPath(base, full string) string {
	rel, err := filepath.Rel(base, full)
	if err != nil {
		return full
	}
	return rel
}

// receptionModelFailure is a reception stage ending as a model failure with
// the step named for its requester. Reception generates and reviews nothing,
// so the generic sentence about "generating or reviewing the work" is not
// merely vague there — it is false.
func receptionModelFailure(step string) Outcome {
	if len(step) > hook.MaxFailedStepBytes {
		return Outcome{Code: hook.TerminalModelFailed}
	}
	return Outcome{Code: hook.TerminalModelFailed, Evidence: map[string]string{"failed_step": step}}
}
