package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/hook"
)

// maxPullRequestBodyBytes is the bound the GitHub client enforces on a pull
// request body (validatePullRequestSpec). GitHub's own limit is 65,536
// characters; the client counts bytes, which for this record's Japanese is
// the stricter of the two.
const maxPullRequestBodyBytes = 64 * 1024

func trailBodyBinding() deliveryBinding {
	return deliveryBinding{
		DeliveryID: "delivery_0123456789abcdef0123456789abcdef", IssueKey: "TICKET-123",
		InputSHA256: strings.Repeat("1", 64), ConfigSHA256: strings.Repeat("2", 64), ToolSHA: strings.Repeat("3", 40),
		SourceSHA256: strings.Repeat("4", 64), CandidateSHA256: strings.Repeat("5", 64),
		DecisionSHA256: strings.Repeat("6", 64), ValidationSHA256: strings.Repeat("7", 64),
		ProductPaths: []string{"client/src/example.tsx"},
	}
}

// The pull request body is where the whole run record is meant to be
// readable: it is the description the requester reads when deciding whether
// to merge. Holding the file to the ticket comment's size was how a record
// three times that long reached the pull request already cut (live
// 2026-09-25) -- and, before that, made the file "invalid" outright.
func TestFeaturePullRequestBodyCarriesTheWholeRecord(t *testing.T) {
	record := "### 実装とレビューの経過\n- 1 周目: 収束\n\n実装者の説明 (要点): " +
		strings.Repeat("有効化の設定例と確認手順を書く。\n", 700)
	if len(record) <= 6*1024 {
		t.Fatalf("fixture record is %d bytes; it must be longer than one ticket comment carried", len(record))
	}
	path := filepath.Join(t.TempDir(), "m1-trail.txt")
	if err := os.WriteFile(path, []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := readTrailFile(path)
	if err != nil {
		t.Fatalf("readTrailFile() = %v; the record file was refused for its length", err)
	}
	if loaded != record {
		t.Fatalf("readTrailFile returned %d of %d bytes", len(loaded), len(record))
	}
	body := featurePullRequestSpec(trailBodyBinding(), loaded).Body
	if !strings.Contains(body, record) {
		t.Fatalf("pull request body carries only part of the record: %d bytes", len(body))
	}
	if !strings.Contains(body, "Issue: TICKET-123") {
		t.Fatalf("pull request body lost its digest header: %q", body[:200])
	}
}

// A record at the composer's bound still has to leave the digest header room
// inside the body limit the client enforces, or the delivery fails at the
// last step with the work already pushed.
func TestPullRequestBodyFitsARecordAtItsBound(t *testing.T) {
	record := strings.Repeat("あ", hook.MaxTrailRecordBytes/3)
	if len(record) > hook.MaxTrailRecordBytes {
		record = record[:hook.MaxTrailRecordBytes]
	}
	body := featurePullRequestSpec(trailBodyBinding(), record).Body
	if len(body) > maxPullRequestBodyBytes {
		t.Fatalf("a record at its bound makes a %d byte body; the client refuses over %d", len(body), maxPullRequestBodyBytes)
	}
}

// A record longer than the composer would ever write is still refused: the
// file sits in a directory the model agents can reach.
func TestReadTrailFileRefusesAnOversizeRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m1-trail.txt")
	if err := os.WriteFile(path, []byte(strings.Repeat("a", hook.MaxTrailRecordBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readTrailFile(path); err == nil {
		t.Fatal("an oversize record file was accepted")
	}
}
