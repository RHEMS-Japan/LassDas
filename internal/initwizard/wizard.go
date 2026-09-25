package initwizard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/worker"
	"github.com/charmbracelet/huh"
)

type UI interface {
	Ask(id, label, defaultValue string, secret bool) (string, error)
	// Choose presents numbered options and returns the index of the one
	// picked. A blank field asks a person to already know the answer's
	// shape; a list asks them to recognise it. It carries an id and a
	// proposal for the same reason Ask does: a run with nobody at the
	// keyboard has to be able to answer it.
	Choose(id, label string, options []Option, proposed int) (int, error)
	Confirm(string) (bool, error)
	Info(string)
}

// boolIndex is where a two-option list starts: the second option is the
// one that turns the thing on.
func boolIndex(on bool) int {
	if on {
		return 1
	}
	return 0
}

// Option is one thing a person can pick: what it is, and what picking it
// means for them.
type Option struct {
	Label  string
	Detail string
}
type TerminalUI struct{}

func (TerminalUI) Ask(_, label, value string, secret bool) (string, error) {
	input := huh.NewInput().Title(label).Value(&value)
	if secret {
		input.EchoMode(huh.EchoModePassword)
	}
	err := input.Run()
	return strings.TrimSpace(value), needsTerminal(err)
}
func (TerminalUI) Choose(_, label string, options []Option, proposed int) (int, error) {
	if len(options) == 0 {
		return 0, errors.New("選べるものがありません")
	}
	choices := make([]huh.Option[int], 0, len(options))
	for index, option := range options {
		text := option.Label
		if option.Detail != "" {
			text += " — " + option.Detail
		}
		choices = append(choices, huh.NewOption(text, index))
	}
	picked := proposed
	if picked < 0 || picked >= len(options) {
		picked = 0
	}
	err := huh.NewSelect[int]().Title(label).Options(choices...).Value(&picked).Run()
	return picked, needsTerminal(err)
}

func (TerminalUI) Confirm(label string) (bool, error) {
	yes := false
	err := huh.NewConfirm().Title(label).Affirmative("進める").Negative("中断").Value(&yes).Run()
	return yes, needsTerminal(err)
}

// needsTerminal replaces the prompt library's own words for "there is no
// terminal here" with words that say what to do about it.
//
// Asked through a wrapper that allocates no terminal - an editor's shell, a
// CI step, an agent running a command on someone's behalf - the prompt
// cannot open and the failure arrived as
// "huh: could not open a new TTY: open /dev/tty: device not configured".
// A person reading that has no idea the answer is "run it in a terminal
// window" (live 2026-09-18, and the wrapper swallowed even that line, so
// the command appeared to do nothing at all).
func needsTerminal(err error) error {
	if err == nil || !strings.Contains(err.Error(), "TTY") {
		return err
	}
	return errors.New("この操作は画面で 1 つずつ聞くので、端末が要ります。" +
		"エディタや自動化からではなく、ターミナルの窓で直接実行してください")
}
func (TerminalUI) Info(value string) { fmt.Println(value) }

// Runtime is the localrun adapter. Keeping its process work behind a seam lets
// interrupted interviews and generated configurations be tested without Docker.
type Runtime interface {
	Start(context.Context, *State, string) (json.RawMessage, error)
	Stop(context.Context, *State, string) error
}
type SmokeFunc func(context.Context, *State, Secrets, func() error) error
type Wizard struct {
	UI      UI
	API     API
	Process Process
	Runtime Runtime
	Smoke   SmokeFunc
	// RegistryLogin is the distributor's login command from the installed
	// note, shown verbatim when the registry denies the image pull. Empty for
	// a public image.
	RegistryLogin string
}
type Options struct{ Project, Home, RepoRoot, Redo string }

// errPlacedElsewhere ends apply where the instance is placed by hand: every
// stage before it has run, and the one that tries a ticket cannot.
var errPlacedElsewhere = errors.New("この instance は別の場所に置かれます。設定と鍵の準備は終わっています")

// placedElsewhere reports the answer that says where this instance runs.
//
// apply used to start a container on the machine it ran on, whatever that
// answer said. Someone who answered "Kubernetes" got a container on their
// laptop - and on 2026-09-18 that put a second instance against a live
// project, pointed at the same tickets as the one already running.
//
// The engine cannot place an instance on an arbitrary host, and naming the
// hosts it knows would only move the limit to the next one. So when the
// answer names a place, apply prepares everything and stops: what has to run
// is in `lassdas run spec`, and whoever is installing puts it there.
func placedElsewhere(s *State) (bool, string) {
	if s == nil || s.RepoRoot == "" {
		return false, ""
	}
	answers, err := LoadAnswers(s.RepoRoot)
	if err != nil {
		return false, ""
	}
	host, ok := answers.Value("host")
	if !ok || strings.TrimSpace(host) == "" {
		return false, ""
	}
	return true, strings.TrimSpace(host)
}

// elsewhereNotice says what is ready and what is left to do.
func elsewhereNotice(ui UI, s *State, host string) error {
	ui.Info("設定と鍵の準備ができました。本体は起動していません。\n" +
		"動かす場所として次が記録されています:\n  " + host + "\n\n" +
		"何を動かせばよいかは `lassdas run spec --project " + s.Project + "` が出します " +
		"(image / 実行ユーザー / 板の port / 環境のファイル / 設定 / 残す必要のある書き込み先)。\n" +
		"そこへ置いて、起動していること・板に到達できること・再起動しても書き込み先が残ることを確かめ、" +
		"見方と止め方を .lassdas/progress.md に実際のコマンドで書いてください。\n\n" +
		"このマシンの docker で動かすなら、回答の host を消してから apply をやり直すか、" +
		"`lassdas run start --project " + s.Project + "` を実行してください。")
	return nil
}

func (w *Wizard) ask(s *State, id, label, defaultValue string, secret bool) (string, error) {
	start := time.Now()
	value, err := w.UI.Ask(id, label, defaultValue, secret)
	s.Metrics.ActiveSeconds += time.Since(start).Seconds()
	s.Metrics.Fields++
	if defaultValue != "" && value == defaultValue {
		s.Metrics.Defaulted++
	} else if defaultValue != "" {
		s.Metrics.Reentered++
	} else {
		s.Metrics.Additional++
	}
	return value, err
}

// choose offers a list and counts the interaction the same way ask does.
func (w *Wizard) choose(s *State, id, label string, options []Option, proposed int) (int, error) {
	start := time.Now()
	picked, err := w.UI.Choose(id, label, options, proposed)
	s.Metrics.ActiveSeconds += time.Since(start).Seconds()
	s.Metrics.Fields++
	return picked, err
}

func (w *Wizard) confirm(s *State, label string) error {
	start := time.Now()
	yes, err := w.UI.Confirm(label)
	s.Metrics.ActiveSeconds += time.Since(start).Seconds()
	s.Metrics.Fields++
	if err != nil {
		return err
	}
	if !yes {
		return errors.New("中断しました。保存した回答から再開できます")
	}
	return nil
}
func (w *Wizard) field(s *State, stage, id, label string, value *string, defaultValue string) error {
	if s.Completed[stage] != "" && *value != "" {
		return nil
	}
	if *value != "" {
		defaultValue = *value
	}
	answer, err := w.ask(s, id, label, defaultValue, false)
	if err != nil {
		return err
	}
	if answer == "" {
		return errors.New(label + "を入力してください")
	}
	*value = answer
	return nil
}
func (w *Wizard) secret(s *State, secrets Secrets, name, label string, replace bool) error {
	if secrets[name] != "" && !replace {
		return nil
	}
	value, err := w.ask(s, name, label, "", true)
	if err != nil {
		return err
	}
	if value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n\x00") {
		return errors.New("鍵は空白・改行を含まない値にしてください")
	}
	secrets[name] = value
	return nil
}

func (w *Wizard) Run(ctx context.Context, options Options) (*State, error) {
	if w.UI == nil {
		w.UI = TerminalUI{}
	}
	if w.Process == nil {
		w.Process = ExecProcess{}
	}
	if w.Runtime == nil {
		return nil, errors.New("本体の起動処理がありません")
	}
	for _, name := range []string{"git", "go", "docker"} {
		if _, err := w.Process.LookPath(name); err != nil {
			return nil, fmt.Errorf("%s が見つかりません。ソース版には git・Go・Docker Desktop が必要です", name)
		}
	}
	if options.Home == "" {
		var err error
		options.Home, err = os.UserHomeDir()
		if err != nil {
			return nil, err
		}
	}
	if options.RepoRoot == "" {
		var err error
		options.RepoRoot, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	root, err := w.Process.Run(ctx, "git", []string{"rev-parse", "--show-toplevel"}, options.RepoRoot)
	if err != nil {
		return nil, errors.New("納品先の git repo 内で実行してください")
	}
	options.RepoRoot = strings.TrimSpace(string(root))
	dir, err := ProjectDir(options.Home, options.Project)
	if err != nil {
		return nil, err
	}
	if err = secureDir(filepath.Dir(dir), 0700); err != nil {
		return nil, err
	}
	if err = secureDir(dir, 0700); err != nil {
		return nil, err
	}
	s, secrets, err := Load(dir)
	if err != nil {
		return nil, err
	}
	if s.Project != "" && s.Project != options.Project {
		return s, errors.New("保存 project の身元が一致しません")
	}
	if s.RepoRoot != "" && s.RepoRoot != options.RepoRoot {
		return s, errors.New("この project は別の納品先 repo で作成済みです")
	}
	s.Project = options.Project
	s.RepoRoot = options.RepoRoot
	s.Metrics.ExternalKeyAcquisition = "利用者が init の外で取得。所要時間は未計測"
	if s.AutomationRunID == "" {
		s.AutomationRunID, err = newRunID()
		if err != nil {
			return s, err
		}
	}
	if options.Redo != "" {
		if !validStage(options.Redo) {
			return s, errors.New("--redo の段名が不正です")
		}
		if s.Completed["runtime"] != "" && options.Redo != "smoke" {
			if err = w.Runtime.Stop(ctx, s, dir); err != nil {
				return s, err
			}
		}
		if err = s.Redo(options.Redo); err != nil {
			return s, err
		}
	}
	save := func() error {
		// Once running, credentials change only together with the stopped
		// instance's config. Before first startup, retain entered runtime keys.
		if s.Completed["runtime"] == "" {
			return Save(dir, s, secrets)
		}
		return Save(dir, s, nil)
	}
	if err = save(); err != nil {
		return s, err
	}
	steps := []struct {
		name string
		run  func() error
	}{
		{"prepare", func() error { return w.prepare(ctx, s, secrets) }},
		{"consumer", func() error { return w.consumer(ctx, s, secrets, dir) }},
		{"tracker", func() error { return w.trackerStage(ctx, s, secrets, save) }},
		{"models", func() error { return w.models(ctx, s, secrets) }},
		{"runtime", func() error { return w.start(ctx, s, secrets, dir) }},
		{"smoke", func() error {
			// Nothing is running here to try a ticket against: the instance
			// is placed by whoever is installing, wherever they answered.
			// Saying "本体は起動しました" after not starting it was the
			// screen contradicting itself in consecutive lines.
			if placed, host := placedElsewhere(s); placed {
				w.UI.Info("試験依頼はまだ流せません。" + host + " に置いて動き出してから、" +
					"`lassdas setup smoke --project " + s.Project + "` を実行してください。")
				return errPlacedElsewhere
			}
			if w.Smoke == nil {
				return errors.New("本体は起動済みですが、依頼から PR までの動作確認が未接続です。init は未完了です")
			}
			return w.Smoke(ctx, s, secrets, save)
		}},
	}
	for _, step := range steps {
		w.UI.Info("段階: " + step.name)
		if err = step.run(); err != nil {
			// Keep the running-instance marker after a readiness error. A
			// failed observation must not authorize overwriting live credentials.
			if step.name != "runtime" {
				delete(s.Completed, step.name)
			}
			_ = save()
			return s, err
		}
		// The fingerprint names nonsecret stage inputs only. Keys and key hashes
		// are excluded, and this marker never skips current authentication.
		s.Completed[step.name] = fingerprint(struct {
			Repository, Branch, Image, BaseURL string
			Mode                               worker.ModeConfig
		}{s.Repository, s.Branch, s.Image, s.BaseURL, s.Mode})
		if err = save(); err != nil {
			return s, err
		}
	}
	w.UI.Info("init 完了: 依頼の PR と検証記録を確認しました")
	return s, nil
}

func (w *Wizard) prepare(ctx context.Context, s *State, secrets Secrets) error {
	w.UI.Info("対象リポジトリへの PR 作成に使う GitHub アクセストークンを入力してください。取得先: https://github.com/settings/personal-access-tokens。対象リポジトリの Contents と Pull requests に書き込み権限が必要です。")
	if err := w.secret(s, secrets, "TARGET_GITHUB_TOKEN", "GitHub アクセストークン", false); err != nil {
		return err
	}
	candidate := ""
	if raw, err := w.Process.Run(ctx, "git", []string{"remote", "get-url", "origin"}, s.RepoRoot); err == nil {
		remote := strings.TrimSuffix(strings.TrimSpace(string(raw)), ".git")
		if strings.HasPrefix(remote, "https://github.com/") {
			candidate = strings.TrimPrefix(remote, "https://github.com/")
		} else if strings.HasPrefix(remote, "git@github.com:") {
			candidate = strings.TrimPrefix(remote, "git@github.com:")
		}
	}
	if err := w.field(s, "prepare", "repository", "納品先 repo (owner/name)", &s.Repository, candidate); err != nil {
		return err
	}
	id, defaultBranch, err := w.API.Repository(ctx, s.Repository, secrets["TARGET_GITHUB_TOKEN"])
	if err != nil {
		var apiErr *APIError
		if !errors.As(err, &apiErr) || (apiErr.Status != 401 && apiErr.Status != 403 && apiErr.Status != 404) {
			return err
		}
		if err = w.secret(s, secrets, "TARGET_GITHUB_TOKEN", "repo が読めません。対象を許可した GitHub トークンを再入力", true); err != nil {
			return err
		}
		id, defaultBranch, err = w.API.Repository(ctx, s.Repository, secrets["TARGET_GITHUB_TOKEN"])
		if err != nil {
			return err
		}
	}
	if s.RepositoryID != 0 && s.RepositoryID != id {
		return errors.New("納品先 repo の数値 ID が保存時と違います")
	}
	s.RepositoryID = id
	s.DefaultBranch = defaultBranch
	if s.Completed["prepare"] == "" {
		for _, path := range []string{"AGENTS.md", "CLAUDE.md", "CONTRIBUTING.md"} {
			raw, readErr := w.API.File(ctx, s.Repository, path, defaultBranch, secrets["TARGET_GITHUB_TOKEN"])
			if readErr == nil {
				w.UI.Info("取り込み枝を決めるための repo 規則 (" + path + "):\n" + string(raw))
			}
		}
	}
	if err = w.field(s, "prepare", "branch", "repo の規則に従う取り込み枝 (default branch とは限りません)", &s.Branch, ""); err != nil {
		return err
	}
	sha, err := w.API.Branch(ctx, s.Repository, s.Branch, secrets["TARGET_GITHUB_TOKEN"])
	if err != nil {
		return err
	}
	s.BaseSHA = sha
	if s.Completed["prepare"] == "" {
		if err = w.confirm(s, "納品先 "+s.Repository+" の "+s.Branch+" へ PR を出す設定で進めます"); err != nil {
			return err
		}
	}
	if err = w.field(s, "prepare", "engine-repository", "image の本体ソース repo (owner/name)", &s.EngineRepository, ""); err != nil {
		return err
	}
	engineID, _, err := w.API.Repository(ctx, s.EngineRepository, secrets["TARGET_GITHUB_TOKEN"])
	if err != nil {
		return err
	}
	if s.EngineRepositoryID != 0 && s.EngineRepositoryID != engineID {
		return errors.New("本体ソース repo の数値 ID が変わりました")
	}
	s.EngineRepositoryID = engineID
	if err = w.field(s, "prepare", "image", "配布者から受け取った image@sha256:digest", &s.Image, ""); err != nil {
		return err
	}
	if err = w.field(s, "prepare", "engine-sha", "その image に対応する本体ソースの 40 桁 SHA", &s.EngineSHA, ""); err != nil {
		return err
	}
	if err = w.field(s, "prepare", "build-record", "image と SHA の対応を確認した既存ビルド記録の URL または場所", &s.BuildRecord, ""); err != nil {
		return err
	}
	if !worker.ValidToolSHA(s.EngineSHA) {
		return errors.New("本体ソースは 40 桁 SHA で指定してください")
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	if err = w.API.GitHub(ctx, "/repos/"+s.EngineRepository+"/commits/"+s.EngineSHA, secrets["TARGET_GITHUB_TOKEN"], &commit); err != nil {
		return err
	}
	if commit.SHA != s.EngineSHA {
		return errors.New("本体ソース SHA の実在を確認できません")
	}
	return w.imageCheck(ctx, s)
}

func (w *Wizard) consumer(ctx context.Context, s *State, secrets Secrets, dir string) error {
	if s.Completed["consumer"] == "" {
		proposal := s.Mode
		if proposal.ID == "" {
			proposal = worker.ModeConfig{ID: "cli-change", ForbiddenCandidateText: []string{"LassDas"}, MaxFiles: 8, MaxFileBytes: 393216, MaxTotalBytes: 1048576, MaxChangedLines: 3000, MaxChangedBytes: 196608, VerifyWorkingDirectory: "."}
			var tree struct {
				Truncated bool                          `json:"truncated"`
				Tree      []struct{ Path, Type string } `json:"tree"`
			}
			if err := w.API.GitHub(ctx, "/repos/"+s.Repository+"/git/trees/"+s.BaseSHA+"?recursive=1", secrets["TARGET_GITHUB_TOKEN"], &tree); err != nil {
				return err
			}
			if tree.Truncated {
				return errors.New("repo のファイル一覧が省略されました。初版の納品先検査では扱えません")
			}
			seen := map[string]bool{}
			hasGo, hasNode, pnpm := false, false, false
			for _, entry := range tree.Tree {
				if entry.Type != "blob" || strings.HasPrefix(entry.Path, ".") {
					continue
				}
				part := strings.Split(entry.Path, "/")[0]
				scope := part
				if strings.Contains(entry.Path, "/") {
					scope += "/"
				}
				if !seen[scope] {
					proposal.AllowedFilePrefixes = append(proposal.AllowedFilePrefixes, scope)
					seen[scope] = true
				}
				hasGo = hasGo || entry.Path == "go.mod"
				hasNode = hasNode || entry.Path == "package.json"
				pnpm = pnpm || entry.Path == "pnpm-lock.yaml"
			}
			if hasGo {
				proposal.InstallCommand = []string{"go", "mod", "download"}
				proposal.VerifyCommands = [][]string{{"go", "test", "./..."}, {"go", "build", "./..."}}
				raw, err := w.docker(ctx, s, "run", "--rm", "--network=none", "--entrypoint", "go", s.Image, "version")
				if err != nil {
					return errors.New("image に Go がありません。配布者に使用可能な道具を確認してください")
				}
				parts := strings.Fields(string(raw))
				if len(parts) != 4 || parts[0] != "go" || parts[1] != "version" || !strings.HasPrefix(parts[2], "go") {
					return errors.New("image の Go の版を読み取れません")
				}
				proposal.Toolchain = []worker.ToolRequirement{{Binary: "go", Version: strings.TrimPrefix(parts[2], "go")}}
			} else if hasNode {
				binary := "npm"
				proposal.InstallCommand = []string{"npm", "ci"}
				if pnpm {
					binary = "pnpm"
					proposal.InstallCommand = []string{"pnpm", "install", "--frozen-lockfile"}
				}
				proposal.VerifyCommands = [][]string{{binary, "test"}}
				for _, tool := range []string{"node", binary} {
					raw, err := w.docker(ctx, s, "run", "--rm", "--network=none", "--entrypoint", tool, s.Image, "--version")
					if err != nil {
						return fmt.Errorf("image に %s がありません", tool)
					}
					proposal.Toolchain = append(proposal.Toolchain, worker.ToolRequirement{Binary: tool, Version: strings.TrimPrefix(strings.TrimSpace(string(raw)), "v"), StripVPrefix: tool == "node"})
				}
			}
		}
		// Each JSON array is one editable value; argument boundaries remain
		// explicit and arbitrary shell fragments are never evaluated by init.
		fields := []struct {
			id, label string
			value     any
		}{
			{"scope", "編集を許すディレクトリ接頭辞と root ファイル (JSON 配列)", &proposal.AllowedFilePrefixes},
			{"toolchain", "道具と必要な版 (JSON 配列: binary, version, strip_v_prefix)", &proposal.Toolchain},
			{"install", "依存導入コマンドの引数 (JSON 配列)", &proposal.InstallCommand},
			{"verify", "検証コマンド 1〜4 本 (JSON 二重配列)", &proposal.VerifyCommands},
		}
		for _, field := range fields {
			raw, _ := json.Marshal(field.value)
			answer, err := w.ask(s, field.id, field.label, string(raw), false)
			if err != nil {
				return err
			}
			if json.Unmarshal([]byte(answer), field.value) != nil {
				return errors.New(field.label + "の JSON が不正です")
			}
		}
		value, err := w.ask(s, "verify-directory", "検証する repo 内ディレクトリ", proposal.VerifyWorkingDirectory, false)
		if err != nil {
			return err
		}
		proposal.VerifyWorkingDirectory = value
		limits := []struct {
			id, label string
			value     *int
		}{{"max-files", "変更ファイル上限", &proposal.MaxFiles}, {"max-file-bytes", "1 ファイルの byte 上限", &proposal.MaxFileBytes}, {"max-total-bytes", "合計 byte 上限", &proposal.MaxTotalBytes}, {"max-changed-lines", "変更行数上限", &proposal.MaxChangedLines}, {"max-changed-bytes", "変更 byte 上限", &proposal.MaxChangedBytes}}
		for _, field := range limits {
			value, err := w.ask(s, field.id, field.label, strconv.Itoa(*field.value), false)
			if err != nil {
				return err
			}
			*field.value, err = strconv.Atoi(value)
			if err != nil {
				return errors.New(field.label + "は整数です")
			}
		}
		s.Mode = proposal
		if err = w.confirm(s, "確定した枝の clone に対し、この image 内で依存導入と検証を実行します"); err != nil {
			return err
		}
	}
	return w.checkConsumer(ctx, s, secrets, dir)
}

func (w *Wizard) trackerStage(ctx context.Context, s *State, secrets Secrets, save func() error) error {
	w.UI.Info("Backlog の API キーは個人設定 → API で取得してください。project の参照・コメント・課題状態更新が必要です。起票者本人のキーも、名義を確認したうえで自動処理に使えます")
	if err := w.field(s, "tracker", "tracker-origin", "Backlog 接続先 (https://space.backlog.com)", &s.Tracker.Origin, ""); err != nil {
		return err
	}
	origin, err := url.Parse(s.Tracker.Origin)
	if err != nil {
		return errors.New("Backlog 接続先が不正です")
	}
	s.Tracker.SpaceKey = strings.Split(origin.Hostname(), ".")[0]
	if err = w.field(s, "tracker", "tracker-project", "Backlog project キー", &s.Tracker.ProjectKey, ""); err != nil {
		return err
	}
	if s.Completed["tracker"] == "" {
		value, err := w.ask(s, "creator-id", "起票を許可する本人の数値 ID", strconv.FormatInt(s.Tracker.AllowedCreatorID, 10), false)
		if err != nil {
			return err
		}
		s.Tracker.AllowedCreatorID, err = strconv.ParseInt(value, 10, 64)
		if err != nil || s.Tracker.AllowedCreatorID <= 0 {
			return errors.New("起票者 ID は正の整数です")
		}
	}
	if err = w.field(s, "tracker", "category", "受付のカテゴリ名", &s.Category, "自動処理"); err != nil {
		return err
	}
	defaults := []string{"自動処理中", "回答待ち", "処理済み", "未対応"}
	labels := []string{"処理中", "回答待ち", "納品済み", "要確認"}
	for i := range defaults {
		if err = w.field(s, "tracker", fmt.Sprintf("status-%d", i), labels[i]+"に対応する既存状態名 (無い場合のみ確認後に作成)", &s.StatusNames[i], defaults[i]); err != nil {
			return err
		}
	}
	if err = w.secret(s, secrets, "BACKLOG_API_KEY", "自動処理に使う Backlog API キー", false); err != nil {
		return err
	}
	err = w.tracker(ctx, s, secrets, save)
	var apiErr *APIError
	if errors.As(err, &apiErr) && (apiErr.Status == 401 || apiErr.Status == 403) {
		if err = w.secret(s, secrets, "BACKLOG_API_KEY", "キーの持ち主・project を読めません。API キーを再入力", true); err != nil {
			return err
		}
		err = w.tracker(ctx, s, secrets, save)
	}
	return err
}

const openRouterBaseURL = "https://openrouter.ai/api/v1"

func (w *Wizard) models(ctx context.Context, s *State, secrets Secrets) error {
	if s.BaseURL == "" {
		// Asked once per project. The engine speaks OpenAI-compatible chat
		// completions and nothing else, so a provider here is a name and a
		// base URL - never an adapter. Changing it later is a new project:
		// the stored keys belong to the provider that issued them.
		options := make([]Option, 0, len(ModelProviders)+1)
		for _, provider := range ModelProviders {
			options = append(options, Option{Label: provider.Name, Detail: provider.Detail})
		}
		options = append(options, Option{Label: "その他 (OpenAI 互換の接続先を自分で入れる)", Detail: "chat completions が OpenAI 互換なら動く。費用の読み取りは接続先次第"})
		picked, err := w.choose(s, "model-base-url", "モデルの接続先", options, 0)
		if err != nil {
			return err
		}
		if picked < len(ModelProviders) {
			s.BaseURL = ModelProviders[picked].BaseURL
		} else {
			value, err := w.ask(s, "model-base-url-other", "接続先の URL (https://…/v1)", "", false)
			if err != nil {
				return err
			}
			s.BaseURL = strings.TrimRight(strings.TrimSpace(value), "/")
		}
	}
	if err := CheckModelBaseURL(s.BaseURL); err != nil {
		return err
	}
	w.UI.Info("モデルの接続先は " + ProviderName(s.BaseURL) + " です。疎通確認にも API の利用料がかかります")
	if s.ModelKeyMode == "" {
		s.ModelKeyMode = modelKeysShared
		// Preserve keys from earlier versions or interrupted role-by-role
		// input; sharing is the default only for a new configuration.
		if secrets["LASSDAS_INTAKE_TARGET_KEY"] != "" || secrets[keyName("implementer")] != "" {
			s.ModelKeyMode = modelKeysSeparate
		}
	}
	if s.ModelKeyMode != modelKeysShared && s.ModelKeyMode != modelKeysSeparate {
		return errors.New("モデルの鍵の設定は shared または separate です")
	}
	if s.Completed["models"] == "" {
		// Offered as a list rather than typed. "yes/no" makes a person work
		// out which way round the question runs and what each way costs
		// them; the options say it.
		picked, err := w.choose(s, "separate-model-keys", "モデルの鍵をどう持つか", []Option{
			{Label: "1 本を全役で共用する", Detail: "始めやすい。費用と利用上限は全役の合算になる"},
			{Label: "役ごとに別の鍵にする", Detail: "役ごとに費用と上限を分けられる。鍵をその数だけ用意する"},
		}, boolIndex(s.ModelKeyMode == modelKeysSeparate))
		if err != nil {
			return err
		}
		s.ModelKeyMode = modelKeysShared
		if picked == 1 {
			s.ModelKeyMode = modelKeysSeparate
		}
		picked, err = w.choose(s, "separate-design", "設計のレビューを誰がやるか", []Option{
			{Label: "変更のレビュー役と同じ 2 モデル", Detail: "設定が 1 組で済む"},
			{Label: "設計専用に別の 2 モデル", Detail: "設計と変更で別の目で見る。鍵とモデルを 2 つ増やす"},
		}, boolIndex(s.SeparateDesignReviews))
		if err != nil {
			return err
		}
		s.SeparateDesignReviews = picked == 1
		for _, role := range allRoles(s) {
			endpoint := s.Models[role]
			value, err := w.ask(s, role+"-model", role+" のモデル名", endpoint.Model, false)
			if err != nil {
				return err
			}
			endpoint.Model = value
			vendor := endpoint.Vendor
			if vendor == "" {
				vendor = VendorFor(value)
			}
			value, err = w.ask(s, role+"-vendor", role+" のモデル提供会社 (接続先ホスト名からは判定しません)", vendor, false)
			if err != nil {
				return err
			}
			endpoint.Vendor = value
			endpoint.ID = role
			endpoint.BaseURL = s.BaseURL
			endpoint.APIKeyEnv = keyName(role)
			endpoint.MaxOutputTokens = 8192
			if strings.Contains(role, "review-") || role == "readiness-checker" {
				endpoint.Lens = "Find concrete correctness, safety, scope, and acceptance gaps."
				endpoint.MaxOutputTokens = 4096
			}
			s.Models[role] = endpoint
		}
	}
	if err := w.modelKeys(s, secrets, false); err != nil {
		return err
	}
	repairKeys := func() error {
		if err := w.confirm(s, "鍵を再入力して全身元を再検査します"); err != nil {
			return err
		}
		return w.modelKeys(s, secrets, true)
	}
	// Saved duplicate values must have a repair path too: --redo retains
	// credentials, so rejecting Generate alone would reject every later run.
	if err := ValidateModelKeys(s, secrets); err != nil {
		w.UI.Info(err.Error())
		if err := repairKeys(); err != nil {
			return err
		}
	}
	if _, _, _, err := Generate(s, secrets); err != nil {
		return err
	}
	if err := w.modelPreflight(ctx, s, secrets); err != nil {
		// On a resumed or partially completed attempt, permit credential repair
		// without deleting answers or manufacturing a new identity.
		w.UI.Info(err.Error())
		if err = repairKeys(); err != nil {
			return err
		}
		return w.modelPreflight(ctx, s, secrets)
	}
	return nil
}

func (w *Wizard) modelKeys(s *State, secrets Secrets, replace bool) error {
	if s.ModelKeyMode == modelKeysShared {
		w.UI.Info(ProviderName(s.BaseURL) + " のキー 1 本を全役で共用します。キー単位の利用上限と費用は全役の合算になります")
		if err := w.secret(s, secrets, "LASSDAS_INTAKE_TARGET_KEY", ProviderName(s.BaseURL)+" の API キー", replace); err != nil {
			return err
		}
		for _, role := range allRoles(s) {
			secrets[keyName(role)] = secrets["LASSDAS_INTAKE_TARGET_KEY"]
		}
		return nil
	}
	if err := w.secret(s, secrets, "LASSDAS_INTAKE_TARGET_KEY", "実装役の身元用の "+ProviderName(s.BaseURL)+" API キー (直接の呼出しは無い)", replace); err != nil {
		return err
	}
	for _, role := range allRoles(s) {
		if err := w.secret(s, secrets, keyName(role), role+" 専用の "+ProviderName(s.BaseURL)+" API キー", replace); err != nil {
			return err
		}
	}
	return nil
}

func (w *Wizard) start(ctx context.Context, s *State, secrets Secrets, dir string) error {
	if s.BoardPort == 0 {
		s.BoardPort = 9200
	}
	if s.Completed["runtime"] == "" {
		value, err := w.ask(s, "board-port", "板を 127.0.0.1 で開く port", strconv.Itoa(s.BoardPort), false)
		if err != nil {
			return err
		}
		s.BoardPort, err = strconv.Atoi(value)
		if err != nil || s.BoardPort < 1024 || s.BoardPort > 65535 {
			return errors.New("板の port は 1024〜65535 です")
		}
	}
	consumer, runtime, env, err := Generate(s, secrets)
	if err != nil {
		return err
	}
	if generatedUnchanged(dir, consumer, runtime, env) {
		if placed, host := placedElsewhere(s); placed {
			return elsewhereNotice(w.UI, s, host)
		}
		result, err := w.Runtime.Start(ctx, s, dir)
		if err != nil {
			return err
		}
		if !json.Valid(result) {
			return errors.New("本体の起動検査記録がありません")
		}
		s.Checks["runtime"] = result
		return nil
	}
	if err = w.confirm(s, fmt.Sprintf("保存先 %s\n納品先 %s / %s\n板 http://127.0.0.1:%d\n設定を保存して本体を起動します", dir, s.Repository, s.Branch, s.BoardPort)); err != nil {
		return err
	}
	// Stop this instance before replacing even one runtime credential/config.
	if err = w.Runtime.Stop(ctx, s, dir); err != nil {
		return err
	}
	if err = writeConfigs(dir, consumer, runtime); err != nil {
		return err
	}
	if err = Save(dir, s, env); err != nil {
		return err
	}
	delete(secrets, "LASSDAS_BOARD_USER")
	delete(secrets, "LASSDAS_BOARD_PASS")
	for key, value := range env {
		secrets[key] = value
	}
	if err = w.checkRuntime(ctx, s, dir); err != nil {
		return errors.New("image 内の本体設定検査に失敗しました。起動していません")
	}
	if placed, host := placedElsewhere(s); placed {
		return elsewhereNotice(w.UI, s, host)
	}
	result, err := w.Runtime.Start(ctx, s, dir)
	if err != nil {
		return err
	}
	if !json.Valid(result) {
		return errors.New("本体の起動検査記録がありません")
	}
	s.Checks["runtime"] = result
	w.UI.Info(fmt.Sprintf("板: http://127.0.0.1:%d (ローカル閲覧用・認証不要)", s.BoardPort))
	return nil
}

func validStage(stage string) bool {
	for _, name := range stages {
		if name == stage {
			return true
		}
	}
	return false
}

func generatedUnchanged(dir string, consumer, runtime any, env Secrets) bool {
	for name, value := range map[string]any{"m1-consumer.json": consumer, "runtime.json": runtime} {
		want, err := marshal(value)
		if err != nil {
			return false
		}
		got, err := os.ReadFile(filepath.Join(dir, "config", name))
		if err != nil || !bytes.Equal(got, want) {
			return false
		}
	}
	_, saved, err := Load(dir)
	return err == nil && reflect.DeepEqual(saved, env)
}
