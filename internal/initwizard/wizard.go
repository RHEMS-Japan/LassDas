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
	Confirm(string) (bool, error)
	Info(string)
}
type TerminalUI struct{}

func (TerminalUI) Ask(_, label, value string, secret bool) (string, error) {
	input := huh.NewInput().Title(label).Value(&value)
	if secret {
		input.EchoMode(huh.EchoModePassword)
	}
	err := input.Run()
	return strings.TrimSpace(value), err
}
func (TerminalUI) Confirm(label string) (bool, error) {
	yes := false
	err := huh.NewConfirm().Title(label).Affirmative("進める").Negative("中断").Value(&yes).Run()
	return yes, err
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
}
type Options struct{ Project, Home, RepoRoot, Redo string }

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
	if s.BaseURL != "" && s.BaseURL != openRouterBaseURL {
		return errors.New("この init のモデル接続先は OpenRouter 固定です。保存済みの別接続先や鍵は変更しません。OpenRouter 用には新しい project を使ってください")
	}
	s.BaseURL = openRouterBaseURL
	w.UI.Info("モデルの接続先は OpenRouter です。疎通確認にも API の利用料がかかります")
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
		value, err := w.ask(s, "separate-model-keys", "役ごとに別の OpenRouter API キーを使いますか (yes/no)", strconv.FormatBool(s.ModelKeyMode == modelKeysSeparate), false)
		if err != nil {
			return err
		}
		switch value {
		case "yes", "true":
			s.ModelKeyMode = modelKeysSeparate
		case "no", "false":
			s.ModelKeyMode = modelKeysShared
		default:
			return errors.New("yes または no を入力してください")
		}
		value, err = w.ask(s, "separate-design", "設計レビューを別の 2 モデルにしますか (yes/no)", strconv.FormatBool(s.SeparateDesignReviews), false)
		if err != nil {
			return err
		}
		switch value {
		case "yes", "true":
			s.SeparateDesignReviews = true
		case "no", "false":
			s.SeparateDesignReviews = false
		default:
			return errors.New("yes または no を入力してください")
		}
		for _, role := range allRoles(s) {
			endpoint := s.Models[role]
			value, err := w.ask(s, role+"-model", role+" のモデル名", endpoint.Model, false)
			if err != nil {
				return err
			}
			endpoint.Model = value
			vendor := endpoint.Vendor
			if vendor == "" {
				prefix, _, found := strings.Cut(value, "/")
				if found {
					switch strings.ToLower(prefix) {
					case "openai":
						vendor = "OpenAI"
					case "anthropic":
						vendor = "Anthropic"
					case "google":
						vendor = "Google"
					}
				}
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
		w.UI.Info("OpenRouter のキー1本を全役で共用します。キー単位の利用上限と費用は全役の合算になります")
		if err := w.secret(s, secrets, "LASSDAS_INTAKE_TARGET_KEY", "OpenRouter API キー", replace); err != nil {
			return err
		}
		for _, role := range allRoles(s) {
			secrets[keyName(role)] = secrets["LASSDAS_INTAKE_TARGET_KEY"]
		}
		return nil
	}
	if err := w.secret(s, secrets, "LASSDAS_INTAKE_TARGET_KEY", "受付・対象導出専用の OpenRouter API キー", replace); err != nil {
		return err
	}
	for _, role := range allRoles(s) {
		if err := w.secret(s, secrets, keyName(role), role+" 専用の OpenRouter API キー", replace); err != nil {
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
