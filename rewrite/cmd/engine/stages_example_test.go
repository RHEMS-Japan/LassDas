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

// The shipped ordered run: every stage that decides whether the work carries on
// is a command the runtime observes, the run ends on one, and the requester is
// reachable only from the entrance.
func TestStagesExampleIsAnOrderedRunNothingWrittenCanAdvance(t *testing.T) {
	cfg := stagesExample(t)
	if cfg.Router.Mode != "stages" {
		t.Fatalf("the example does not use the ordered run: %q", cfg.Router.Mode)
	}
	want := []chain.Stage{
		{Name: "elicit", Kind: chain.ModelStage},
		{Name: "work", Kind: chain.ModelStage},
		{Name: "verify", Kind: chain.CommandStage, OnFailure: "work"},
		{Name: "review", Kind: chain.CommandStage, OnFailure: "work"},
		{Name: "deliver", Kind: chain.CommandStage, OnFailure: "work"},
		{Name: "verify_merged", Kind: chain.CommandStage, OnFailure: "work"},
		{Name: "report", Kind: chain.ModelStage},
		{Name: "confirm_report", Kind: chain.CommandStage, OnFailure: "report"},
	}
	if !slices.Equal(cfg.Workflow.Stages, want) {
		t.Fatalf("the shipped run is %+v", cfg.Workflow.Stages)
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
	if cfg.ModelSelection == nil || len(cfg.ModelSelection.Authors) != 5 || cfg.Router.LLM.Model != "" {
		t.Fatal("example lost current-catalog selection")
	}
	useCatalogTransport(t, func(*http.Request) (*http.Response, error) {
		t.Error("unedited example made an external request")
		return nil, fmt.Errorf("unconfigured")
	})
	root := filepath.Join(t.TempDir(), "must-not-be-created")
	var log bytes.Buffer
	err := watchRequests(context.Background(), cfg, root, &log)
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
const stagesQuestion = "依頼者にしか決められない点があります。納品先は (a) release/ か (b) dist/ のどちらにしますか。\n"
const stagesAnswer = "(a) release/ でお願いします。\n"

// Every model stage in the fixture claims the whole request is finished. In an
// ordered run that claim is never read, so it must move nothing at all.
const stagesClaim = "Everything is done, verified and delivered; the request is complete.\n"

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
	// os.Exit below skips deferred work, so the claim is written up front.
	if slices.Contains([]string{"elicit", "ask_requester", "work", "report"}, action) {
		fmt.Print(stagesClaim)
	}
	switch action {
	case "elicit":
		entries, err := os.ReadDir(".")
		if err != nil || len(entries) != 0 {
			t.Fatal("the entrance prepared or changed project work", err)
		}
	case "ask_requester":
		post(stagesQuestion)
		stored := storedComments()
		if len(stored) == 0 || stored[len(stored)-1] != stagesQuestion {
			t.Fatal("the stored question differs from the actual question")
		}
	case "work":
		write("src/greeting.txt", stagesArtifact)
	case "verify":
		read("src/greeting.txt", stagesArtifact)
		// One configured check fails the first time it runs, so the repair
		// stage and the second observation are actually exercised.
		if os.Getenv("EXAMPLE_STAGE_PROCESS") == "project-tests" {
			marker, err := os.OpenFile(".fixture-verify", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err == nil {
				marker.Close()
				fmt.Print("1 check failed against the current change\n")
				os.Exit(1)
			}
		}
	case "review":
		read("src/greeting.txt", stagesArtifact)
		// The adversarial review objects once, so the work stage runs again
		// on its objection and the review is observed a second time.
		marker, err := os.OpenFile(".fixture-review", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			marker.Close()
			fmt.Print("Review by fixture/reviewer: SENT BACK to the worker. Send-backs so far: 1 of at most 2.\nthe greeting lacks a trailing newline\n")
			os.Exit(1)
		}
		fmt.Print("Review by fixture/reviewer: PASSED. Send-backs so far: 1 of at most 2.\n(no findings)\n")
	case "deliver":
		read("src/greeting.txt", stagesArtifact)
		write("release/greeting.txt", stagesArtifact)
		// The receipt goes where the example's delivery process names it, so
		// the runtime's read-back of that setting is what is exercised.
		write(os.Getenv("EXAMPLE_STAGE_RECEIPT"), stagesReceipt)
	case "verify_merged":
		read("release/greeting.txt", stagesArtifact)
	case "report":
		read("release/greeting.txt", stagesArtifact)
		write("report/result.md", stagesReport)
		stored := 0
		for _, content := range storedComments() {
			if content == stagesReport {
				stored++
			}
		}
		if stored == 0 {
			post(stagesReport)
			stored = 1
		}
		if stored != 1 {
			t.Fatalf("the actual report is stored %d times", stored)
		}
		fmt.Print(stagesReport)
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
	for _, asked := range []bool{false, true} {
		t.Run(fmt.Sprintf("asked=%v", asked), func(t *testing.T) {
			cfg := stagesFixtureConfig(t)
			issue := 61
			if asked {
				issue = 62
			}
			root := t.TempDir()
			var mu sync.Mutex
			var comments []any
			stored, posts, catalogs, selections, routes := 0, 0, 0, 0, 0
			answered := false
			var offered [][]string
			want := []string{"work"}
			if asked {
				want = []string{"ask_requester", "work"}
			}
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
						if _, err := os.Stat(filepath.Join(root, "jobs", fmt.Sprint(issue), "question.json")); err == nil && !answered {
							answered = true
							add(55, stagesAnswer)
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
			state, err := loadWatchState(root, issue)
			if err != nil || state.Pending != nil || state.Waiting || state.Step != "confirm_report" {
				t.Fatalf("the run ended somewhere other than its last stage: %+v %v", state.Step, err)
			}
			actual, err := os.ReadFile(filepath.Join(root, "jobs", fmt.Sprint(issue), "workspace", "release", "greeting.txt"))
			if err != nil || string(actual) != stagesArtifact {
				t.Fatal("actual delivery missing", err)
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
					if result.Output != stagesAnswer {
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
			for _, enum := range offered {
				slices.Sort(enum)
				if !slices.Equal(enum, []string{"ask_requester", "work"}) {
					t.Fatalf("the entrance was offered %v", enum)
				}
			}
			questions := 0
			for _, row := range comments {
				if row.(map[string]any)["content"] == stagesQuestion {
					questions++
				}
			}
			if asked != (questions == 1 && answers == 1) || (!asked && (questions != 0 || answers != 0)) {
				t.Fatalf("asked=%v questions=%d answers=%d", asked, questions, answers)
			}
			if posts != 1+questions || catalogs != selections || selections != routes+workingModels {
				t.Fatalf("posts=%d catalogs=%d selections=%d routes=%d working=%d", posts, catalogs, selections, routes, workingModels)
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
		{"the report stage is launched again", "EXAMPLE_CONFIRM_FAILS_ONCE", "", "報告をやり直します。選定モデル: "},
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
						return routingSelectionReply(r, chain.Assignment{Role: "work"}), nil
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
