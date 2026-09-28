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

func loadExample(t *testing.T, path string) config {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func operatorExample(t *testing.T) config {
	t.Helper()
	return loadExample(t, "../../examples/operator.json")
}

// The same shipped configuration with invocation moved to a gateway account.
func gatewayExample(t *testing.T) config {
	t.Helper()
	return loadExample(t, "../../examples/operator-gateway.json")
}

// Both shipped examples are held to the same boundaries. The expected
// credential names are written here, not read back out of the file.
var operatorExamples = []struct {
	name, path, workerKey string
	gateway               bool
}{
	{"openrouter", "../../examples/operator.json", "MODEL_API_KEY", false},
	{"gateway", "../../examples/operator-gateway.json", "GATEWAY_API_KEY", true},
}

func TestOperatorExampleHasExplicitBoundariesAndNoActiveIntake(t *testing.T) {
	for _, example := range operatorExamples {
		t.Run(example.name, func(t *testing.T) {
			exampleBoundaries(t, example.path, example.workerKey, example.gateway)
		})
	}
}

func exampleBoundaries(t *testing.T, path, workerKey string, gateway bool) {
	cfg := loadExample(t, path)
	purposes := map[string]string{}
	for _, role := range cfg.Roles {
		purposes[role.Name] = role.Purpose
		for _, process := range role.Processes {
			if process.Env["NATIVE_MODEL"] != "" {
				t.Fatal("example pinned a working model")
			}
			if process.ModelEnv != "" && (process.ModelEnv != "NATIVE_MODEL" || process.Secrets["OPENROUTER_API_KEY"] != workerKey) {
				t.Fatal("example lost per-launch selection or named credential source")
			}
			if process.ModelEnv != "" && gateway && process.Env["OPENROUTER_BASE_URL"] != gatewayBase(cfg) {
				t.Fatal("worker still invokes the catalog account directly")
			}
			for _, source := range process.Secrets {
				if source == cfg.Backlog.KeyEnv {
					t.Fatal("account tracker credential assigned to worker")
				}
			}
			if process.TrackerAccess == "comment" && !slices.Contains([]string{"post_report", "stop_report", "ask_requester"}, role.Name) {
				t.Fatal("posting granted outside posting role")
			}
			if slices.Contains([]string{"elicit", "ask_requester", "review", "review_report", "post_report", "confirm_report", "stop_report"}, role.Name) && slices.Contains(process.Command, "--write") {
				t.Fatal("read-only example role can write workspace")
			}
			if !slices.Contains(process.Command, "/opt/ticket-automation/bundle/harnesses/linux_role.py") {
				t.Fatal("example role bypasses actual launcher")
			}
			if role.Name == "stop_report" && slices.Contains(process.Command, "/opt/ticket-automation/bundle/harnesses/git_workspace.py") {
				t.Fatal("stopped reporting must not prepare new project work")
			}
		}
		if role.Name == "review" || role.Name == "review_report" {
			models := 0
			for _, process := range role.Processes {
				if process.ModelEnv != "" {
					models++
				}
			}
			if models != 2 {
				t.Fatalf("%s needs two model processes, got %d", role.Name, models)
			}
		}
	}
	if err := cfg.Workflow.Validate(purposes); err != nil {
		t.Fatal(err)
	}
	if err := validateStopReporter(cfg); err != nil || cfg.Intake.StopReportRole != "stop_report" {
		t.Fatal("missing bounded stopped-report role", err)
	}
	if err := validateQuestionRole(cfg); err != nil || cfg.Intake.QuestionRole != "ask_requester" {
		t.Fatal("missing the configured question role", err)
	}
	for _, choices := range cfg.Workflow.After {
		if slices.Contains(choices, "stop_report") {
			t.Fatal("stopped reporter available to normal workflow")
		}
	}
	if cfg.ModelSelection == nil || len(cfg.ModelSelection.Authors) != 5 || cfg.Router.LLM.Model != "" {
		t.Fatal("example lost current-catalog selection")
	}
	if (cfg.ModelSelection.Gateway != nil) != gateway {
		t.Fatal("example gained or lost its invocation gateway")
	}
	if gateway {
		if err := cfg.ModelSelection.validate(); err != nil {
			t.Fatal(err)
		}
		if cfg.ModelSelection.Gateway.Prefix != "openrouter/" || cfg.ModelSelection.Gateway.KeyEnv != workerKey {
			t.Fatalf("gateway invocation route incomplete: %+v", cfg.ModelSelection.Gateway)
		}
		// Only invocation moves. The decisions API is not served by the
		// gateway, so selection and routing decisions stay where they are.
		for _, decisions := range []chain.Jev{cfg.ModelSelection.Judge, cfg.Router.Decision} {
			if decisions.KeyEnv != "MODEL_API_KEY" || !strings.Contains(decisions.URL, "openrouter.ai") {
				t.Fatalf("decision service left the catalog account: %+v", decisions)
			}
		}
		if cfg.Router.LLM.KeyEnv != workerKey || !strings.HasPrefix(cfg.Router.LLM.URL, gatewayBase(cfg)+"/") {
			t.Fatalf("routing invocation did not follow the roles: %+v", cfg.Router.LLM)
		}
	}
	if _, err := bindRequestConfig(cfg, filepath.Join(t.TempDir(), "job"), "EXAMPLE-41"); err != nil {
		t.Fatal(err)
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

// The gateway's own base URL, derived from the one place it is configured.
func gatewayBase(cfg config) string {
	return strings.TrimSuffix(cfg.ModelSelection.Gateway.ModelsURL, "/models")
}

const exampleRequest = "Create and deliver Hello 日本語, independently review it, then post the verified outcome."
const exampleArtifact = "Hello 日本語\n"
const exampleReport = "できるようになったこと\n試験用の納品先からHello 日本語を読み戻せます。これは本番ではありません。\n"
const exampleStoppedReport = "停止指示に従って作業を止めました。成果物は納品していません。\n"
const exampleQuestion = "依頼者にしか決められない点があります。納品先は (a) release/ か (b) dist/ のどちらにしますか。\n"

// The same two sentences the decision model is given in the chain package. The
// example is where an operator sees them, so pin them here as well.
const byMorningStandard = "can this request be carried to a delivered, verified result by morning with nobody available to answer?"
const askWithChoices = "each with two to four concrete choices, in ordinary prose the requester can answer in a single reply"
const notAGeneralPlea = "A general request for clarification is not a question: name the undecided points and their choices."
const requesterPointTest = "A point is the requester's to decide only when the request, the repository and the operator instructions do not settle it and it changes what the delivered result does, where it goes or what the work may touch: a behaviour the request leaves open without saying you may choose, a target that cannot be told apart, access or a credential that was not given, instructions that contradict each other, or an action that cannot be undone."
const preferencesAreSettled = "Wording, naming, language, level of detail and style are never questions: take the reading closest to the request and to what the repository already does, write the choice down with its reason, and leave it to the review of the delivered result; a point once decided is settled and is not listed again as a question."
const askWhenInDoubt = "Proceeding with such an open point costs a night's work and asking costs one reply, so proceed only when every point of that kind is absent or already answered and the settled requirements state the completion condition to be held to; when you cannot tell whether a point is of that kind, ask the requester, and never proceed in order to find out."
const exampleAnswer = "(a) release/ でお願いします。\n"

// Actual subprocess fixture, not a native SDK/model or permission-sandbox test.
func TestExampleRoleHelper(t *testing.T) {
	action := os.Getenv("EXAMPLE_TEST_ACTION")
	if action == "" {
		return
	}
	prompt, err := io.ReadAll(os.Stdin)
	if err != nil || !bytes.Contains(prompt, []byte(exampleRequest)) {
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
	emptyWorkspace := func() {
		entries, err := os.ReadDir(".")
		if err != nil || len(entries) != 0 {
			t.Fatal("the entrance prepared or changed project work", err)
		}
	}
	switch action {
	case "elicit":
		emptyWorkspace()
	case "ask_requester":
		emptyWorkspace()
		client, err := tracker.CertificateClient(os.Getenv("TASK_TRACKER_CERT"))
		if err != nil {
			t.Fatal(err)
		}
		defer client.CloseIdleConnections()
		b := tracker.Backlog{BaseURL: os.Getenv("TASK_TRACKER_URL"), KeyEnv: "TASK_TRACKER_KEY", Client: client}
		issue := os.Getenv("TASK_TRACKER_ISSUE")
		if _, err := b.AddComment(context.Background(), issue, exampleQuestion); err != nil {
			t.Fatal(err)
		}
		rows, err := b.Comments(context.Background(), issue, 0)
		if err != nil || len(rows) == 0 {
			t.Fatal("stored question missing", err)
		}
		var row struct{ Content string }
		if json.Unmarshal(rows[len(rows)-1], &row) != nil || row.Content != exampleQuestion {
			t.Fatal("stored question differs from the actual question")
		}
		fmt.Print(row.Content)
	case "implement":
		write("src/greeting.txt", exampleArtifact)
	case "review":
		read("src/greeting.txt", exampleArtifact)
	case "deliver":
		read("src/greeting.txt", exampleArtifact)
		write("release/greeting.txt", exampleArtifact)
	case "verify":
		read("release/greeting.txt", exampleArtifact)
	case "draft_report":
		read("release/greeting.txt", exampleArtifact)
		write("report/result.md", exampleReport)
	case "review_report":
		read("report/result.md", exampleReport)
		read("release/greeting.txt", exampleArtifact)
	case "post_report", "confirm_report", "stop_report":
		wantReport := exampleReport
		if action == "stop_report" {
			wantReport = exampleStoppedReport
			entries, err := os.ReadDir(".")
			if err != nil || len(entries) != 0 {
				t.Fatal("stopped report prepared project work", err)
			}
		} else {
			read("report/result.md", exampleReport)
		}
		client, err := tracker.CertificateClient(os.Getenv("TASK_TRACKER_CERT"))
		if err != nil {
			t.Fatal(err)
		}
		defer client.CloseIdleConnections()
		b := tracker.Backlog{BaseURL: os.Getenv("TASK_TRACKER_URL"), KeyEnv: "TASK_TRACKER_KEY", Client: client}
		issue := os.Getenv("TASK_TRACKER_ISSUE")
		if action == "post_report" || action == "stop_report" {
			if _, err := b.AddComment(context.Background(), issue, wantReport); err != nil {
				t.Fatal(err)
			}
		}
		// Read the actual stored text, not a position: the entrance may have
		// left its own question and the requester's answer on the same issue.
		rows, err := b.Comments(context.Background(), issue, 0)
		if err != nil {
			t.Fatal("stored comments unavailable", err)
		}
		stored := 0
		for _, raw := range rows {
			var row struct{ Content string }
			if json.Unmarshal(raw, &row) != nil {
				t.Fatal("stored comment could not be read")
			}
			if row.Content == wantReport {
				stored++
			}
		}
		if stored != 1 {
			t.Fatalf("the actual report is stored %d times among %d comments", stored, len(rows))
		}
		fmt.Print(wantReport)
	}
	fmt.Print("\nActual fixture operation observed; ordinary prose, no approval object.\n")
	os.Exit(0)
}

func TestOperatorExampleStopReportsWithoutPreparingWork(t *testing.T) {
	for _, example := range operatorExamples {
		t.Run(example.name, func(t *testing.T) {
			exampleStopReport(t, example.path)
		})
	}
}

func exampleStopReport(t *testing.T, path string) {
	cfg := loadExample(t, path)
	t.Setenv("MODEL_API_KEY", "synthetic-example-model")
	t.Setenv("GATEWAY_API_KEY", "synthetic-example-gateway")
	t.Setenv("TRACKER_API_KEY", "synthetic-example-tracker")
	cfg.Intake.ProjectID, cfg.Intake.CreatedSince = 17, "2026-01-02T00:00:00Z"
	// Test stopped reporting with a fixture decision API. Fresh model selection
	// and peer separation are exercised by the full example test below.
	cfg.Router.Mode, cfg.ModelSelection = "jev", nil
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for i := range cfg.Roles {
		for j := range cfg.Roles[i].Processes {
			p := &cfg.Roles[i].Processes[j]
			p.Command = []string{binary, "-test.run=^TestExampleRoleHelper$"}
			p.Env = map[string]string{"EXAMPLE_TEST_ACTION": cfg.Roles[i].Name}
			p.ModelEnv = ""
		}
	}
	var mu sync.Mutex
	rows := []json.RawMessage{stopComment(51, 55, "停止\nDo not prepare new work.")}
	routes, posts := 0, 0
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Host == "tracker.example.invalid" {
			switch r.Method + " " + r.URL.Path {
			case "GET /api/v2/issues":
				return selectionReply(r, 200, []any{watchedIssue(51, exampleRequest, "2026-01-03T00:00:00Z")}), nil
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
		if r.URL.Host == "openrouter.ai" && r.URL.Path == "/api/alpha/decisions" {
			var input struct {
				State     chain.State
				Questions map[string]struct{ Criteria map[string]string }
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				return nil, err
			}
			choices := input.Questions["next"].Criteria
			if len(choices) != 2 || choices["stop_report"] == "" || choices["done"] == "" || input.State.Workflow != nil {
				t.Error("normal work remains dispatchable after stop")
			}
			choice := "stop_report"
			if routes > 0 {
				choice = "done"
			}
			routes++
			return selectionReply(r, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": choice}}}), nil
		}
		return nil, fmt.Errorf("unexpected stopped-example destination")
	})
	root := t.TempDir()
	var log bytes.Buffer
	finish := startStopQueue(t, cfg, root, 30*time.Millisecond, &log)
	waitFor(t, func() bool { s, e := stopReportState(root); return e == nil && s.Done })
	finish()
	state, err := loadWatchState(root, 51)
	if err != nil || state.Done || state.Pending != nil || len(state.History) != 0 {
		t.Fatalf("stopped original was run or completed: %+v %v", state, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if posts != 1 || routes != 2 {
		t.Fatalf("stopped-report posts=%d routes=%d", posts, routes)
	}
}

// Run the shipped action graph/role groups/current-model configuration through
// the real collector, processes, scope server and persisted history. Only the
// process programs and external service/model responses are fixtures here.
func TestOperatorExampleIntakeToActualArtifactAndStoredComment(t *testing.T) {
	for _, example := range operatorExamples {
		t.Run(example.name, func(t *testing.T) {
			exampleIntakeToArtifact(t, example.path)
		})
	}
}

func exampleIntakeToArtifact(t *testing.T, path string) {
	cfg := loadExample(t, path)
	t.Setenv("MODEL_API_KEY", "synthetic-example-model")
	t.Setenv("GATEWAY_API_KEY", "synthetic-example-gateway")
	t.Setenv("TRACKER_API_KEY", "synthetic-example-tracker")
	cfg.Intake.ProjectID, cfg.Intake.CreatedSince = 17, "2026-01-02T00:00:00Z"
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for i := range cfg.Roles {
		for j := range cfg.Roles[i].Processes {
			p := &cfg.Roles[i].Processes[j]
			p.Command = []string{binary, "-test.run=^TestExampleRoleHelper$"}
			p.Env = map[string]string{"EXAMPLE_TEST_ACTION": cfg.Roles[i].Name}
		}
	}
	var mu sync.Mutex
	var comments []any
	catalogs, selections, routes, posts, lists := 0, 0, 0, 0, 0
	want := []string{"elicit", "investigate", "design", "implement", "review", "deliver", "verify", "draft_report", "review_report", "post_report", "confirm_report", "done"}
	gateway := cfg.ModelSelection.Gateway
	// Routing is an invocation too, so it moves to the gateway with the roles.
	routing := func(r *http.Request) (*http.Response, error) {
		if routes >= len(want) {
			return nil, fmt.Errorf("unexpected extra route")
		}
		var input struct{ Model string }
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			return nil, err
		}
		if (gateway != nil) != strings.HasPrefix(input.Model, "openrouter/") {
			t.Errorf("routing invoked %q on the wrong account", input.Model)
		}
		choice := want[routes]
		routes++
		return routingSelectionReply(r, chain.Assignment{Role: choice}), nil
	}
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Host == "tracker.example.invalid" {
			switch r.Method + " " + r.URL.Path {
			case "GET /api/v2/issues":
				return selectionReply(r, 200, []any{watchedIssue(41, exampleRequest, "2026-01-03T00:00:00Z")}), nil
			case "GET /api/v2/issues/EXAMPLE-41/comments":
				return selectionReply(r, 200, append([]any{}, comments...)), nil
			case "POST /api/v2/issues/EXAMPLE-41/comments":
				if err := r.ParseForm(); err != nil {
					return nil, err
				}
				posts++
				row := map[string]any{"id": posts, "issueId": 41, "projectId": 17, "content": r.Form.Get("content"), "createdUser": map[string]any{"id": 99}}
				comments = append(comments, row)
				return selectionReply(r, 201, row), nil
			}
		}
		if r.URL.Host == "openrouter.ai" {
			switch r.URL.Path {
			case "/api/v1/models":
				catalogs++
				if r.Header.Get("Authorization") != "" || r.Header.Get("Cache-Control") != "no-cache" {
					t.Error("catalog not fresh/credential-free")
				}
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
				return routing(r)
			}
		}
		if gateway != nil && r.URL.Host == "gateway.example.invalid" {
			switch r.URL.Path {
			case "/v1/models":
				lists++
				if r.Header.Get("Authorization") != "Bearer synthetic-example-gateway" {
					t.Error("gateway list did not use the gateway credential")
				}
				return selectionReply(r, 200, map[string]any{"data": []any{
					gatewayEntry(gateway.Prefix + fmt.Sprintf("qwen/fixture-%d", catalogs)),
					gatewayEntry(gateway.Prefix + fmt.Sprintf("z-ai/fixture-%d", catalogs)),
				}}), nil
			case "/v1/chat/completions":
				return routing(r)
			}
		}
		return nil, fmt.Errorf("unexpected fixture destination %s %s", r.Method, r.URL.Path)
	})
	root := t.TempDir()
	var log bytes.Buffer
	finish := startStopQueue(t, cfg, root, 30*time.Millisecond, &log)
	// A race-instrumented helper deliberately waits during process teardown.
	// Ten sequential roles need longer than the short single-role test helper.
	deadline := time.Now().Add(30 * time.Second)
	for {
		state, err := loadWatchState(root, 41)
		if err == nil && state.Done {
			break
		}
		if time.Now().After(deadline) {
			finish()
			t.Fatalf("example did not reach completion: step=%s pending=%v error=%v\n%s", state.Step, state.Pending, err, log.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	finish()
	state, err := loadWatchState(root, 41)
	if err != nil || state.Pending != nil || !strings.HasSuffix(state.Request, exampleRequest) {
		t.Fatal("bad completion state", err)
	}
	actual, err := os.ReadFile(filepath.Join(root, "jobs", "41", "workspace", "release", "greeting.txt"))
	if err != nil || string(actual) != exampleArtifact {
		t.Fatal("actual delivery missing", err)
	}
	groups := map[string][]string{}
	workingModels := 0
	for _, result := range state.History {
		if result.Error != "" {
			t.Fatalf("fixture role failed: %+v", result)
		}
		if result.Model != "" {
			workingModels++
			groups[result.Role] = append(groups[result.Role], result.Model)
			wantPrefix := ""
			if gateway != nil {
				wantPrefix = gateway.Prefix
			}
			// The catalog id is what was chosen; the prefix says how it was
			// reached. Recording the joined id would lose the publisher.
			if result.ModelPrefix != wantPrefix || strings.HasPrefix(result.Model, "openrouter/") {
				t.Fatalf("history did not separate the chosen model from its route: %+v", result)
			}
		}
	}
	for _, name := range []string{"review", "review_report"} {
		if len(groups[name]) != 2 || strings.Split(groups[name][0], "/")[0] == strings.Split(groups[name][1], "/")[0] {
			t.Fatalf("not two independent selected publishers: %v", groups[name])
		}
	}
	mu.Lock()
	defer mu.Unlock()
	wantLists := 0
	if gateway != nil {
		wantLists = selections
	}
	if posts != 1 || len(comments) != 1 || routes != len(want) || catalogs != selections || selections != routes+workingModels || lists != wantLists {
		t.Fatalf("posts=%d routes=%d selections=%d catalogs=%d working=%d gateway lists=%d", posts, routes, selections, catalogs, workingModels, lists)
	}
	t.Logf("11 actions, 2 independent review groups, %d fresh selections, actual artifact and one stored/read-back comment", selections)
}

// The shipped example asks the requester only at the entrance: the request is
// settled before anything is built, the question is reachable only from there,
// and neither of those two actions can finish the work or change the checkout.
// Every shipped example's entrance actions carry the standard in the same
// words; a copy that drifted would tell its decision something different.
func TestEveryExampleCarriesTheEntranceStandard(t *testing.T) {
	for _, path := range []string{"../../examples/operator.json", "../../examples/operator-gateway.json", "../../examples/operator-stages.json"} {
		cfg := loadExample(t, path)
		roles := map[string]chain.Role{}
		for _, role := range cfg.Roles {
			roles[role.Name] = role
		}
		for _, name := range []string{"elicit", "ask_requester"} {
			for _, sentence := range []string{requesterPointTest, preferencesAreSettled, askWhenInDoubt} {
				if !strings.Contains(routingRoleDescription(roles[name]), sentence) {
					t.Fatalf("%s: %s does not carry: %s", path, name, sentence)
				}
			}
		}
		if !strings.Contains(routingRoleDescription(roles["elicit"]), byMorningStandard) {
			t.Fatalf("%s: the entrance does not say what standard it settles the request against", path)
		}
	}
}

// The report writer is capped in every connected-role example, so a report
// review that keeps objecting cannot run all night: the decision after
// review_report still has post_report as a way forward. implement is not
// capped: after review the only other connection is deliver, and a cap there
// would force an unreviewed delivery.
func TestOperatorExampleCapsTheReportWriterAndNotTheImplementer(t *testing.T) {
	for _, example := range operatorExamples {
		cfg := loadExample(t, example.path)
		if cfg.Workflow == nil || cfg.Workflow.LaunchLimit["draft_report"] < 1 {
			t.Fatalf("%s: draft_report has no launch limit", example.path)
		}
		if _, capped := cfg.Workflow.LaunchLimit["implement"]; capped {
			t.Fatalf("%s: implement is capped, which would force delivery over a reviewer's objection", example.path)
		}
	}
}

func TestOperatorExampleAsksTheRequesterOnlyAtTheEntrance(t *testing.T) {
	cfg := operatorExample(t)
	if !slices.Equal(cfg.Workflow.Start, []string{"elicit"}) {
		t.Fatalf("the example does not settle the request first: %v", cfg.Workflow.Start)
	}
	if strings.Contains(cfg.Instructions, "after acceptance") || !strings.Contains(cfg.Instructions, "configured question role") {
		t.Fatal("the example instructions do not say who may ask the requester")
	}
	if !slices.Equal(cfg.Workflow.After["ask_requester"], []string{"elicit"}) ||
		!slices.Equal(cfg.Workflow.Recover["ask_requester"], []string{"elicit"}) {
		t.Fatalf("an answer does not return to the entrance: %v %v", cfg.Workflow.After["ask_requester"], cfg.Workflow.Recover["ask_requester"])
	}
	for source, targets := range cfg.Workflow.After {
		if slices.Contains(targets, "ask_requester") && source != "elicit" {
			t.Fatalf("the requester is also asked from %q", source)
		}
	}
	for _, source := range []string{"elicit", "ask_requester"} {
		for _, section := range []map[string][]string{cfg.Workflow.After, cfg.Workflow.Recover} {
			if slices.Contains(section[source], "done") {
				t.Fatalf("%s can end the request at a person", source)
			}
		}
	}
	roles := map[string]chain.Role{}
	for _, role := range cfg.Roles {
		roles[role.Name] = role
	}
	// The decision at the entrance is one-sided: guessing costs a night, asking
	// costs one reply. Every action the router can choose there says so, in the
	// same words, and the entrance says which standard it settles against.
	for _, name := range []string{"elicit", "ask_requester", "investigate"} {
		for _, sentence := range []string{requesterPointTest, preferencesAreSettled, askWhenInDoubt} {
			if !strings.Contains(routingRoleDescription(roles[name]), sentence) {
				t.Fatalf("%s does not tell the decision which way to err: %s", name, sentence)
			}
		}
	}
	if !strings.Contains(routingRoleDescription(roles["elicit"]), byMorningStandard) {
		t.Fatal("the entrance does not say what standard it settles the request against")
	}
	// A question is a list of undecided points with concrete choices the
	// requester answers in one reply; a general plea for clarification would
	// cost the second round trip this entrance exists to avoid.
	for _, sentence := range []string{askWithChoices, notAGeneralPlea} {
		if !strings.Contains(routingRoleDescription(roles["ask_requester"]), sentence) {
			t.Fatalf("ask_requester no longer says: %s", sentence)
		}
	}
	for name, access := range map[string]string{"elicit": "read", "ask_requester": "comment"} {
		role, configured := roles[name]
		if !configured || len(role.Processes) != 1 {
			t.Fatalf("%s is not one configured action: %+v", name, role)
		}
		if role.Processes[0].TrackerAccess != access {
			t.Fatalf("%s tracker access is %q", name, role.Processes[0].TrackerAccess)
		}
		if slices.Contains(role.Processes[0].Command, "--write") {
			t.Fatalf("%s can change the checkout", name)
		}
	}
}

// The other path through the same shipped example: the entrance decides it
// cannot settle a point by itself, the requester answers at the issue, and the
// work carries on to the actual artifact without a second question.
func TestOperatorExampleCarriesTheRequestersAnswerOnToDelivery(t *testing.T) {
	cfg := operatorExample(t)
	t.Setenv("MODEL_API_KEY", "synthetic-example-model")
	t.Setenv("TRACKER_API_KEY", "synthetic-example-tracker")
	cfg.Intake.ProjectID, cfg.Intake.CreatedSince = 17, "2026-01-02T00:00:00Z"
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for i := range cfg.Roles {
		for j := range cfg.Roles[i].Processes {
			p := &cfg.Roles[i].Processes[j]
			p.Command = []string{binary, "-test.run=^TestExampleRoleHelper$"}
			p.Env = map[string]string{"EXAMPLE_TEST_ACTION": cfg.Roles[i].Name}
		}
	}
	root := t.TempDir()
	var mu sync.Mutex
	var comments []any
	stored, posts, catalogs, selections, routes := 0, 0, 0, 0, 0
	answered := false
	add := func(user int, content string) map[string]any {
		stored++
		row := map[string]any{"id": stored, "issueId": 42, "projectId": 17, "content": content, "createdUser": map[string]any{"id": user}}
		comments = append(comments, row)
		return row
	}
	want := []string{"elicit", "ask_requester", "elicit", "investigate", "design", "implement", "review", "deliver", "verify", "draft_report", "review_report", "post_report", "confirm_report", "done"}
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Host == "tracker.example.invalid" {
			switch r.Method + " " + r.URL.Path {
			case "GET /api/v2/issues":
				return selectionReply(r, 200, []any{watchedIssue(42, exampleRequest, "2026-01-03T00:00:00Z")}), nil
			case "GET /api/v2/issues/EXAMPLE-42/comments":
				// The requester replies only once the question has actually been
				// asked and the queue has recorded where the comments stood.
				if _, err := os.Stat(filepath.Join(root, "jobs", "42", "question.json")); err == nil && !answered {
					answered = true
					add(55, exampleAnswer)
				}
				return selectionReply(r, 200, append([]any{}, comments...)), nil
			case "POST /api/v2/issues/EXAMPLE-42/comments":
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
				if routes >= len(want) {
					return nil, fmt.Errorf("unexpected extra route")
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
	deadline := time.Now().Add(30 * time.Second)
	for {
		state, err := loadWatchState(root, 42)
		if err == nil && state.Done {
			break
		}
		if time.Now().After(deadline) {
			finish()
			t.Fatalf("the answered request did not reach completion: step=%s waiting=%t pending=%v error=%v\n%s", state.Step, state.Waiting, state.Pending, err, log.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	finish()
	state, err := loadWatchState(root, 42)
	if err != nil || state.Waiting || state.Pending != nil {
		t.Fatal("bad completion state", err)
	}
	actual, err := os.ReadFile(filepath.Join(root, "jobs", "42", "workspace", "release", "greeting.txt"))
	if err != nil || string(actual) != exampleArtifact {
		t.Fatal("actual delivery missing", err)
	}
	// The open point went back to the requester at once: the entrance ran
	// first, and the very next action asked them, with nothing investigated
	// or built in between.
	if len(state.History) < 2 || state.History[0].Role != "elicit" || state.History[1].Role != "ask_requester" {
		t.Fatalf("the open point was not put to the requester first: %+v", state.History)
	}
	answers, workingModels := 0, 0
	for _, result := range state.History {
		if result.Error != "" {
			t.Fatalf("fixture role failed: %+v", result)
		}
		if result.Model != "" {
			workingModels++
		}
		if result.Speaker == "requester" {
			answers++
			if result.Role != "ask_requester" || result.Output != exampleAnswer {
				t.Fatalf("the requester's own words did not reach the history unchanged: %+v", result)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(root, "jobs", "42", "answer-2.json")); err != nil {
		t.Fatal("the answered question was not kept", err)
	}
	mu.Lock()
	defer mu.Unlock()
	questions := 0
	for _, row := range comments {
		if row.(map[string]any)["content"] == exampleQuestion {
			questions++
		}
	}
	if answers != 1 || questions != 1 || posts != 2 || len(comments) != 3 {
		t.Fatalf("answers=%d questions=%d posts=%d comments=%d", answers, questions, posts, len(comments))
	}
	if routes != len(want) || catalogs != selections || selections != routes+workingModels {
		t.Fatalf("routes=%d selections=%d catalogs=%d working=%d", routes, selections, catalogs, workingModels)
	}
	t.Logf("one question asked, one answer carried on, %d actions, actual artifact and one stored/read-back report", routes-1)
}
