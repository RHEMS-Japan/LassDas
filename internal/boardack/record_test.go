package boardack

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A record that cannot be read is no acknowledgement at all, and that is the
// safe way round: every finished card then stays in the running lane, which
// asks a person to look rather than hiding anything.
func TestAnUnreadableRecordClearsNothing(t *testing.T) {
	dir := t.TempDir()
	if got := Read(dir); len(got) != 0 {
		t.Fatalf("a missing record cleared %d cards", len(got))
	}
	for name, body := range map[string]string{
		"not JSON":       "{",
		"no runs":        `{"schema_version":1}`,
		"runs not a map": `{"schema_version":1,"runs":[]}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := Read(dir); len(got) != 0 {
			t.Fatalf("%s cleared %d cards", name, len(got))
		}
	}
	// Bigger than the bound is refused whole rather than read in part.
	padding := strings.Repeat("x", maxFileBytes)
	if err := os.WriteFile(filepath.Join(dir, FileName),
		[]byte(`{"schema_version":1,"runs":{"d1":{"at":"2026-09-25T02:00:00Z","user":"`+padding+`"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Read(dir); len(got) != 0 {
		t.Fatalf("an oversized record cleared %d cards", len(got))
	}
}

// An entry with no time says nothing about when, which is the one thing the
// record exists to say.
func TestAnEntryWithoutATimeIsNotAnAcknowledgement(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName),
		[]byte(`{"schema_version":1,"runs":{"d1":{"at":"2026-09-25T02:00:00Z"},"d2":{"user":"someone"},"":{"at":"2026-09-25T02:00:00Z"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got := Read(dir)
	if len(got) != 1 || got["d1"].At.IsZero() {
		t.Fatalf("read %v, want only the one entry that says when", got)
	}
}

// The record is bounded, and what goes is what a person dealt with longest
// ago. Writing leaves no half-written file behind either.
func TestWritingBoundsTheRecordAndLeavesNoRemnant(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	runs := map[string]Entry{}
	for i := range MaxEntries + 5 {
		runs[fmt.Sprintf("d%04d", i)] = Entry{At: start.Add(time.Duration(i) * time.Minute), User: "someone"}
	}
	if err := Write(dir, runs); err != nil {
		t.Fatal(err)
	}
	back := Read(dir)
	if len(back) != MaxEntries {
		t.Fatalf("kept %d entries, want %d", len(back), MaxEntries)
	}
	for i := range 5 {
		if _, kept := back[fmt.Sprintf("d%04d", i)]; kept {
			t.Fatalf("the oldest entry d%04d was kept over a newer one", i)
		}
	}
	if _, kept := back[fmt.Sprintf("d%04d", MaxEntries+4)]; !kept {
		t.Fatal("the newest entry went")
	}
	if entry := back["d0100"]; entry.User != "someone" || !entry.At.Equal(start.Add(100*time.Minute)) {
		t.Fatalf("an entry did not survive the round trip: %+v", entry)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != FileName {
			t.Fatalf("writing left %q behind", entry.Name())
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	var shape struct {
		SchemaVersion int `json:"schema_version"`
	}
	if json.Unmarshal(raw, &shape) != nil || shape.SchemaVersion != 1 {
		t.Fatalf("the record carries no version: %s", raw[:min(120, len(raw))])
	}
}
