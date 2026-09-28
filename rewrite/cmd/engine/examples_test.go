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
			if process.TrackerAccess == "comment" && role.Name != "post_report" && role.Name != "stop_report" {
				t.Fatal("posting granted outside posting role")
			}
			if slices.Contains([]string{"review", "review_report", "post_report", "confirm_report", "stop_report"}, role.Name) && slices.Contains(process.Command, "--write") {
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
		if err := cfg.ModelSelection.validateGateway(); err != nil {
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
	switch action {
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
		wantReport, wantRows := exampleReport, 1
		if action == "stop_report" {
			wantReport, wantRows = exampleStoppedReport, 2
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
		rows, err := b.Comments(context.Background(), issue, 0)
		if err != nil || len(rows) != wantRows {
			t.Fatal("stored comment missing", err)
		}
		var row struct{ Content string }
		if json.Unmarshal(rows[len(rows)-1], &row) != nil || row.Content != wantReport {
			t.Fatal("stored comment differs from actual report")
		}
		fmt.Print(row.Content)
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
	want := []string{"investigate", "design", "implement", "review", "deliver", "verify", "draft_report", "review_report", "post_report", "confirm_report", "done"}
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
	t.Logf("10 actions, 2 independent review groups, %d fresh selections, actual artifact and one stored/read-back comment", selections)
}
