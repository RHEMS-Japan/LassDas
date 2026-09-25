package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/initsmoke"
	"automation.internal/ticket-ingress/internal/initwizard"
	"automation.internal/ticket-ingress/internal/localrun"
	"automation.internal/ticket-ingress/internal/worker"
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
		return setupCheck(ctx, root, home, project, nil, output)
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

func setupCheck(ctx context.Context, root, home, project string, client *http.Client, output io.Writer) error {
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
	if notice := keyLimitNotice(ctx, home, project, client); notice != "" {
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

// keyLimitNotice warns when a stored provider key has no spending limit on
// it. The engine carries no budget: it goes on changing its approach until
// the request is done, and what stops a delivery that never will be is the
// provider refusing the key. A key with no limit removes that stop, and
// nothing downstream would ever say so.
//
// It reads, never writes, and never prints a key. A project with no stored
// key yet — the ordinary state the first time this runs — has nothing to
// check and is passed over in silence; a provider that cannot be reached is
// said plainly rather than reported as "no limit", which would be a claim
// this could not support.
func keyLimitNotice(ctx context.Context, home, project string, client *http.Client) string {
	if project == "" {
		return ""
	}
	dir, err := initwizard.ProjectDir(home, project)
	if err != nil {
		return ""
	}
	state, secrets, err := initwizard.Load(dir)
	if err != nil || state.BaseURL == "" {
		return ""
	}
	// One reading per distinct key, not per variable: a setup that shares
	// one key across every role would otherwise ask the provider about the
	// same key seven times and say the same thing seven times.
	//
	// One client for all of them, with a deadline short enough that a
	// provider which has stopped answering costs seconds rather than
	// minutes. A check that runs nothing must not sit silent while somebody
	// waits for it: seven keys against a client of its own, at the usual
	// timeout, is nearly two minutes of nothing.
	if client == nil {
		client = keyLimitClient()
	}
	checked := map[string]bool{}
	unlimited, unreachable := 0, 0
	for _, name := range providerKeyNames(secrets) {
		value := secrets[name]
		if value == "" || checked[value] {
			continue
		}
		checked[value] = true
		limit, err := worker.ReadKeyLimit(ctx, client, state.BaseURL, value)
		if err != nil {
			unreachable++
			continue
		}
		if !limit.Limited() {
			unlimited++
		}
	}
	// Both, when both happened. A key with no limit and a key that could
	// not be reached are different things to do something about, and the
	// second one said nothing about itself while the first was reported.
	var lines []string
	if unlimited > 0 {
		lines = append(lines, "警告: モデルの鍵に利用上限が設定されていません ("+strconv.Itoa(unlimited)+" 本)。本体は自分では費用を打ち切りません — 依頼が終わるまで手を替えて進み続けるので、止まるのは提供元が鍵を断ったときだけです。"+initwizard.ProviderName(state.BaseURL)+" の鍵の設定画面で上限とリセット周期 (日次・週次・月次) を決めてください。上限に達したら本体は課題にその旨を書いて待ち、上限が上がるかリセットされた時点で続きから再開します")
	}
	if unreachable > 0 {
		lines = append(lines, "鍵の利用上限を確認できませんでした ("+strconv.Itoa(unreachable)+" 本。"+initwizard.ProviderName(state.BaseURL)+" に接続できないか、その鍵が使えません)。上限が未設定のままだと本体は費用を自分で打ち切りません")
	}
	return strings.Join(lines, "\n")
}

// keyLimitClient is the one client every key's reading goes through. One
// rather than one each: the readings are sequential against the same host,
// so they share a connection instead of opening and closing one per key.
func keyLimitClient() *http.Client { return &http.Client{Timeout: keyLimitTimeout} }

// keyLimitTimeout bounds the whole of one key's reading. The check does
// nothing else and somebody is waiting at a terminal for it; at the usual
// timeout a provider that had stopped answering held a setup with a key
// per role for nearly two minutes, saying nothing.
const keyLimitTimeout = 5 * time.Second

// providerKeyNames are the stored variables that hold a key to the model
// provider, in a stable order. The destination and tracker keys are not
// among them: they are not billed by the gateway and have no limit to read.
func providerKeyNames(secrets initwizard.Secrets) []string {
	names := make([]string, 0, len(secrets))
	for name := range secrets {
		if strings.HasPrefix(name, "LASSDAS_") && strings.HasSuffix(name, "_KEY") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
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
	// the wizard's own path, never a silent overwrite. The connection
	// target is the file's decision for the same reason: changing it on an
	// existing project would point stored keys at a provider that never
	// issued them.
	baseURL := modelBaseURL(answers)
	if err := initwizard.CheckModelBaseURL(baseURL); err != nil {
		return err
	}
	if state.BaseURL != "" && state.BaseURL != baseURL {
		return fmt.Errorf("この project のモデル接続先は %s で作られています。変えるなら別の project 名を使ってください (保存済みの鍵はその接続先のものです)", state.BaseURL)
	}
	state.BaseURL = baseURL
	mode, separateDesign := keyMode(answers)
	if state.ModelKeyMode != "" && state.ModelKeyMode != mode {
		return fmt.Errorf("この project の鍵の持ち方は %s で作られています。変えるなら `lassdas init --project %s --redo models` を利用者が対話で実行するか、別の project 名を使ってください", state.ModelKeyMode, project)
	}
	if state.Completed["models"] != "" && state.SeparateDesignReviews != separateDesign {
		return fmt.Errorf("この project の設計レビューの構成 (separate-design=%v) は確定済みです。変えるなら `lassdas init --project %s --redo models` を利用者が対話で実行するか、別の project 名を使ってください", state.SeparateDesignReviews, project)
	}
	state.ModelKeyMode, state.SeparateDesignReviews = mode, separateDesign
	// The depth is offered as a list, and a list answers with the wizard's
	// own proposal: nobody is at the keyboard to pick anything else. So the
	// file's answer is put into the state here, before the wizard runs, and
	// the wizard proposes it back — the same way the two key questions
	// above already travel from the file to the interview.
	if depth, ok := answers.Value("delivery-depth"); ok {
		if err := initwizard.CheckDeliveryDepth(depth); err != nil {
			return err
		}
		state.Delivery = depth
	}
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

// modelBaseURL reads where the roles' models are reached, defaulting to the
// first offered provider so an answers file written before this existed
// keeps working.
func modelBaseURL(answers initwizard.Answers) string {
	if value, ok := answers.Value("model-base-url"); ok {
		return value
	}
	return initwizard.ModelProviders[0].BaseURL
}

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
		return append(names, secretEntry{"LASSDAS_INTAKE_TARGET_KEY", initwizard.ProviderName(modelBaseURL(answers)) + " の API キー (既定では全役で共用)"})
	}
	names = append(names, secretEntry{"LASSDAS_INTAKE_TARGET_KEY", "実装役の身元用の " + initwizard.ProviderName(modelBaseURL(answers)) + " API キー (直接の呼出しは無い)"})
	roles := initwizard.ModelRoles()
	if separateDesign {
		roles = append(roles, "design-review-a", "design-review-b")
	}
	for _, role := range roles {
		names = append(names, secretEntry{initwizard.KeyName(role), role + " 専用の " + initwizard.ProviderName(modelBaseURL(answers)) + " API キー"})
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
