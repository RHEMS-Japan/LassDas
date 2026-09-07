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
func TestWriteIncompleteKeepsTheLastAnswerAndObjection(t *testing.T) {
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
	if record["reason"] == "" || record["last_answer"] != `{"report":{}}` || !strings.Contains(record["last_objection"], "limit 600") {
		t.Fatalf("incomplete record = %v", record)
	}
	if err := writeIncomplete(t.TempDir(), "budget spent", "", ""); err == nil {
		t.Fatal("writeIncomplete must return the incomplete error")
	}
}
