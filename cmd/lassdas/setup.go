package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"automation.internal/ticket-ingress/internal/initsmoke"
	"automation.internal/ticket-ingress/internal/initwizard"
	"automation.internal/ticket-ingress/internal/localrun"
)

// errSmokePending ends `setup apply` where the person takes over: the body
// is running, and the test request is filed under the person's own name
// with a key the agent never sees.
var errSmokePending = errors.New("本体は起動しました。次は利用者が `lassdas setup smoke --project <name>` を実行し、本人の鍵で試験依頼を 1 本流してください。導入はまだ完了していません")

// runSetup is the entry an agent uses. check reads the answers file and
// says what is missing without running anything; apply runs the wizard's
// stages with the file's answers up to the running body; secrets and smoke
// are the person's two turns (keys, and the test request in their name).
func runSetup(ctx context.Context, command, project, repoRoot, home string, manager localrun.Manager, output io.Writer) error {
	root, err := repositoryRoot(ctx, repoRoot)
	if err != nil {
		return err
	}
	switch command {
	case "setup check":
		return setupCheck(root, output)
	case "setup secrets":
		return setupSecrets(project, root, home, output)
	case "setup apply", "setup smoke":
		if project == "" {
			return errors.New("--project NAME が必要です (この本体の保存名。英小文字・数字・ハイフン)")
		}
		dir, err := initwizard.ProjectDir(home, project)
		if err != nil {
			return err
		}
		api := initwizard.API{}
		process := initwizard.ExecProcess{}
		var ui initwizard.UI
		var smoke initwizard.SmokeFunc
		if command == "setup apply" {
			answers, err := initwizard.LoadAnswers(root)
			if err != nil {
				return err
			}
			if problems := answers.Check(); len(problems) > 0 {
				return errors.New("回答が足りません。`lassdas setup check` の指摘を直してください:\n" + strings.Join(problems, "\n"))
			}
			ui = &initwizard.AnswersUI{Answers: answers, Project: project, Out: func(line string) { _, _ = fmt.Fprintln(output, line) }}
			smoke = func(context.Context, *initwizard.State, initwizard.Secrets, func() error) error {
				return errSmokePending
			}
		} else {
			terminal := initwizard.TerminalUI{}
			ui = terminal
			runner := initsmoke.Runner{UI: terminal, API: api, Observer: initsmoke.RuntimeObserver{Manager: manager, Process: process, API: api, Dir: dir}}
			smoke = runner.Run
		}
		wizard := initwizard.Wizard{UI: ui, API: api, Process: process, Runtime: runtimeAdapter{manager}, Smoke: smoke}
		_, err = wizard.Run(ctx, initwizard.Options{Project: project, Home: home, RepoRoot: root})
		return err
	}
	return errors.New("setup の操作名が不明です")
}

// repositoryRoot resolves the delivery repository the answers file lives
// in: the given root, or the current directory, must be inside a git repo.
func repositoryRoot(ctx context.Context, repoRoot string) (string, error) {
	if repoRoot == "" {
		var err error
		if repoRoot, err = os.Getwd(); err != nil {
			return "", err
		}
	}
	command := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	command.Dir = repoRoot
	root, err := command.Output()
	if err != nil {
		return "", errors.New("納品先の git repo 内で実行するか、--repo-root で指定してください")
	}
	return strings.TrimSpace(string(root)), nil
}

// setupCheck says, without running anything, what the setup still lacks:
// the tools the body needs on this machine, and the answers the file does
// not carry. It never asks for a key.
func setupCheck(root string, output io.Writer) error {
	var problems []string
	// The same three the wizard requires before it starts (source build,
	// clone, container), named here so the gap is known before anything runs.
	for _, name := range []string{"git", "go", "docker"} {
		if _, err := exec.LookPath(name); err != nil {
			problems = append(problems, fmt.Sprintf("道具がありません: %s (ローカルで本体を動かすのに必要)", name))
		}
	}
	answers, err := initwizard.LoadAnswers(root)
	if err != nil {
		problems = append(problems, err.Error())
	} else {
		problems = append(problems, answers.Check()...)
		for _, name := range []string{"agreement.md", "progress.md"} {
			if _, err := os.Stat(filepath.Join(root, ".lassdas", name)); err != nil {
				problems = append(problems, fmt.Sprintf(".lassdas/%s がありません (docs/SETUP.md の 4 段を参照)", name))
			}
		}
	}
	if len(problems) == 0 {
		_, err := fmt.Fprintln(output, "不足なし。次は利用者が `lassdas setup secrets --project <name>` で鍵を入れ、AI が `lassdas setup apply --project <name>` を実行します")
		return err
	}
	for _, problem := range problems {
		_, _ = fmt.Fprintln(output, "- "+problem)
	}
	return fmt.Errorf("不足が %d 件あります", len(problems))
}

// setupSecrets is the person's turn: the keys the setup needs, typed here
// and stored under the project directory (0600), never in the repository
// and never through the agent.
func setupSecrets(project, root, home string, output io.Writer) error {
	if project == "" {
		return errors.New("--project NAME が必要です (この本体の保存名。英小文字・数字・ハイフン)")
	}
	dir, err := initwizard.ProjectDir(home, project)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	state, secrets, err := initwizard.Load(dir)
	if err != nil {
		return err
	}
	if state.RepoRoot != "" && state.RepoRoot != root {
		return errors.New("この project は別の納品先 repo で作成済みです")
	}
	state.Project, state.RepoRoot = project, root
	answers, _ := initwizard.LoadAnswers(root)
	separate, _ := answers.Value("separate-model-keys")
	names := []struct{ name, label string }{
		{"TARGET_GITHUB_TOKEN", "納品先 repo に PR を出す GitHub アクセストークン (Contents と Pull requests の書き込み権限)"},
		{"BACKLOG_API_KEY", "自動処理に使う Backlog API キー (個人設定 → API)"},
		{"LASSDAS_INTAKE_TARGET_KEY", "OpenRouter API キー (既定では全役で共用)"},
	}
	if separate == "true" || separate == "yes" {
		names = names[:2]
		names = append(names, struct{ name, label string }{"LASSDAS_INTAKE_TARGET_KEY", "受付・対象導出専用の OpenRouter API キー"})
		for _, role := range initwizard.ModelRoles() {
			names = append(names, struct{ name, label string }{initwizard.KeyName(role), role + " 専用の OpenRouter API キー"})
		}
	}
	terminal := initwizard.TerminalUI{}
	for _, entry := range names {
		if secrets[entry.name] != "" {
			_, _ = fmt.Fprintln(output, entry.name+": 保存済み (置き換えるなら空のまま Enter せず新しい値を入力)")
		}
		value, err := terminal.Ask(entry.name, entry.label, "", true)
		if err != nil {
			return err
		}
		if value == "" && secrets[entry.name] != "" {
			continue
		}
		if value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n\x00") {
			return errors.New("鍵は空白・改行を含まない値にしてください: " + entry.name)
		}
		secrets[entry.name] = value
	}
	if err := initwizard.Save(dir, state, secrets); err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "鍵を %s に保存しました (0600)。値は repo にも会話にも出ません。次は AI が `lassdas setup apply --project %s` を実行します\n", dir, project)
	return err
}
