package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// always_ask is read from the operator's file on the stage that confirms the
// change, and a watch starts on it; written on another stage it is refused
// before work is accepted, with the stage it was written on.
func TestTheCheckReadsAlwaysAsk(t *testing.T) {
	check := func(cfg config) (string, error) {
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "operator.json")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		return string(data), run(context.Background(), []string{"--check", "--config", path}, io.Discard, io.Discard)
	}
	cfg := previewStagesConfig(t)
	cfg.Workflow.Preview = nil
	cfg.Workflow.Stages[1].AlwaysAsk = true // confirm_change
	written, err := check(cfg)
	if err != nil {
		t.Fatalf("always_ask on the stage that confirms the change was refused: %v", err)
	}
	if !strings.Contains(written, `"confirm":true,"always_ask":true`) {
		t.Fatalf("the setting is not written as an operator writes it: %s", written)
	}
	cfg.Workflow.Stages[1].AlwaysAsk = false
	cfg.Workflow.Stages[0].AlwaysAsk = true // elicit
	if _, err := check(cfg); err == nil || !strings.Contains(err.Error(), `stage "elicit"`) || !strings.Contains(err.Error(), "always_ask") {
		t.Fatalf("always_ask on a stage that does not confirm the change was not refused where it is: %v", err)
	}
}
