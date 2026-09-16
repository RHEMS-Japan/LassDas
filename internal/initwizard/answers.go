package initwizard

import (
	"encoding/json"
	"errors"
	"fmt"
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
			return Answers{}, fmt.Errorf("%s がありません。導入の指示 (docs/SETUP.md) に従って回答を書いてください", AnswersFile)
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
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, true
	}
	return strings.TrimSpace(string(raw)), true
}

// MissingSecret ends a run that reached a question only a person answers:
// a key. It names the command the person runs.
type MissingSecret struct {
	Name, Label, Project string
}

func (m *MissingSecret) Error() string {
	return fmt.Sprintf("鍵 %s (%s) が保存されていません。利用者が `lassdas setup secrets --project %s` を実行して入力してください。AI はこの値を扱いません", m.Name, m.Label, m.Project)
}

// MissingAnswer ends a run at a question the file does not answer and the
// wizard has no default for.
type MissingAnswer struct {
	ID, Label string
}

func (m *MissingAnswer) Error() string {
	return fmt.Sprintf("%s に %q の回答がありません (%s)。docs/SETUP.md の回答表を見て書いてください", AnswersFile, m.ID, m.Label)
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
	if fallback != "" {
		u.Defaulted = append(u.Defaulted, id)
		u.say(fmt.Sprintf("回答なし: %s (%s) → 本体の提案 %q を採用", id, label, fallback))
		return fallback, nil
	}
	return "", &MissingAnswer{ID: id, Label: label}
}

func (u *AnswersUI) Confirm(label string) (bool, error) {
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
		{"engine-repository", "本体イメージの元ソース repo (owner/name)", "配布者の案内"},
		{"image", "本体イメージ (registry/name@sha256:digest)", "配布者の案内。タグ名から推定しない"},
		{"engine-sha", "そのイメージに対応する本体ソースの 40 桁 SHA", "配布者の案内"},
		{"build-record", "イメージと SHA の対応を確認できるビルド記録の URL", "配布者の案内"},
		{"tracker-origin", "課題管理 (Backlog) の接続先 URL", "利用者に確認"},
		{"tracker-project", "Backlog の project キー", "利用者に確認"},
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
		{"creator-id", "起票を許可する本人の数値 ID", "鍵の持ち主から提案される"},
		{"category", "受付のカテゴリ名", "既定は 自動処理"},
		{"status-0", "自動処理中 に対応する状態名", "既定あり"},
		{"status-1", "回答待ち に対応する状態名", "既定あり"},
		{"status-2", "納品済み に対応する状態名", "既定あり"},
		{"status-3", "要確認 に対応する状態名", "既定あり"},
		{"separate-model-keys", "役ごとに別の API キーを使うか (true/false)", "既定は false (1 本を共用)"},
		{"separate-design", "設計レビューを別の 2 モデルにするか (true/false)", "既定は false"},
		{"board-port", "板を 127.0.0.1 で開く port", "既定は 9200"},
	}
}

// Check reports what the file lacks, in plain words, without running
// anything: the missing required answers and the answers whose id the
// wizard never asks (a typo, or a question from another version).
func (a Answers) Check() []string {
	var problems []string
	for _, requirement := range RequiredAnswers() {
		if _, ok := a.Value(requirement.ID); !ok {
			problems = append(problems, fmt.Sprintf("回答がありません: %s (%s)。%s", requirement.ID, requirement.Label, requirement.How))
		}
	}
	known := map[string]bool{}
	for _, requirement := range append(RequiredAnswers(), OptionalAnswers()...) {
		known[requirement.ID] = true
	}
	for _, role := range []string{"design-review-a", "design-review-b"} {
		known[role+"-model"] = true
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
