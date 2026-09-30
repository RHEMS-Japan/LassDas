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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"ticket-runner/internal/chain"
)

func stopComment(issue, user int64, body string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"id": 701, "issueId": issue, "projectId": 17,
		"createdUser": map[string]any{"id": user}, "content": body, "unknown": "keep this native metadata"})
	return raw
}

func TestStopRecognizesOnlyAuthorizedNativeInstructions(t *testing.T) {
	issue := sourceIssue{ID: 51, ProjectID: 17}
	issue.Creator.ID = 55
	for _, test := range []struct {
		name string
		user int64
		body string
		want bool
	}{
		{"requester", 55, "停止", true},
		{"reason after first line", 55, "\r\n  停止  \r\nReason in ordinary prose", true},
		{"operator", 77, "停止", true},
		{"other member", 88, "停止", false},
		{"no author", 0, "停止", false},
		{"quotation", 55, "> 停止", false},
		{"code example", 55, "```\n停止\n```", false},
		{"later line", 55, "Here is an example:\n停止", false},
		{"not an instruction", 55, "停止しないでください", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := stopComment(51, test.user, test.body)
			got, err := stopInstruction([]json.RawMessage{raw}, issue, []int64{77})
			if err != nil || (got != nil) != test.want || (test.want && !bytes.Equal(got, raw)) {
				t.Fatalf("stop=%s error=%v", got, err)
			}
		})
	}
	for _, raw := range []json.RawMessage{stopComment(52, 55, "停止"), []byte(`{"id":701,"projectId":99,"issueId":51,"content":"停止"}`), []byte(`{"id":"broken"}`)} {
		if _, err := stopInstruction([]json.RawMessage{raw}, issue, nil); err == nil {
			t.Fatal("unreadable or wrong-issue controls treated as a successful read")
		}
	}
	directory := t.TempDir()
	if err := writeRuntimeFile(filepath.Join(directory, "stop-request.json"), stopComment(52, 55, "停止")); err != nil {
		t.Fatal(err)
	}
	if stopped, err := savedStop(directory, issue, nil); err == nil || stopped {
		t.Fatal("damaged saved instruction did not hold the work")
	}
}

func startStopQueue(t *testing.T, cfg config, root string, interval time.Duration, log io.Writer) func() {
	t.Helper()
	jobs := filepath.Join(root, "jobs")
	if err := os.MkdirAll(jobs, 0700); err != nil {
		t.Fatal(err)
	}
	since, _ := time.Parse(time.RFC3339, cfg.Intake.CreatedSince)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- pollRequests(ctx, cfg, jobs, since, interval, cfg.Intake.MaxRunning, &serialLog{writer: log})
	}()
	var once sync.Once
	finish := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Errorf("queue exit: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Error("queue did not reap its known test children")
			}
		})
	}
	t.Cleanup(finish)
	return finish
}

func waitTestPID(t *testing.T, path string) int {
	t.Helper()
	pid := 0
	waitFor(t, func() bool {
		data, _ := os.ReadFile(path)
		pid, _ = strconv.Atoi(string(data))
		return pid > 0
	})
	return pid
}

func TestStopCancelsEveryRunningRoleWhileDiscoveryIsStuckAndSurvivesRestart(t *testing.T) {
	for _, role := range []string{"implement", "review", "deliver", "report", "waiting-router"} {
		t.Run(role, func(t *testing.T) {
			cfg := watchConfiguration(t)
			cfg.Roles[0].Name = role
			cfg.Roles[0].Processes[0].Command = []string{"/bin/sh", "-c", `cat > received.txt; printf '%s' "$$" > child-pid; exec sleep 60`}
			root := t.TempDir()
			var requested, restarted atomic.Bool
			var scans, readsAfterStop, modelCalls atomic.Int64
			stuck := make(chan struct{})
			var signal sync.Once
			useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Host == "watch-tracker.example" {
					if strings.HasSuffix(r.URL.Path, "/comments") {
						if restarted.Load() {
							return selectionReply(r, 200, []any{}), nil
						}
						if requested.Load() {
							readsAfterStop.Add(1)
							return selectionReply(r, 200, []json.RawMessage{stopComment(51, 55, "停止\nDo not continue this request.")}), nil
						}
						return selectionReply(r, 200, []json.RawMessage{stopComment(51, 88, "停止")}), nil
					}
					if scans.Add(1) == 1 {
						return selectionReply(r, 200, []any{watchedIssue(51, "Original conditions", "2026-01-03T00:00:00Z")}), nil
					}
					signal.Do(func() { close(stuck) })
					<-r.Context().Done()
					return nil, r.Context().Err()
				}
				modelCalls.Add(1)
				if role == "waiting-router" {
					<-r.Context().Done()
					return nil, r.Context().Err()
				}
				return selectionReply(r, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": role}}}), nil
			})
			var log bytes.Buffer
			finish := startStopQueue(t, cfg, root, 40*time.Millisecond, &log)
			pid := 0
			if role == "waiting-router" {
				waitFor(t, func() bool { return modelCalls.Load() == 1 })
			} else {
				pid = waitTestPID(t, filepath.Join(root, "jobs", "51", "workspace", "child-pid"))
			}
			select {
			case <-stuck:
			case <-time.After(time.Second):
				t.Fatal("did not establish a stuck discovery call")
			}
			requestedAt := time.Now()
			requested.Store(true)
			stopPath := filepath.Join(root, "jobs", "51", "stop-request.json")
			waitFor(t, func() bool { _, err := os.Stat(stopPath); return err == nil })
			if readsAfterStop.Load() != 1 {
				t.Fatal("authorized stop was not applied in its first read tick")
			}
			if pid > 0 && !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
				t.Fatal("known work child survived the stop")
			}
			t.Logf("role=%s first-stop-read=%d poll=40ms observed-cancel-and-save=%s", role, readsAfterStop.Load(), time.Since(requestedAt))
			state, err := loadWatchState(root, 51)
			if err != nil || state.Done {
				t.Fatalf("stop was lost or falsely marked as completion: %#v %v", state, err)
			}
			finish()
			if !strings.Contains(log.String(), "earlier external effects have not been undone") {
				t.Fatal("stop log falsely omitted the possibility of prior external effects")
			}
			raw, _ := os.ReadFile(stopPath)
			if !bytes.Equal(raw, stopComment(51, 55, "停止\nDo not continue this request.")) {
				t.Fatal("native stop instruction was rewritten")
			}
			restarted.Store(true)
			finishAgain := startStopQueue(t, cfg, root, 20*time.Millisecond, io.Discard)
			// Wait for a real resumed discovery request; local scheduling runs
			// while that request remains blocked.
			waitFor(t, func() bool { return scans.Load() >= 3 })
			time.Sleep(60 * time.Millisecond)
			finishAgain()
			if modelCalls.Load() != 1 {
				t.Fatal("stopped request was restarted after remote comments disappeared")
			}
		})
	}
}

func TestStopQueuedRequestDoesNotNeedAnExecutionSlot(t *testing.T) {
	cfg := watchConfiguration(t)
	cfg.Intake.MaxRunning = 1
	root := t.TempDir()
	var models atomic.Int64
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "watch-tracker.example" {
			if strings.HasSuffix(r.URL.Path, "/comments") {
				return selectionReply(r, 200, []json.RawMessage{stopComment(51, 55, "停止")}), nil
			}
			return selectionReply(r, 200, []any{}), nil
		}
		models.Add(1)
		return nil, errors.New("queued stopped work must never reach a model")
	})
	slots := newTurnstile(1)
	slots.slots <- struct{}{} // A different request owns the only execution slot.
	issue := sourceIssue{ID: 51, ProjectID: 17, Key: "EXAMPLE-51"}
	issue.Creator.ID = 55
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := runWatchedRequest(ctx, cfg, issue, root, "unused-config", "unused-request", 40*time.Millisecond, slots, io.Discard); err != nil {
		t.Fatal(err)
	}
	if stopped, err := savedStop(root, issue, nil); err != nil || !stopped || models.Load() != 0 || len(slots.slots) != 1 {
		t.Fatalf("queued stop consumed a work slot: stopped=%v error=%v models=%d", stopped, err, models.Load())
	}
}

func TestStopQueuedRequestIsObservedWhileAnotherRequestOwnsTheOnlySlot(t *testing.T) {
	cfg := watchConfiguration(t)
	cfg.Intake.MaxRunning = 1
	cfg.Roles[0].Processes[0].Command = []string{"/bin/sh", "-c", `cat > received.txt; printf '%s' "$$" > child-pid; exec sleep 60`}
	root := t.TempDir()
	var requested, models atomic.Int64
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "watch-tracker.example" {
			if strings.HasSuffix(r.URL.Path, "/comments") {
				id := requested.Load()
				if id > 0 && strings.Contains(r.URL.Path, fmt.Sprintf("EXAMPLE-%d/", id)) {
					return selectionReply(r, 200, []json.RawMessage{stopComment(id, 55, "停止")}), nil
				}
				return selectionReply(r, 200, []any{}), nil
			}
			return selectionReply(r, 200, []any{watchedIssue(51, "one", "2026-01-03T00:00:00Z"), watchedIssue(52, "two", "2026-01-03T00:00:00Z")}), nil
		}
		models.Add(1)
		return selectionReply(r, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": "implement"}}}), nil
	})
	finish := startStopQueue(t, cfg, root, 30*time.Millisecond, io.Discard)
	working, pid := 0, 0
	waitFor(t, func() bool {
		for _, id := range []int{51, 52} {
			data, _ := os.ReadFile(filepath.Join(root, "jobs", fmt.Sprint(id), "workspace", "child-pid"))
			if value, _ := strconv.Atoi(string(data)); value > 0 {
				working, pid = id, value
				return true
			}
		}
		return false
	})
	waiting := 51 + 52 - working
	requested.Store(int64(waiting))
	waitFor(t, func() bool {
		_, err := os.Stat(filepath.Join(root, "jobs", fmt.Sprint(waiting), "stop-request.json"))
		return err == nil
	})
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("stopping a queued request cancelled another request: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "jobs", fmt.Sprint(waiting), "workspace", "received.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("queued stopped request executed work")
	}
	finish()
	if models.Load() != 1 {
		t.Fatal("queued request reached a model while execution capacity was occupied")
	}
}

func TestStopReadOutagePausesAndRecoversPendingWorkInsteadOfEndingIt(t *testing.T) {
	for _, unresponsive := range []bool{false, true} {
		t.Run(fmt.Sprintf("unresponsive=%v", unresponsive), func(t *testing.T) {
			cfg := watchConfiguration(t)
			cfg.Roles[0].Processes[0].Command = []string{"/bin/sh", "-c", `if test -f started-once; then printf 'Recovered original work\n'; else cat > received.txt; touch started-once; printf '%s' "$$" > child-pid; exec sleep 60; fi`}
			root := t.TempDir()
			var unavailable atomic.Bool
			var badReads, models atomic.Int64
			useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Host == "watch-tracker.example" {
					if strings.HasSuffix(r.URL.Path, "/comments") {
						if unavailable.Load() {
							badReads.Add(1)
							if unresponsive {
								<-r.Context().Done()
								return nil, fmt.Errorf("control channel offline: %w", r.Context().Err())
							}
							return catalogReply(r, 503, "control channel offline synthetic-watch-key"), nil
						}
						return selectionReply(r, 200, []any{}), nil
					}
					return selectionReply(r, 200, []any{watchedIssue(51, "Original conditions", "2026-01-03T00:00:00Z")}), nil
				}
				models.Add(1)
				var input struct{ State chain.State }
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					return nil, err
				}
				if !strings.Contains(input.State.Request, "Original conditions") {
					t.Error("control outage lost original request")
				}
				choice := "implement"
				if len(input.State.History) == 3 && strings.Contains(input.State.History[2].Output, "Recovered original work") {
					choice = "done"
					if input.State.History[0].Speaker != "worker" || input.State.History[0].Error != context.Canceled.Error() || input.State.History[1].Speaker != "runtime" || !strings.Contains(input.State.History[1].Error, "may have taken effect") {
						t.Error("control outage lost returned cancellation or warning about interrupted effects")
					}
				}
				return selectionReply(r, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": choice}}}), nil
			})
			var log bytes.Buffer
			finish := startStopQueue(t, cfg, root, 30*time.Millisecond, &log)
			pid := waitTestPID(t, filepath.Join(root, "jobs", "51", "workspace", "child-pid"))
			unavailable.Store(true)
			waitFor(t, func() bool { return badReads.Load() >= 2 && errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) })
			state, err := loadWatchState(root, 51)
			if err != nil || state.Done || state.Pending == nil || len(state.History) != 1 || state.History[0].Error != context.Canceled.Error() || models.Load() != 1 {
				t.Fatalf("control outage lost pending work: %#v %v models=%d", state, err, models.Load())
			}
			if _, err := os.Stat(filepath.Join(root, "jobs", "51", "stop-request.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("an outage was recorded as a user's stop")
			}
			unavailable.Store(false)
			waitFor(t, func() bool { state, err := loadWatchState(root, 51); return err == nil && state.Done })
			finish()
			if models.Load() != 3 {
				t.Fatalf("control outage repeated recovered work: calls=%d", models.Load())
			}
			if !strings.Contains(log.String(), "control channel offline") || strings.Contains(log.String(), "synthetic-watch-key") {
				t.Fatal("control failure cause was lost or leaked a credential")
			}
		})
	}
}

func TestStopKeepsObservedInstructionInMemoryWhenSavingFails(t *testing.T) {
	cfg := watchConfiguration(t)
	directory := t.TempDir()
	path := filepath.Join(directory, "stop-request.json")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	var reads atomic.Int64
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		if reads.Add(1) == 1 {
			return selectionReply(r, 200, []json.RawMessage{stopComment(51, 55, "停止")}), nil
		}
		return selectionReply(r, 200, []any{}), nil
	})
	issue := sourceIssue{ID: 51, ProjectID: 17, Key: "EXAMPLE-51"}
	issue.Creator.ID = 55
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	failedSave := make(chan struct{}, 1)
	log := stopLogFunc(func(p []byte) (int, error) {
		if strings.Contains(string(p), "waiting to retain the original stop instruction") {
			select {
			case failedSave <- struct{}{}:
			default:
			}
		}
		return len(p), nil
	})
	go func() {
		result <- runWatchedRequest(ctx, cfg, issue, directory, "unused", "unused", 30*time.Millisecond, newTurnstile(1), log)
	}()
	select {
	case <-failedSave:
	case <-time.After(time.Second):
		t.Fatal("did not actually observe a failed stop-instruction save")
	}
	if err := os.Remove(path); err != nil { // Only this test's own empty obstruction.
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("observed stop was lost while waiting for storage")
	}
	if reads.Load() != 1 {
		t.Fatal("re-read replaced the stop already observed")
	}
	if stopped, err := savedStop(directory, issue, nil); err != nil || !stopped {
		t.Fatalf("did not retain the original stop: %v %v", stopped, err)
	}
}

type stopLogFunc func([]byte) (int, error)

func (f stopLogFunc) Write(p []byte) (int, error) { return f(p) }

func TestStopUnreadableControlNeverLaunchesWork(t *testing.T) {
	for _, raw := range []string{`[{"id":701,"issueId":52,"projectId":17,"content":"停止","createdUser":{"id":55}}]`, `[{"id":701,"issueId":51,"projectId":17,"content":42}]`} {
		t.Run(fmt.Sprint(len(raw)), func(t *testing.T) {
			cfg := watchConfiguration(t)
			useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "watch-tracker.example" {
					t.Error("unreadable controls launched a model")
				}
				return catalogReply(r, 200, raw), nil
			})
			issue := sourceIssue{ID: 51, ProjectID: 17, Key: "EXAMPLE-51"}
			issue.Creator.ID = 55
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
			defer cancel()
			var log bytes.Buffer
			if err := runWatchedRequest(ctx, cfg, issue, t.TempDir(), "unused", "unused", 20*time.Millisecond, newTurnstile(1), &log); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
			if strings.Contains(log.String(), "starting accepted request") || !strings.Contains(log.String(), "stop comments could not be read") {
				t.Fatalf("unreadable controls did not hold work: %s", &log)
			}
		})
	}
}

func TestStopMissingRequesterIdentityHoldsWorkUnlessAnOperatorIsConfigured(t *testing.T) {
	cfg := watchConfiguration(t)
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		return selectionReply(r, 200, []json.RawMessage{stopComment(51, 77, "停止")}), nil
	})
	issue := sourceIssue{ID: 51, ProjectID: 17, Key: "EXAMPLE-51"}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	var log bytes.Buffer
	if err := runWatchedRequest(ctx, cfg, issue, t.TempDir(), "unused", "unused", 20*time.Millisecond, newTurnstile(1), &log); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "requester identity is unavailable") || strings.Contains(log.String(), "starting accepted request") {
		t.Fatalf("unknown requester started work: %s", &log)
	}
	cfg.Intake.StopUserIDs = []int64{77}
	directory := t.TempDir()
	if err := runWatchedRequest(context.Background(), cfg, issue, directory, "unused", "unused", 20*time.Millisecond, newTurnstile(1), io.Discard); err != nil {
		t.Fatal(err)
	}
	if stopped, err := savedStop(directory, issue, []int64{77}); err != nil || !stopped {
		t.Fatal("explicitly configured operator could not stop work")
	}
}
