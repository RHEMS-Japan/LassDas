package initwizard

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/worker"
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
	problems := answers.Check("")
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
	models := map[string]string{"implementer-model": "deepseek/a", "review-a-model": "anthropic/b", "review-b-model": "openai/c", "readiness-assessor-model": "google/d", "readiness-checker-model": "openai/e", "designer-model": "anthropic/f", "applier-model": "deepseek/g", "tracker-origin": "https://example.backlog.com"}
	for _, requirement := range RequiredAnswers() {
		value := "x"
		if m, ok := models[requirement.ID]; ok {
			value = m
		}
		fields = append(fields, `"`+requirement.ID+`":"`+value+`"`)
	}
	writeAnswers(t, root, `{"answers":{`+strings.Join(fields, ",")+`}}`)
	answers, err := LoadAnswers(root)
	if err != nil {
		t.Fatal(err)
	}
	if problems := answers.Check(""); len(problems) != 0 {
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

// The wizard's "nothing" proposals ("0" for an unset id, "null" for an
// empty list) are not answers: the run stops naming the question instead
// of adopting them. A null in the file is "not answered"; a string is
// trimmed as a terminal would.
func TestAnswersUIDoesNotAdoptEmptyProposals(t *testing.T) {
	root := t.TempDir()
	writeAnswers(t, root, `{"answers":{"branch":" main ","scope":null}}`)
	answers, err := LoadAnswers(root)
	if err != nil {
		t.Fatal(err)
	}
	ui := &AnswersUI{Answers: answers, Project: "sample"}
	if v, _ := ui.Ask("branch", "枝", "", false); v != "main" {
		t.Fatalf("strings are trimmed: %q", v)
	}
	var missing *MissingAnswer
	if _, err := ui.Ask("creator-id", "起票者", "0", false); !errors.As(err, &missing) {
		t.Fatalf("\"0\" is not a proposal: %v", err)
	}
	if _, err := ui.Ask("scope", "範囲", "null", false); !errors.As(err, &missing) {
		t.Fatalf("\"null\" is not a proposal, and null in the file is not an answer: %v", err)
	}
	if len(ui.Defaulted) != 0 {
		t.Fatalf("nothing was defaulted: %v", ui.Defaulted)
	}
}

// Writing to the tracker and accepting the requester's own key as the
// bot's are the person's decisions: without their yes in the file the
// run stops and says what to ask; with it, the confirmation is recorded.
func TestAnswersUIHoldsConsentGatesForThePerson(t *testing.T) {
	root := t.TempDir()
	writeAnswers(t, root, `{"answers":{}}`)
	answers, _ := LoadAnswers(root)
	ui := &AnswersUI{Answers: answers, Project: "sample"}
	var consent *ConsentRequired
	if ok, err := ui.Confirm("不足する項目だけ作成します:\nカテゴリ: 自動処理"); ok || !errors.As(err, &consent) || consent.ID != "tracker-create" || !strings.Contains(err.Error(), `"tracker-create": true`) {
		t.Fatalf("tracker writes need consent: %v %v", ok, err)
	}
	if ok, err := ui.Confirm("この API キーは起票者本人 (ID: 7) のものです。…"); ok || !errors.As(err, &consent) || consent.ID != "requester-key-ok" {
		t.Fatalf("the naming needs consent: %v %v", ok, err)
	}
	if ok, err := ui.Confirm("納品先 x の main へ PR を出す設定で進めます"); !ok || err != nil {
		t.Fatalf("other confirmations are recorded: %v %v", ok, err)
	}
	writeAnswers(t, root, `{"answers":{"tracker-create":true,"requester-key-ok":"yes"}}`)
	answers, _ = LoadAnswers(root)
	ui = &AnswersUI{Answers: answers, Project: "sample"}
	if ok, err := ui.Confirm("不足する項目だけ作成します:\n状態: 回答待ち"); !ok || err != nil {
		t.Fatalf("with consent the write proceeds: %v %v", ok, err)
	}
	if ok, err := ui.Confirm("この API キーは起票者本人 (ID: 7) のものです"); !ok || err != nil {
		t.Fatalf("with consent the naming proceeds: %v %v", ok, err)
	}
	// The admin key can never come from the file; the message names the
	// person's two ways out, not a command that would not ask for it.
	_, err := ui.Ask("tracker-admin-key", "管理者の鍵", "", true)
	if err == nil || !strings.Contains(err.Error(), "--redo tracker") || strings.Contains(err.Error(), "setup secrets") {
		t.Fatalf("admin key message: %v", err)
	}
}

// The model rules the body enforces later are checked from the file
// first, in plain words: a vendor the name does not tell, two reviewers
// of one company or one model, the same model for the assessor and the
// checker, and design reviewers required when they are separate.
func TestAnswersCheckMirrorsTheModelRules(t *testing.T) {
	root := t.TempDir()
	base := `"repository":"e/a","branch":"main","engine-repository":"e/b","image":"r/e@sha256:0","engine-sha":"a","build-record":"u","tracker-origin":"https://x.backlog.com","tracker-project":"P","creator-id":7`
	writeAnswers(t, root, `{"answers":{`+base+`,"implementer-model":"anthropic/claude-sonnet-4","review-a-model":"anthropic/claude-sonnet-4","review-b-model":"anthropic/claude-sonnet-4","readiness-assessor-model":"anthropic/claude-sonnet-4","readiness-checker-model":"anthropic/claude-sonnet-4","designer-model":"anthropic/claude-sonnet-4","applier-model":"anthropic/claude-sonnet-4"}}`)
	answers, _ := LoadAnswers(root)
	joined := strings.Join(answers.Check(""), "\n")
	for _, want := range []string{"review-a と review-b は別のモデル", "別の提供会社", "readiness-assessor と readiness-checker", "implementer と同じモデルにできるレビュー役は 1 つまで"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("check should say %q: %s", want, joined)
		}
	}
	writeAnswers(t, root, `{"answers":{`+base+`,"implementer-model":"deepseek/deepseek-chat","review-a-model":"anthropic/claude-sonnet-4","review-b-model":"openai/gpt-5","readiness-assessor-model":"google/gemini-2.5-pro","readiness-checker-model":"openai/gpt-5-mini","designer-model":"anthropic/claude-opus-4","applier-model":"somevendor/model-x","separate-design":true,"design-review-a-model":"openai/gpt-5"}}`)
	answers, _ = LoadAnswers(root)
	joined = strings.Join(answers.Check(""), "\n")
	if !strings.Contains(joined, "applier-vendor") || !strings.Contains(joined, "design-review-b-model") || strings.Contains(joined, "implementer-vendor") {
		t.Fatalf("vendor and design reviewer requirements: %s", joined)
	}
	if VendorFor("deepseek/deepseek-chat") != "DeepSeek" || VendorFor("x-ai/grok-4") != "xAI" || VendorFor("nobody/x") != "" || VendorFor("plain") != "" {
		t.Fatal("vendor derivation")
	}
	writeAnswers(t, root, `{"answers":{`+base+`,"implementer-model":"deepseek/deepseek-chat","review-a-model":"anthropic/claude-sonnet-4","review-b-model":"openai/gpt-5","readiness-assessor-model":"google/gemini-2.5-pro","readiness-checker-model":"openai/gpt-5-mini","designer-model":"anthropic/claude-opus-4","applier-model":"somevendor/model-x","applier-vendor":"SomeVendor"}}`)
	answers, _ = LoadAnswers(root)
	if problems := answers.Check(""); len(problems) != 0 {
		t.Fatalf("a sound combination must pass: %v", problems)
	}
}

// Without go.mod or package.json the four proposals the wizard cannot
// make are required; an explicit empty string is "not answered".
func TestAnswersCheckRequiresWhatTheRepoCannotPropose(t *testing.T) {
	root := t.TempDir()
	writeAnswers(t, root, `{"answers":{"scope":["docs/"],"verify":[["true"]],"creator-id":""}}`)
	answers, _ := LoadAnswers(root)
	joined := strings.Join(answers.Check(root), "\n")
	for _, want := range []string{"回答がありません: toolchain", "回答がありません: install", "回答がありません: creator-id"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("check should say %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "回答がありません: scope") || strings.Contains(joined, "回答がありません: verify") {
		t.Fatalf("answered ones are not listed: %s", joined)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if joined := strings.Join(answers.Check(root), "\n"); strings.Contains(joined, "toolchain") {
		t.Fatalf("with a manifest the wizard proposes them: %s", joined)
	}
}

// A run stopped at the requester-key consent gate keeps the stored
// tracker key; only a decline by the person removes it.
func TestAConsentStopInTheTrackerStageKeepsTheKey(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := "{}"
		switch {
		case strings.HasSuffix(r.URL.Path, "/users/myself"):
			body = `{"id":7,"name":"person"}`
		case strings.HasSuffix(r.URL.Path, "/projects/P"):
			body = `{"id":11,"projectKey":"P"}`
		case strings.HasSuffix(r.URL.Path, "/projects/11/users"):
			body = `[{"id":7,"name":"person"}]`
		case strings.HasSuffix(r.URL.Path, "/categories"), strings.HasSuffix(r.URL.Path, "/statuses"):
			body = `[]`
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	root := t.TempDir()
	writeAnswers(t, root, `{"answers":{}}`)
	answers, _ := LoadAnswers(root)
	ui := &AnswersUI{Answers: answers, Project: "sample"}
	w := &Wizard{UI: ui, API: API{HTTP: &http.Client{Transport: transport}}}
	s := &State{Models: map[string]worker.ModelEndpoint{}, Completed: map[string]string{}, Pins: map[string]string{}}
	s.Tracker.Origin, s.Tracker.SpaceKey, s.Tracker.ProjectKey, s.Tracker.AllowedCreatorID = "https://example.backlog.com", "example", "P", 7
	s.Category, s.StatusNames = "自動処理", [4]string{"a", "b", "c", "d"}
	secrets := Secrets{"BACKLOG_API_KEY": "key-value"}
	err := w.tracker(context.Background(), s, secrets, func() error { return nil })
	var consent *ConsentRequired
	if !errors.As(err, &consent) || consent.ID != "requester-key-ok" {
		t.Fatalf("want the requester-key gate: %v", err)
	}
	if secrets["BACKLOG_API_KEY"] != "key-value" {
		t.Fatal("a consent stop must keep the stored key")
	}
	// The person's decline (the terminal's "no") still removes it.
	w.UI = &fakeUI{approve: false}
	secrets = Secrets{"BACKLOG_API_KEY": "key-value"}
	if err := w.tracker(context.Background(), s, secrets, func() error { return nil }); err == nil || secrets["BACKLOG_API_KEY"] != "" {
		t.Fatalf("a decline removes the key: %v %q", err, secrets["BACKLOG_API_KEY"])
	}
	// With the yes recorded, the next gate is the tracker write.
	writeAnswers(t, root, `{"answers":{"requester-key-ok":true}}`)
	answers, _ = LoadAnswers(root)
	w.UI = &AnswersUI{Answers: answers, Project: "sample"}
	secrets = Secrets{"BACKLOG_API_KEY": "key-value"}
	if err := w.tracker(context.Background(), s, secrets, func() error { return nil }); !errors.As(err, &consent) || consent.ID != "tracker-create" {
		t.Fatalf("want the tracker-create gate: %v", err)
	}
}

// The tracker origin is checked for shape before a key is stored against
// it: https and a bare host; a trailing slash or a path is named.
func TestAnswersCheckNamesABadTrackerOrigin(t *testing.T) {
	root := t.TempDir()
	for origin, want := range map[string]string{
		`"https://example.backlog.com"`:     "",
		`"https://example.backlog.com/"`:    "末尾の /",
		`"http://example.backlog.com"`:      "形が違います",
		`"https://example.backlog.com/api"`: "形が違います",
	} {
		writeAnswers(t, root, `{"answers":{"tracker-origin":`+origin+`}}`)
		answers, _ := LoadAnswers(root)
		joined := strings.Join(answers.Check(""), "\n")
		if want == "" && strings.Contains(joined, "tracker-origin の") {
			t.Fatalf("%s should pass: %s", origin, joined)
		}
		if want != "" && !strings.Contains(joined, want) {
			t.Fatalf("%s should say %q: %s", origin, want, joined)
		}
	}
}
