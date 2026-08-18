package main

import (
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"
)

// Answers is everything the wizard needs to build an instance. Secrets carry
// `json:"-"`: they live in this process and in their vaults, never in the
// resumable state file - which is also why a resumed session asks for them
// again.
type Answers struct {
	Tracker string `json:"tracker"`
	Cloud   string `json:"cloud"`

	InstanceRepo string `json:"instance_repo"`
	EnginePin    string `json:"engine_pin"`

	BacklogDomain      string `json:"backlog_domain"`
	ProjectKey         string `json:"project_key"`
	ResolvedProjectID  int64  `json:"resolved_project_id,omitempty"`
	ResolvedProjectKey string `json:"resolved_project_key,omitempty"`
	AllowedCreatorID   string `json:"allowed_creator_id"`
	CategoryName       string `json:"category_name"`
	BotAPIKey          string `json:"-"`

	ConsumerRepo       string `json:"consumer_repo"`
	WritablePrefixes   string `json:"writable_prefixes"`
	DeliveryBranch     string `json:"delivery_branch"`
	ReleaseBranch      string `json:"release_branch"`
	StagingOrigin      string `json:"staging_origin"`
	ProductionOrigin   string `json:"production_origin"`
	StagingWorkflow    string `json:"staging_workflow"`
	ProductionWorkflow string `json:"production_workflow"`

	GatewayBaseURL   string `json:"gateway_base_url"`
	ImplementerModel string `json:"implementer_model"`
	ReviewerModel    string `json:"reviewer_model"`
	ImplementerKey   string `json:"-"`
	ReviewerKey      string `json:"-"`

	Region     string `json:"region"`
	Profile    string `json:"profile"`
	NamePrefix string `json:"name_prefix"`

	AppID      string `json:"app_id"`
	AppKeyPath string `json:"app_key_path"`
}

var (
	repoPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9._-]+$`)
	domainPattern = regexp.MustCompile(`^[a-z0-9-]+\.backlog\.(com|jp)$`)
	digitsPattern = regexp.MustCompile(`^[0-9]+$`)
)

func requireMatch(pattern *regexp.Regexp, hint string) func(string) error {
	return func(value string) error {
		if !pattern.MatchString(strings.TrimSpace(value)) {
			return errors.New(hint)
		}
		return nil
	}
}

func requireNonEmpty(hint string) func(string) error {
	return func(value string) error {
		if strings.TrimSpace(value) == "" {
			return errors.New(hint)
		}
		return nil
	}
}

// askQuestions runs the full interview. Non-secret answers from a resumed
// session pre-fill the fields, so an interrupted operator only re-enters
// secrets and confirms.
func askQuestions(state *State) error {
	a := &state.Answers
	if a.Tracker == "" {
		a.Tracker = "backlog"
	}
	if a.Cloud == "" {
		a.Cloud = "aws"
	}
	if a.EnginePin == "" {
		if out, err := exec.Command("git", "rev-parse", "HEAD").Output(); err == nil {
			a.EnginePin = strings.TrimSpace(string(out))
		}
	}
	if a.CategoryName == "" {
		a.CategoryName = "自動処理"
	}
	if a.DeliveryBranch == "" {
		a.DeliveryBranch = "stg"
	}
	if a.ReleaseBranch == "" {
		a.ReleaseBranch = "prod"
	}
	if a.StagingWorkflow == "" {
		a.StagingWorkflow = "deploy-stg.yml"
	}
	if a.ProductionWorkflow == "" {
		a.ProductionWorkflow = "deploy.yml"
	}
	if a.ImplementerModel == "" {
		a.ImplementerModel = "anthropic/claude-opus-5"
	}
	if a.ReviewerModel == "" {
		a.ReviewerModel = "openai/gpt-5.6-sol-pro"
	}
	if a.Region == "" {
		a.Region = "ap-northeast-1"
	}
	if a.NamePrefix == "" {
		a.NamePrefix = "ticket-automation"
	}

	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().Title("チケットトラッカー").
				Description("起票と質問応答の場。今日は Backlog のみ (GitHub Issues は将来枠)").
				Options(huh.NewOption("Backlog", "backlog")).Value(&a.Tracker),
			huh.NewSelect[string]().Title("クラウド").
				Description("受け口 (webhook/claim) と状態台帳の置き場。今日は AWS のみ").
				Options(huh.NewOption("AWS", "aws")).Value(&a.Cloud),
			huh.NewInput().Title("インスタンス repo (owner/name)").
				Description("これから作る private repo。設定と knowledge と実行履歴の置き場").
				Placeholder("your-org/your-product-ticket-automation").
				Validate(requireMatch(repoPattern, "owner/name の形式で入力してください")).Value(&a.InstanceRepo),
			huh.NewInput().Title("エンジンの版 (commit SHA)").
				Description("インスタンスが固定して呼ぶエンジンの版。既定 = この clone の HEAD").
				Validate(requireNonEmpty("SHA を入力してください")).Value(&a.EnginePin),
		).Title("基本"),

		huh.NewGroup(
			huh.NewInput().Title("Backlog ドメイン").
				Placeholder("your-space.backlog.com").
				Validate(requireMatch(domainPattern, "your-space.backlog.com / .jp の形式で")).Value(&a.BacklogDomain),
			huh.NewInput().Title("プロジェクトキー").
				Description("チケットを受け付けるプロジェクト (例: PROJ)").
				Validate(requireNonEmpty("プロジェクトキーを入力してください")).Value(&a.ProjectKey),
			huh.NewInput().Title("起票を許可するユーザーの数値 ID").
				Description("この人が作ったチケットだけを自動処理する。Backlog の /users ページの数値 ID").
				Validate(requireMatch(digitsPattern, "数値で入力してください")).Value(&a.AllowedCreatorID),
			huh.NewInput().Title("自動処理カテゴリー名").
				Description("このカテゴリーが付いたチケットだけが対象になる (無ければ作成)").Value(&a.CategoryName),
			huh.NewInput().Title("bot ユーザーの API キー").
				Description("受付コメントや状態変更を投稿する専用ユーザーのキー (画面には表示しません)").
				EchoMode(huh.EchoModePassword).
				Validate(requireNonEmpty("API キーを入力してください")).Value(&a.BotAPIKey),
		).Title("トラッカー (Backlog)"),

		huh.NewGroup(
			huh.NewInput().Title("納品先 repo (owner/name)").
				Description("自動処理が Pull Request を出す先").
				Validate(requireMatch(repoPattern, "owner/name の形式で入力してください")).Value(&a.ConsumerRepo),
			huh.NewInput().Title("書き込みを許す path 接頭辞 (カンマ区切り)").
				Placeholder("client/src/,api/").
				Validate(requireNonEmpty("最低 1 つ入力してください")).Value(&a.WritablePrefixes),
			huh.NewInput().Title("納品ブランチ (PR の base)").Value(&a.DeliveryBranch),
			huh.NewInput().Title("本番ブランチ").Value(&a.ReleaseBranch),
			huh.NewInput().Title("stg 環境の URL").
				Placeholder("https://stg.example.com").
				Validate(requireNonEmpty("URL を入力してください")).Value(&a.StagingOrigin),
			huh.NewInput().Title("本番環境の URL").
				Placeholder("https://example.com").
				Validate(requireNonEmpty("URL を入力してください")).Value(&a.ProductionOrigin),
		).Title("納品先"),

		huh.NewGroup(
			huh.NewInput().Title("モデル API の base URL").
				Description("OpenAI 互換のゲートウェイ。実装役と検査役の両方がここを呼ぶ").
				Placeholder("https://gateway.example.com/api/v1").
				Validate(requireNonEmpty("URL を入力してください")).Value(&a.GatewayBaseURL),
			huh.NewInput().Title("実装役のモデル").Value(&a.ImplementerModel),
			huh.NewInput().Title("検査役のモデル").
				Description("実装役と別ベンダーにするのが敵対検査の前提").Value(&a.ReviewerModel),
			huh.NewInput().Title("実装役の API キー").EchoMode(huh.EchoModePassword).
				Validate(requireNonEmpty("キーを入力してください")).Value(&a.ImplementerKey),
			huh.NewInput().Title("検査役の API キー").EchoMode(huh.EchoModePassword).
				Validate(requireNonEmpty("キーを入力してください")).Value(&a.ReviewerKey),
		).Title("モデル供給"),

		huh.NewGroup(
			huh.NewInput().Title("AWS リージョン").Value(&a.Region),
			huh.NewInput().Title("AWS プロファイル (空 = 既定の資格情報)").Value(&a.Profile),
			huh.NewInput().Title("リソース名の接頭辞").
				Description("Lambda・台帳・秘密保管の名前に使う (例: ticket-automation)").Value(&a.NamePrefix),
		).Title("クラウド (AWS)"),

		huh.NewGroup(
			huh.NewInput().Title("納品用 GitHub App の App ID (後で設定するなら空)").
				Description("納品 PR を作る App。未作成なら空のまま進み、最後に作成ガイドを表示します").Value(&a.AppID),
			huh.NewInput().Title("App の秘密鍵 (.pem) のパス (App ID を入れた場合)").Value(&a.AppKeyPath),
		).Title("納品用 GitHub App"),
	)
	if err := form.Run(); err != nil {
		return err
	}

	normalize(a)
	if err := crossValidate(a); err != nil {
		return err
	}
	printSummary(a)

	confirmed := true
	if err := huh.NewConfirm().Title("この内容で構築を開始しますか?").Value(&confirmed).Run(); err != nil {
		return err
	}
	if !confirmed {
		return errors.New("操作者が中断しました (回答は保存済み・再実行で続きから)")
	}
	return nil
}

// crossValidate mirrors the engine rules that span multiple answers, so a
// combination the engine would reject dies here as a readable message and
// not twenty minutes later in a workflow log.
func crossValidate(a *Answers) error {
	if a.DeliveryBranch == a.ReleaseBranch {
		return errors.New("納品ブランチと本番ブランチは別にしてください (エンジンの契約検証で弾かれます)")
	}
	if a.ImplementerModel == a.ReviewerModel {
		return errors.New("実装役と検査役は別モデルにしてください (敵対検査の前提)")
	}
	for _, prefix := range a.prefixList() {
		if !strings.HasSuffix(prefix, "/") {
			return errors.New("path 接頭辞は / で終わらせてください: " + prefix)
		}
	}
	return nil
}

func normalize(a *Answers) {
	trim := func(values ...*string) {
		for _, v := range values {
			*v = strings.TrimSpace(*v)
		}
	}
	trim(&a.InstanceRepo, &a.EnginePin, &a.BacklogDomain, &a.ProjectKey, &a.AllowedCreatorID,
		&a.CategoryName, &a.ConsumerRepo, &a.WritablePrefixes, &a.DeliveryBranch, &a.ReleaseBranch,
		&a.StagingOrigin, &a.ProductionOrigin, &a.GatewayBaseURL, &a.ImplementerModel,
		&a.ReviewerModel, &a.Region, &a.Profile, &a.NamePrefix, &a.AppID, &a.AppKeyPath)
	a.GatewayBaseURL = strings.TrimRight(a.GatewayBaseURL, "/")
	a.StagingOrigin = strings.TrimRight(a.StagingOrigin, "/")
	a.ProductionOrigin = strings.TrimRight(a.ProductionOrigin, "/")
}

func (a *Answers) prefixList() []string {
	var prefixes []string
	for _, p := range strings.Split(a.WritablePrefixes, ",") {
		if p = strings.TrimSpace(p); p != "" {
			prefixes = append(prefixes, p)
		}
	}
	return prefixes
}

func (a *Answers) creatorID() int64 {
	id, _ := strconv.ParseInt(a.AllowedCreatorID, 10, 64)
	return id
}

func printSummary(a *Answers) {
	rows := []struct{ label, value string }{
		{"インスタンス repo", a.InstanceRepo},
		{"エンジン版", short(a.EnginePin)},
		{"トラッカー", a.Tracker + " (" + a.BacklogDomain + " / " + a.ProjectKey + ")"},
		{"起票許可 ID", a.AllowedCreatorID},
		{"納品先", a.ConsumerRepo + " (" + a.WritablePrefixes + ")"},
		{"モデル", a.ImplementerModel + " / " + a.ReviewerModel},
		{"クラウド", a.Cloud + " (" + a.Region + ")"},
	}
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(fmt.Sprintf("%-16s %s\n", r.label, styleValue.Render(r.value)))
	}
	fmt.Println(styleSummary.Render(strings.TrimRight(b.String(), "\n")))
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
