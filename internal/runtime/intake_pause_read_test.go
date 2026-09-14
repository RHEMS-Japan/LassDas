package runtime

import (
	"path/filepath"
	"strings"
	"testing"
)

// The pause is read from the config file as it is now — the one setting an
// operator changes on a running pod — and only the cards orchestration may
// carry it: the runner orchestration would accept the value and start every
// queued run regardless.
func TestTheIntakePauseIsReadFromTheFileNowAndOnlyUnderCards(t *testing.T) {
	live := validRuntimeConfigMap()
	live["orchestration"] = "cards"
	liveChain := cardsChainMap()
	liveChain["intake_paused_since"] = "2026-09-14T08:30:00+09:00"
	live["chain"] = liveChain
	if value, err := ReadIntakePause(writeRuntimeConfig(t, live)); err != nil || value != "2026-09-14T08:30:00+09:00" {
		t.Fatalf("ReadIntakePause() = %q, %v", value, err)
	}
	delete(liveChain, "intake_paused_since")
	live["chain"] = liveChain
	if value, err := ReadIntakePause(writeRuntimeConfig(t, live)); err != nil || value != "" {
		t.Fatalf("ReadIntakePause() after the value was removed = %q, %v", value, err)
	}
	liveChain["intake_paused_since"] = "yesterday"
	live["chain"] = liveChain
	if _, err := ReadIntakePause(writeRuntimeConfig(t, live)); err == nil {
		t.Fatal("a value that is not a time was read as a pause")
	}
	if _, err := ReadIntakePause(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("a missing file was read as a pause")
	}
	// A mistyped key or a dropped chain object is an error to keep the
	// last value over, never a silent "not paused" (the boot would refuse
	// the same file).
	typo := validRuntimeConfigMap()
	typo["orchestration"] = "cards"
	chain := cardsChainMap()
	chain["intake_pause_since"] = "2026-09-14T08:30:00+09:00"
	typo["chain"] = chain
	if _, err := ReadIntakePause(writeRuntimeConfig(t, typo)); err == nil {
		t.Fatal("a mistyped pause key was read as not paused")
	}
	cards := validRuntimeConfigMap()
	cards["orchestration"] = "cards"
	cards["chain"] = cardsChainMap()
	if value, err := ReadIntakePause(writeRuntimeConfig(t, cards)); err != nil || value != "" {
		t.Fatalf("a cards config without the pause = %q, %v", value, err)
	}
	raw := validRuntimeConfigMap()
	raw["chain"] = map[string]any{"intake_paused_since": "2026-09-14T08:30:00+09:00"}
	if _, err := Load(writeRuntimeConfig(t, raw)); err == nil || !strings.Contains(err.Error(), "cards") {
		t.Fatalf("Load() under the runner orchestration error = %v, want the pause refused", err)
	}
}
