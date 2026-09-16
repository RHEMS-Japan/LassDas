package initwizard

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeAnswers(t *testing.T, root, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".lassdas"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".lassdas", "setup.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The file answers the wizard the way a person would have typed: a string
// as its text, an array or a boolean as its JSON. A question it does not
// answer takes the wizard's own proposal and is recorded as defaulted; one
// with no proposal ends the run naming the question; a key is never read
// from the file, and the run says which command the person runs.
func TestAnswersUIAnswersFromTheFileAndStopsAtWhatOnlyAPersonGives(t *testing.T) {
	root := t.TempDir()
	writeAnswers(t, root, `{"answers":{"repository":"example/app","verify":[["go","test","./..."]],"separate-model-keys":false,"board-port":9300}}`)
	answers, err := LoadAnswers(root)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	ui := &AnswersUI{Answers: answers, Project: "sample", Out: func(line string) { out = append(out, line) }}
	if v, err := ui.Ask("repository", "納品先", "other/app", false); err != nil || v != "example/app" {
		t.Fatalf("string answer: %q %v", v, err)
	}
	if v, err := ui.Ask("verify", "検証", `[["make","test"]]`, false); err != nil || v != `[["go","test","./..."]]` {
		t.Fatalf("json answer: %q %v", v, err)
	}
	if v, err := ui.Ask("separate-model-keys", "鍵", "true", false); err != nil || v != "false" {
		t.Fatalf("bool answer: %q %v", v, err)
	}
	if v, err := ui.Ask("board-port", "port", "9200", false); err != nil || v != "9300" {
		t.Fatalf("number answer: %q %v", v, err)
	}
	if v, err := ui.Ask("category", "カテゴリ", "自動処理", false); err != nil || v != "自動処理" || len(ui.Defaulted) != 1 || ui.Defaulted[0] != "category" {
		t.Fatalf("a proposal must be taken and recorded: %q %v %v", v, err, ui.Defaulted)
	}
	_, err = ui.Ask("image", "イメージ", "", false)
	var missing *MissingAnswer
	if !errors.As(err, &missing) || missing.ID != "image" || !strings.Contains(err.Error(), "docs/SETUP.md") {
		t.Fatalf("a question with no proposal must end the run naming it: %v", err)
	}
	_, err = ui.Ask("TARGET_GITHUB_TOKEN", "GitHub トークン", "", true)
	var secret *MissingSecret
	if !errors.As(err, &secret) || !strings.Contains(err.Error(), "lassdas setup secrets --project sample") {
		t.Fatalf("a secret must never come from the file: %v", err)
	}
	if ok, err := ui.Confirm("PR を出す設定で進めます"); !ok || err != nil || len(ui.Confirmed) != 1 {
		t.Fatalf("confirmations are recorded, not asked: %v %v %v", ok, err, ui.Confirmed)
	}
	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, "本体の提案") || !strings.Contains(joined, "確認済みとして進めます") {
		t.Fatalf("the transcript must say what was assumed and confirmed: %q", joined)
	}
}

// check names every required answer the file lacks, and every answer the
// wizard would never ask for (a typo), and refuses unknown top-level
// fields. It never runs anything.
func TestAnswersCheckNamesWhatIsMissingAndWhatIsUnknown(t *testing.T) {
	root := t.TempDir()
	writeAnswers(t, root, `{"answers":{"repository":"example/app","branch":"main","reposiory":"typo"}}`)
	answers, err := LoadAnswers(root)
	if err != nil {
		t.Fatal(err)
	}
	problems := answers.Check()
	joined := strings.Join(problems, "\n")
	for _, want := range []string{"image", "engine-sha", "build-record", "tracker-origin", "tracker-project", "implementer-model", "applier-model", "綴りを確認): reposiory"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("check should name %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "回答がありません: repository") || strings.Contains(joined, "回答がありません: branch") {
		t.Fatalf("answered questions must not be listed: %s", joined)
	}
	writeAnswers(t, root, `{"answers":{},"secrets":{"TARGET_GITHUB_TOKEN":"x"}}`)
	if _, err := LoadAnswers(root); err == nil || !strings.Contains(err.Error(), "読めません") {
		t.Fatalf("unknown top-level fields must be refused: %v", err)
	}
	if _, err := LoadAnswers(t.TempDir()); err == nil || !strings.Contains(err.Error(), "docs/SETUP.md") {
		t.Fatalf("a missing file must point at the instruction: %v", err)
	}
}

// A complete file has nothing to report; every required id is one the
// wizard asks (the lists cannot drift apart silently).
func TestACompleteAnswersFilePassesCheck(t *testing.T) {
	root := t.TempDir()
	var fields []string
	for _, requirement := range RequiredAnswers() {
		fields = append(fields, `"`+requirement.ID+`":"x"`)
	}
	writeAnswers(t, root, `{"answers":{`+strings.Join(fields, ",")+`}}`)
	answers, err := LoadAnswers(root)
	if err != nil {
		t.Fatal(err)
	}
	if problems := answers.Check(); len(problems) != 0 {
		t.Fatalf("a complete file must pass: %v", problems)
	}
	for _, role := range modelRoles {
		found := false
		for _, requirement := range RequiredAnswers() {
			found = found || requirement.ID == role+"-model"
		}
		if !found {
			t.Fatalf("every model role must be required: %s", role)
		}
	}
}
