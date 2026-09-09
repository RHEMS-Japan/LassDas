package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An incomplete round records why, and what the role last answered and was
// told, so the operator can see what kept being refused.
func TestWriteIncompleteKeepsTheLastRefusedAnswerAndObjection(t *testing.T) {
	dir := t.TempDir()
	if err := writeIncomplete(dir, "the model's report kept failing the checks", `{"report":{}}`, "investigation finding 1: claim is 700 bytes (limit 600)"); err == nil {
		t.Fatal("writeIncomplete must return the incomplete error")
	}
	raw, err := os.ReadFile(filepath.Join(dir, incompleteFile))
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]string
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	if record["reason"] == "" || record["last_refused_answer"] != `{"report":{}}` || !strings.Contains(record["last_refused_objection"], "limit 600") {
		t.Fatalf("incomplete record = %v", record)
	}
	if info, err := os.Stat(filepath.Join(dir, incompleteFile)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("incomplete record mode = %v, %v; want 0600", info, err)
	}
	if err := writeIncomplete(t.TempDir(), "budget spent", "", ""); err == nil {
		t.Fatal("writeIncomplete must return the incomplete error")
	}
}

// The next round is handed the earlier round's sealed report along with
// its design and the verdicts; without it the designer could only cite ids
// it never saw. The earlier round's files are a real sealed investigation
// and design, which previousRound verifies against the run's identity.
func TestPreviousRoundCarriesTheInvestigation(t *testing.T) {
	fixture := newDesignFixture(t, passVerdict, nil)
	dir := t.TempDir()
	for from, to := range map[string]string{fixture.investigationPath: "investigation.json", fixture.designPath: "design.json"} {
		content, err := os.ReadFile(from)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, to), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "decision.json"), []byte(`{"outcome":"revise"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, encoded, err := previousRound(dir, fixture.identity, fixture.measurementsPath)
	if err != nil {
		t.Fatal(err)
	}
	var context map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &context); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"investigation", "design", "decision"} {
		if _, ok := context[key]; !ok {
			t.Errorf("previous_round lacks %q: %s", key, encoded)
		}
	}
}
