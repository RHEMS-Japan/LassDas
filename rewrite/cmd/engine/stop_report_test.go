package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"ticket-runner/internal/chain"
	"ticket-runner/internal/tracker"
)

const stoppedProse = "停止指示に従って作業を止めました。本番への納品は確認していません。\n{\"unfamiliar\":true}\n"

// The real subprocess exercises access and recovery, not model judgment.
func TestStoppedReporterHelper(t *testing.T) {
	if os.Getenv("STOP_REPORT_TEST_CHILD") == "" {
		return
	}
	prompt, err := io.ReadAll(os.Stdin)
	if err != nil || !bytes.Contains(prompt, []byte("Original accepted request (stopped):\nOriginal issue: EXAMPLE-51\nTitle: Original title\n\nOriginal conditions")) || !bytes.Contains(prompt, []byte("Do not continue this request.")) {
		t.Fatalf("reporter lost the original request or stop: %v", err)
	}
	if os.Getenv("WATCH_TEST_KEY") != "" {
		t.Fatal("controller key reached stopped reporter")
	}
	if !strings.HasSuffix(os.Getenv("TASK_HOME"), "/homes/1-0") || os.Getenv("TASK_ISSUE") != "EXAMPLE-51" {
		t.Fatal("stopped reporter lost its original role home or issue binding")
	}
	if err := os.WriteFile("stop-report-prompt.txt", prompt, 0600); err != nil {
		t.Fatal(err)
	}
	client, err := tracker.CertificateClient(os.Getenv("TASK_TRACKER_CERT"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	b := tracker.Backlog{BaseURL: os.Getenv("TASK_TRACKER_URL"), KeyEnv: "TASK_TRACKER_KEY", Client: client}
	issue := os.Getenv("TASK_TRACKER_ISSUE")
	rows, err := b.Comments(context.Background(), issue, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range rows {
		var row struct {
			ID      int64
			Content string
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			t.Fatal(err)
		}
		if row.Content == stoppedProse {
			if _, err := b.Comment(context.Background(), issue, row.ID); err != nil {
				t.Fatal(err)
			}
			fmt.Print("Read the existing stop report back; did not repeat the post.\n" + stoppedProse)
			os.Exit(0)
		}
	}
	_, err = b.AddComment(context.Background(), issue, stoppedProse)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Print(stoppedProse)
	os.Exit(0)
}

func stoppedReportConfiguration(t *testing.T) config {
	t.Helper()
	cfg := watchConfiguration(t)
	cfg.Roles[0].Processes[0].Command = []string{"/bin/sh", "-c", `cat > received.txt; printf '%s' "$$" > child-pid; exec sleep 60`}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Roles = append(cfg.Roles, chain.Role{Name: "report", Purpose: "Report actual observations at the assigned issue without performing implementation or delivery.", Processes: []chain.Process{{Name: "reporter", TrackerAccess: "comment", Command: []string{binary, "-test.run=^TestStoppedReporterHelper$"}, Env: map[string]string{"STOP_REPORT_TEST_CHILD": "1"}}}})
	cfg.Workflow = &chain.Workflow{
		Start:   []string{"implement"},
		After:   map[string][]string{"implement": {"report"}, "report": {"done"}},
		Recover: map[string][]string{"implement": {"implement"}, "report": {"report"}},
	}
	// JSON keeps the pre-implementation test executable: an old engine ignores
	// this operator setting and therefore never supplies the promised report.
	raw, _ := json.Marshal(cfg)
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["intake"].(map[string]any)["stop_report_role"] = "report"
	raw, _ = json.Marshal(doc)
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func stopReportState(root string) (chain.State, error) {
	var state chain.State
	raw, err := os.ReadFile(filepath.Join(root, "jobs", "51", "stop-report", "history.json"))
	if err == nil {
		err = json.Unmarshal(raw, &state)
	}
	return state, err
}

func TestStoppedRequestReportsWithoutResumingWorkOrDuplicatingAmbiguousPost(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(fmt.Sprintf("active=%v", active), func(t *testing.T) {
			cfg := stoppedReportConfiguration(t)
			selector := testSelector(t)
			cfg.ModelSelection = &selector
			cfg.Roles[1].Processes[0].ModelEnv = "STOP_REPORT_MODEL"
			root := t.TempDir()
			var stop atomic.Bool
			stop.Store(!active)
			var posts, reportRoutes, normalRoutes, readbacks atomic.Int64
			var catalogs atomic.Int64
			comment := func() map[string]any {
				return map[string]any{"id": 702, "issueId": 51, "projectId": 17, "createdUser": map[string]any{"id": 999}, "content": stoppedProse}
			}
			useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Host == "openrouter.ai" {
					n := catalogs.Add(1)
					if r.Header.Get("Cache-Control") != "no-cache" {
						t.Error("stopped reporter reused model catalog")
					}
					return selectionReply(r, 200, map[string]any{"data": []any{selectionModel(fmt.Sprintf("maker-one/stopped-%d", n))}}), nil
				}
				if r.URL.Host == "selection.example" {
					return selectionReply(r, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": fmt.Sprintf("maker-one/stopped-%d", catalogs.Load())}}}), nil
				}
				if r.URL.Host == "watch-tracker.example" {
					switch r.Method + " " + r.URL.Path {
					case "GET /api/v2/issues":
						return selectionReply(r, 200, []any{watchedIssue(51, "Original conditions", "2026-01-03T00:00:00Z")}), nil
					case "GET /api/v2/issues/EXAMPLE-51/comments":
						rows := []any{}
						if stop.Load() {
							rows = append(rows, stopComment(51, 55, "停止\nDo not continue this request."))
						}
						if posts.Load() > 0 {
							rows = append(rows, comment())
						}
						return selectionReply(r, 200, rows), nil
					case "POST /api/v2/issues/EXAMPLE-51/comments":
						if err := r.ParseForm(); err != nil || r.PostForm.Get("content") != stoppedProse {
							t.Error("report text changed")
						}
						posts.Add(1)
						return catalogReply(r, 503, "saved stop report but receipt unavailable"), nil
					case "GET /api/v2/issues/EXAMPLE-51/comments/702":
						readbacks.Add(1)
						return selectionReply(r, 200, comment()), nil
					default:
						return nil, fmt.Errorf("unexpected tracker action %s %s", r.Method, r.URL.Path)
					}
				}
				var input struct {
					State     chain.State
					Questions map[string]struct{ Criteria map[string]string }
				}
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					return nil, err
				}
				choice := "implement"
				if len(input.Questions["next"].Criteria) == 2 {
					reportRoutes.Add(1)
					choices := input.Questions["next"].Criteria
					if _, ok := choices["report"]; !ok {
						t.Error("report-only routing omitted reporter")
					}
					if _, ok := choices["implement"]; ok {
						t.Error("stopped work remained executable")
					}
					choice = "report"
					if len(input.State.History) == 1 && !strings.Contains(input.State.History[0].Error, "saved stop report but receipt unavailable") {
						t.Error("ambiguous post reason lost")
					}
					if len(input.State.History) == 2 && strings.Contains(input.State.History[1].Output, "Read the existing stop report back") {
						choice = "done"
					}
				} else {
					normalRoutes.Add(1)
				}
				return selectionReply(r, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": choice}}}), nil
			})
			finish := startStopQueue(t, cfg, root, 30*time.Millisecond, io.Discard)
			pid := 0
			if active {
				pid = waitTestPID(t, filepath.Join(root, "jobs", "51", "workspace", "child-pid"))
				stop.Store(true)
			}
			waitFor(t, func() bool { s, e := stopReportState(root); return e == nil && s.Done })
			finish()
			report, err := stopReportState(root)
			if err != nil || len(report.History) != 2 || report.History[0].Model != "maker-one/stopped-1" || report.History[1].Model != "maker-one/stopped-2" || catalogs.Load() != 2 {
				t.Fatalf("report retries did not select afresh: %#v %v catalogs=%d", report, err, catalogs.Load())
			}
			if posts.Load() != 1 || reportRoutes.Load() != 3 || readbacks.Load() != 1 {
				t.Fatalf("report missing/repeated: posts=%d routes=%d readbacks=%d", posts.Load(), reportRoutes.Load(), readbacks.Load())
			}
			wantRoutes := int64(0)
			if active {
				wantRoutes = 1
			}
			if normalRoutes.Load() != wantRoutes {
				t.Fatal("stopped implementation resumed")
			}
			if pid > 0 && !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
				t.Fatal("stopped child still alive")
			}
			original, err := loadWatchState(root, 51)
			if err != nil || original.Done || !strings.Contains(original.Request, "Original conditions") {
				t.Fatalf("stop report replaced original state: %#v %v", original, err)
			}
			if active && (len(original.History) != 1 || original.Pending == nil) {
				t.Fatal("reporting changed original interrupted history")
			}
			before, _ := os.ReadFile(filepath.Join(root, "jobs", "51", "run", "history.json"))
			stop.Store(false) // Even removing the remote instruction cannot resume work.
			finishAgain := startStopQueue(t, cfg, root, 15*time.Millisecond, io.Discard)
			time.Sleep(80 * time.Millisecond)
			finishAgain()
			after, _ := os.ReadFile(filepath.Join(root, "jobs", "51", "run", "history.json"))
			if !bytes.Equal(before, after) || posts.Load() != 1 || reportRoutes.Load() != 3 || normalRoutes.Load() != wantRoutes {
				t.Fatal("restart repeated stopped or reported work")
			}
		})
	}
}

func TestStoppedReportResumesAfterControllerCancellationWithoutRepeatingPost(t *testing.T) {
	cfg := stoppedReportConfiguration(t)
	root := t.TempDir()
	dir := filepath.Join(root, "jobs", "51")
	if err := os.MkdirAll(filepath.Join(dir, "workspace"), 0700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(watchedIssue(51, "Original conditions", "2026-01-03T00:00:00Z"))
	if err := writeRuntimeFile(filepath.Join(dir, "issue.json"), raw); err != nil {
		t.Fatal(err)
	}
	if err := writeRuntimeFile(filepath.Join(dir, "stop-request.json"), stopComment(51, 55, "停止\nDo not continue this request.")); err != nil {
		t.Fatal(err)
	}
	request, _ := tracker.RequestText(raw)
	store, err := chain.Open(filepath.Join(dir, "run"), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(chain.State{Request: request, Pending: &chain.Assignment{Role: "deliver"}, History: []chain.Result{{Role: "deliver", Output: "External operation may already exist.", Error: "context canceled"}}}); err != nil {
		t.Fatal(err)
	}
	store.Close()
	before, _ := os.ReadFile(filepath.Join(dir, "run", "history.json"))
	var posts, routes, readbacks atomic.Int64
	var sawInterrupted atomic.Bool
	comment := map[string]any{"id": 702, "issueId": 51, "projectId": 17, "createdUser": map[string]any{"id": 999}, "content": stoppedProse}
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "watch-tracker.example" {
			switch r.Method + " " + r.URL.Path {
			case "GET /api/v2/issues":
				return catalogReply(r, 503, "discovery offline"), nil
			case "GET /api/v2/issues/EXAMPLE-51/comments":
				rows := []any{}
				if posts.Load() > 0 {
					rows = append(rows, comment)
				}
				return selectionReply(r, 200, rows), nil
			case "POST /api/v2/issues/EXAMPLE-51/comments":
				if err := r.ParseForm(); err != nil || r.PostForm.Get("content") != stoppedProse {
					t.Error("bad stop report")
				}
				posts.Add(1)
				<-r.Context().Done()
				return nil, r.Context().Err()
			case "GET /api/v2/issues/EXAMPLE-51/comments/702":
				readbacks.Add(1)
				return selectionReply(r, 200, comment), nil
			default:
				return nil, fmt.Errorf("unexpected tracker operation %s %s", r.Method, r.URL.Path)
			}
		}
		routes.Add(1)
		var input struct {
			State     chain.State
			Questions map[string]struct{ Criteria map[string]string }
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			return nil, err
		}
		if len(input.Questions["next"].Criteria) != 2 || !strings.Contains(input.State.Request, "External operation may already exist.") {
			t.Error("restart broadened work or lost observations")
		}
		choice := "report"
		if len(input.State.History) >= 2 {
			if input.State.History[0].Error == "" || input.State.History[1].Speaker != "runtime" || !strings.Contains(input.State.History[1].Error, "may have taken effect") {
				t.Error("lost returned error or ambiguous action warning")
			}
			sawInterrupted.Store(true)
		}
		if len(input.State.History) == 3 && strings.Contains(input.State.History[2].Output, "Read the existing stop report back") {
			choice = "done"
		}
		return selectionReply(r, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": choice}}}), nil
	})
	finish := startStopQueue(t, cfg, root, 20*time.Millisecond, io.Discard)
	waitFor(t, func() bool { return posts.Load() == 1 })
	finish()
	stopped, err := stopReportState(root)
	if err != nil || stopped.Done || stopped.Pending == nil || stopped.Pending.Role != "report" || len(stopped.History) != 1 || !strings.Contains(stopped.History[0].Error, "context canceled") {
		t.Fatalf("stopped report history lost: %#v %v", stopped, err)
	}
	finishAgain := startStopQueue(t, cfg, root, 20*time.Millisecond, io.Discard)
	waitFor(t, func() bool { s, e := stopReportState(root); return e == nil && s.Done })
	finishAgain()
	after, _ := os.ReadFile(filepath.Join(dir, "run", "history.json"))
	if !bytes.Equal(before, after) || posts.Load() != 1 || routes.Load() != 3 || readbacks.Load() != 1 || !sawInterrupted.Load() {
		t.Fatalf("restart changed work/repeated post/lost warning: posts=%d routes=%d readbacks=%d warning=%v", posts.Load(), routes.Load(), readbacks.Load(), sawInterrupted.Load())
	}
}

func TestStopReporterMustReferToAnExistingRoleBeforeIntake(t *testing.T) {
	for _, name := range []string{"unknown", "done"} {
		cfg := stoppedReportConfiguration(t)
		cfg.Intake.StopReportRole = name
		var calls atomic.Int64
		useCatalogTransport(t, func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return nil, errors.New("must not reach intake")
		})
		err := watchRequests(context.Background(), cfg, t.TempDir(), io.Discard)
		if err == nil || !strings.Contains(err.Error(), "stop_report_role") || calls.Load() != 0 {
			t.Fatalf("invalid stop reporter accepted: %v calls=%d", err, calls.Load())
		}
	}
}

func TestStoppedReporterCannotDispatchAnotherRole(t *testing.T) {
	cfg := stoppedReportConfiguration(t)
	root := t.TempDir()
	var reportRouting atomic.Int64
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "watch-tracker.example" {
			if strings.HasSuffix(r.URL.Path, "/comments") {
				return selectionReply(r, 200, []json.RawMessage{stopComment(51, 55, "停止\nDo not continue this request.")}), nil
			}
			return selectionReply(r, 200, []any{watchedIssue(51, "Original conditions", "2026-01-03T00:00:00Z")}), nil
		}
		reportRouting.Add(1)
		return selectionReply(r, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": "implement"}}}), nil
	})
	var log bytes.Buffer
	var rejected atomic.Bool
	writer := stopLogFunc(func(p []byte) (int, error) {
		n, err := log.Write(p)
		if bytes.Contains(p, []byte("unconfigured role")) {
			rejected.Store(true)
		}
		return n, err
	})
	finish := startStopQueue(t, cfg, root, 20*time.Millisecond, writer)
	waitFor(t, func() bool {
		state, err := stopReportState(root)
		return rejected.Load() && err == nil && len(state.History) == 1
	})
	finish()
	if !strings.Contains(log.String(), "unconfigured role") {
		t.Fatal("routing into stopped work was not rejected")
	}
	if _, err := os.Stat(filepath.Join(root, "jobs", "51", "workspace", "child-pid")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a stopped implementation was launched")
	}
	state, err := stopReportState(root)
	if err != nil || state.Done || state.Pending != nil || len(state.History) != 1 {
		t.Fatalf("invalid dispatch claimed a report: %#v %v", state, err)
	}
	observed := state.History[0]
	if observed.Role != "router" || observed.Speaker != "runtime" || observed.Output != "" || !strings.Contains(observed.Error, "unconfigured role") {
		t.Fatalf("rejected dispatch was not kept solely as a runtime failure: %+v", observed)
	}
}

func TestStoppedReportRequiresReadableSavedStopAndHistory(t *testing.T) {
	cfg := stoppedReportConfiguration(t)
	issue := sourceIssue{ID: 51, ProjectID: 17, Key: "EXAMPLE-51"}
	issue.Creator.ID = 55
	for _, raw := range []json.RawMessage{[]byte(`broken`), stopComment(52, 55, "停止"), stopComment(51, 88, "停止")} {
		dir := t.TempDir()
		if err := writeRuntimeFile(filepath.Join(dir, "stop-request.json"), raw); err != nil {
			t.Fatal(err)
		}
		if err := reportStoppedRequest(context.Background(), cfg, issue, dir, make(chan struct{}, 1), io.Discard); err == nil {
			t.Fatal("reported an unauthorized or unreadable stop")
		}
		if _, err := os.Stat(filepath.Join(dir, "stop-report")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("bad stop reached reporting chain")
		}
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "stop-report"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeRuntimeFile(filepath.Join(dir, "stop-report", "history.json"), []byte(`{`)); err != nil {
		t.Fatal(err)
	}
	if done, err := stoppedReportDone(dir); err == nil || done {
		t.Fatal("damaged reporting state treated as completed or fresh")
	}
}

func TestTheStoppedReporterReadsTheCollapsedRecord(t *testing.T) {
	var history []chain.Result
	for i := 0; i < 40; i++ {
		history = append(history, chain.Result{Role: "elicit", Speaker: "elicit-process", Model: "m", Error: "fork/exec elicit: no such file or directory"})
		history = append(history, chain.Result{Role: "elicit", Speaker: "runtime", Output: "Process elicit-process did not exit 0."})
	}
	raw, err := stopObservations(chain.State{Request: "original", History: history, Pending: &chain.Assignment{Role: "elicit"}})
	if err != nil {
		t.Fatal(err)
	}
	var observed chain.State
	if err := json.Unmarshal(raw, &observed); err != nil {
		t.Fatal(err)
	}
	if len(observed.History) > 4 || observed.Request != "original" || observed.Pending == nil {
		t.Fatalf("the record was handed over uncollapsed: %d entries, request %q, pending %v", len(observed.History), observed.Request, observed.Pending)
	}
	if !strings.Contains(string(raw), "repeated 40 times in a row") {
		t.Fatalf("the repetition is not said: %s", raw)
	}
	// A runtime note that says something new inside the repetition, such as
	// a receipt read back from a process that still had an effect, is kept.
	withReceipt := append([]chain.Result{}, history[:20]...)
	withReceipt = append(withReceipt, chain.Result{Role: "elicit", Speaker: "elicit-process", Model: "m", Error: "fork/exec elicit: no such file or directory"},
		chain.Result{Role: "elicit", Speaker: "runtime", Output: "Process elicit-process did not exit 0.\nReceipt: pull request 12 opened at the destination"})
	withReceipt = append(withReceipt, history[20:]...)
	raw, err = stopObservations(chain.State{Request: "original", History: withReceipt})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "pull request 12 opened") {
		t.Fatalf("a receipt inside the repetition was dropped: %s", raw)
	}
	if err := json.Unmarshal(raw, &observed); err != nil || len(observed.History) > 6 {
		t.Fatalf("the record with a receipt was not collapsed: %v, %d entries", err, len(observed.History))
	}
	// Each record's text is bounded the way a role's prompt bounds it: the
	// opening of what was written, the end of what failed, at character
	// boundaries; the whole record stays on disk.
	raw, err = stopObservations(chain.State{History: []chain.Result{{Role: "implement", Speaker: "implement-process",
		Output: strings.Repeat("報", 5000), Diagnostics: strings.Repeat("診", 5000) + "END", Error: "exit status 1", Instruction: strings.Repeat("指", 2000)}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &observed); err != nil {
		t.Fatal(err)
	}
	record := observed.History[0]
	if !strings.HasPrefix(record.Output, strings.Repeat("報", 4000)) || !strings.Contains(record.Output, "1000 more characters") || utf8.RuneCountInString(record.Output) > 4100 {
		t.Fatalf("the output was not cut to its opening: %d characters", utf8.RuneCountInString(record.Output))
	}
	if !strings.HasSuffix(record.Diagnostics, "END") || !strings.HasPrefix(record.Diagnostics, "[1003 earlier characters") || !utf8.ValidString(record.Diagnostics) {
		t.Fatalf("the diagnostics were not cut to their end: %q", record.Diagnostics[:60])
	}
	if record.Error != "exit status 1" || utf8.RuneCountInString(record.Instruction) > 1100 {
		t.Fatalf("a short error was altered or the instruction was not cut: %q, %d", record.Error, utf8.RuneCountInString(record.Instruction))
	}
	// A long failure repeated many times keeps its count after the cut.
	var long []chain.Result
	for i := 0; i < 7; i++ {
		long = append(long, chain.Result{Role: "verify", Speaker: "verify-process", Error: "exit status 1\n" + strings.Repeat("診断", 3000) + "END"})
	}
	raw, err = stopObservations(chain.State{History: long})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &observed); err != nil || len(observed.History) != 1 {
		t.Fatalf("a long repeated failure was not collapsed: %v, %d entries", err, len(observed.History))
	}
	if e := observed.History[0].Error; !strings.HasPrefix(e, "(this failure repeated 7 times in a row; this is the latest)\n[") || !strings.HasSuffix(e, "END") || utf8.RuneCountInString(e) > 4200 {
		t.Fatalf("the count was cut away with the failure's head, or the cut is wrong: %d characters, starts %q", utf8.RuneCountInString(e), e[:80])
	}
}
