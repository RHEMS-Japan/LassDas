package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The trail of a round that sealed nothing is rendered from the run record,
// which cannot say which card then blocked; the attendant's step name is
// what supplies that, so it has to reach the composer.
func TestTheBlockedStepReachesTheTrailComposer(t *testing.T) {
	record := filepath.Join(t.TempDir(), "argv.txt")
	script := filepath.Join(t.TempDir(), "worker")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+record+"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	pipeline := trailPipeline(t, script)
	pipeline.NoteBlockedStep("変更の確定")
	if err := pipeline.EnsureTrail(context.Background()); err != nil {
		t.Fatalf("EnsureTrail: %v", err)
	}
	argv, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(argv), "--blocked-step 変更の確定") {
		t.Fatalf("the blocked step did not reach the composer: %s", argv)
	}

	// A run with no card to name passes nothing rather than an empty value.
	plain := trailPipeline(t, script)
	if err := plain.EnsureTrail(context.Background()); err != nil {
		t.Fatalf("EnsureTrail: %v", err)
	}
	second, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(second), "--blocked-step") != 1 {
		t.Fatalf("a run with no card named a blocked step: %s", second)
	}
}
