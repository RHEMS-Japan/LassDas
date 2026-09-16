package initwizard

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// AnswersFile is where an agent that sets a project up leaves the answers
// the wizard would otherwise ask a person for: .lassdas/setup.json in the
// delivery repository, keyed by the wizard's question ids. It carries no
// secret: a key is stored by the person with `lassdas setup secrets`, and
// the file is committed and read by whoever continues the setup.
const AnswersFile = ".lassdas/setup.json"

// Answers is the file's shape: {"answers": {"<id>": value, ...}}. A value
// is a string, or the JSON the wizard would have parsed from a typed answer
// (an array for a command, true/false for a yes/no question).
type Answers struct {
	Answers map[string]json.RawMessage `json:"answers"`
}

// LoadAnswers reads the answers file under a repository root. A missing
// file is reported as such; unknown top-level fields are refused so a typo
// does not silently drop a whole section.
func LoadAnswers(repoRoot string) (Answers, error) {
	path := filepath.Join(repoRoot, filepath.FromSlash(AnswersFile))
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Answers{}, fmt.Errorf("%s がありません。導入の指示 (~/%s) に従って回答を書いてください", AnswersFile, InstalledInstruction)
		}
		return Answers{}, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var answers Answers
	if err := decoder.Decode(&answers); err != nil {
		return Answers{}, fmt.Errorf("%s を読めません: %v", AnswersFile, err)
	}
	if answers.Answers == nil {
		answers.Answers = map[string]json.RawMessage{}
	}
	return answers, nil
}

// Value renders one answer the way a person would have typed it: a JSON
// string as its text, anything else as its JSON, which is what the wizard
// parses for its typed questions.
func (a Answers) Value(id string) (string, bool) {
	raw, ok := a.Answers[id]
	if !ok {
		return "", false
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "null" {
		// null is "not answered", never an answer of "null".
		return "", false
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		// Trimmed as a terminal would: a stray space around a branch or
		// a digest must not fail the lookup with a generic message. An
		// empty string is "not answered", like null.
		text = strings.TrimSpace(text)
		return text, text != ""
	}
	return trimmed, true
}

// Flag reads a yes/no answer; absent or unreadable is false.
func (a Answers) Flag(id string) bool {
	value, ok := a.Value(id)
	return ok && (value == "true" || value == "yes")
}

// MissingSecret ends a run that reached a question only a person answers:
// a key. It names the command the person runs.
type MissingSecret struct {
	Name, Label, Project string
}

func (m *MissingSecret) Error() string {
	if m.Name == "tracker-admin-key" {
		return fmt.Sprintf("課題管理の project に受付のカテゴリか状態が無く、保存された鍵では作れません (直前の行が理由)。利用者が Backlog の画面でその項目を作って再実行するか、`lassdas init --project %s --redo tracker` を利用者が対話で実行して管理者の鍵を入力し (tracker 段が終わったら Ctrl-C で抜けてよい)、そのあと `lassdas setup apply` に戻ってください。AI は鍵を扱いません", m.Project)
	}
	return fmt.Sprintf("鍵 %s (%s) が未保存か、直前の行の理由で使えませんでした。利用者が `lassdas setup secrets --project %s` を実行して入れ直してください。AI はこの値を扱いません", m.Name, m.Label, m.Project)
}

// consentGates are the confirmations a person must give before the setup
// writes outside the repository or accepts a naming it cannot undo; the
// agent records the person's yes in the answers file, and the run stops
// here without it.
var consentGates = []struct{ prefix, id, what string }{
	{confirmTrackerCreatePrefix, "tracker-create", "課題管理の project にカテゴリ・状態を作る"},
	{confirmRequesterKeyPrefix, "requester-key-ok", "自動処理のコメントと状態更新が起票者本人の名義になる"},
}

// ConsentRequired ends a run at a confirmation the file does not carry.
type ConsentRequired struct {
	ID, What, Label string
}

func (c *ConsentRequired) Error() string {
	return fmt.Sprintf("利用者の承認が要る操作です: %s。利用者に「%s」を確認し、承認されたら %s に %q: true を書いてから再実行してください", c.What, strings.ReplaceAll(c.Label, "\n", " / "), AnswersFile, c.ID)
}

// MissingAnswer ends a run at a question the file does not answer and the
// wizard has no default for.
type MissingAnswer struct {
	ID, Label string
}

func (m *MissingAnswer) Error() string {
	return fmt.Sprintf("%s に %q の回答がありません (%s)。~/%s の回答表を見て書いてください", AnswersFile, m.ID, m.Label, InstalledInstruction)
}

// AnswersUI answers the wizard from the file instead of a terminal. A
// question the file answers is answered; one it does not is answered with
// the wizard's own proposal when there is one (recorded in Defaulted so the
// transcript says what was assumed) and ends the run otherwise; a secret
// is never answered here; a confirmation is recorded, not asked - the
// instruction has the agent agree each of them with the person before
// running apply.
type AnswersUI struct {
	Answers   Answers
	Project   string
	Out       func(string)
	Asked     []string
	Defaulted []string
	Confirmed []string
}

func (u *AnswersUI) Ask(id, label, fallback string, secret bool) (string, error) {
	u.Asked = append(u.Asked, id)
	if secret {
		return "", &MissingSecret{Name: id, Label: label, Project: u.Project}
	}
	if value, ok := u.Answers.Value(id); ok {
		return value, nil
	}
	// "0" and "null" are the wizard's way of proposing nothing (an unset
	// id, an empty list); they are not answers.
	if fallback != "" && fallback != "0" && fallback != "null" {
		u.Defaulted = append(u.Defaulted, id)
		u.say(fmt.Sprintf("回答なし: %s (%s) → 本体の提案 %q を採用", id, label, fallback))
		return fallback, nil
	}
	return "", &MissingAnswer{ID: id, Label: label}
}

func (u *AnswersUI) Confirm(label string) (bool, error) {
	for _, gate := range consentGates {
		if strings.HasPrefix(label, gate.prefix) && !u.Answers.Flag(gate.id) {
			return false, &ConsentRequired{ID: gate.id, What: gate.what, Label: label}
		}
	}
	u.Confirmed = append(u.Confirmed, label)
	u.say("確認済みとして進めます: " + strings.ReplaceAll(label, "\n", " / "))
	return true, nil
}

func (u *AnswersUI) Info(value string) { u.say(value) }

func (u *AnswersUI) say(value string) {
	if u.Out != nil {
		u.Out(value)
	}
}

// RequiredAnswers lists the questions the wizard cannot answer by itself:
// what only the project, its distributor or the person knows. `lassdas
// setup check` reports the missing ones before anything runs.
func RequiredAnswers() []Requirement {
	requirements := []Requirement{
		{"repository", "納品先 repo (owner/name)", "git remote から読める。fork や別 remote なら確認する"},
		{"branch", "取り込み枝 (PR の宛先。default branch とは限らない)", "repo の規則 (AGENTS.md / CLAUDE.md / CONTRIBUTING.md) と最近の PR の宛先から確かめる"},
		{"engine-repository", "本体イメージの元ソース repo (owner/name)", "配布者の案内 (~/" + DistributionFile + "、`lassdas setup install` が置く)。無ければ install が未実行"},
		{"image", "本体イメージ (registry/name@sha256:digest)", "同上。タグ名から推定しない"},
		{"engine-sha", "そのイメージに対応する本体ソースの 40 桁 SHA", "同上"},
		{"build-record", "イメージと SHA の対応を確認できるビルド記録の URL", "同上"},
		{"tracker-origin", "課題管理 (Backlog) の接続先 URL", "利用者に確認"},
		{"tracker-project", "Backlog の project キー", "利用者に確認"},
		{"creator-id", "起票を許可する本人の Backlog 利用者 ID (数値)", "`lassdas setup secrets` が鍵の持ち主の ID を表示する。別の人が起票するならその人の ID を利用者に確認"},
	}
	for _, role := range modelRoles {
		requirements = append(requirements, Requirement{role + "-model", role + " のモデル名 (OpenRouter の名前)", "品質と費用の希望を聞いて推奨を出し、利用者が確定"})
	}
	return requirements
}

// Requirement is one answer the file must carry.
type Requirement struct {
	ID, Label, How string
}

// OptionalAnswers lists the questions the wizard proposes a value for; the
// file may override them.
func OptionalAnswers() []Requirement {
	return []Requirement{
		{"scope", "編集を許すディレクトリ接頭辞と root ファイル (JSON 配列)", "repo の構成から提案される。任せる範囲に合わせて直す"},
		{"toolchain", "道具と必要な版 (JSON 配列)", "go.mod / package.json から提案される"},
		{"install", "依存導入コマンド (JSON 配列)", "同上"},
		{"verify", "検証コマンド 1〜4 本 (JSON 二重配列)", "既存のテストの動かし方から"},
		{"verify-directory", "検証する repo 内ディレクトリ", "通常は repo の root"},
		{"max-files", "変更ファイル上限", "既定でよい"},
		{"max-file-bytes", "1 ファイルの byte 上限", "既定でよい"},
		{"max-total-bytes", "合計 byte 上限", "既定でよい"},
		{"max-changed-lines", "変更行数の上限", "既定でよい"},
		{"max-changed-bytes", "変更 byte の上限", "既定でよい"},
		{"category", "受付のカテゴリ名", "既定は 自動処理"},
		{"status-0", "自動処理中 に対応する状態名", "既定あり"},
		{"status-1", "回答待ち に対応する状態名", "既定あり"},
		{"status-2", "納品済み に対応する状態名", "既定あり"},
		{"status-3", "要確認 に対応する状態名", "既定あり"},
		{"separate-model-keys", "役ごとに別の API キーを使うか (true/false)", "既定は false (1 本を共用)"},
		{"separate-design", "設計レビューを別の 2 モデルにするか (true/false)", "既定は false"},
		{"design-review-a-model", "設計レビュー A のモデル名 (separate-design が true のとき必須)", "利用者が確定"},
		{"design-review-b-model", "設計レビュー B のモデル名 (separate-design が true のとき必須)", "利用者が確定"},
		{"tracker-create", "受付のカテゴリ・状態が無いとき、課題管理の project に作ってよいか (true/false)", "利用者に確認してから書く"},
		{"requester-key-ok", "自動処理の鍵が起票者本人のもので、コメントと状態更新が本人名義になってよいか (true/false)", "利用者に確認してから書く"},
		{"board-port", "板を 127.0.0.1 で開く port", "既定は 9200"},
	}
}

// VendorFor is the model provider the wizard proposes from a model's
// prefix (OpenRouter names models vendor/model); "" when it cannot tell,
// and the answer <role>-vendor is then required.
func VendorFor(model string) string {
	prefix, _, found := strings.Cut(model, "/")
	if !found {
		return ""
	}
	switch strings.ToLower(prefix) {
	case "openai":
		return "OpenAI"
	case "anthropic":
		return "Anthropic"
	case "google":
		return "Google"
	case "deepseek":
		return "DeepSeek"
	case "x-ai":
		return "xAI"
	case "meta-llama":
		return "Meta"
	case "mistralai":
		return "Mistral"
	case "qwen":
		return "Qwen"
	case "moonshotai":
		return "Moonshot"
	case "z-ai":
		return "Z.ai"
	case "cohere":
		return "Cohere"
	case "amazon":
		return "Amazon"
	}
	return ""
}

// checkModels mirrors, offline, what the body's configuration refuses
// later (internal/worker Config.Validate), so a combination that cannot
// run is named before any stage runs: every role needs a vendor, the two
// reviewers must be different models from two vendors, at most one of them
// may share the implementer's model (and the designer's), the readiness
// assessor and checker must be different vendors, and design reviewers,
// when separate, follow the reviewers' rules.
func (a Answers) checkModels() []string {
	var problems []string
	roles := append([]string(nil), modelRoles...)
	if a.Flag("separate-design") {
		roles = append(roles, "design-review-a", "design-review-b")
	}
	model := map[string]string{}
	vendor := map[string]string{}
	for _, role := range roles {
		name, ok := a.Value(role + "-model")
		if !ok || name == "" {
			if role == "design-review-a" || role == "design-review-b" {
				problems = append(problems, fmt.Sprintf("回答がありません: %s-model (separate-design が true なので必須)", role))
			}
			continue
		}
		model[role] = strings.ToLower(name)
		if v, ok := a.Value(role + "-vendor"); ok && v != "" {
			vendor[role] = strings.ToLower(v)
		} else if v := VendorFor(name); v != "" {
			vendor[role] = strings.ToLower(v)
		} else {
			problems = append(problems, fmt.Sprintf("回答がありません: %s-vendor (モデル名 %q からは提供会社を判定できません)", role, name))
		}
	}
	pair := func(a, b, what string) {
		if model[a] == "" || model[b] == "" {
			return
		}
		if model[a] == model[b] {
			problems = append(problems, fmt.Sprintf("%s と %s は別のモデルにしてください (%s)", a, b, what))
		}
		if vendor[a] != "" && vendor[a] == vendor[b] {
			problems = append(problems, fmt.Sprintf("%s と %s は別の提供会社にしてください (%s)", a, b, what))
		}
	}
	pair("review-a", "review-b", "レビューは 2 社で行う")
	pair("readiness-assessor", "readiness-checker", "受付の起案と確認は別の会社で行う")
	shared := func(owner string, reviewers ...string) {
		if model[owner] == "" {
			return
		}
		count := 0
		for _, reviewer := range reviewers {
			if model[reviewer] != "" && model[reviewer] == model[owner] {
				count++
			}
		}
		if count > 1 {
			problems = append(problems, fmt.Sprintf("%s と同じモデルにできるレビュー役は 1 つまでです", owner))
		}
	}
	shared("implementer", "review-a", "review-b")
	shared("designer", "review-a", "review-b")
	if a.Flag("separate-design") {
		pair("design-review-a", "design-review-b", "設計レビューは 2 社で行う")
	}
	return problems
}

// Check reports what the file lacks, in plain words, without running
// anything: the missing required answers and the answers whose id the
// wizard never asks (a typo, or a question from another version).
func (a Answers) Check(repoRoot string) []string {
	var problems []string
	for _, requirement := range RequiredAnswers() {
		if _, ok := a.Value(requirement.ID); !ok {
			problems = append(problems, fmt.Sprintf("回答がありません: %s (%s)。%s", requirement.ID, requirement.Label, requirement.How))
		}
	}
	// The tracker origin's shape is checked here, before a key is stored
	// against it: https, a host, nothing after it.
	if origin, ok := a.Value("tracker-origin"); ok {
		if parsed, err := url.Parse(origin); err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.Port() != "" ||
			(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
			problems = append(problems, fmt.Sprintf("tracker-origin の形が違います: %q (https://<space>.backlog.com のように、https でホスト名だけ。末尾の / や path は付けない)", origin))
		} else if strings.HasSuffix(origin, "/") {
			problems = append(problems, fmt.Sprintf("tracker-origin の末尾の / を外してください: %q", origin))
		}
	}
	// Without go.mod or package.json the wizard proposes nothing for the
	// scope, the toolchain, the install and the verify commands: the file
	// must carry all four, and the check says so before any stage runs.
	if repoRoot != "" && !exists(filepath.Join(repoRoot, "go.mod")) && !exists(filepath.Join(repoRoot, "package.json")) {
		for _, id := range []string{"scope", "toolchain", "install", "verify"} {
			if _, ok := a.Value(id); !ok {
				problems = append(problems, fmt.Sprintf("回答がありません: %s (go.mod も package.json も無い repo では本体が提案できません。toolchain と install は不要なら [] と書く)", id))
			}
		}
	}
	problems = append(problems, a.checkModels()...)
	known := map[string]bool{}
	for _, requirement := range append(RequiredAnswers(), OptionalAnswers()...) {
		known[requirement.ID] = true
	}
	for _, role := range allRolesWithDesign() {
		known[role+"-vendor"] = true
	}
	var unknown []string
	for id := range a.Answers {
		if !known[id] {
			unknown = append(unknown, id)
		}
	}
	sort.Strings(unknown)
	for _, id := range unknown {
		problems = append(problems, fmt.Sprintf("本体が聞かない質問への回答です (綴りを確認): %s", id))
	}
	return problems
}

func allRolesWithDesign() []string {
	return append(append([]string(nil), modelRoles...), "design-review-a", "design-review-b")
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
