package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"ticket-runner/internal/chain"
	"ticket-runner/internal/stagename"
	"ticket-runner/internal/tracker"
)

func stagesExample(t *testing.T) config {
	t.Helper()
	data, err := os.ReadFile("../../examples/operator-stages.json")
	if err != nil {
		t.Fatal(err)
	}
	var cfg config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestOrderedExampleReconsidersFailuresWithoutGrantingWiderAccess(t *testing.T) {
	cfg := stagesExample(t)
	for _, phrase := range []string{"A failed verification, review or delivery returns to elicitation", "concrete alternatives", "an in-scope alternative", "an answer does not change filesystem, delivery or credential permissions", "Do not ask again about a settled point"} {
		if !strings.Contains(cfg.Instructions, phrase) {
			t.Errorf("shared recovery instruction omits %q", phrase)
		}
	}
	for _, role := range cfg.Roles {
		if role.Name == "elicit" || role.Name == "ask_requester" {
			for _, text := range []string{role.Purpose, role.Processes[0].Instructions} {
				for _, phrase := range []string{"newly required expansion of authority", "unknown cause", "initial elicitation only"} {
					if !strings.Contains(text, phrase) {
						t.Errorf("%s recovery guidance omits %q", role.Name, phrase)
					}
				}
			}
		}
		if role.Name == "elicit" {
			if !strings.Contains(role.Processes[0].Instructions, "On a return after a failed stage, read that failure and the previous work first") {
				t.Fatal("requirements role was not told to reconsider the actual failure")
			}
		}
	}
}

func TestStageDescriptionsDoNotPromiseAMergeForPullRequestOnlyDelivery(t *testing.T) {
	want := map[string]string{
		"deliver":       "Carry the reviewed change to the operator-approved repository with the image's fixed delivery process: open a pull request, and merge it only when the configured DELIVERY_MERGE_METHOD permits it.",
		"verify_merged": "Check the delivered commit with the image's fixed process: the integration branch after a merge, or the pull request's commit when DELIVERY_MERGE_METHOD=none; not the local working tree.",
	}
	for _, role := range stagesExample(t).Roles {
		if description, checked := want[role.Name]; checked {
			if role.Purpose != description {
				t.Errorf("%s description does not cover the selected delivery depth: %q", role.Name, role.Purpose)
			}
			delete(want, role.Name)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing described roles: %v", want)
	}
}

func TestStageProcessInstructionsDescribeBothDeliveryDepths(t *testing.T) {
	want := map[string][]string{
		"deliver":       {"DELIVERY_MERGE_METHOD=none", "nothing is merged", "configured merge method"},
		"verify_merged": {"DELIVERY_MERGE_METHOD=none", "pull request's head", "integration branch", "build and tests"},
	}
	for _, role := range stagesExample(t).Roles {
		phrases, checked := want[role.Name]
		if !checked {
			continue
		}
		if len(role.Processes) != 1 {
			t.Fatalf("%s: expected the fixed command's one instruction", role.Name)
		}
		for _, phrase := range phrases {
			if !strings.Contains(role.Processes[0].Instructions, phrase) {
				t.Errorf("%s process instruction omits %q", role.Name, phrase)
			}
		}
		delete(want, role.Name)
	}
	if len(want) != 0 {
		t.Fatalf("missing process instructions: %v", want)
	}
}

// The shipped ordered run: every stage that decides whether the work carries on
// is a command the runtime observes, the run ends on one, and the requester is
// reachable from requirements, initially and after a failed command.
func TestStagesExampleIsAnOrderedRunNothingWrittenCanAdvance(t *testing.T) {
	cfg := stagesExample(t)
	if cfg.Router.Mode != "stages" {
		t.Fatalf("the example does not use the ordered run: %q", cfg.Router.Mode)
	}
	want := []chain.Stage{
		{Name: "elicit", Kind: chain.ModelStage},
		{Name: "work", Kind: chain.ModelStage},
		{Name: "verify", Kind: chain.CommandStage, OnFailure: "elicit"},
		{Name: "review", Kind: chain.CommandStage, OnFailure: "elicit"},
		{Name: "confirm_change", Kind: chain.ModelStage, Confirm: true},
		{Name: "deliver", Kind: chain.CommandStage, OnFailure: "elicit"},
		{Name: "verify_merged", Kind: chain.CommandStage, OnFailure: "elicit"},
		{Name: "report", Kind: chain.ModelStage},
		{Name: "confirm_report", Kind: chain.CommandStage, OnFailure: "report"},
	}
	if !slices.Equal(cfg.Workflow.Stages, want) {
		t.Fatalf("the shipped run is %+v", cfg.Workflow.Stages)
	}
	// The requester and the status page name each shipped stage in Japanese.
	for _, stage := range cfg.Workflow.Stages {
		if _, named := stagename.Japanese(stage.Name); !named {
			t.Fatalf("stage %s has no Japanese name", stage.Name)
		}
	}
	// The change is read before delivery, a return to requirements from there
	// is bounded, and a question that waits a day is said again.
	if cfg.Workflow.ConfirmationReworkLimit != 2 || cfg.Intake.QuestionReminderMinutes != 1440 {
		t.Fatalf("confirmation_rework_limit=%d question_reminder_minutes=%d", cfg.Workflow.ConfirmationReworkLimit, cfg.Intake.QuestionReminderMinutes)
	}
	roles, purposes := map[string]chain.Role{}, map[string]string{}
	for _, role := range cfg.Roles {
		roles[role.Name] = role
		purposes[role.Name] = role.Purpose
		for _, process := range role.Processes {
			if process.Env["NATIVE_MODEL"] != "" {
				t.Fatal("example pinned a working model")
			}
			if !slices.Contains(process.Command, "/opt/ticket-automation/bundle/harnesses/linux_role.py") && role.Name != "stop_report" {
				t.Fatal("example role bypasses actual launcher")
			}
			if process.TrackerAccess != "" && process.TrackerAccess != "read" && !slices.Contains([]string{"ask_requester", "report", "stop_report"}, role.Name) {
				t.Fatalf("posting granted to %s", role.Name)
			}
			writes := slices.Contains(process.Command, "--write") || slices.Contains(process.Command, "--create")
			if writes != slices.Contains([]string{"work", "deliver", "report"}, role.Name) {
				t.Fatalf("%s workspace write permission is %t", role.Name, writes)
			}
			// .git holds hooks and configuration the credentialed delivery
			// runs; only that fixed process, which launches no model, may write it.
			metadata := false
			for k := 1; k < len(process.Command); k++ {
				metadata = metadata || process.Command[k-1] == "--write" && process.Command[k] == ".git"
			}
			if metadata != (role.Name == "deliver") || metadata && process.ModelEnv != "" {
				t.Fatalf("%s may write .git: %t", role.Name, metadata)
			}
		}
	}
	for _, stage := range cfg.Workflow.Stages {
		models := 0
		for _, process := range roles[stage.Name].Processes {
			if process.ModelEnv != "" {
				models++
			}
		}
		if (stage.Kind == chain.ModelStage) != (models > 0) {
			t.Fatalf("stage %s is declared %s but launches %d models", stage.Name, stage.Kind, models)
		}
	}
	if roles["deliver"].Processes[0].Receipt == "" {
		t.Fatal("the delivery stage leaves the runtime nothing to read back")
	}
	// The stage that reads the change does not prepare the checkout: a model
	// stage that failed runs again, so a lost workspace prepared afresh there
	// would let the delivery go on as if the review had passed on it. The
	// delivery's own preparation finds it and sends the work back instead.
	for _, process := range roles["confirm_change"].Processes {
		if slices.Contains(process.Command, "/opt/ticket-automation/bundle/harnesses/git_workspace.py") || process.TrackerAccess != "read" {
			t.Fatalf("the stage that reads the change prepares the checkout or posts: %+v", process)
		}
	}
	method := roles["review"].Processes[0].Env["DELIVERY_MERGE_METHOD"]
	if method == "" || method != roles["deliver"].Processes[0].Env["DELIVERY_MERGE_METHOD"] {
		t.Fatal("review and delivery must size the same pull request introduction")
	}
	if _, staged := roles["ask_requester"]; !staged {
		t.Fatal("the example has no question role")
	}
	for _, stage := range cfg.Workflow.Stages {
		if stage.Name == cfg.Intake.QuestionRole {
			t.Fatal("the question role is also a stage")
		}
	}
	// The entrance question is attached from the same intake setting the
	// connected form uses, so the accepted run says where a person may be asked.
	if err := prepareStages(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Workflow.Question != "ask_requester" || cfg.Intake.QuestionRole != "ask_requester" {
		t.Fatalf("question=%q intake=%q", cfg.Workflow.Question, cfg.Intake.QuestionRole)
	}
	if err := cfg.Workflow.Validate(purposes); err != nil {
		t.Fatal(err)
	}
	if err := validateQuestionRole(cfg); err != nil || validateStopReporter(cfg) != nil {
		t.Fatal("the example lost its question or stopped-report role", err)
	}
	if cfg.ModelSelection == nil || len(cfg.ModelSelection.Authors) != 4 || cfg.Router.LLM.Model != "" {
		t.Fatal("example lost current-catalog selection")
	}
	// The review comes from another publisher than the work (README.md): no
	// model it names may share a publisher with the launches' candidates.
	reviewers := 0
	for _, process := range roles["review"].Processes {
		if model := process.Env["REVIEW_MODEL"]; model != "" {
			reviewers++
			if publisher, _, _ := strings.Cut(model, "/"); slices.Contains(cfg.ModelSelection.Authors, publisher) {
				t.Fatalf("the review model %s shares its publisher with the candidates %v", model, cfg.ModelSelection.Authors)
			}
		}
	}
	if reviewers == 0 {
		t.Fatal("the example's review names no model")
	}
	useCatalogTransport(t, func(*http.Request) (*http.Response, error) {
		t.Error("unedited example made an external request")
		return nil, fmt.Errorf("unconfigured")
	})
	root := filepath.Join(t.TempDir(), "must-not-be-created")
	var log bytes.Buffer
	err := watchRequests(exampleCheckContext(t), cfg, root, &log)
	if err == nil || !strings.Contains(err.Error(), "explicit intake.project_id") {
		t.Fatalf("unset scope did not stop intake: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("unset scope created queue")
	}
}

// A stage whose declared kind disagrees with what its role launches would let
// the wrong thing decide the run, so it is refused before any work is accepted.
func TestStagesRefuseRolesThatCannotSatisfyTheirDeclaredStage(t *testing.T) {
	for _, variant := range []string{"command-launches-a-model", "model-launches-none", "mode-without-stages", "stages-without-mode", "stage-without-role"} {
		t.Run(variant, func(t *testing.T) {
			cfg := stagesExample(t)
			switch variant {
			case "command-launches-a-model":
				for i := range cfg.Roles {
					if cfg.Roles[i].Name == "verify" {
						cfg.Roles[i].Processes[0].ModelEnv = "NATIVE_MODEL"
					}
				}
			case "model-launches-none":
				for i := range cfg.Roles {
					if cfg.Roles[i].Name == "work" {
						cfg.Roles[i].Processes[0].ModelEnv = ""
					}
				}
			case "mode-without-stages":
				cfg.Workflow.Stages = nil
			case "stages-without-mode":
				cfg.Router.Mode = "llm"
			case "stage-without-role":
				cfg.Workflow.Stages[2].Name = "not-a-role"
			}
			if err := prepareStages(&cfg); err == nil {
				t.Fatal("a run whose progress the wrong thing decides was accepted")
			}
		})
	}
	// Without an intake setting nobody is asked anything, and the entrance
	// carries straight on instead of holding the request at a person.
	cfg := stagesExample(t)
	cfg.Intake = nil
	if err := prepareStages(&cfg); err != nil || cfg.Workflow.Question != "" {
		t.Fatalf("question=%q err=%v", cfg.Workflow.Question, err)
	}
}

const stagesRequest = "Create and deliver Hello 日本語 through the configured stages, then post the verified outcome."
const stagesArtifact = "Hello 日本語\n"
const stagesReceipt = "delivered release/greeting.txt\n"
const stagesReport = "できるようになったこと\n試験用の納品先からHello 日本語を読み戻せます。これは本番ではありません。\n"

const stagesUnitObservation = "project tests: greeting read-back passed"
const stagesLiveObservation = "fixture live check: Hello 日本語"
const stagesNoLiveMethod = "Live verification method: none is supplied."
const stagesLiveMethod = "Live verification method: read the greeting from the synthetic delivery directory, not a production environment."

func stagesEvidenceReport(live bool) string {
	report := stagesReport + "\n観察できる結果\nrelease/greeting.txt: " + stagesArtifact + "\n単体テスト\n" + stagesUnitObservation + "\nライブ確認\n"
	if live {
		return report + stagesLiveObservation + " (架空の検証先。本番ではありません)\n"
	}
	return report + "なし (導入先に検証の手段が無い)\n"
}

const stagesQuestion = "依頼者にしか決められない点があります。納品先は (a) release/ か (b) dist/ のどちらにしますか。\n"
const stagesAnswer = "(a) release/ でお願いします。\n"
const stagesScopeQuestion = "許可された範囲のままでは満たせません。(a) 許可済みの静的ページで挨拶を表示する (b) 運用者に API の変更権限を設定してもらう、どちらにしますか。\n"
const stagesScopeAnswer = "(a) 許可済みの静的ページでお願いします。\n"
const stagesScopeFailure = "The requested API change is outside the granted paths; a static page within the current grant is an alternative."
const stagesKnowledgePath = "project/answers.md"
const stagesKnowledge = "## Delivery target\n\nQuestion: release/ or dist/?\nAnswer: release/.\n"

// Every model stage in the fixture claims the whole request is finished. In an
// ordered run that claim is never read, so it must move nothing at all.
const stagesClaim = "Everything is done, verified and delivered; the request is complete.\n"

// Changes of the same nature as two real deliveries, and an internal one.
const stagesOptionChange = "lister: new option --compact, one line per entry; the help text gains: --compact  print one line per entry\n"
const stagesOrderChange = "lister --tally --sum: the tally line now follows the sum line, and English output says 1 item for one\n"
const stagesOrderKept = "lister --tally --sum: the tally line stays before the sum line; English output says 1 item for one\n"
const stagesInternalChange = "an unexported helper renamed and a unit test added; no option, output or file format changed\n"
const stagesNoConfirmation = "\n依頼者の確認: なし (操作の流れ・画面・公開 API は変わらない)\n"
const stagesChangeQuestion = "納品の前に確認してください。一覧のコマンドの使い方か出力が変わります。\n1. このまま納品する\n2. 直してほしい点を書く\n3. 納品しない\n返答があるまで納品しません。\n"
const stagesCorrection = "出力の行の順番は変えないでください。"
const stagesAcceptance = "このままで納品してください。"

// orderedRunChoice is what a stand-in decision service chooses in a run that
// asks the requester nothing: the next stage after the first stage, and the
// delivery after the stage that confirms the change.
func orderedRunChoice(t *testing.T, r *http.Request) string {
	t.Helper()
	offered, _ := routingRequest(t, r)
	if slices.Contains(offered, "deliver") {
		return "deliver"
	}
	return "work"
}

// routingRequest reads what a chat routing request offered and the state it
// carried.
func routingRequest(t *testing.T, r *http.Request) ([]string, chain.State) {
	t.Helper()
	var body struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Parameters struct {
					Properties struct {
						Role struct {
							Enum []string `json:"enum"`
						} `json:"role"`
					} `json:"properties"`
				} `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	var state chain.State
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Tools) == 0 || len(body.Messages) < 2 {
		t.Errorf("unreadable routing request: %v", err)
		return nil, state
	}
	if err := json.Unmarshal([]byte(body.Messages[1].Content), &state); err != nil {
		t.Errorf("the routing request carried no state: %v", err)
	}
	offered := slices.Clone(body.Tools[0].Function.Parameters.Properties.Role.Enum)
	slices.Sort(offered)
	return offered, state
}

// Actual subprocess fixture for the ordered run. The command stages are real
// child processes whose exit status the runtime observes; no model runs here.
func TestStagesRoleHelper(t *testing.T) {
	action := os.Getenv("EXAMPLE_STAGE_ACTION")
	if action == "" {
		return
	}
	prompt, err := io.ReadAll(os.Stdin)
	if err != nil || !bytes.Contains(prompt, []byte(stagesRequest)) {
		t.Fatal("lost original request", err)
	}
	if os.Getenv("TRACKER_API_KEY") != "" {
		t.Fatal("controller tracker key leaked")
	}
	write := func(path, text string) {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	read := func(path, want string) {
		text, err := os.ReadFile(path)
		if err != nil || string(text) != want {
			t.Fatalf("actual %s=%q err=%v", path, text, err)
		}
	}
	storedComments := func() []string { return fixtureComments(t) }
	post := func(text string) { fixturePost(t, text) }
	repairQuestion := os.Getenv("EXAMPLE_SCOPE_QUESTION") != ""
	// os.Exit below skips deferred work, so the claim is written up front.
	if slices.Contains([]string{"elicit", "ask_requester", "work", "report"}, action) {
		fmt.Print(stagesClaim)
	}
	switch action {
	case "elicit":
		entries, err := os.ReadDir(".")
		if err != nil || len(entries) != 0 && !bytes.Contains(prompt, []byte("stage did not exit 0")) && !bytes.Contains(prompt, []byte(stagesScopeAnswer)) && !bytes.Contains(prompt, []byte(stagesCorrection)) {
			t.Fatal("the entrance prepared or changed project work", err)
		}
		if repairQuestion && len(entries) != 0 && !bytes.Contains(prompt, []byte(stagesScopeFailure)) {
			t.Fatal("requirements lost the worker's actual scope problem")
		}
	case "ask_requester":
		if bytes.Contains(prompt, []byte("the stage that confirms the change chose to ask")) {
			// The question about the change. A silent one posts nothing.
			if os.Getenv("EXAMPLE_QUESTION_SILENT") == "" {
				post(stagesChangeQuestion)
			}
			break
		}
		question := stagesQuestion
		if repairQuestion {
			if !bytes.Contains(prompt, []byte(stagesScopeFailure)) {
				t.Fatal("the question role lost the actual scope problem")
			}
			question = stagesScopeQuestion
		}
		post(question)
		stored := storedComments()
		if len(stored) == 0 || stored[len(stored)-1] != question {
			t.Fatal("the stored question differs from the actual question")
		}
	case "work":
		if repairQuestion && !bytes.Contains(prompt, []byte(stagesScopeAnswer)) {
			fmt.Println(stagesScopeFailure)
			break
		}
		write("src/greeting.txt", stagesArtifact)
		// Changes of the same nature as two real deliveries, written fresh:
		// a new option with its help line, and two output lines in a new order.
		switch os.Getenv("EXAMPLE_CHANGE") {
		case "option":
			write("src/change.txt", stagesOptionChange)
		case "order":
			change := stagesOrderChange
			if bytes.Contains(prompt, []byte(stagesCorrection)) {
				change = stagesOrderKept
			}
			write("src/change.txt", change)
		case "internal":
			write("src/change.txt", stagesInternalChange)
		}
		if os.Getenv("EXAMPLE_KNOWLEDGE") == "answered" {
			if !bytes.Contains(prompt, []byte(stagesAnswer)) || !bytes.Contains(prompt, []byte(stagesKnowledgePath)) {
				t.Fatal("work lost the answer or the configured knowledge destination")
			}
			comments := storedComments()
			if !slices.Contains(comments, stagesQuestion) || !slices.Contains(comments, stagesAnswer) {
				t.Fatal("the work role cannot read the actual exchange through its assigned scope")
			}
			if _, err := os.Stat(stagesKnowledgePath); os.IsNotExist(err) {
				write(stagesKnowledgePath, stagesKnowledge)
			} else {
				read(stagesKnowledgePath, stagesKnowledge)
			}
			fmt.Print("Recorded the actual question and answer without a duplicate: " + stagesKnowledge)
		}
	case "verify":
		// One configured check fails the first time it runs, so the repair
		// stage and the second observation are actually exercised.
		if os.Getenv("EXAMPLE_STAGE_PROCESS") == "project-tests" {
			marker, err := os.OpenFile(".fixture-verify", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err == nil {
				marker.Close()
				if repairQuestion {
					fmt.Println(stagesScopeFailure)
				} else {
					fmt.Print("1 check failed against the current change\n")
				}
				os.Exit(1)
			}
		}
		if !repairQuestion || os.Getenv("EXAMPLE_STAGE_PROCESS") == "project-tests" {
			read("src/greeting.txt", stagesArtifact)
		}
		if os.Getenv("EXAMPLE_STAGE_PROCESS") == "project-tests" {
			fmt.Println(stagesUnitObservation)
		}
	case "review":
		read("src/greeting.txt", stagesArtifact)
		if os.Getenv("EXAMPLE_KNOWLEDGE") == "answered" {
			read(stagesKnowledgePath, stagesKnowledge)
			if !bytes.Contains(prompt, []byte(stagesAnswer)) || !bytes.Contains(prompt, []byte(stagesKnowledge)) {
				t.Fatal("review lost the answer or the worker's knowledge report")
			}
		}
		// The adversarial review objects once, so the work stage runs again
		// on its objection and the review is observed a second time.
		marker, err := os.OpenFile(".fixture-review", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			marker.Close()
			fmt.Print("Review by fixture/reviewer: SENT BACK to the worker. Send-backs so far: 1 of at most 2.\nthe greeting lacks a trailing newline\n")
			os.Exit(1)
		}
		fmt.Print("Review by fixture/reviewer: PASSED. Send-backs so far: 1 of at most 2.\n(no findings)\n")
	case "confirm_change":
		// The stage reads the change the work left in the checkout and says
		// what it does, in ordinary prose. The decision after it is the test's.
		read("src/greeting.txt", stagesArtifact)
		change, err := os.ReadFile("src/change.txt")
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		fmt.Printf("The change read from the checkout: %s", change)
		if kind := os.Getenv("EXAMPLE_CHANGE"); kind == "" || kind == "internal" {
			fmt.Print(stagesNoConfirmation)
		}
	case "deliver":
		read("src/greeting.txt", stagesArtifact)
		write("release/greeting.txt", stagesArtifact)
		if os.Getenv("EXAMPLE_KNOWLEDGE") == "answered" {
			read(stagesKnowledgePath, stagesKnowledge)
			write("release/answers.md", stagesKnowledge)
		}
		// The receipt goes where the example's delivery process names it, so
		// the runtime's read-back of that setting is what is exercised.
		write(os.Getenv("EXAMPLE_STAGE_RECEIPT"), stagesReceipt)
	case "verify_merged":
		read("release/greeting.txt", stagesArtifact)
		if os.Getenv("EXAMPLE_REPORT_EVIDENCE") == "live" {
			fmt.Println(stagesLiveObservation)
		}
	case "report":
		read("release/greeting.txt", stagesArtifact)
		report := stagesReport
		if evidence := os.Getenv("EXAMPLE_REPORT_EVIDENCE"); evidence != "" {
			for _, phrase := range []string{"Observable results", "Unit tests", "Live verification", stagesUnitObservation} {
				if !bytes.Contains(prompt, []byte(phrase)) {
					t.Fatalf("reporter did not receive %q", phrase)
				}
			}
			live := evidence == "live"
			if live && !bytes.Contains(prompt, []byte(stagesLiveObservation)) {
				t.Fatal("reporter lost the observed live-check output")
			}
			method := stagesNoLiveMethod
			if live {
				method = stagesLiveMethod
			}
			if !bytes.Contains(prompt, []byte(method)) {
				t.Fatal("reporter lost the installation's verification method")
			}
			report = stagesEvidenceReport(live)
		}
		write("report/result.md", report)
		stored := 0
		for _, content := range storedComments() {
			if content == report {
				stored++
			}
		}
		if stored == 0 {
			post(report)
			stored = 1
		}
		if stored != 1 {
			t.Fatalf("the actual report is stored %d times", stored)
		}
		fmt.Print(report)
	case "confirm_report":
		reported, err := os.ReadFile("report/result.md")
		if err != nil {
			t.Fatal(err)
		}
		// A check a test asks to fail once sends the work back to the report
		// stage, which then runs again with the report already stored.
		if os.Getenv("EXAMPLE_CONFIRM_FAILS_ONCE") != "" {
			marker, err := os.OpenFile(".fixture-confirm", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err == nil {
				marker.Close()
				fmt.Print("the stored comment could not be read this time\n")
				os.Exit(1)
			}
		}
		// A check a test asks to read late reads after the runtime has said
		// whatever it says at the start of this stage.
		if os.Getenv("EXAMPLE_CONFIRM_READS_LATE") != "" {
			time.Sleep(300 * time.Millisecond)
		}
		// The runtime's own notices, a declaration of the model a launch
		// chose, a stage's sentence or a restart notice, can follow the
		// report, so the stored comments are searched, newest first, for the
		// reported text; it need not be the last one.
		stored := storedComments()
		found := false
		for i := len(stored) - 1; i >= 0 && !found; i-- {
			found = stored[i] == string(reported)
		}
		if !found {
			fmt.Print("no stored comment matches the reported text\n")
			os.Exit(1)
		}
	case "stop_report":
		entries, err := os.ReadDir(".")
		if err != nil || len(entries) != 0 {
			t.Fatal("stopped report prepared project work", err)
		}
		post("停止指示に従って作業を止めました。成果物は納品していません。\n")
	}
	fmt.Print("\nActual fixture operation observed; ordinary prose, no approval object.\n")
	os.Exit(0)
}

// fixtureTracker is the assigned issue as a fixture process reaches it: through
// the scope the runtime handed that process, never with the controller's key.
func fixtureTracker(t *testing.T) (tracker.Backlog, string, func()) {
	t.Helper()
	client, err := tracker.CertificateClient(os.Getenv("TASK_TRACKER_CERT"))
	if err != nil {
		t.Fatal(err)
	}
	return tracker.Backlog{BaseURL: os.Getenv("TASK_TRACKER_URL"), KeyEnv: "TASK_TRACKER_KEY", Client: client},
		os.Getenv("TASK_TRACKER_ISSUE"), client.CloseIdleConnections
}

func fixtureComments(t *testing.T) []string {
	t.Helper()
	b, issue, closer := fixtureTracker(t)
	defer closer()
	rows, err := b.Comments(context.Background(), issue, 0)
	if err != nil {
		t.Fatal("stored comments unavailable", err)
	}
	var contents []string
	for _, raw := range rows {
		var row struct{ Content string }
		if json.Unmarshal(raw, &row) != nil {
			t.Fatal("stored comment could not be read")
		}
		contents = append(contents, row.Content)
	}
	return contents
}

func fixturePost(t *testing.T, text string) {
	t.Helper()
	b, issue, closer := fixtureTracker(t)
	defer closer()
	if _, err := b.AddComment(context.Background(), issue, text); err != nil {
		t.Fatal(err)
	}
}

func stagesFixtureConfig(t *testing.T) config {
	t.Helper()
	cfg := stagesExample(t)
	t.Setenv("MODEL_API_KEY", "synthetic-example-model")
	t.Setenv("TRACKER_API_KEY", "synthetic-example-tracker")
	t.Setenv("DELIVERY_GITHUB_TOKEN", "synthetic-example-delivery")
	cfg.Intake.ProjectID, cfg.Intake.CreatedSince = 17, "2026-01-02T00:00:00Z"
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for i := range cfg.Roles {
		for j := range cfg.Roles[i].Processes {
			p := &cfg.Roles[i].Processes[j]
			p.Command = []string{binary, "-test.run=^TestStagesRoleHelper$"}
			p.Env = map[string]string{"EXAMPLE_STAGE_ACTION": cfg.Roles[i].Name, "EXAMPLE_STAGE_PROCESS": p.Name}
			if p.Receipt != "" {
				p.Env["EXAMPLE_STAGE_RECEIPT"] = p.Receipt
			}
		}
	}
	return cfg
}

// Run the shipped ordered run through the real collector, processes, scope
// server and persisted history. A configured check fails once, so the repair
// stage runs and the command is observed again; nothing any model wrote moves
// the run, and only the last stage's exit status ends it.
func TestStagesExampleRunsToADeliveredArtifactAndAReadBackComment(t *testing.T) {
	for _, when := range []string{"never", "entrance", "after-scope-failure"} {
		t.Run(when, func(t *testing.T) {
			asked, repairQuestion := when != "never", when == "after-scope-failure"
			question, answer := stagesQuestion, stagesAnswer
			if repairQuestion {
				question, answer = stagesScopeQuestion, stagesScopeAnswer
			}
			cfg := stagesFixtureConfig(t)
			cfg.Instructions += "\nApproved knowledge write destination: " + stagesKnowledgePath + "."
			// These two existing full runs also exercise report evidence. The
			// role is scripted; it does not prove a real model writes truthfully.
			if !repairQuestion {
				method := stagesNoLiveMethod
				if asked {
					method = stagesLiveMethod
				}
				cfg.Instructions += "\n" + method
				for i := range cfg.Roles {
					for j := range cfg.Roles[i].Processes {
						evidence := "none"
						if asked {
							evidence = "live"
						}
						cfg.Roles[i].Processes[j].Env["EXAMPLE_REPORT_EVIDENCE"] = evidence
					}
				}
			}
			if asked {
				for i := range cfg.Roles {
					for j := range cfg.Roles[i].Processes {
						if repairQuestion {
							cfg.Roles[i].Processes[j].Env["EXAMPLE_SCOPE_QUESTION"] = "1"
						} else {
							cfg.Roles[i].Processes[j].Env["EXAMPLE_KNOWLEDGE"] = "answered"
						}
					}
				}
			}
			issue := 61
			if asked {
				issue = 62
			}
			root := t.TempDir()
			var mu sync.Mutex
			var comments []any
			stored, posts, catalogs, selections, routes := 0, 0, 0, 0, 0
			answered := false
			allowAnswer, restarted := !repairQuestion, false
			var offered [][]string
			want := []string{"work"}
			if asked {
				want = []string{"ask_requester", "work"}
			}
			want = append(want, "work", "work") // verification and review each fail once
			if repairQuestion {
				want = []string{"work", "ask_requester", "work", "work"}
			}
			want = append(want, "deliver") // after the change was read: nothing a person must see
			add := func(user int, content string) map[string]any {
				stored++
				row := map[string]any{"id": stored, "issueId": issue, "projectId": 17, "content": content, "createdUser": map[string]any{"id": user}}
				comments = append(comments, row)
				return row
			}
			key := fmt.Sprintf("EXAMPLE-%d", issue)
			useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
				mu.Lock()
				defer mu.Unlock()
				if r.URL.Host == "tracker.example.invalid" {
					switch r.Method + " " + r.URL.Path {
					case "GET /api/v2/issues":
						return selectionReply(r, 200, []any{watchedIssue(issue, stagesRequest, "2026-01-03T00:00:00Z")}), nil
					case "GET /api/v2/issues/" + key + "/comments":
						if _, err := os.Stat(filepath.Join(root, "jobs", fmt.Sprint(issue), "question.json")); err == nil && !answered && allowAnswer {
							answered = true
							add(55, answer)
						}
						return selectionReply(r, 200, append([]any{}, comments...)), nil
					case "POST /api/v2/issues/" + key + "/comments":
						if err := r.ParseForm(); err != nil {
							return nil, err
						}
						posts++
						return selectionReply(r, 201, add(99, r.Form.Get("content"))), nil
					}
				}
				if r.URL.Host == "openrouter.ai" {
					switch r.URL.Path {
					case "/api/v1/models":
						catalogs++
						return selectionReply(r, 200, map[string]any{"data": []any{selectionModel(fmt.Sprintf("qwen/fixture-%d", catalogs)), selectionModel(fmt.Sprintf("z-ai/fixture-%d", catalogs))}}), nil
					case "/api/alpha/decisions":
						selections++
						var input struct {
							Questions map[string]struct{ Criteria map[string]string }
						}
						if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
							return nil, err
						}
						choice := fmt.Sprintf("qwen/fixture-%d", catalogs)
						if _, ok := input.Questions["next"].Criteria[choice]; !ok {
							choice = fmt.Sprintf("z-ai/fixture-%d", catalogs)
						}
						return selectionReply(r, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": choice}}}), nil
					case "/api/v1/chat/completions":
						var body struct {
							Tools []struct {
								Function struct {
									Parameters struct {
										Properties struct {
											Role struct {
												Enum []string `json:"enum"`
											} `json:"role"`
										} `json:"properties"`
									} `json:"parameters"`
								} `json:"function"`
							} `json:"tools"`
						}
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							return nil, err
						}
						offered = append(offered, body.Tools[0].Function.Parameters.Properties.Role.Enum)
						if routes >= len(want) {
							return nil, fmt.Errorf("the ordered run asked a model to decide again")
						}
						choice := want[routes]
						routes++
						return routingSelectionReply(r, chain.Assignment{Role: choice}), nil
					}
				}
				return nil, fmt.Errorf("unexpected fixture destination %s %s", r.Method, r.URL.Path)
			})
			var log bytes.Buffer
			finish := startStopQueue(t, cfg, root, 30*time.Millisecond, &log)
			deadline := time.Now().Add(60 * time.Second)
			for {
				state, err := loadWatchState(root, issue)
				if err == nil && state.Waiting && repairQuestion && !restarted {
					if _, err := os.Stat(filepath.Join(root, "jobs", fmt.Sprint(issue), "question.json")); err == nil {
						finish()
						mu.Lock()
						allowAnswer = true
						mu.Unlock()
						restarted = true
						finish = startStopQueue(t, cfg, root, 30*time.Millisecond, &log)
					}
				}
				if err == nil && state.Done {
					break
				}
				if time.Now().After(deadline) {
					finish()
					t.Fatalf("the ordered run did not reach its last stage: step=%s waiting=%t pending=%v error=%v\n%s", state.Step, state.Waiting, state.Pending, err, log.String())
				}
				time.Sleep(10 * time.Millisecond)
			}
			finish()
			if repairQuestion && !restarted {
				t.Fatal("scope question did not survive a restart while waiting")
			}
			state, err := loadWatchState(root, issue)
			if err != nil || state.Pending != nil || state.Waiting || state.Step != "confirm_report" {
				t.Fatalf("the run ended somewhere other than its last stage: %+v %v", state.Step, err)
			}
			actual, err := os.ReadFile(filepath.Join(root, "jobs", fmt.Sprint(issue), "workspace", "release", "greeting.txt"))
			if err != nil || string(actual) != stagesArtifact {
				t.Fatal("actual delivery missing", err)
			}
			knowledge, knowledgeErr := os.ReadFile(filepath.Join(root, "jobs", fmt.Sprint(issue), "workspace", "release", "answers.md"))
			if asked && !repairQuestion && (knowledgeErr != nil || string(knowledge) != stagesKnowledge) {
				t.Fatalf("knowledge did not survive the repair and delivery: %q %v", knowledge, knowledgeErr)
			}
			if (!asked || repairQuestion) && !os.IsNotExist(knowledgeErr) {
				t.Fatal("a fixture without knowledge writeback produced a knowledge file", knowledgeErr)
			}
			launches, records, failures, workingModels, answers := map[string]int{}, 0, 0, 0, 0
			receipts := 0
			for _, result := range state.History {
				if result.Speaker == "runtime" {
					records++
					if strings.Contains(result.Output, stagesReceipt) {
						receipts++
					}
					continue
				}
				if result.Speaker == "requester" {
					answers++
					if result.Output != answer {
						t.Fatalf("the requester's own words were changed: %+v", result)
					}
					continue
				}
				launches[result.Role]++
				if result.Error != "" {
					failures++
				}
				if slices.Contains([]string{"elicit", "work", "report"}, result.Role) && !strings.Contains(result.Output, stagesClaim) {
					t.Fatalf("the fixture stopped claiming completion, so nothing pins that the run ignores it: %+v", result)
				}
				if result.Model != "" {
					workingModels++
				}
			}
			// The repair stage ran because a configured command did not exit 0,
			// and the run carried on only once that command was observed again.
			if launches["work"] != 3 || launches["review"] != 2 || failures != 2 || receipts != 1 {
				t.Fatalf("work=%d review=%d failures=%d receipts=%d", launches["work"], launches["review"], failures, receipts)
			}
			if launches["confirm_report"] != 1 || launches["verify_merged"] != 1 {
				t.Fatalf("the closing stages ran %d and %d times", launches["confirm_report"], launches["verify_merged"])
			}
			mu.Lock()
			defer mu.Unlock()
			if routes != len(want) {
				t.Fatalf("a model was asked to decide %d times", routes)
			}
			for i, enum := range offered {
				slices.Sort(enum)
				choices := []string{"ask_requester", "elicit", "work"}
				if i == len(offered)-1 {
					choices = []string{"ask_requester", "deliver", "elicit"}
				}
				if !slices.Equal(enum, choices) {
					t.Fatalf("decision %d was offered %v, not %v", i+1, enum, choices)
				}
			}
			questions := 0
			for _, row := range comments {
				if row.(map[string]any)["content"] == question {
					questions++
				}
			}
			if asked != (questions == 1 && answers == 1) || (!asked && (questions != 0 || answers != 0)) {
				t.Fatalf("asked=%v questions=%d answers=%d", asked, questions, answers)
			}
			if posts != 1+questions || catalogs != selections || selections != routes+workingModels {
				t.Fatalf("posts=%d catalogs=%d selections=%d routes=%d working=%d", posts, catalogs, selections, routes, workingModels)
			}
			if !repairQuestion {
				expected, found := stagesEvidenceReport(asked), false
				for _, row := range comments {
					found = found || row.(map[string]any)["content"] == expected
				}
				if !found {
					t.Fatal("the evidence report was not actually stored on the assigned issue")
				}
				t.Logf("stored and read back:\n%s", expected)
			}
			t.Logf("%d stage launches recorded, %d model decisions, one delivered artifact and one stored/read-back comment", records, routes)
		})
	}
}

// The runtime's own words can follow the report: a report stage launched
// again after its report is stored, because the check failed once, is
// declared again after it, and the check stage's sentence goes out when the
// check begins, before it reads. The shipped check looks for the report among
// the stored comments, so the run still ends, with one report on the issue.
func TestTheRuntimesWordsAfterTheReportDoNotHoldTheShippedCheck(t *testing.T) {
	for _, shape := range []struct {
		name     string
		knob     string
		sentence string
		after    string
	}{
		{"the report stage is launched again", "EXAMPLE_CONFIRM_FAILS_ONCE", "", "報告の照合が通りませんでした。報告をやり直します。（モデル: "},
		{"the check's stage says its sentence first", "EXAMPLE_CONFIRM_READS_LATE", "報告を照合します。", "報告を照合します。"},
	} {
		t.Run(shape.name, func(t *testing.T) {
			cfg := stagesFixtureConfig(t)
			cfg.Intake.DeclareModels = true
			if shape.sentence != "" {
				cfg.Intake.Announce = true
				for i := range cfg.Workflow.Stages {
					if cfg.Workflow.Stages[i].Name == "confirm_report" {
						cfg.Workflow.Stages[i].Announce = shape.sentence
					}
				}
			}
			for i := range cfg.Roles {
				if cfg.Roles[i].Name == "confirm_report" {
					for j := range cfg.Roles[i].Processes {
						cfg.Roles[i].Processes[j].Env[shape.knob] = "1"
					}
				}
			}
			lookEvery(t, 20*time.Millisecond)
			issue := 63
			key := fmt.Sprintf("EXAMPLE-%d", issue)
			root := t.TempDir()
			var mu sync.Mutex
			var rows []any
			var contents []string
			catalogs := 0
			useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
				mu.Lock()
				defer mu.Unlock()
				if r.URL.Host == "tracker.example.invalid" {
					switch r.Method + " " + r.URL.Path {
					case "GET /api/v2/issues":
						return selectionReply(r, 200, []any{watchedIssue(issue, stagesRequest, "2026-01-03T00:00:00Z")}), nil
					case "GET /api/v2/issues/" + key + "/comments":
						return selectionReply(r, 200, append([]any{}, rows...)), nil
					case "POST /api/v2/issues/" + key + "/comments":
						if err := r.ParseForm(); err != nil {
							return nil, err
						}
						row := map[string]any{"id": len(rows) + 1, "issueId": issue, "projectId": 17, "content": r.Form.Get("content"), "createdUser": map[string]any{"id": 99}}
						rows, contents = append(rows, row), append(contents, r.Form.Get("content"))
						return selectionReply(r, 201, row), nil
					}
				}
				if r.URL.Host == "openrouter.ai" {
					switch r.URL.Path {
					case "/api/v1/models":
						catalogs++
						return selectionReply(r, 200, map[string]any{"data": []any{selectionModel(fmt.Sprintf("qwen/fixture-%d", catalogs)), selectionModel(fmt.Sprintf("z-ai/fixture-%d", catalogs))}}), nil
					case "/api/alpha/decisions":
						var input struct {
							Questions map[string]struct{ Criteria map[string]string }
						}
						if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
							return nil, err
						}
						choice := fmt.Sprintf("qwen/fixture-%d", catalogs)
						if _, ok := input.Questions["next"].Criteria[choice]; !ok {
							choice = fmt.Sprintf("z-ai/fixture-%d", catalogs)
						}
						return selectionReply(r, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": choice}}}), nil
					case "/api/v1/chat/completions":
						return routingSelectionReply(r, chain.Assignment{Role: orderedRunChoice(t, r)}), nil
					}
				}
				return nil, fmt.Errorf("unexpected fixture destination %s %s", r.Method, r.URL.Path)
			})
			var log bytes.Buffer
			finish := startStopQueue(t, cfg, root, 30*time.Millisecond, &log)
			deadline := time.Now().Add(30 * time.Second)
			for {
				state, err := loadWatchState(root, issue)
				if err == nil && state.Done {
					break
				}
				if time.Now().After(deadline) {
					finish()
					mu.Lock()
					defer mu.Unlock()
					t.Fatalf("the run never ended: step=%s comments=%q\n%s", state.Step, contents, log.String())
				}
				time.Sleep(10 * time.Millisecond)
			}
			finish()
			mu.Lock()
			defer mu.Unlock()
			report, after := -1, -1
			for i, content := range contents {
				switch {
				case content == stagesReport && report >= 0:
					t.Fatalf("the report was stored twice: %q", contents)
				case content == stagesReport:
					report = i
				case strings.HasPrefix(content, shape.after) && report >= 0 && after < 0:
					after = i
				}
			}
			if report < 0 || after < 0 {
				t.Fatalf("the runtime said nothing after the one report: %q", contents)
			}
		})
	}
}

// A stop from the requester still wins over an ordered run. The stopped report
// is not a stage and asks no decision service: it runs its one role until that
// role returns, and the stopped request itself stays untouched and unfinished.
func TestStoppedOrderedRunReportsWithoutWalkingItsStages(t *testing.T) {
	cfg := stagesFixtureConfig(t)
	var mu sync.Mutex
	rows := []json.RawMessage{stopComment(51, 55, "停止\nDo not prepare new work.")}
	routes, posts, catalogs, selections := 0, 0, 0, 0
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Host == "tracker.example.invalid" {
			switch r.Method + " " + r.URL.Path {
			case "GET /api/v2/issues":
				return selectionReply(r, 200, []any{watchedIssue(51, stagesRequest, "2026-01-03T00:00:00Z")}), nil
			case "GET /api/v2/issues/EXAMPLE-51/comments":
				return selectionReply(r, 200, rows), nil
			case "POST /api/v2/issues/EXAMPLE-51/comments":
				if err := r.ParseForm(); err != nil {
					return nil, err
				}
				posts++
				row, _ := json.Marshal(map[string]any{"id": 702, "issueId": 51, "projectId": 17, "createdUser": map[string]int{"id": 99}, "content": r.Form.Get("content")})
				rows = append(rows, row)
				return selectionReply(r, 201, json.RawMessage(row)), nil
			}
		}
		if r.URL.Host == "openrouter.ai" {
			switch r.URL.Path {
			case "/api/v1/models":
				catalogs++
				return selectionReply(r, 200, map[string]any{"data": []any{selectionModel(fmt.Sprintf("qwen/fixture-%d", catalogs))}}), nil
			case "/api/alpha/decisions":
				selections++
				return selectionReply(r, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": fmt.Sprintf("qwen/fixture-%d", catalogs)}}}), nil
			case "/api/v1/chat/completions":
				var body struct {
					Tools []struct {
						Function struct {
							Parameters struct {
								Properties struct {
									Role struct {
										Enum []string `json:"enum"`
									} `json:"role"`
								} `json:"properties"`
							} `json:"parameters"`
						} `json:"function"`
					} `json:"tools"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					return nil, err
				}
				offered := body.Tools[0].Function.Parameters.Properties.Role.Enum
				slices.Sort(offered)
				if !slices.Equal(offered, []string{"done", "stop_report"}) {
					t.Errorf("the ordered run remained dispatchable after the stop: %v", offered)
				}
				choice := "stop_report"
				if routes > 0 {
					choice = "done"
				}
				routes++
				return routingSelectionReply(r, chain.Assignment{Role: choice}), nil
			}
		}
		return nil, fmt.Errorf("unexpected stopped-run destination %s %s", r.Method, r.URL.Path)
	})
	root := t.TempDir()
	var log bytes.Buffer
	finish := startStopQueue(t, cfg, root, 30*time.Millisecond, &log)
	waitFor(t, func() bool { s, e := stopReportState(root); return e == nil && s.Done })
	finish()
	state, err := loadWatchState(root, 51)
	if err != nil || state.Done || state.Pending != nil || len(state.History) != 0 {
		t.Fatalf("the stopped run was walked or completed: %+v %v", state, err)
	}
	report, err := stopReportState(root)
	if err != nil || report.Workflow != nil {
		t.Fatalf("the stopped report carried the ordered run: %+v %v", report.Workflow, err)
	}
	for _, result := range report.History {
		if result.Speaker == "runtime" && strings.Contains(result.Output, "Runtime record for stage") {
			t.Fatalf("the stopped report was recorded as a stage: %+v", result)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if posts != 1 || routes != 0 {
		t.Fatalf("stopped-report posts=%d routes=%d", posts, routes)
	}
}

// changeDecision stands in for the decision after the change was read: it
// asks about a change that has no reply yet, delivers what the reply accepts
// and sends a correction back to requirements. The runtime reads none of it.
func changeDecision(change string, state chain.State) string {
	if change == "internal" {
		return "deliver"
	}
	lastWork, lastReply := -1, -1
	for i, result := range state.History {
		if result.Role == "work" && result.Speaker != "runtime" {
			lastWork = i
		}
		if result.Speaker == "requester" {
			lastReply = i
		}
	}
	switch {
	case lastReply < lastWork:
		return "ask_requester"
	case strings.Contains(state.History[lastReply].Output, "このまま"):
		return "deliver"
	}
	return "elicit"
}

// The shipped run reads the change before it is delivered. Changes of the
// same nature as two real deliveries, a new option with its help line and two
// output lines in a new order, are shown to the requester and delivered only
// after their reply; a correction is made and asked about again. An internal
// change is delivered with 依頼者の確認: なし. A question that reaches nobody
// holds the request, across a restart, until the requester comments. What is
// judged is how often the delivery ran and where it sits in the record.
func TestStagesExampleConfirmsTheChangeBeforeDelivery(t *testing.T) {
	for _, tc := range []struct {
		name, change         string
		silent               bool
		replies              []string
		questions, decisions int
	}{
		{"new-option", "option", false, []string{stagesAcceptance}, 1, 2},
		{"output-order-corrected-then-accepted", "order", false, []string{stagesCorrection, stagesAcceptance}, 2, 4},
		{"internal-only", "internal", false, nil, 0, 1},
		{"question-reached-nobody-across-a-restart", "option", true, []string{stagesAcceptance}, 0, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := stagesFixtureConfig(t)
			for i := range cfg.Roles {
				for j := range cfg.Roles[i].Processes {
					cfg.Roles[i].Processes[j].Env["EXAMPLE_CHANGE"] = tc.change
					if tc.silent {
						cfg.Roles[i].Processes[j].Env["EXAMPLE_QUESTION_SILENT"] = "1"
					}
				}
			}
			issue := 71
			key := fmt.Sprintf("EXAMPLE-%d", issue)
			root := t.TempDir()
			var mu sync.Mutex
			var comments []any
			var contents []string
			stored, answered, decisions, catalogs := 0, 0, 0, 0
			allowAnswer, lastAnswered := !tc.silent, ""
			add := func(user int, content string) map[string]any {
				stored++
				row := map[string]any{"id": stored, "issueId": issue, "projectId": 17, "content": content, "createdUser": map[string]any{"id": user}}
				comments = append(comments, row)
				return row
			}
			useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
				mu.Lock()
				defer mu.Unlock()
				if r.URL.Host == "tracker.example.invalid" {
					switch r.Method + " " + r.URL.Path {
					case "GET /api/v2/issues":
						return selectionReply(r, 200, []any{watchedIssue(issue, stagesRequest, "2026-01-03T00:00:00Z")}), nil
					case "GET /api/v2/issues/" + key + "/comments":
						// The requester answers each recorded question once.
						boundary, err := os.ReadFile(filepath.Join(root, "jobs", fmt.Sprint(issue), "question.json"))
						if err == nil && allowAnswer && answered < len(tc.replies) && string(boundary) != lastAnswered {
							lastAnswered = string(boundary)
							add(55, tc.replies[answered])
							answered++
						}
						return selectionReply(r, 200, append([]any{}, comments...)), nil
					case "POST /api/v2/issues/" + key + "/comments":
						if err := r.ParseForm(); err != nil {
							return nil, err
						}
						contents = append(contents, r.Form.Get("content"))
						return selectionReply(r, 201, add(99, r.Form.Get("content"))), nil
					}
				}
				if r.URL.Host == "openrouter.ai" {
					switch r.URL.Path {
					case "/api/v1/models":
						catalogs++
						return selectionReply(r, 200, map[string]any{"data": []any{selectionModel(fmt.Sprintf("qwen/fixture-%d", catalogs))}}), nil
					case "/api/alpha/decisions":
						return selectionReply(r, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": fmt.Sprintf("qwen/fixture-%d", catalogs)}}}), nil
					case "/api/v1/chat/completions":
						offered, state := routingRequest(t, r)
						choice := "work"
						if slices.Contains(offered, "deliver") {
							decisions++
							if want := []string{"ask_requester", "deliver", "elicit"}; !slices.Equal(offered, want) {
								t.Errorf("after the change was read the decision was offered %v", offered)
							}
							choice = changeDecision(tc.change, state)
						}
						return routingSelectionReply(r, chain.Assignment{Role: choice}), nil
					}
				}
				return nil, fmt.Errorf("unexpected fixture destination %s %s", r.Method, r.URL.Path)
			})
			var log bytes.Buffer
			finish := startStopQueue(t, cfg, root, 30*time.Millisecond, &log)
			delivered := func(state chain.State) int {
				count := 0
				for _, result := range state.History {
					if result.Role == "deliver" && result.Speaker != "runtime" {
						count++
					}
				}
				return count
			}
			noticesPosted := func() int {
				mu.Lock()
				defer mu.Unlock()
				count := 0
				for _, content := range contents {
					if strings.HasPrefix(content, unseenQuestionText) {
						count++
					}
				}
				return count
			}
			if tc.silent {
				// The question posted nothing: the engine says so once and the
				// request waits, with nothing delivered, also after a restart.
				deadline := time.Now().Add(60 * time.Second)
				for {
					state, err := loadWatchState(root, issue)
					if err == nil && state.Waiting && state.WaitingWithoutQuestion && noticesPosted() == 1 {
						break
					}
					if time.Now().After(deadline) {
						finish()
						t.Fatalf("the request did not wait with the notice: step=%s err=%v\n%s", state.Step, err, log.String())
					}
					time.Sleep(10 * time.Millisecond)
				}
				finish()
				finish = startStopQueue(t, cfg, root, 30*time.Millisecond, &log)
				time.Sleep(300 * time.Millisecond)
				state, err := loadWatchState(root, issue)
				if err != nil || !state.Waiting || delivered(state) != 0 || noticesPosted() != 1 {
					t.Fatalf("after the restart: waiting=%v delivered=%d notices=%d err=%v", state.Waiting, delivered(state), noticesPosted(), err)
				}
				mu.Lock()
				allowAnswer = true
				mu.Unlock()
			}
			deadline := time.Now().Add(60 * time.Second)
			for {
				state, err := loadWatchState(root, issue)
				if err == nil && state.Done {
					break
				}
				if time.Now().After(deadline) {
					finish()
					t.Fatalf("the run did not finish: step=%s waiting=%v err=%v\n%s", state.Step, state.Waiting, err, log.String())
				}
				time.Sleep(10 * time.Millisecond)
			}
			finish()
			state, err := loadWatchState(root, issue)
			if err != nil {
				t.Fatal(err)
			}
			firstDelivery, acceptance := -1, -1
			var read []string
			for i, result := range state.History {
				switch {
				case result.Role == "deliver" && result.Speaker != "runtime" && firstDelivery < 0:
					firstDelivery = i
				case result.Speaker == "requester" && result.Output == stagesAcceptance:
					acceptance = i
				case result.Role == "confirm_change" && result.Speaker != "runtime":
					read = append(read, result.Output)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			questions, notices, wantNotices := 0, 0, 0
			for _, content := range contents {
				switch {
				case content == stagesChangeQuestion:
					questions++
				case strings.HasPrefix(content, unseenQuestionText):
					notices++
				}
			}
			if tc.silent {
				wantNotices = 1
			}
			if delivered(state) != 1 || questions != tc.questions || decisions != tc.decisions || notices != wantNotices {
				t.Fatalf("delivered=%d questions=%d decisions=%d notices=%d", delivered(state), questions, decisions, notices)
			}
			if len(tc.replies) > 0 && (acceptance < 0 || firstDelivery < acceptance) {
				t.Fatalf("the change was delivered before the requester accepted it: delivery at %d, acceptance at %d", firstDelivery, acceptance)
			}
			switch tc.change {
			case "internal":
				if acceptance >= 0 || len(read) != 1 || !strings.Contains(read[0], stagesInternalChange) || !strings.Contains(read[0], stagesNoConfirmation) {
					t.Fatalf("the internal change was not read and passed with 依頼者の確認: なし: %q", read)
				}
			case "order":
				// The correction was made, read again and asked about again.
				if len(read) != 4 || !strings.Contains(read[0], stagesOrderChange) || !strings.Contains(read[len(read)-1], stagesOrderKept) {
					t.Fatalf("the corrected change was not read again: %q", read)
				}
			default:
				if len(read) != 2 || !strings.Contains(read[0], stagesOptionChange) {
					t.Fatalf("the stage did not read the actual change: %q", read)
				}
			}
			if _, err := os.ReadFile(filepath.Join(root, "jobs", fmt.Sprint(issue), "workspace", "release", "greeting.txt")); err != nil {
				t.Fatal("nothing was delivered", err)
			}
			t.Logf("%s: %d deliveries, %d questions about the change, %d decisions after it was read, %d notices", tc.name, delivered(state), questions, decisions, notices)
		})
	}
}

// What the shipped roles are told about the change before delivery. These are
// instructions to models; the test pins the words, not what a model makes of
// them.
func TestOrderedExampleSaysWhatTheConfirmationReadsAndAsks(t *testing.T) {
	cfg := stagesExample(t)
	roles := map[string]chain.Role{}
	for _, role := range cfg.Roles {
		roles[role.Name] = role
	}
	const carveOut = "after the stage that confirms the change before delivery, about that change"
	if !strings.Contains(cfg.Instructions, carveOut) || !strings.Contains(cfg.Instructions, "nothing is delivered before their reply") {
		t.Error("the shared instructions do not allow the question about the change or say that it holds the delivery")
	}
	for name, phrases := range map[string][]string{
		"elicit":        {carveOut, "On a return from the stage that confirms the change"},
		"ask_requester": {carveOut, "deliver it as it is", "name what to change", "do not deliver it", "返答があるまで納品しません。このまま納品してよいか、直してほしい点があるか、納品しないかを返答してください。"},
		"confirm_change": {"git diff", "The project's knowledge decides what its public API is", "HTTP routes", "command arguments and options and the output other programs read",
			"exported functions and types", "configuration keys and file formats", "Name the material you read and what you could not read",
			"too long to read whole is not an internal one", "依頼者の確認: なし", "or you cannot tell", "covers only the change it was given about"},
		"report": {"依頼者の確認: なし", "quote the question about the change and the requester's reply"},
	} {
		description := routingRoleDescription(roles[name])
		for _, phrase := range phrases {
			if !strings.Contains(description, phrase) {
				t.Errorf("%s does not say %q", name, phrase)
			}
		}
	}
}
