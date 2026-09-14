package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The pause is read from the config file as it is now — the one setting an
// operator changes on a running pod — and only the cards orchestration may
// carry it: the runner orchestration would accept the value and start every
// queued run regardless.
func TestTheIntakePauseIsReadFromTheFileNowAndOnlyUnderCards(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.json")
	if err := os.WriteFile(path, []byte(`{"chain":{"intake_paused_since":"2026-09-14T08:30:00+09:00"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if value, err := ReadIntakePause(path); err != nil || value != "2026-09-14T08:30:00+09:00" {
		t.Fatalf("ReadIntakePause() = %q, %v", value, err)
	}
	if err := os.WriteFile(path, []byte(`{"chain":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if value, err := ReadIntakePause(path); err != nil || value != "" {
		t.Fatalf("ReadIntakePause() after the value was removed = %q, %v", value, err)
	}
	if err := os.WriteFile(path, []byte(`{"chain":{"intake_paused_since":"yesterday"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadIntakePause(path); err == nil {
		t.Fatal("a value that is not a time was read as a pause")
	}
	if _, err := ReadIntakePause(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("a missing file was read as a pause")
	}
	raw := validRuntimeConfigMap()
	raw["chain"] = map[string]any{"intake_paused_since": "2026-09-14T08:30:00+09:00"}
	if _, err := Load(writeRuntimeConfig(t, raw)); err == nil || !strings.Contains(err.Error(), "cards") {
		t.Fatalf("Load() under the runner orchestration error = %v, want the pause refused", err)
	}
}
