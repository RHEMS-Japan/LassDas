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
const unchangedReport = "できるようになったこと\n依頼の挙動はすでにあり、変更は要りませんでした。何も納品していません。\n"

// What the review command hands its model in place of an empty diff.
const noChangeSentence = "No file was changed. Judge whether the request and the settled requirements are satisfied with the repository exactly as it is; if a change is needed and none was made, that is a blocking defect."

// The model stages and the operator's own build and report checks of the run
// below, as real child processes. Only the report is written into the
// checkout, after the delivery: anything else would be a change, and this
// run is about a request that needs none.
func TestUnchangedStagesRoleHelper(t *testing.T) {
	action := os.Getenv("UNCHANGED_STAGE_ACTION")
	if action == "" {
		return
	}
	prompt, err := io.ReadAll(os.Stdin)
	if err != nil || !bytes.Contains(prompt, []byte(unchangedRequest)) {
		t.Fatal("lost original request", err)
	}
	// os.Exit below skips deferred work, so the claim is written up front.
	if slices.Contains([]string{"elicit", "work", "report"}, action) {
		fmt.Print(stagesClaim)
	}
	switch action {
	case "elicit", "work", "verify":
		// The behaviour asked for is there already; nothing is written.
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

// A request whose right outcome is that nothing changes, through the shipped
// ordered run. The review, the delivery and the check after it are the
// shipped programs, run as real processes on a real checkout against a real
// Git target and a stand-in delivery service; the operator has allowed an
// ending without a change for the delivery. The work stage changes nothing,
// the review is told so in plain words, the delivery opens no pull request,
// and the run ends on the read-back report. When the review objects once, the
// work goes back to the work stage and nothing is delivered until it passes.
func TestAnOrderedRunFinishesARequestThatNeedsNoChange(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("the shipped harnesses need Python")
	}
	git, err := exec.LookPath("git")
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
	for _, objections := range []int{0, 1} {
		t.Run(fmt.Sprintf("objections=%d", objections), func(t *testing.T) {
			root := t.TempDir()
			runGit := func(directory string, args ...string) string {
				t.Helper()
				arguments := []string{"-C", directory, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "core.hooksPath=/dev/null"}
				command := exec.Command(git, append(arguments, args...)...)
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
			runGit(source, "init", "--initial-branch=master")
			runGit(source, "add", "-A")
			runGit(source, "commit", "-m", "Codex: the greeting already exists")
			target := filepath.Join(root, "target.git")
			runGit(root, "clone", "--bare", source, target)
			tip := runGit(target, "rev-parse", "refs/heads/master")

			var mu sync.Mutex
			var serviceCalls, reviews []string
			// The delivery service: anything asked of it is recorded, and an
			// ending without a change must not ask it anything.
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				serviceCalls = append(serviceCalls, r.Method+" "+r.URL.Path)
				mu.Unlock()
				http.Error(w, `{"message":"this stand-in opens no pull request"}`, http.StatusNotFound)
			}))
			t.Cleanup(service.Close)
			// The reviewing model: it objects the given number of times, then
			// passes, and keeps what it was handed.
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
				mu.Lock()
				reviews = append(reviews, body.Messages[1].Content)
				blocking := len(reviews) <= objections
				mu.Unlock()
				findings := ""
				if blocking {
					findings = "the request also asks for a closing line, and none was written"
				}
				arguments, _ := json.Marshal(map[string]any{"blocking": blocking, "findings": findings})
				json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{
					"role": "assistant", "tool_calls": []any{map[string]any{"id": "call-1", "type": "function",
						"function": map[string]any{"name": "verdict", "arguments": string(arguments)}}}}}}})
			}))
			t.Cleanup(model.Close)

			cfg := stagesExample(t)
			t.Setenv("MODEL_API_KEY", "synthetic-example-model")
			t.Setenv("TRACKER_API_KEY", "synthetic-example-tracker")
			t.Setenv("DELIVERY_TEST_TOKEN", "synthetic-delivery-credential")
			cfg.Intake.ProjectID, cfg.Intake.CreatedSince = 17, "2026-01-02T00:00:00Z"
			prepare := []string{python, "-B", harness("git_workspace.py"), "--"}
			delivery := map[string]string{"DELIVERY_REPOSITORY": "owner/project", "DELIVERY_BASE_BRANCH": "master", "DELIVERY_REMOTE_URL": target}
			for i := range cfg.Roles {
				for j := range cfg.Roles[i].Processes {
					p := &cfg.Roles[i].Processes[j]
					env := map[string]string{"TASK_REPOSITORY": target, "TASK_BRANCH": "master", "PYTHONDONTWRITEBYTECODE": "1"}
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
						env["UNCHANGED_STAGE_ACTION"] = cfg.Roles[i].Name
						p.Command = append(slices.Clone(prepare), binary, "-test.run=^TestUnchangedStagesRoleHelper$")
					}
					p.Env = env
				}
			}

			issue := 64 + objections
			key := fmt.Sprintf("EXAMPLE-%d", issue)
			var comments []any
			var stored []string
			catalogs := 0
			useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
				mu.Lock()
				defer mu.Unlock()
				if r.URL.Host == "tracker.example.invalid" {
					switch r.Method + " " + r.URL.Path {
					case "GET /api/v2/issues":
						return selectionReply(r, 200, []any{watchedIssue(issue, unchangedRequest, "2026-01-03T00:00:00Z")}), nil
					case "GET /api/v2/issues/" + key + "/comments":
						return selectionReply(r, 200, append([]any{}, comments...)), nil
					case "POST /api/v2/issues/" + key + "/comments":
						if err := r.ParseForm(); err != nil {
							return nil, err
						}
						row := map[string]any{"id": len(comments) + 1, "issueId": issue, "projectId": 17, "content": r.Form.Get("content"), "createdUser": map[string]any{"id": 99}}
						comments, stored = append(comments, row), append(stored, r.Form.Get("content"))
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

			queue := filepath.Join(root, "queue")
			var log bytes.Buffer
			finish := startStopQueue(t, cfg, queue, 30*time.Millisecond, &log)
			deadline := time.Now().Add(120 * time.Second)
			for {
				state, err := loadWatchState(queue, issue)
				if err == nil && state.Done {
					break
				}
				if time.Now().After(deadline) {
					finish()
					t.Fatalf("the run did not finish: step=%s waiting=%t error=%v\n%+v\n%s", state.Step, state.Waiting, err, state.History, log.String())
				}
				time.Sleep(20 * time.Millisecond)
			}
			finish()
			state, err := loadWatchState(queue, issue)
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
			if !strings.Contains(delivered, fmt.Sprintf("Nothing was delivered for %s: no file was changed.", key)) ||
				!strings.Contains(delivered, "as it is at "+tip) || receipts != 1 {
				t.Fatalf("the delivery did not say it ended without a change (receipts read back: %d):\n%s", receipts, delivered)
			}
			if !strings.Contains(verified, "No merge was made") || !strings.Contains(verified, "0 of 1 configured verification commands failed.") {
				t.Fatalf("the check after delivery did not say what it ran:\n%s", verified)
			}
			var receipt map[string]any
			data, err := os.ReadFile(filepath.Join(queue, "jobs", fmt.Sprint(issue), "workspace", ".git", "ticket-engine", "delivery.json"))
			if err != nil || json.Unmarshal(data, &receipt) != nil || receipt["unchanged"] != true || receipt["base_sha"] != tip {
				t.Fatalf("the delivery's receipt is %s %v", data, err)
			}
			if refs := runGit(target, "for-each-ref", "--format=%(refname)", "refs/heads"); refs != "refs/heads/master" {
				t.Fatalf("something was pushed to the target: %s", refs)
			}
			if now := runGit(target, "rev-parse", "refs/heads/master"); now != tip {
				t.Fatal("the integration branch moved although nothing was delivered")
			}
			mu.Lock()
			defer mu.Unlock()
			if len(serviceCalls) != 0 {
				t.Fatalf("the delivery service was asked %v, so a pull request may have been opened", serviceCalls)
			}
			if len(reviews) != objections+1 {
				t.Fatalf("the model reviewed %d times", len(reviews))
			}
			for _, text := range reviews {
				if !strings.Contains(text, "Diff of the change:\n"+noChangeSentence) {
					t.Fatalf("the reviewer was not told that no file was changed:\n%s", text)
				}
			}
			if !slices.Equal(stored, []string{unchangedReport}) {
				t.Fatalf("the issue holds %q", stored)
			}
		})
	}
}
