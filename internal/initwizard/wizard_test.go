package initwizard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/localrun"
	runtimeconfig "automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/worker"
)

func wizardFixture(t *testing.T) (*State, Secrets) {
	t.Helper()
	s := &State{Version: 1, Project: "sample-cli", Repository: "example/cli", RepositoryID: 1, DefaultBranch: "main", Branch: "develop", BaseSHA: strings.Repeat("a", 40), EngineRepository: "example/engine", EngineRepositoryID: 2, EngineSHA: strings.Repeat("b", 40), Image: "registry.example.com/engine@sha256:" + strings.Repeat("c", 64), DockerContext: "desktop-linux", BuildRecord: "https://example.com/build/1", Pins: map[string]string{"worker": strings.Repeat("d", 64), "controller": strings.Repeat("e", 64)}, Models: map[string]worker.ModelEndpoint{}, BaseURL: openRouterBaseURL, Completed: map[string]string{}, Checks: map[string]json.RawMessage{}, BoardPort: 9200, AutomationRunID: "run_20260908_" + strings.Repeat("a", 24)}
	s.Mode = worker.ModeConfig{ID: "cli-change", AllowedFilePrefixes: []string{"main.go", "README.md"}, ForbiddenCandidateText: []string{"LassDas"}, MaxFiles: 8, MaxFileBytes: 393216, MaxTotalBytes: 1048576, MaxChangedLines: 3000, MaxChangedBytes: 196608, VerifyWorkingDirectory: ".", InstallCommand: []string{"go", "mod", "download"}, VerifyCommands: [][]string{{"go", "test", "./..."}}}
	s.Tracker = runtimeconfig.TrackerConfig{Origin: "https://example.backlog.com", SpaceKey: "example", ProjectID: 1, ProjectKey: "EXAMPLE", AllowedCreatorID: 7, AllowedActivityType: 1, RequiredCategoryID: 9}
	secrets := Secrets{"TARGET_GITHUB_TOKEN": "artificial-delivery-key", "BACKLOG_API_KEY": "artificial-bot-key", "LASSDAS_BOARD_USER": "operator", "LASSDAS_BOARD_PASS": "artificial-board-password", "LASSDAS_INTAKE_TARGET_KEY": "artificial-intake-key"}
	for index, role := range append(append([]string{}, modelRoles...), "design-review-a", "design-review-b") {
		endpoint := worker.ModelEndpoint{ID: role, Vendor: fmt.Sprintf("vendor-%d", index), Model: role + "-model", APIKeyEnv: keyName(role), BaseURL: s.BaseURL, MaxOutputTokens: 4096}
		if strings.Contains(role, "review-") || role == "readiness-checker" {
			endpoint.Lens = "Find concrete correctness and acceptance failures."
		}
		s.Models[role] = endpoint
		secrets[keyName(role)] = "artificial-" + role + "-key"
	}
	return s, secrets
}

// The worker accepts any config basename, while the delivery controller uses
// its established fixed basename. Exercise the generated path at that boundary
// before a real ticket has to discover a mismatch. A missing draft stops the
// controller immediately after configuration loading, before any GitHub call.
func TestGeneratedConfigAcceptedByController(t *testing.T) {
	s, secrets := wizardFixture(t)
	consumer, runtime, _, err := Generate(s, secrets)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := writeConfigs(dir, consumer, runtime); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config", filepath.Base(runtime.ConsumerConfigPath))
	cmd := exec.Command("go", "run", "../../cmd/controller", "baseline",
		"--config", configPath, "--draft", filepath.Join(dir, "missing-draft.json"),
		"--out", filepath.Join(dir, "baseline.json"))
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "controller: ticket_artifact_invalid") {
		t.Fatalf("generated configuration did not reach the controller's draft check: %v\n%s", err, output)
	}
}

func TestGeneratedHermesLaunchUsesWorkingCopyAndPreservesPrompt(t *testing.T) {
	s, secrets := wizardFixture(t)
	config, _, _, err := Generate(s, secrets)
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "working copy ' with $syntax")
	home := filepath.Join(root, "home")
	bin := filepath.Join(root, "bin")
	for _, dir := range []string{workspace, home, bin} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	const readme = "working-copy contents\n"
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte(readme), 0600); err != nil {
		t.Fatal(err)
	}
	// Exercise the generated command boundary without contacting a model. Hermes
	// one-shot tools resolve relative paths through TERMINAL_CWD, not HOME.
	const fakeHermes = "#!/bin/sh\nprintf '%s\\000' \"$TERMINAL_CWD\" \"$@\"\ncat \"$TERMINAL_CWD/README.md\"\n"
	if err := os.WriteFile(filepath.Join(bin, "hermes"), []byte(fakeHermes), 0700); err != nil {
		t.Fatal(err)
	}
	prompt := "Apply the design.\n\"quotes\" 'quotes'; $(touch prompt-executed) `touch prompt-executed`\n"
	agent := config.Agents.Applier
	args := append(append([]string{}, agent.Args...), prompt)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	cmd := exec.Command(agent.Command, args...)
	cmd.Dir = workspace
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TERMINAL_CWD=" + home}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("generated Hermes launch failed: %v\n%s", err, output)
	}
	want := strings.Join([]string{workspace, "--profile", agent.Profile, "-z", prompt, readme}, "\x00")
	if string(output) != want {
		t.Fatalf("working copy or positional arguments changed:\ngot %q\nwant %q", output, want)
	}
	if _, err := os.Stat(filepath.Join(workspace, "prompt-executed")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prompt was interpreted as shell commands: %v", err)
	}
}

func TestGenerateUsesExistingValidatorsAndDistinctDirectProfileKeys(t *testing.T) {
	for _, separate := range []bool{false, true} {
		s, secrets := wizardFixture(t)
		s.SeparateDesignReviews = separate
		config, runtime, env, err := Generate(s, secrets)
		if err != nil {
			t.Fatal(err)
		}
		if config.Consumers[0].EffectiveKind() != "cli" || config.Consumers[0].Delivery != worker.DeliverPullRequest || runtime.Orchestration != "cards" || config.Consumers[0].Design.Default != "on" || len(config.Consumers[0].Design.TriggerWords) != 0 || config.Agents.Applier == nil {
			t.Fatal("generated path does not pass design -> applier -> PR")
		}
		if config.Models.Implementer.APIKeyEnv != "LASSDAS_INTAKE_TARGET_KEY" || config.Agents.Implementer.SecretEnv["LASSDAS_IMPLEMENTER_KEY"] != "LASSDAS_IMPLEMENTER_KEY" {
			t.Fatal("target derivation shares the implementation identity")
		}
		if env["HERMES_KANBAN_BOARD"] != runtime.HermesBoard || len(config.Agents.ReviewerAgents) != 2 {
			t.Fatal("runtime/profile identity mismatch")
		}
		if env["LASSDAS_BOARD_AUTH"] != "local" || env["LASSDAS_BOARD_USER"] != "" || env["LASSDAS_BOARD_PASS"] != "" {
			t.Fatal("local init must generate a board without authentication credentials")
		}
		if separate != (len(config.Models.DesignReviewers) == 2) || separate != (len(config.Agents.DesignReviewerAgents) == 2) {
			t.Fatal("partial design review binding")
		}
		dir := t.TempDir()
		if err := writeConfigs(dir, config, runtime); err != nil {
			t.Fatal(err)
		}
		// Only the test copy points at host paths. Generated JSON remains a
		// container configuration, as loaded by worker check-runtime there.
		runtime.ConsumerConfigPath = filepath.Join(dir, "config", "m1-consumer.json")
		raw, _ := marshal(runtime)
		path := filepath.Join(dir, "runtime-test.json")
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := runtimeconfig.Load(path); err != nil {
			t.Fatal(err)
		}
		for _, file := range []string{"m1-consumer.json", "runtime.json"} {
			raw, _ := os.ReadFile(filepath.Join(dir, "config", file))
			for _, value := range secrets {
				if strings.Contains(string(raw), value) {
					t.Fatalf("credential in %s", file)
				}
			}
		}
		secrets["LASSDAS_APPLIER_KEY"] = secrets["LASSDAS_IMPLEMENTER_KEY"]
		if _, _, _, err := Generate(s, secrets); err == nil {
			t.Fatal("duplicate role key accepted")
		}
	}
}

type fakeUI struct {
	answers       map[string]string
	approve       bool
	messages      []string
	questions     []string
	confirmations []string
}

func (u *fakeUI) Ask(id, _, fallback string, _ bool) (string, error) {
	u.questions = append(u.questions, id)
	if answer, ok := u.answers[id]; ok {
		return answer, nil
	}
	return fallback, nil
}
func (u *fakeUI) Confirm(label string) (bool, error) {
	u.confirmations = append(u.confirmations, label)
	return u.approve, nil
}
func (u *fakeUI) Info(value string) { u.messages = append(u.messages, value) }

type fakeProcess struct{ commands [][]string }

func (*fakeProcess) LookPath(name string) (string, error) { return "/bin/" + name, nil }
func (p *fakeProcess) Run(_ context.Context, _ string, args []string, _ string) ([]byte, error) {
	p.commands = append(p.commands, append([]string{}, args...))
	return nil, nil
}

type fakeRuntime struct{ starts, stops int }

func (r *fakeRuntime) Start(context.Context, *State, string) (json.RawMessage, error) {
	r.starts++
	return json.RawMessage(`{"state":"ready","agent_uid":2001}`), nil
}
func (r *fakeRuntime) Stop(context.Context, *State, string) error { r.stops++; return nil }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(status int, data any) *http.Response {
	raw, _ := json.Marshal(data)
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(string(raw))), Header: make(http.Header)}
}

func TestStartResumeKeepsRunningInstanceAndLocalrunAcceptsGeneratedEnvironment(t *testing.T) {
	s, secrets := wizardFixture(t)
	dir := t.TempDir()
	runtime := &fakeRuntime{}
	w := Wizard{UI: &fakeUI{approve: true}, Runtime: runtime, Process: &fakeProcess{}}
	if err := w.start(context.Background(), s, secrets, dir); err != nil {
		t.Fatal(err)
	}
	s.Completed["runtime"] = "done"
	if err := w.start(context.Background(), s, secrets, dir); err != nil {
		t.Fatal(err)
	}
	if runtime.starts != 2 || runtime.stops != 1 {
		t.Fatalf("resume restarted the instance: %+v", runtime)
	}
	if secrets["LASSDAS_BOARD_USER"] != "" || secrets["LASSDAS_BOARD_PASS"] != "" || secrets["LASSDAS_BOARD_AUTH"] != "local" {
		t.Fatal("resume would restore obsolete board credentials")
	}
	manager := localrun.Manager{Docker: noContainerDocker{}}
	status, err := manager.Start(context.Background(), localrun.Instance{ID: s.Project, Dir: dir, Image: s.Image, EngineSHA: s.EngineSHA, DockerContext: s.DockerContext, BoardPort: s.BoardPort})
	// The injected Docker deliberately stops after host validation. Its exact
	// error proves generated env/config reached the Docker boundary.
	if err == nil || !strings.Contains(err.Error(), "docker container failed") || status.State != "" {
		t.Fatalf("localrun rejected generated input before Docker: %+v %v", status, err)
	}
	secrets["LASSDAS_APPLIER_KEY"] = "artificial-rotated-key"
	if err := w.start(context.Background(), s, secrets, dir); err != nil {
		t.Fatal(err)
	}
	if runtime.stops != 2 {
		t.Fatal("credential change was applied without stopping")
	}
}

func TestStartMigratesLegacyConfigAndKeepsRuntimeState(t *testing.T) {
	s, secrets := wizardFixture(t)
	s.Completed["runtime"] = "done"
	s.Smoke = json.RawMessage(`{"correlation":"retained-request","issue_id":42}`)
	consumer, runtime, _, err := Generate(s, secrets)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := writeConfigs(dir, consumer, runtime); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(dir, "config", "consumer.json")
	if err := os.Rename(filepath.Join(dir, "config", "m1-consumer.json"), legacy); err != nil {
		t.Fatal(err)
	}
	runtime.ConsumerConfigPath = "/etc/lassdas/config/consumer.json"
	raw, _ := marshal(runtime)
	if err := atomicWrite(filepath.Join(dir, "config", "runtime.json"), raw, 0644); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, s, secrets); err != nil {
		t.Fatal(err)
	}
	resident := &fakeRuntime{}
	w := Wizard{UI: &fakeUI{approve: true}, Runtime: resident, Process: &fakeProcess{}}
	if err := w.start(context.Background(), s, secrets, dir); err != nil {
		t.Fatal(err)
	}
	if resident.stops != 1 || resident.starts != 1 {
		t.Fatal("legacy config migration must stop the old instance before restarting")
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatal("legacy config left in the strict two-file mount")
	}
	loaded, keys, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	var smoke map[string]any
	if json.Unmarshal(loaded.Smoke, &smoke) != nil || smoke["correlation"] != "retained-request" || smoke["issue_id"] != float64(42) || keys["TARGET_GITHUB_TOKEN"] != secrets["TARGET_GITHUB_TOKEN"] {
		t.Fatal("migration changed the request or saved credential")
	}
	manager := localrun.Manager{Docker: noContainerDocker{}}
	_, err = manager.Start(context.Background(), localrun.Instance{ID: s.Project, Dir: dir, Image: s.Image, EngineSHA: s.EngineSHA, DockerContext: s.DockerContext, BoardPort: s.BoardPort})
	if err == nil || !strings.Contains(err.Error(), "docker container failed") {
		t.Fatalf("migrated config rejected before the Docker boundary: %v", err)
	}
}

type noContainerDocker struct{}

func (noContainerDocker) Run(context.Context, []string) ([]byte, error) {
	return nil, errors.New("intentional offline boundary")
}

func TestSaveAndRedoPreserveCorrelationWithoutPuttingKeysInJournal(t *testing.T) {
	s, secrets := wizardFixture(t)
	s.Smoke = json.RawMessage(`{"correlation":"same-request","issue_id":42}`)
	for _, stage := range stages {
		s.Completed[stage] = "done"
	}
	dir := t.TempDir()
	if err := Save(dir, s, secrets); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "init.json"))
	for _, value := range secrets {
		if strings.Contains(string(raw), value) {
			t.Fatal("credential in journal")
		}
	}
	loaded, keys, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := loaded.Redo("models"); err != nil {
		t.Fatal(err)
	}
	var smoke map[string]any
	if err := json.Unmarshal(loaded.Smoke, &smoke); err != nil {
		t.Fatal(err)
	}
	if loaded.Completed["tracker"] == "" || loaded.Completed["runtime"] != "" || smoke["issue_id"] != float64(42) || smoke["correlation"] != "same-request" || keys["TARGET_GITHUB_TOKEN"] != secrets["TARGET_GITHUB_TOKEN"] {
		t.Fatal("redo lost external identity or saved keys")
	}
	info, _ := os.Stat(filepath.Join(dir, "runtime.env"))
	if info.Mode().Perm() != 0600 {
		t.Fatal("credential permissions")
	}
	if err := os.Chmod(filepath.Join(dir, "runtime.env"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(dir); err == nil {
		t.Fatal("readable credential file accepted")
	}
}

func TestControlStateRemainsAvailableWhenCredentialsAreBroken(t *testing.T) {
	for _, brokenMode := range []bool{false, true} {
		t.Run(fmt.Sprint(brokenMode), func(t *testing.T) {
			s, secrets := wizardFixture(t)
			dir := t.TempDir()
			if err := Save(dir, s, secrets); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "runtime.env")
			if brokenMode {
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, []byte("invalid env line\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := Load(dir); err == nil {
				t.Fatal("init accepted broken credentials")
			}
			control, err := LoadState(dir)
			if err != nil || control.Project != s.Project || control.DockerContext != s.DockerContext || control.Image != s.Image {
				t.Fatalf("cannot locate the owned runtime for stop: state=%+v err=%v", control, err)
			}
		})
	}
}

func TestModelPreflightChecksEveryActualIdentityAndRejectsInvalidResponse(t *testing.T) {
	s, secrets := wizardFixture(t)
	seen := map[string]bool{}
	fail := false
	w := Wizard{UI: &fakeUI{approve: true}, API: API{HTTP: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		seen[req.Header.Get("Authorization")] = true
		if req.URL.String() != s.BaseURL+"/chat/completions" {
			t.Fatal("model used a different gateway")
		}
		answer := `{"status":"ready"}`
		if fail {
			answer = `{"status":"not-ready"}`
		}
		return response(200, map[string]any{"id": "chatcmpl-fixture", "choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": answer}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}}), nil
	})}}}
	if err := w.modelPreflight(context.Background(), s, secrets); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 8 {
		t.Fatalf("checked %d identities, want 8", len(seen))
	}
	fail = true
	if err := w.modelPreflight(context.Background(), s, secrets); err == nil {
		t.Fatal("arbitrary successful HTTP response counted as preflight")
	}
	for _, value := range secrets {
		if strings.Contains(strings.Join(w.UI.(*fakeUI).messages, "\n"), value) {
			t.Fatal("key leaked to UI")
		}
	}
}

func TestModelsResumeRepairsSavedDuplicateKeys(t *testing.T) {
	for _, approve := range []bool{false, true} {
		t.Run(fmt.Sprint(approve), func(t *testing.T) {
			s, secrets := wizardFixture(t)
			s.Completed["models"] = "previous-attempt"
			replacements := map[string]string{}
			for name, value := range secrets {
				replacements[name] = value
			}
			secrets["LASSDAS_APPLIER_KEY"] = secrets["LASSDAS_IMPLEMENTER_KEY"]
			dir := t.TempDir()
			if err := Save(dir, s, secrets); err != nil {
				t.Fatal(err)
			}
			s, secrets, err := Load(dir)
			if err != nil {
				t.Fatal(err)
			}
			ui := &fakeUI{approve: approve, answers: replacements}
			calls := 0
			w := Wizard{UI: ui, API: API{HTTP: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				return response(200, map[string]any{"id": "chatcmpl-fixture", "choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": `{"status":"ready"}`}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}}), nil
			})}}}
			err = w.models(context.Background(), s, secrets)
			if approve {
				if err != nil || calls != 8 || ValidateModelKeys(s, secrets) != nil || len(ui.questions) != 8 {
					t.Fatalf("saved duplicate keys could not be repaired: err=%v calls=%d questions=%v", err, calls, ui.questions)
				}
			} else if err == nil || calls != 0 || len(ui.questions) != 0 {
				t.Fatal("declined repair changed keys or called models")
			}
			for _, value := range secrets {
				if strings.Contains(strings.Join(ui.messages, "\n"), value) {
					t.Fatal("credential leaked during repair")
				}
			}
		})
	}
}

func TestTrackerCreateLostResponseReconcilesOnNextRun(t *testing.T) {
	s, secrets := wizardFixture(t)
	s.Category = "自動処理"
	s.StatusNames = [4]string{"処理中", "回答待ち", "納品済み", "要確認"}
	created, posts, saves := false, 0, 0
	w := Wizard{UI: &fakeUI{approve: true}, API: API{HTTP: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Query().Get("apiKey") != secrets["BACKLOG_API_KEY"] {
			t.Fatal("wrong tracker identity")
		}
		switch req.URL.Path {
		case "/api/v2/users/myself":
			return response(200, NamedID{ID: 8}), nil
		case "/api/v2/projects/EXAMPLE":
			return response(200, map[string]any{"id": 1, "projectKey": "EXAMPLE"}), nil
		case "/api/v2/projects/1/users":
			return response(200, []NamedID{{ID: 7}, {ID: 8}}), nil
		case "/api/v2/projects/1/categories":
			if req.Method == "POST" {
				posts++
				created = true
				return nil, errors.New("lost response containing artificial-bot-key")
			}
			if created {
				return response(200, []NamedID{{ID: 9, Name: s.Category}}), nil
			}
			return response(200, []NamedID{}), nil
		case "/api/v2/projects/1/statuses":
			var states []NamedID
			for i, name := range s.StatusNames {
				states = append(states, NamedID{ID: int64(i + 10), Name: name})
			}
			return response(200, states), nil
		}
		t.Fatalf("unexpected tracker operation: %s %s", req.Method, req.URL.Path)
		return nil, nil
	})}}}
	save := func() error { saves++; return nil }
	if err := w.tracker(context.Background(), s, secrets, save); err == nil || strings.Contains(err.Error(), secrets["BACKLOG_API_KEY"]) {
		t.Fatalf("lost response was not safely surfaced: %v", err)
	}
	if err := w.tracker(context.Background(), s, secrets, save); err != nil {
		t.Fatal(err)
	}
	if posts != 1 || saves == 0 || s.Tracker.RequiredCategoryID != 9 || s.Tracker.AllowedCreatorID != 7 {
		t.Fatal("resume duplicated category or changed allowed creator")
	}
}

func TestTrackerRequesterKeyRequiresConfirmationBeforeUse(t *testing.T) {
	for _, tc := range []struct {
		name             string
		identity         int64
		creator          int64
		approve          bool
		wantOK           bool
		wantConfirmation bool
	}{
		{"separate account", 8, 7, false, true, false},
		{"requester approved", 7, 7, true, true, true},
		{"requester declined", 7, 7, false, false, true},
		{"nonmember", 8, 99, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, secrets := wizardFixture(t)
			s.Tracker.AllowedCreatorID = tc.creator
			s.Category = "自動処理"
			s.StatusNames = [4]string{"処理中", "回答待ち", "納品済み", "要確認"}
			key := secrets["BACKLOG_API_KEY"]
			ui := &fakeUI{approve: tc.approve}
			saves := 0
			w := Wizard{UI: ui, API: API{HTTP: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Method != "GET" || req.URL.Query().Get("apiKey") != key {
					t.Fatal("unexpected mutation or identity change")
				}
				switch req.URL.Path {
				case "/api/v2/users/myself":
					return response(200, NamedID{ID: tc.identity}), nil
				case "/api/v2/projects/EXAMPLE":
					return response(200, map[string]any{"id": 1, "projectKey": "EXAMPLE"}), nil
				case "/api/v2/projects/1/users":
					return response(200, []NamedID{{ID: 7}, {ID: 8}}), nil
				case "/api/v2/projects/1/categories":
					if tc.wantConfirmation && len(ui.confirmations) != 1 {
						t.Fatal("continued before confirming requester identity")
					}
					return response(200, []NamedID{{ID: 9, Name: s.Category}}), nil
				case "/api/v2/projects/1/statuses":
					var states []NamedID
					for i, name := range s.StatusNames {
						states = append(states, NamedID{ID: int64(10 + i), Name: name})
					}
					return response(200, states), nil
				}
				t.Fatalf("unexpected path: %s", req.URL.Path)
				return nil, nil
			})}}}
			err := w.tracker(context.Background(), s, secrets, func() error { saves++; return nil })
			if (err == nil) != tc.wantOK || (saves > 0) != tc.wantOK {
				t.Fatalf("success=%v saves=%d, want success=%v", err == nil, saves, tc.wantOK)
			}
			if (len(ui.confirmations) == 1) != tc.wantConfirmation || s.Tracker.AllowedCreatorID != tc.creator {
				t.Fatal("confirmation or fixed requester changed")
			}
			if tc.wantConfirmation {
				for _, required := range []string{"ID: 7", "本人名義", "区別できません", "runtime.env", "0600"} {
					if !strings.Contains(ui.confirmations[0], required) {
						t.Fatalf("confirmation missing %q", required)
					}
				}
				if strings.Contains(ui.confirmations[0], key) {
					t.Fatal("confirmation exposed key")
				}
			}
			// Run saves partial input after an error. Exercise that same save:
			// declining runtime use must not persist the requester credential.
			dir := t.TempDir()
			if err := Save(dir, s, secrets); err != nil {
				t.Fatal(err)
			}
			_, saved, err := Load(dir)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantConfirmation && !tc.approve {
				if saved["BACKLOG_API_KEY"] != "" {
					t.Fatal("declined key was persisted")
				}
			} else if saved["BACKLOG_API_KEY"] != key {
				t.Fatal("runtime key changed")
			}
		})
	}
}

func TestModelsUseOpenRouterWithoutEndpointInput(t *testing.T) {
	s, secrets := wizardFixture(t)
	s.BaseURL = ""
	ui := &fakeUI{approve: true}
	calls := 0
	w := Wizard{UI: ui, API: API{HTTP: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.String() != "https://openrouter.ai/api/v1/chat/completions" {
			t.Fatal("model credential sent outside OpenRouter")
		}
		return response(200, map[string]any{"id": "chatcmpl-fixture", "choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": `{"status":"ready"}`}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}}), nil
	})}}}
	if err := w.models(context.Background(), s, secrets); err != nil {
		t.Fatal(err)
	}
	if calls != 8 || s.BaseURL != "https://openrouter.ai/api/v1" {
		t.Fatal("OpenRouter preflight incomplete")
	}
	for _, id := range ui.questions {
		if id == "model-url" {
			t.Fatal("URL input is still exposed")
		}
	}
	config, _, env, err := Generate(s, secrets)
	if err != nil {
		t.Fatal(err)
	}
	if env["LASSDAS_GATEWAY_BASE_URL"] != s.BaseURL || config.Models.Implementer.BaseURL != s.BaseURL {
		t.Fatal("direct and profile endpoints differ")
	}
}

func TestModelsDoNotRedirectExistingProviderCredentials(t *testing.T) {
	s, secrets := wizardFixture(t)
	s.BaseURL = "https://models.example.com/v1"
	ui := &fakeUI{approve: true}
	w := Wizard{UI: ui, API: API{HTTP: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatal("saved credentials sent to a different provider")
		return nil, nil
	})}}}
	before, _ := marshal(s)
	beforeKeys, _ := marshal(secrets)
	if err := w.models(context.Background(), s, secrets); err == nil {
		t.Fatal("saved provider silently changed")
	}
	after, _ := marshal(s)
	afterKeys, _ := marshal(secrets)
	if string(before) != string(after) || string(beforeKeys) != string(afterKeys) || len(ui.questions) != 0 {
		t.Fatal("saved state or credentials changed before provider check")
	}
}

func TestModelKeysDefaultToOneAndSeparateKeysAreOptional(t *testing.T) {
	for _, separate := range []bool{false, true} {
		t.Run(fmt.Sprint(separate), func(t *testing.T) {
			s, secrets := wizardFixture(t)
			s.ModelKeyMode = ""
			answers := map[string]string{}
			if separate {
				answers["separate-model-keys"] = "yes"
			}
			names := []string{"LASSDAS_INTAKE_TARGET_KEY"}
			for _, role := range allRoles(s) {
				names = append(names, keyName(role))
			}
			for _, name := range names {
				answers[name] = secrets[name]
				delete(secrets, name)
			}
			ui := &fakeUI{approve: true, answers: answers}
			calls := 0
			identities := map[string]bool{}
			w := Wizard{UI: ui, API: API{HTTP: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				identities[req.Header.Get("Authorization")] = true
				return response(200, map[string]any{"id": "chatcmpl-fixture", "choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": `{"status":"ready"}`}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}}), nil
			})}}}
			if err := w.models(context.Background(), s, secrets); err != nil {
				t.Fatal(err)
			}
			keyInputs := 0
			for _, id := range ui.questions {
				if strings.HasSuffix(id, "_KEY") {
					keyInputs++
				}
			}
			want := 1
			mode := modelKeysShared
			if separate {
				want = 8
				mode = modelKeysSeparate
			}
			if calls != 8 || len(identities) != want || keyInputs != want || s.ModelKeyMode != mode {
				t.Fatalf("calls=%d identities=%d key inputs=%d mode=%s", calls, len(identities), keyInputs, s.ModelKeyMode)
			}
			config, _, env, err := Generate(s, secrets)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := marshal(config)
			for _, name := range names {
				if env[name] == "" || strings.Contains(string(raw), env[name]) {
					t.Fatal("key missing or leaked into config")
				}
				if !separate && env[name] != secrets["LASSDAS_INTAKE_TARGET_KEY"] {
					t.Fatal("shared key did not reach all roles")
				}
			}
			// Persist the selected mode so a resumed shared setup never asks
			// for eight keys, and a role-key setup never silently collapses.
			dir := t.TempDir()
			if err := Save(dir, s, secrets); err != nil {
				t.Fatal(err)
			}
			loaded, saved, err := Load(dir)
			if err != nil || loaded.ModelKeyMode != mode || ValidateModelKeys(loaded, saved) != nil {
				t.Fatal("key mode did not survive resume")
			}
			loaded.Completed["models"] = "previous"
			resumeUI := &fakeUI{approve: true}
			w.UI = resumeUI
			if err := w.models(context.Background(), loaded, saved); err != nil || len(resumeUI.questions) != 0 || loaded.ModelKeyMode != mode {
				t.Fatal("resume reentered credentials or changed key mode")
			}
			if separate {
				saved[keyName("applier")] = saved[keyName("implementer")]
			} else {
				saved[keyName("applier")] = "artificial-unexpected-key"
			}
			if _, _, _, err := Generate(loaded, saved); err == nil {
				t.Fatal("key mode mismatch accepted")
			}
		})
	}
}
