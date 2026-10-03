package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ticket-runner/internal/chain"
)

func TestBranchOnlyCompletionKeepsItsReceiptWithoutInventingAMerge(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "23", chain.State{Done: true, Request: "Publish the branch"})
	receipt := `{"branch_only":true,"branch":"ticket/EXAMPLE-23","published_head":"` + strings.Repeat("a", 40) + `","branch_confirmed_at":"2026-01-02T00:00:00Z"}`
	path := filepath.Join(root, "jobs", "23", "workspace", ".git", "ticket-engine", "delivery.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(receipt), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := newServer(root, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, detail := range []bool{false, true} {
		j := s.loadJob("23", time.Now(), detail)
		if j.Receipt != receipt || j.Lane != "delivered" || j.endedUnmerged() {
			t.Fatalf("detail=%v job=%+v", detail, j)
		}
		if strings.Contains(j.Status, "merge") || strings.Contains(j.Status, "pull request") {
			t.Fatalf("branch publication invented a PR outcome: %s", j.Status)
		}
	}
}
