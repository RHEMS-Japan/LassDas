// Command setup is the interactive installer: one guided session takes a new
// operator from a cloned engine to a working instance - repository, secrets,
// queue, hook, tracker wiring and a first green preflight - without editing
// a single file by hand. Every stage is idempotent and the session can be
// resumed after any interruption; secrets are prompted hidden and travel
// straight to their vaults, never through a file on disk.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// stateFileName holds everything needed to resume a half-finished setup in
// the directory the operator ran it from: the non-secret answers and the
// stages already completed. Secrets are deliberately absent - a resumed
// session asks for them again.
const stateFileName = "lassdas-setup.state.json"

var (
	styleTitle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	styleStage   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("99"))
	styleOK      = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	styleSkip    = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	styleWarn    = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	styleFail    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196"))
	styleValue   = lipgloss.NewStyle().Foreground(lipgloss.Color("81"))
	styleFaint   = lipgloss.NewStyle().Faint(true)
	styleBanner  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("212")).Padding(0, 2)
	styleSummary = lipgloss.NewStyle().Border(lipgloss.NormalBorder(), false, false, false, true).BorderForeground(lipgloss.Color("99")).PaddingLeft(2)
)

// State is the resumable record of one setup session.
type State struct {
	Answers   Answers  `json:"answers"`
	Outputs   Outputs  `json:"outputs"`
	Completed []string `json:"completed_stages"`
}

func (s *State) done(stage string) bool {
	for _, name := range s.Completed {
		if name == stage {
			return true
		}
	}
	return false
}

func (s *State) markDone(stage string) {
	if !s.done(stage) {
		s.Completed = append(s.Completed, stage)
	}
	saveState(s)
}

func loadState() *State {
	raw, err := os.ReadFile(stateFileName)
	if err != nil {
		return &State{}
	}
	var state State
	if json.Unmarshal(raw, &state) != nil {
		return &State{}
	}
	return &state
}

func saveState(state *State) {
	// The state file carries repository names and IDs, never secrets; 0600
	// anyway, because operators copy these files around.
	encoded, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(stateFileName, encoded, 0o600)
}

// Stage is one idempotent step of the installation. Run must be safe to call
// again after a partial failure: every resource it creates is checked for
// existence first.
type Stage struct {
	Name  string
	Title string
	Run   func(*State) error
}

func main() {
	if err := run(); err != nil {
		fmt.Println(styleFail.Render("✗ ") + err.Error())
		fmt.Println(styleFaint.Render("状態は " + stateFileName + " に保存済み。原因を直して同じコマンドをもう一度叩けば、終わった段は飛ばして続きから再開します。"))
		os.Exit(1)
	}
}

// nonInteractive is the headless mode for rehearsals and CI: every answer
// comes from the state file, secrets come from LASSDAS_SETUP_* environment
// variables, and nothing prompts - including the final ingestion toggle,
// which stays off.
var nonInteractive = slices.Contains(os.Args[1:], "--non-interactive")

func run() error {
	// A misspelled flag must not fall through to the interview: in CI there
	// is no terminal and the form would hang or crash far from the typo.
	for _, argument := range os.Args[1:] {
		if argument != "--non-interactive" {
			return errors.New("不明な引数です: " + argument + " (使える引数は --non-interactive のみ)")
		}
	}
	fmt.Println(styleBanner.Render(styleTitle.Render("LassDas setup") + "\n" + styleFaint.Render("チケット自動処理の新しいインスタンスを、対話だけで組み上げます")))
	fmt.Println()

	if err := checkPrerequisites(); err != nil {
		return err
	}

	state := loadState()
	if len(state.Completed) > 0 {
		fmt.Println(styleWarn.Render("↻ 前回の途中から再開します") + styleFaint.Render(" (完了済み: "+strings.Join(state.Completed, ", ")+")"))
		fmt.Println()
	}

	if nonInteractive {
		if err := loadHeadlessAnswers(state); err != nil {
			return err
		}
	} else if err := askQuestions(state); err != nil {
		return err
	}
	saveState(state)

	if err := checkCloudCredentials(&state.Answers); err != nil {
		return err
	}

	stages := []Stage{
		{Name: "github", Title: "GitHub 構築 (インスタンス repo・secrets・vars)", Run: provisionGitHub},
		{Name: "cloud", Title: "クラウド構築 (" + state.Answers.Cloud + ": 台帳・秘密保管・受け口)", Run: provisionCloud},
		{Name: "tracker", Title: "トラッカー構築 (" + state.Answers.Tracker + ": カテゴリー・ボード列・webhook)", Run: provisionTracker},
		{Name: "verify", Title: "動作確認 (model-preflight)", Run: verifyPreflight},
	}
	for index, stage := range stages {
		header := fmt.Sprintf("[%d/%d] %s", index+1, len(stages), stage.Title)
		if state.done(stage.Name) {
			fmt.Println(styleSkip.Render("◌ " + header + " — 完了済み・スキップ"))
			continue
		}
		fmt.Println(styleStage.Render("◆ " + header))
		if err := stage.Run(state); err != nil {
			return fmt.Errorf("%s: %w", stage.Title, err)
		}
		state.markDone(stage.Name)
		fmt.Println(styleOK.Render("✓ " + stage.Title + " 完了"))
		fmt.Println()
	}

	fmt.Println(styleBanner.Render(styleOK.Render("すべて完了しました。") + "\n" +
		"次の一歩: トラッカーでカテゴリー「" + state.Answers.CategoryName + "」付きのチケットを 1 枚切ってください。\n" +
		styleFaint.Render("受付は "+state.Answers.InstanceRepo+" の vars TICKET_INGRESS_ENABLED で常時 ON/OFF できます。")))
	fmt.Println(styleWarn.Render("手で合わせる残り 2 箇所") + styleFaint.Render(" (config/m1-consumer.json — 納品先の実態に依存するため対話では決めきれません):"))
	fmt.Println(styleFaint.Render("  1. mode.toolchain / install_command / verify_commands — 納品先の言語とビルド手順に合わせる (見本は node+pnpm)"))
	fmt.Println(styleFaint.Render("  2. github_contract.staging_digest_commit — stg デプロイが digest 更新コミットを積む repo だけ、その規約に合わせる"))
	return nil
}

// checkPrerequisites verifies every external tool and its authentication up
// front, so a missing permission surfaces as one readable list instead of a
// failure halfway through provisioning.
func checkPrerequisites() error {
	fmt.Println(styleStage.Render("◆ 前提チェック"))
	var problems []string

	if _, err := exec.LookPath("gh"); err != nil {
		problems = append(problems, "gh (GitHub CLI) が見つかりません — https://cli.github.com/")
	} else if err := exec.Command("gh", "auth", "status").Run(); err != nil {
		problems = append(problems, "gh が未ログインです — `gh auth login` を先に")
	}
	if _, err := exec.LookPath("go"); err != nil {
		problems = append(problems, "go が見つかりません (受け口のビルドに必要)")
	}
	if _, err := exec.LookPath("git"); err != nil {
		problems = append(problems, "git が見つかりません")
	}

	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Println(styleFail.Render("  ✗ ") + p)
		}
		return errors.New("前提が揃っていません (上の一覧を解消してから再実行してください)")
	}
	fmt.Println(styleOK.Render("  ✓ gh / go / git と認証を確認"))
	fmt.Println()
	return nil
}
