package main

import (
	"bytes"
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
)

func stoppedTrimFixture(t *testing.T, cfg config, report []byte) (string, string, map[string][]byte) {
	t.Helper()
	root := t.TempDir()
	directory := filepath.Join(root, "jobs", "51")
	raw, err := json.Marshal(watchedIssue(51, "Original conditions", "2026-01-03T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	request, err := cfg.source().RequestText(raw)
	if err != nil {
		t.Fatal(err)
	}
	history, err := json.Marshal(chain.State{Request: request, Pending: &chain.Assignment{Role: "deliver"},
		History: []chain.Result{{Role: "deliver", Error: "context canceled", Output: "External operation may already exist."}}})
	if err != nil {
		t.Fatal(err)
	}
	kept := map[string][]byte{
		"issue.json":                                 raw,
		"stop-request.json":                          stopComment(51, 55, "停止\nDo not continue this request."),
		"run/history.json":                           history,
		"workspace/undelivered.txt":                  []byte("work that has not been delivered\n"),
		"workspace/.git/ticket-engine/delivery.json": []byte(`{"head":"local-work","pull_request":7}`),
		"homes/0-0/logs/agent.log":                   []byte("original observations\n"),
		"homes/0-0/transcript.json":                  []byte(`{"report":"original observations"}`),
	}
	if report != nil {
		kept["stop-report/history.json"] = report
	}
	for name, data := range kept {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(directory, name)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := writeRuntimeFile(filepath.Join(directory, name), data); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(directory, "homes/0-0/.cache"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeRuntimeFile(filepath.Join(directory, "homes/0-0/.cache/blob"), []byte("rebuildable")); err != nil {
		t.Fatal(err)
	}
	return root, directory, kept
}

func assertStoppedFilesKept(t *testing.T, directory string, kept map[string][]byte) {
	t.Helper()
	for name, want := range kept {
		got, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("retained file %s changed: %q, %v", name, got, err)
		}
	}
}

func TestStoppedRequestsRemoveOnlyCachesAfterReporting(t *testing.T) {
	for _, reporter := range []bool{false, true} {
		name := "no reporter"
		if reporter {
			name = "report completed"
		}
		t.Run(name, func(t *testing.T) {
			cfg := watchConfiguration(t)
			var report []byte
			if reporter {
				cfg.Intake.StopReportRole = "implement"
				report = []byte(`{"done":true}`)
			}
			root, directory, kept := stoppedTrimFixture(t, cfg, report)
			useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/issues") {
					return selectionReply(r, 200, []any{}), nil
				}
				t.Errorf("stopped work made an unexpected request: %s %s", r.Method, r.URL.Path)
				return catalogReply(r, 503, "not under test"), nil
			})
			finish := startStopQueue(t, cfg, root, 20*time.Millisecond, io.Discard)
			waitFor(t, func() bool {
				_, err := os.Stat(filepath.Join(directory, "homes", ".trimmed"))
				return err == nil
			})
			finish()
			if _, err := os.Stat(filepath.Join(directory, "homes/0-0/.cache")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stopped caches remain: %v", err)
			}
			assertStoppedFilesKept(t, directory, kept)
		})
	}
}

func TestStoppedCacheRemovalRetriesWithoutChangingRetainedWork(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root is not kept out by directory permissions")
	}
	cfg := watchConfiguration(t)
	root, directory, kept := stoppedTrimFixture(t, cfg, nil)
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		return selectionReply(r, 200, []any{}), nil
	})
	homes := filepath.Join(directory, "homes")
	if err := os.Chmod(homes, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(homes, 0700) })
	var log trimTestLog
	finish := startStopQueue(t, cfg, root, 20*time.Millisecond, &log)
	waitFor(t, func() bool { return strings.Contains(log.String(), "finished caches not removed") })
	time.Sleep(100 * time.Millisecond)
	if count := strings.Count(log.String(), "finished caches not removed"); count != 1 {
		t.Fatalf("the same removal error was repeated %d times: %s", count, log.String())
	}
	if err := os.Chmod(homes, 0700); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return strings.Contains(log.String(), "finished caches removed") })
	finish()
	if _, err := os.Stat(filepath.Join(homes, "0-0", ".cache")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the retry left caches behind: %v", err)
	}
	assertStoppedFilesKept(t, directory, kept)
}

func TestStoppedCachesRemainWhileTheReportIsUnreadable(t *testing.T) {
	cfg := watchConfiguration(t)
	cfg.Intake.StopReportRole = "implement"
	root, directory, kept := stoppedTrimFixture(t, cfg, []byte("unreadable history"))
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		return selectionReply(r, 200, []any{}), nil
	})
	var log trimTestLog
	finish := startStopQueue(t, cfg, root, 20*time.Millisecond, &log)
	waitFor(t, func() bool { return strings.Count(log.String(), "reading stopped report") >= 2 })
	finish()
	assertStoppedFilesKept(t, directory, kept)
	if _, err := os.Stat(filepath.Join(directory, "homes/0-0/.cache/blob")); err != nil {
		t.Fatalf("unreadable report lost its caches: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, "homes", ".trimmed")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unreadable report was treated as finished: %v", err)
	}
}
