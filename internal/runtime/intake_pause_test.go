package runtime

import (
	"strings"
	"testing"
	"time"
)

// The operator's pause is a time, not a flag: the board and the ticket say
// since when, and a value that is not a time is refused at load rather than
// read as "not paused" by a running attendant.
func TestIntakePauseIsAnRFC3339TimeOrAbsent(t *testing.T) {
	raw := validRuntimeConfigMap()
	raw["orchestration"] = "cards"
	chain := cardsChainMap()
	chain["intake_paused_since"] = "2026-09-14T09:00:00+09:00"
	raw["chain"] = chain
	config, err := Load(writeRuntimeConfig(t, raw))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	since, paused := config.Chain.IntakePaused()
	if !paused || !since.Equal(time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("IntakePaused() = %v %v, want the configured instant", since, paused)
	}
	chain["intake_paused_since"] = "yesterday"
	raw["chain"] = chain
	if _, err := Load(writeRuntimeConfig(t, raw)); err == nil || !strings.Contains(err.Error(), "intake_paused_since") {
		t.Fatalf("Load() error = %v, want the pause value refused", err)
	}
	delete(chain, "intake_paused_since")
	raw["chain"] = chain
	config, err = Load(writeRuntimeConfig(t, raw))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if _, paused := config.Chain.IntakePaused(); paused {
		t.Fatal("an absent value reads as paused")
	}
}
