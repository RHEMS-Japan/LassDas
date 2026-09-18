package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/initsmoke"
	"automation.internal/ticket-ingress/internal/initwizard"
	"automation.internal/ticket-ingress/internal/localrun"
)

// errSmokePending ends `setup apply` where the person takes over: the body
// is running, and the test request is filed under the person's own name
// with a key the agent never sees.
var errSmokePending = fmt.Errorf("本体は起動しました。次は利用者が `lassdas setup smoke --project <name>` を実行し、本人の鍵で試験依頼を 1 本流してください。導入はまだ完了していません (%w)", initsmoke.ErrPending)

// runSetup is the entry an agent uses. check reads the answers file and
// says what is missing without running anything; apply runs the wizard's
// stages with the file's answers up to the running body; secrets and smoke
// are the person's two turns (keys, and the test request in their name).
func runSetup(ctx context.Context, command, project, repoRoot, home, redo string, manager localrun.Manager, output io.Writer) error {
	root, err := repositoryRoot(ctx, repoRoot)
	if err != nil {
		return err
	}
	switch command {
	case "setup check":
		return setupCheck(root, home, output)
	case "setup secrets":
		return setupSecrets(ctx, project, root, home, output)
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
			answers, err := loadAnswersWithDistribution(root, home)
			if err != nil {
				return err
			}
			if problems := answers.Check(root); len(problems) > 0 {
				return errors.New("回答が足りません。`lassdas setup check` の指摘を直してください:\n" + strings.Join(problems, "\n"))
			}
			if notice := initwizard.HostNotice(answers); notice != "" {
				_, _ = fmt.Fprintln(output, notice)
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
		if command == "setup apply" && redo == "" {
			stale, err := staleAgainstNote(dir, root, home)
			if err != nil {
				return err
			}
			if stale != "" {
				_, _ = fmt.Fprintln(output, stale)
				redo = "prepare"
			}
		}
		wizard := initwizard.Wizard{UI: ui, API: api, Process: process, Runtime: runtimeAdapter{manager}, Smoke: smoke, RegistryLogin: noteRegistryLogin(home)}
		_, err = wizard.Run(ctx, initwizard.Options{Project: project, Home: home, RepoRoot: root, Redo: redo})
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
// loadAnswersWithDistribution reads the file and fills the four answers
// the distributor's note carries (image, engine sha, build record, body
// repository) where the file leaves them out.
func loadAnswersWithDistribution(root, home string) (initwizard.Answers, error) {
	answers, err := initwizard.LoadAnswers(root)
	if err != nil {
		return initwizard.Answers{}, err
	}
	distribution, found, err := initwizard.LoadDistribution(home)
	if err != nil {
		return initwizard.Answers{}, err
	}
	if found {
		answers = answers.WithDistribution(distribution)
	}
	return answers, nil
}

func setupCheck(root, home string, output io.Writer) error {
	var problems []string
	// The same three the wizard requires before it starts (source build,
	// clone, container), named here so the gap is known before anything runs.
	for _, name := range []string{"git", "go", "docker"} {
		if _, err := exec.LookPath(name); err != nil {
			problems = append(problems, fmt.Sprintf("道具がありません: %s (ローカルで本体を動かすのに必要)", name))
		}
	}
	if _, err := exec.LookPath("docker"); err == nil {
		// Bounded: a stuck daemon must not hang a check that runs nothing.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		info := exec.CommandContext(ctx, "docker", "info", "--format", "{{.ServerVersion}}")
		if out, err := info.Output(); err != nil || strings.TrimSpace(string(out)) == "" {
			problems = append(problems, "Docker が動いていません (Docker Desktop を起動してください)")
		}
	}
	answers, err := loadAnswersWithDistribution(root, home)
	if err != nil {
		problems = append(problems, err.Error())
	} else {
		problems = append(problems, answers.Check(root)...)
		for _, name := range []string{"agreement.md", "progress.md"} {
			if _, err := os.Stat(filepath.Join(root, ".lassdas", name)); err != nil {
				problems = append(problems, fmt.Sprintf(".lassdas/%s がありません (~/%s の 5 段を参照)", name, initwizard.InstalledInstruction))
			}
		}
	}
	// Said here, where the answer can still be changed, and not only after
	// a whole apply has run.
	if notice := initwizard.HostNotice(answers); notice != "" {
		_, _ = fmt.Fprintln(output, notice)
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
func setupSecrets(ctx context.Context, project, root, home string, output io.Writer) error {
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
	// The key mode is the file's decision, written down here so the
	// wizard never infers "separate" from the presence of a stored key. A
	// project set up the other way keeps its keys: changing the mode is
	// the wizard's own path, never a silent overwrite.
	mode, separateDesign := keyMode(answers)
	if state.ModelKeyMode != "" && state.ModelKeyMode != mode {
		return fmt.Errorf("この project の鍵の持ち方は %s で作られています。変えるなら `lassdas init --project %s --redo models` を利用者が対話で実行するか、別の project 名を使ってください", state.ModelKeyMode, project)
	}
	if state.Completed["models"] != "" && state.SeparateDesignReviews != separateDesign {
		return fmt.Errorf("この project の設計レビューの構成 (separate-design=%v) は確定済みです。変えるなら `lassdas init --project %s --redo models` を利用者が対話で実行するか、別の project 名を使ってください", state.SeparateDesignReviews, project)
	}
	state.ModelKeyMode, state.SeparateDesignReviews = mode, separateDesign
	names := secretPlan(answers)
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
	_, err = fmt.Fprintf(output, "鍵を %s に保存しました (0600)。値は repo にも会話にも出ません。\n", dir)
	if err != nil {
		return err
	}
	// The key's owner is the usual requester: shown here so the agent can
	// write creator-id from it, or ask the person for someone else's.
	if origin, ok := answers.Value("tracker-origin"); ok && secrets["BACKLOG_API_KEY"] != "" {
		id, name, err := trackerOwner(ctx, initwizard.API{}, origin, secrets["BACKLOG_API_KEY"])
		if err != nil {
			_, _ = fmt.Fprintln(output, "Backlog の鍵の持ち主を確認できませんでした ("+err.Error()+")。creator-id は利用者に確認して書いてください")
		} else {
			_, _ = fmt.Fprintf(output, "Backlog の鍵の持ち主: %s (利用者 ID %d)。起票する本人がこの人なら、setup.json の creator-id に %d を書きます\n", name, id, id)
		}
	}
	_, err = fmt.Fprintf(output, "次は AI が `lassdas setup apply --project %s` を実行します\n", project)
	return err
}

type secretEntry struct{ name, label string }

// keyMode reads the file's two choices that decide which keys exist.
func keyMode(answers initwizard.Answers) (string, bool) {
	mode := initwizard.ModelKeysShared
	if answers.Flag("separate-model-keys") {
		mode = initwizard.ModelKeysSeparate
	}
	return mode, answers.Flag("separate-design")
}

// secretPlan lists exactly the keys the wizard will look for under the
// file's choices: one OpenRouter key shared by every role, or one per
// role - the design reviewers included when they are separate.
func secretPlan(answers initwizard.Answers) []secretEntry {
	names := []secretEntry{
		{"TARGET_GITHUB_TOKEN", "納品先 repo に PR を出す GitHub アクセストークン (Contents と Pull requests の書き込み権限)"},
		{"BACKLOG_API_KEY", "自動処理に使う Backlog API キー (個人設定 → API)"},
	}
	mode, separateDesign := keyMode(answers)
	if mode == initwizard.ModelKeysShared {
		return append(names, secretEntry{"LASSDAS_INTAKE_TARGET_KEY", "OpenRouter API キー (既定では全役で共用)"})
	}
	names = append(names, secretEntry{"LASSDAS_INTAKE_TARGET_KEY", "受付・対象導出専用の OpenRouter API キー"})
	roles := initwizard.ModelRoles()
	if separateDesign {
		roles = append(roles, "design-review-a", "design-review-b")
	}
	for _, role := range roles {
		names = append(names, secretEntry{initwizard.KeyName(role), role + " 専用の OpenRouter API キー"})
	}
	return names
}

// trackerOwner asks the tracker who the key belongs to: the usual
// requester, shown so the agent can write creator-id. The space key is
// the origin's first host label, as the wizard derives it; the call fails
// softly and the key never leaves the process.
func trackerOwner(ctx context.Context, api initwizard.API, origin, key string) (int64, string, error) {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Hostname() == "" {
		return 0, "", errors.New("接続先の URL が読めません")
	}
	state := &initwizard.State{}
	state.Tracker.Origin = origin
	state.Tracker.SpaceKey = strings.Split(parsed.Hostname(), ".")[0]
	var owner struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := api.Tracker(ctx, state, key, "GET", "/api/v2/users/myself", nil, &owner); err != nil {
		return 0, "", err
	}
	if owner.ID <= 0 {
		return 0, "", errors.New("持ち主の ID がありません")
	}
	return owner.ID, owner.Name, nil
}

// noteRegistryLogin returns the login command the distributor put in the
// installed note, if any, so a denied pull can show it. A missing or broken
// note yields "" here, and the denial message then says the command could not
// be found rather than that none exists.
func noteRegistryLogin(home string) string {
	distribution, found, err := initwizard.LoadDistribution(home)
	if err != nil || !found {
		return ""
	}
	return distribution.RegistryLogin
}

// staleAgainstNote reports, in the operator's words, that this project is
// running an older body than the installed note names. A completed stage is
// skipped on the next apply, so a new note used to change nothing: the
// instance kept the image it was built with, apply said it had succeeded,
// and a fix never reached the person who installed it (live 2026-09-17).
// The answer is a line to print and a prepare to redo; an empty string
// means the project already matches the note.
func staleAgainstNote(dir, root, home string) (string, error) {
	state, _, err := initwizard.Load(dir)
	if err != nil {
		return "", err
	}
	if state.Completed["prepare"] == "" || state.Image == "" {
		// Nothing has been prepared yet; the run below does it from the
		// note as it stands.
		return "", nil
	}
	answers, err := loadAnswersWithDistribution(root, home)
	if err != nil {
		return "", err
	}
	image, _ := answers.Value("image")
	engineSHA, _ := answers.Value("engine-sha")
	if image == "" || (image == state.Image && (engineSHA == "" || engineSHA == state.EngineSHA)) {
		return "", nil
	}
	return "配布者の案内が新しくなっています (この本体: " + shortDigest(state.Image) + " / 案内: " + shortDigest(image) + ")。入れ替えるため prepare からやり直します。", nil
}

// shortDigest names an image by the head of its digest, which is what a
// person compares when they look at two of them.
func shortDigest(image string) string {
	_, digest, found := strings.Cut(image, "@sha256:")
	if !found || len(digest) < 12 {
		return image
	}
	return digest[:12]
}
