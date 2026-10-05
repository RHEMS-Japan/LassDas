package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDiscardedWorkspaceStillShowsRetainedEvidence(t *testing.T) {
	root := fixtureQueue(t)
	dir := filepath.Join(root, "jobs", "7")
	wantReceipt := `{"head":"previous-work","pull_request":7}`
	evidence, _ := json.Marshal(map[string][]byte{".git/ticket-engine/delivery.json": []byte(wantReceipt), "report/result.md": []byte("retained stop report")})
	if err := os.WriteFile(filepath.Join(dir, "workspace-evidence.json"), evidence, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "workspace")); err != nil {
		t.Fatal(err)
	}
	server := &server{runDir: root}
	for _, record := range []string{`{"removing":true}`, `{"removing":true,"removed_at":"2026-01-02T03:04:05Z"}`} {
		if err := os.WriteFile(filepath.Join(dir, "workspace-removal.json"), []byte(record), 0600); err != nil {
			t.Fatal(err)
		}
		job := server.loadJob("7", time.Now(), true)
		if job.Receipt != wantReceipt || job.Report != "retained stop report" {
			t.Fatalf("retained evidence not shown: receipt=%q report=%q", job.Receipt, job.Report)
		}
		if job.Workspace == nil || !strings.Contains(job.Workspace.Note, "discard") || strings.Contains(job.Workspace.Note, "no checkout yet") {
			t.Fatalf("removal not explained: %+v", job.Workspace)
		}
	}
}
