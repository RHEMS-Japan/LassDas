package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ticket-runner/internal/chain"
	"ticket-runner/internal/tracker"
)

func startupArguments(t *testing.T) []string {
	t.Helper()
	t.Setenv("STARTUP_TEST_KEY", "synthetic-startup-key")
	dir := t.TempDir()
	var cfg config
	cfg.Router.Mode = "jev"
	cfg.Router.Decision = chain.Jev{URL: "https://startup-model.example/decisions", Model: "fixture", KeyEnv: "STARTUP_TEST_KEY"}
	cfg.Backlog = tracker.Backlog{BaseURL: "https://startup-tracker.example/api/v2", KeyEnv: "STARTUP_TEST_KEY"}
	cfg.Roles = []chain.Role{{Name: "implement", Processes: []chain.Process{{Name: "worker", Command: []string{"/bin/cat"}}}}}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "operator.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return []string{"--config", path, "--issue", "EXAMPLE-1", "--run-dir", filepath.Join(dir, "run")}
}

func TestStartupIntakeOutageDoesNotEndRequestAndReachesRealProcess(t *testing.T) {
	args := startupArguments(t)
	const original = "Original 日本語 request, with all requirements and `literal` words."
	reads, routes := 0, 0
	useCatalogTransport(t, func(request *http.Request) (*http.Response, error) {
		if request.URL.Host == "startup-tracker.example" {
			reads++
			if reads == 1 {
				return catalogReply(request, 503, "temporary intake outage: synthetic-startup-key"), nil
			}
			return selectionReply(request, 200, map[string]any{"issueKey": "EXAMPLE-1", "summary": "Original title", "description": original}), nil
		}
		routes++
		var input struct{ State chain.State }
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			return nil, err
		}
		if !strings.Contains(input.State.Request, original) {
			t.Errorf("request changed: %s", input.State.Request)
		}
		choice := "implement"
		if routes == 2 {
			if len(input.State.History) != 1 || input.State.History[0].Error != "" || !strings.Contains(input.State.History[0].Output, original) {
				t.Errorf("real child did not receive the original: %#v", input.State.History)
			}
			choice = "done"
		}
		return selectionReply(request, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": choice}}}), nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var log bytes.Buffer
	if err := run(ctx, args, io.Discard, &log); err != nil {
		t.Fatalf("intake fault ended the request: %v", err)
	}
	if reads != 2 || routes != 2 || !strings.Contains(log.String(), "temporary intake outage") || strings.Contains(log.String(), "synthetic-startup-key") {
		t.Fatalf("recovery/cause/credential handling wrong: reads=%d routes=%d log=%s", reads, routes, &log)
	}
}

func TestStartupPersistentIntakeFailureWaitsUntilCancellation(t *testing.T) {
	args := startupArguments(t)
	reads := 0
	useCatalogTransport(t, func(request *http.Request) (*http.Response, error) {
		reads++
		if request.URL.Host != "startup-tracker.example" {
			t.Error("model launched without a request")
		}
		return catalogReply(request, 503, "original service cause"), nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	var log bytes.Buffer
	started := time.Now()
	err := run(ctx, args, io.Discard, &log)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second || reads != 1 || !strings.Contains(log.String(), "original service cause") {
		t.Fatalf("failure ended work or stop waited for retry: %v reads=%d log=%s", err, reads, &log)
	}
}

func TestStartupHasNoReadAttemptTerminal(t *testing.T) {
	reads, observed := 0, 0
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	store, err := acquireRequest(ctx, filepath.Join(t.TempDir(), "run"), func(context.Context) (string, error) {
		reads++
		if reads <= 20 {
			return "", errors.New("still unavailable")
		}
		return "unchanged original", nil
	}, time.Millisecond, func(message string) {
		observed++
		if !strings.Contains(message, "still unavailable") {
			t.Errorf("reason missing: %s", message)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	state, err := store.Load()
	if err != nil || reads != 21 || observed != 20 || state.Request != "unchanged original" || state.Done {
		t.Fatalf("read failures ended or changed work: %#v %v reads=%d observations=%d", state, err, reads, observed)
	}
}

func TestStartupWaitsForStorageWithoutReadingAChangedRequest(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "run")
	if err := os.WriteFile(directory, []byte("synthetic unavailable storage"), 0600); err != nil {
		t.Fatal(err)
	}
	reads, observed := 0, 0
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	store, err := acquireRequest(ctx, directory, func(context.Context) (string, error) {
		reads++
		if reads > 1 {
			t.Error("re-read a request already obtained")
			return "later edited text", nil
		}
		return "first original", nil
	}, time.Millisecond, func(message string) {
		observed++
		if !strings.Contains(message, "not a directory") {
			t.Errorf("storage reason missing: %s", message)
		}
		// Recover this generated fixture without discarding its contents.
		if err := os.Rename(directory, directory+".saved"); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	state, err := store.Load()
	if err != nil || observed != 1 || reads != 1 || state.Request != "first original" {
		t.Fatalf("storage recovery lost original: %#v %v reads=%d observations=%d", state, err, reads, observed)
	}
}

func TestStartupWaitPreservesExclusiveOwnershipAndInterruptedHistory(t *testing.T) {
	directory := t.TempDir()
	first, err := chain.Open(directory, "original")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { first.Close() })
	want := chain.State{Request: "original", Pending: &chain.Assignment{Role: "report"}, History: []chain.Result{{Speaker: "worker", Output: "full original report"}}}
	if err := first.Save(want); err != nil {
		t.Fatal(err)
	}
	reads, observations := 0, 0
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	store, err := acquireRequest(ctx, directory, func(context.Context) (string, error) { reads++; return "original", nil }, time.Millisecond, func(message string) {
		observations++
		if !strings.Contains(message, "another process owns") {
			t.Errorf("ownership error lost: %s", message)
		}
		if err := first.Close(); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	state, err := store.Load()
	gotJSON, _ := json.Marshal(state)
	wantJSON, _ := json.Marshal(want)
	if err != nil || !bytes.Equal(gotJSON, wantJSON) || reads != 1 || observations != 1 {
		t.Fatalf("ownership or history lost: %s %v reads=%d observations=%d", gotJSON, err, reads, observations)
	}
}

func TestStartupNeverReplacesAnotherRequestsHistory(t *testing.T) {
	directory := t.TempDir()
	first, err := chain.Open(directory, "original A")
	if err != nil {
		t.Fatal(err)
	}
	first.Close()
	before, err := os.ReadFile(filepath.Join(directory, "history.json"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := acquireRequest(ctx, directory, func(context.Context) (string, error) { return "different B", nil }, time.Hour, func(message string) {
		if !strings.Contains(message, "different request") {
			t.Errorf("request mismatch hidden: %s", message)
		}
		cancel()
	})
	after, readErr := os.ReadFile(filepath.Join(directory, "history.json"))
	if store != nil || !errors.Is(err, context.Canceled) || readErr != nil || !bytes.Equal(before, after) {
		t.Fatalf("recovery replaced history: store=%v error=%v read=%v", store, err, readErr)
	}
}

func TestStartupCancelledDuringReadDoesNotCreateHistory(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "run")
	ctx, cancel := context.WithCancel(context.Background())
	store, err := acquireRequest(ctx, directory, func(context.Context) (string, error) {
		cancel()
		return "original", nil
	}, time.Hour, func(string) { t.Error("waiting after explicit stop") })
	_, statErr := os.Stat(directory)
	if store != nil || !errors.Is(err, context.Canceled) || !os.IsNotExist(statErr) {
		t.Fatalf("stop created request state: %v %v %v", store, err, statErr)
	}
}
