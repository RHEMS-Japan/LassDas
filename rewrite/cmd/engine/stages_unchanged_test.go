package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"ticket-runner/internal/chain"
)

// What this request asks for is already in the target repository.
const unchangedRequest = "Make src/greeting.txt say Hello 日本語, then post the verified outcome."

// What this one asks for is not, and the work stage below writes nothing.
const neededChangeRequest = "Add src/farewell.txt saying Goodbye 日本語, then post the verified outcome."

const unchangedReport = "できるようになったこと\n依頼の挙動はすでにあり、変更は要りませんでした。何も納品していません。\n"

// What the review command hands its model in place of an empty diff.
const noChangeSentence = "No file was changed. Judge whether the request and the settled requirements are satisfied with the repository exactly as it is; if a change is needed and none was made, that is a blocking defect."

// The model stages and the operator's own build and report checks of the runs
// below, as real child processes. Only the report is written into the
// checkout, after the delivery: anything else would be a change, and these
// runs are about a work stage that changes nothing.
func TestUnchangedStagesRoleHelper(t *testing.T) {
	action := os.Getenv("UNCHANGED_STAGE_ACTION")
	if action == "" {
		return
	}
	prompt, err := io.ReadAll(os.Stdin)
	if err != nil || !bytes.Contains(prompt, []byte(os.Getenv("UNCHANGED_STAGE_REQUEST"))) {
		t.Fatal("lost original request", err)
	}
	// os.Exit below skips deferred work, so the claim is written up front.
	if slices.Contains([]string{"elicit", "work", "report"}, action) {
		fmt.Print(stagesClaim)
	}
	switch action {
	case "elicit", "work", "verify":
		// Whatever the request asks for, nothing is written.
		if text, err := os.ReadFile("src/greeting.txt"); err != nil || string(text) != stagesArtifact {
			t.Fatalf("the prepared checkout is not the target's: %q %v", text, err)
		}
	case "report":
		if err := os.MkdirAll("report", 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile("report/result.md", []byte(unchangedReport), 0600); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(fixtureComments(t), unchangedReport) {
			fixturePost(t, unchangedReport)
		}
		fmt.Print(unchangedReport)
	case "confirm_report":
		reported, err := os.ReadFile("report/result.md")
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(fixtureComments(t), string(reported)) {
			fmt.Print("no stored comment matches the reported text\n")
			os.Exit(1)
		}
	default:
		t.Fatalf("stage %s is not part of this run", action)
	}
	fmt.Print("\nActual fixture operation observed; ordinary prose, no approval object.\n")
	os.Exit(0)
}

// unchangedRun is the shipped ordered run with a work stage that writes
// nothing. The review, the delivery and the check after it are the shipped
// programs, run as real processes on a real checkout against a real Git
// target and a stand-in delivery service; the operator has allowed an ending
// without a change for the delivery.
type unchangedRun struct {
	queue, target, tip, key string
	issue                   int
	finish                  func()
	git                     func(directory string, args ...string) string
	mu                      sync.Mutex
	serviceCalls, reviews   []string
	stored                  []string
	log                     bytes.Buffer
}

// startUnchangedRun starts that run on the given request. answer is the
// reviewing model: it is handed the number of the review it is asked for,
// counted from 1, and writes the model service's reply.
func startUnchangedRun(t *testing.T, issue int, request string, answer func(w http.ResponseWriter, n int)) *unchangedRun {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("the shipped harnesses need Python")
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("the shipped harnesses need Git")
	}
	harness := func(name string) string {
		path, err := filepath.Abs(filepath.Join("../../harnesses", name))
		if err != nil {
			t.Fatal(err)
		}
		return path
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	run := &unchangedRun{issue: issue, key: fmt.Sprintf("EXAMPLE-%d", issue)}
	root := t.TempDir()
	run.git = func(directory string, args ...string) string {
		t.Helper()
		arguments := []string{"-C", directory, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "core.hooksPath=/dev/null"}
		command := exec.Command(gitPath, append(arguments, args...)...)
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0"}
		out, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("fixture Git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	source := filepath.Join(root, "source")
	if err := os.MkdirAll(filepath.Join(source, "src"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "src", "greeting.txt"), []byte(stagesArtifact), 0600); err != nil {
		t.Fatal(err)
	}
	run.git(source, "init", "--initial-branch=master")
	run.git(source, "add", "-A")
	run.git(source, "commit", "-m", "Codex: the greeting already exists")
	run.target = filepath.Join(root, "target.git")
	run.git(root, "clone", "--bare", source, run.target)
	run.tip = run.git(run.target, "rev-parse", "refs/heads/master")

	// The delivery service: anything asked of it is recorded, and an ending
	// without a change must not ask it anything.
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		run.mu.Lock()
		run.serviceCalls = append(run.serviceCalls, r.Method+" "+r.URL.Path)
		run.mu.Unlock()
		http.Error(w, `{"message":"this stand-in opens no pull request"}`, http.StatusNotFound)
	}))
	t.Cleanup(service.Close)
	// The reviewing model keeps what it was handed and answers as told.
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Messages) < 2 {
			http.Error(w, "unreadable review request", http.StatusBadRequest)
			return
		}
		run.mu.Lock()
		run.reviews = append(run.reviews, body.Messages[1].Content)
		n := len(run.reviews)
		run.mu.Unlock()
		answer(w, n)
	}))
	t.Cleanup(model.Close)

	cfg := stagesExample(t)
	t.Setenv("MODEL_API_KEY", "synthetic-example-model")
	t.Setenv("TRACKER_API_KEY", "synthetic-example-tracker")
	t.Setenv("DELIVERY_TEST_TOKEN", "synthetic-delivery-credential")
	cfg.Intake.ProjectID, cfg.Intake.CreatedSince = 17, "2026-01-02T00:00:00Z"
	prepare := []string{python, "-B", harness("git_workspace.py"), "--"}
	delivery := map[string]string{"DELIVERY_REPOSITORY": "owner/project", "DELIVERY_BASE_BRANCH": "master", "DELIVERY_REMOTE_URL": run.target}
	for i := range cfg.Roles {
		for j := range cfg.Roles[i].Processes {
			p := &cfg.Roles[i].Processes[j]
			env := map[string]string{"TASK_REPOSITORY": run.target, "TASK_BRANCH": "master", "PYTHONDONTWRITEBYTECODE": "1"}
			switch cfg.Roles[i].Name {
			case "review":
				// The example's own review settings, pointed at the stand-in model.
				for _, name := range []string{"REVIEW_KEY_ENV", "REVIEW_DIFF_PATHS"} {
					env[name] = p.Env[name]
				}
				env["REVIEW_MODEL_URL"], env["REVIEW_MODEL"] = model.URL+"/v1/chat/completions", "fixture/reviewer"
				env["REVIEW_TEST_COMMANDS"], env["REVIEW_ATTEMPTS"] = `/bin/sh -c "grep -q Hello src/greeting.txt"`, "1"
				p.Command = append(slices.Clone(prepare), python, "-B", harness("adversarial_review.py"))
			case "deliver":
				maps.Copy(env, delivery)
				env["DELIVERY_ALLOWED_PATHS"], env["DELIVERY_API_BASE"] = "src", service.URL
				// The operator's opt-in. The shipped example leaves it off.
				env["DELIVERY_ALLOW_UNCHANGED"] = "1"
				p.Secrets = map[string]string{"GITHUB_TOKEN": "DELIVERY_TEST_TOKEN"}
				p.Receipt = ".git/ticket-engine/delivery.json"
				p.Command = append(slices.Clone(prepare), python, "-B", harness("deliver_git.py"))
			case "verify_merged":
				maps.Copy(env, delivery)
				env["VERIFY_COMMANDS"] = `/bin/sh -c "grep -q Hello src/greeting.txt"`
				p.Command = append(slices.Clone(prepare), python, "-B", harness("verify_merged.py"))
			default:
				env["UNCHANGED_STAGE_ACTION"], env["UNCHANGED_STAGE_REQUEST"] = cfg.Roles[i].Name, request
				p.Command = append(slices.Clone(prepare), binary, "-test.run=^TestUnchangedStagesRoleHelper$")
			}
			p.Env = env
		}
	}

	var comments []any
	catalogs := 0
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		run.mu.Lock()
		defer run.mu.Unlock()
		if r.URL.Host == "tracker.example.invalid" {
			switch r.Method + " " + r.URL.Path {
			case "GET /api/v2/issues":
				return selectionReply(r, 200, []any{watchedIssue(issue, request, "2026-01-03T00:00:00Z")}), nil
			case "GET /api/v2/issues/" + run.key + "/comments":
				return selectionReply(r, 200, append([]any{}, comments...)), nil
			case "POST /api/v2/issues/" + run.key + "/comments":
				if err := r.ParseForm(); err != nil {
					return nil, err
				}
				row := map[string]any{"id": len(comments) + 1, "issueId": issue, "projectId": 17, "content": r.Form.Get("content"), "createdUser": map[string]any{"id": 99}}
				comments, run.stored = append(comments, row), append(run.stored, r.Form.Get("content"))
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
				// The entrance carries straight on; nobody is asked anything.
				return routingSelectionReply(r, chain.Assignment{Role: "work"}), nil
			}
		}
		return nil, fmt.Errorf("unexpected fixture destination %s %s", r.Method, r.URL.Path)
	})

	run.queue = filepath.Join(root, "queue")
	run.finish = startStopQueue(t, cfg, run.queue, 30*time.Millisecond, &run.log)
	return run
}

// verdict writes the structured verdict the review command asks its model for.
func verdict(w http.ResponseWriter, blocking bool, findings string) {
	arguments, _ := json.Marshal(map[string]any{"blocking": blocking, "findings": findings})
	json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{
		"role": "assistant", "tool_calls": []any{map[string]any{"id": "call-1", "type": "function",
			"function": map[string]any{"name": "verdict", "arguments": string(arguments)}}}}}}})
}

// A request whose right outcome is that nothing changes, through the shipped
// ordered run: the work stage changes nothing, the review is told so in plain
// words and does not object, the delivery opens no pull request, and the run
// ends on the read-back report. When the review objects once, the work goes
// back to the work stage and nothing is delivered until it passes.
func TestAnOrderedRunFinishesARequestThatNeedsNoChange(t *testing.T) {
	for _, objections := range []int{0, 1} {
		t.Run(fmt.Sprintf("objections=%d", objections), func(t *testing.T) {
			run := startUnchangedRun(t, 64+objections, unchangedRequest, func(w http.ResponseWriter, n int) {
				if n <= objections {
					verdict(w, true, "the request also asks for a closing line, and none was written")
					return
				}
				verdict(w, false, "")
			})
			deadline := time.Now().Add(120 * time.Second)
			for {
				state, err := loadWatchState(run.queue, run.issue)
				if err == nil && state.Done {
					break
				}
				if time.Now().After(deadline) {
					run.finish()
					t.Fatalf("the run did not finish: step=%s waiting=%t error=%v\n%+v\n%s", state.Step, state.Waiting, err, state.History, run.log.String())
				}
				time.Sleep(20 * time.Millisecond)
			}
			run.finish()
			state, err := loadWatchState(run.queue, run.issue)
			if err != nil || state.Pending != nil || state.Waiting || state.Step != "confirm_report" {
				t.Fatalf("the run ended somewhere other than its last stage: %s %v", state.Step, err)
			}

			// Each launch ends on the runtime's own record, so those records
			// are the order the stages ran in.
			want := []string{"elicit", "work", "verify", "review"}
			for range objections {
				want = append(want, "work", "verify", "review")
			}
			want = append(want, "deliver", "verify_merged", "report", "confirm_report")
			var ran, reviewed []string
			delivered, verified, receipts := "", "", 0
			for _, result := range state.History {
				switch {
				case result.Speaker == "runtime":
					ran = append(ran, result.Role)
					if result.Role == "deliver" && strings.Contains(result.Output, `"unchanged": true`) {
						receipts++
					}
				case result.Role == "review":
					reviewed = append(reviewed, result.Error)
				case result.Role == "deliver":
					delivered = result.Output
				case result.Role == "verify_merged":
					verified = result.Output
				}
			}
			if !slices.Equal(ran, want) {
				t.Fatalf("the stages ran as %v, not %v", ran, want)
			}
			// Only the last review passed; every one before it sent the work back.
			for i, failure := range reviewed {
				if (failure == "") != (i == len(reviewed)-1) {
					t.Fatalf("review %d of %d ended with %q", i+1, len(reviewed), failure)
				}
			}
			if !strings.Contains(delivered, fmt.Sprintf("Nothing was delivered for %s: no file was changed.", run.key)) ||
				!strings.Contains(delivered, "as it is at "+run.tip) || receipts != 1 {
				t.Fatalf("the delivery did not say it ended without a change (receipts read back: %d):\n%s", receipts, delivered)
			}
			if !strings.Contains(verified, "No merge was made") || !strings.Contains(verified, "0 of 1 configured verification commands failed.") {
				t.Fatalf("the check after delivery did not say what it ran:\n%s", verified)
			}
			var receipt map[string]any
			data, err := os.ReadFile(filepath.Join(run.queue, "jobs", fmt.Sprint(run.issue), "workspace", ".git", "ticket-engine", "delivery.json"))
			if err != nil || json.Unmarshal(data, &receipt) != nil || receipt["unchanged"] != true || receipt["base_sha"] != run.tip {
				t.Fatalf("the delivery's receipt is %s %v", data, err)
			}
			if refs := run.git(run.target, "for-each-ref", "--format=%(refname)", "refs/heads"); refs != "refs/heads/master" {
				t.Fatalf("something was pushed to the target: %s", refs)
			}
			if now := run.git(run.target, "rev-parse", "refs/heads/master"); now != run.tip {
				t.Fatal("the integration branch moved although nothing was delivered")
			}
			run.mu.Lock()
			defer run.mu.Unlock()
			if len(run.serviceCalls) != 0 {
				t.Fatalf("the delivery service was asked %v, so a pull request may have been opened", run.serviceCalls)
			}
			if len(run.reviews) != objections+1 {
				t.Fatalf("the model reviewed %d times", len(run.reviews))
			}
			for _, text := range run.reviews {
				if !strings.Contains(text, "Diff of the change:\n"+noChangeSentence) {
					t.Fatalf("the reviewer was not told that no file was changed:\n%s", text)
				}
			}
			if !slices.Equal(run.stored, []string{unchangedReport}) {
				t.Fatalf("the issue holds %q", run.stored)
			}
		})
	}
}

// A request that needs a change, a work stage that writes nothing, and a
// reviewing model that is down or answers without a verdict. Let through,
// the work would end with nothing delivered and nobody's judgement, so the
// review sends it back each time: the run never reaches the delivery, and
// nothing is pushed, opened or reported.
func TestAnOrderedRunDoesNotEndUnchangedWithoutAVerdict(t *testing.T) {
	for i, shape := range []struct {
		name   string
		answer func(w http.ResponseWriter, n int)
	}{
		{"the model service is down", func(w http.ResponseWriter, n int) {
			http.Error(w, "the model service is busy", http.StatusServiceUnavailable)
		}},
		{"the model answers without a verdict", func(w http.ResponseWriter, n int) {
			json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{
				"role": "assistant", "content": "Looks fine to me."}}}})
		}},
	} {
		t.Run(shape.name, func(t *testing.T) {
			run := startUnchangedRun(t, 70+i, neededChangeRequest, shape.answer)
			deadline := time.Now().Add(120 * time.Second)
			for {
				run.mu.Lock()
				asked := len(run.reviews)
				run.mu.Unlock()
				if state, err := loadWatchState(run.queue, run.issue); asked >= 3 || (err == nil && state.Done) {
					break
				}
				if time.Now().After(deadline) {
					run.finish()
					t.Fatalf("the review was asked %d times\n%s", asked, run.log.String())
				}
				time.Sleep(20 * time.Millisecond)
			}
			run.finish()
			state, err := loadWatchState(run.queue, run.issue)
			if err != nil || state.Done {
				t.Fatalf("the run ended without a verdict: step=%s err=%v", state.Step, err)
			}
			sentBack := 0
			for _, result := range state.History {
				switch {
				case slices.Contains([]string{"deliver", "verify_merged", "report", "confirm_report"}, result.Role):
					t.Fatalf("the run went past the review without a verdict: %+v", result)
				case result.Role == "review" && result.Speaker != "runtime" && result.Error == "":
					t.Fatalf("a review without a verdict let the work through: %s", result.Output)
				case result.Role == "review" && strings.HasPrefix(result.Error, "exit status 1"):
					if !strings.Contains(result.Output, "an ending with nothing delivered needs a verdict, so the work goes back this time") {
						t.Fatalf("the review did not say why the work goes back: %s", result.Output)
					}
					sentBack++
				}
			}
			if sentBack < 2 {
				t.Fatalf("only %d reviews ended, each sending the work back", sentBack)
			}
			if _, err := os.Stat(filepath.Join(run.queue, "jobs", fmt.Sprint(run.issue), "workspace", ".git", "ticket-engine", "delivery.json")); !os.IsNotExist(err) {
				t.Fatalf("a delivery receipt exists: %v", err)
			}
			if refs := run.git(run.target, "for-each-ref", "--format=%(refname)", "refs/heads"); refs != "refs/heads/master" {
				t.Fatalf("something was pushed to the target: %s", refs)
			}
			run.mu.Lock()
			defer run.mu.Unlock()
			if len(run.serviceCalls) != 0 || len(run.stored) != 0 {
				t.Fatalf("service calls %v, stored comments %q", run.serviceCalls, run.stored)
			}
		})
	}
}
