package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ticket-runner/internal/chain"
	"ticket-runner/internal/tracker"
)

func TestAFinishedRequestLosesItsCachesAndKeepsWhatPeopleRead(t *testing.T) {
	directory := t.TempDir()
	for _, path := range []string{"homes/2-0/.cache/go-build/x", "homes/2-0/go/pkg/mod/y", "homes/2-0/logs", "homes/3-0/lsp/z", "homes/3-0/logs", "workspace/src"} {
		if err := os.MkdirAll(filepath.Join(directory, path), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"homes/2-0/logs/agent.log", "homes/2-0/transcript.json", "homes/2-0/state.db", "homes/3-0/logs/errors.log", "workspace/src/main.go", "homes/2-0/.cache/go-build/x/blob"} {
		if err := os.WriteFile(filepath.Join(directory, path), []byte("kept?"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// A module cache is left read-only by the build tool.
	if err := os.Chmod(filepath.Join(directory, "homes/2-0/go/pkg/mod/y"), 0500); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(directory, "homes/2-0/go/pkg/mod"), 0500); err != nil {
		t.Fatal(err)
	}
	if err := trimFinished(directory); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"homes/2-0/.cache", "homes/2-0/go", "homes/3-0/lsp"} {
		if _, err := os.Stat(filepath.Join(directory, gone)); err == nil {
			t.Errorf("%s survived the trim", gone)
		}
	}
	for _, kept := range []string{"homes/2-0/logs/agent.log", "homes/2-0/transcript.json", "homes/2-0/state.db", "homes/3-0/logs/errors.log", "workspace/src/main.go", "homes/.trimmed"} {
		if _, err := os.Stat(filepath.Join(directory, kept)); err != nil {
			t.Errorf("%s was removed or not written: %v", kept, err)
		}
	}
	// A second pass is a no-op, and a request without homes needs nothing.
	if err := trimFinished(directory); err != nil {
		t.Fatal(err)
	}
	if err := trimFinished(t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

// trimTestLog is the queue's log, readable while the queue writes it.
type trimTestLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *trimTestLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *trimTestLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestCachesThatCannotBeRemovedAreSaidOncePerReasonAndRetried(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root is not kept out by directory permissions")
	}
	cfg := watchConfiguration(t)
	cfg.Intake.StopReportRole = ""
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "watch-tracker.example" && strings.HasSuffix(r.URL.Path, "/issues") {
			return selectionReply(r, 200, []any{}), nil
		}
		return catalogReply(r, 503, "not under test"), nil
	})
	root := t.TempDir()
	dir := filepath.Join(root, "jobs", "70")
	homes := filepath.Join(dir, "homes")
	for _, path := range []string{filepath.Join(homes, "2-0", "go", "pkg"), filepath.Join(dir, "run")} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := json.Marshal(watchedIssue(70, "Original conditions", "2026-01-03T00:00:00Z"))
	if err := os.WriteFile(filepath.Join(dir, "issue.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	request, err := tracker.RequestText(raw)
	if err != nil {
		t.Fatal(err)
	}
	saved, _ := json.Marshal(chain.State{Done: true, Request: request})
	if err := os.WriteFile(filepath.Join(dir, "run", "history.json"), saved, 0600); err != nil {
		t.Fatal(err)
	}
	// The homes cannot be read, so the trim fails on every tick.
	if err := os.Chmod(homes, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(homes, 0700) })
	var queueLog trimTestLog
	finish := startStopQueue(t, cfg, root, 20*time.Millisecond, &queueLog)
	waitFor(t, func() bool { return strings.Contains(queueLog.String(), "request 70: finished caches not removed") })
	time.Sleep(200 * time.Millisecond)
	if n := strings.Count(queueLog.String(), "finished caches not removed"); n != 1 {
		t.Fatalf("the same failure was said %d times:\n%s", n, queueLog.String())
	}
	if err := os.Chmod(homes, 0700); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return strings.Contains(queueLog.String(), "request 70: finished caches removed") })
	finish()
	if _, err := os.Stat(filepath.Join(homes, "2-0", "go")); err == nil {
		t.Fatal("the caches survived once the homes could be read")
	}
	if strings.Count(queueLog.String(), "finished caches not removed") != 1 || strings.Count(queueLog.String(), "finished caches removed") != 1 {
		t.Fatalf("log:\n%s", queueLog.String())
	}
}
