package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeSnapshot(t *testing.T, dir, step string, at time.Time) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"schema_version": 1, "generated_at": at, "runs": []any{map[string]any{"delivery_id": "delivery-example", "issue_key": "PROJ-7", "state": "claimed", "step": step, "workspace_path": "/private/internal/workspace"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "board.tmp"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "board.tmp"), filepath.Join(dir, "board.json")); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotAvailabilityAndPrivateRouting(t *testing.T) {
	s := boardServer{statusDir: t.TempDir()}
	if got := s.payload(); got.SnapshotState != "missing" {
		t.Fatalf("%+v", got)
	}
	if err := os.WriteFile(filepath.Join(s.statusDir, "board.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := s.payload(); got.SnapshotState != "invalid" {
		t.Fatalf("%+v", got)
	}
	for _, test := range []struct {
		age  time.Duration
		want string
	}{{0, "ready"}, {4 * time.Minute, "stale"}, {-2 * time.Minute, "stale"}} {
		writeSnapshot(t, s.statusDir, "review", time.Now().Add(-test.age))
		got := s.payload()
		if got.SnapshotState != test.want || strings.Contains(string(got.Board), "workspace_path") || strings.Contains(string(got.Board), "/private/") {
			t.Fatalf("%+v", got)
		}
	}
	for _, raw := range []string{`null`, `{"schema_version":1,"runs":[]}`, `{"schema_version":1,"generated_at":"2026-01-01T00:00:00Z","runs":[null]}`} {
		if err := os.WriteFile(filepath.Join(s.statusDir, "board.json"), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if got := s.payload(); got.SnapshotState != "invalid" {
			t.Fatalf("invalid shape accepted: %+v", got)
		}
	}
}

func TestStreamPublishesReplacedSnapshotWithoutReload(t *testing.T) {
	s := &boardServer{statusDir: t.TempDir()}
	writeSnapshot(t, s.statusDir, "intake", time.Now())
	server := httptest.NewServer(http.HandlerFunc(s.serveStream))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	scanner := bufio.NewScanner(response.Body)
	read := func(want string) {
		t.Helper()
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			if !strings.Contains(line, `"step":"`+want+`"`) || !strings.Contains(line, `"snapshot_state":"ready"`) || strings.Contains(line, "workspace_path") {
				t.Fatalf("bad event: %s", line)
			}
			return
		}
		t.Fatalf("no event: %v", scanner.Err())
	}
	read("intake")
	writeSnapshot(t, s.statusDir, "review", time.Now())
	read("review")
}
