package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/runtime"
)

// What a step produced is kept beside the run, not only in the container's
// log: a container that is rebuilt otherwise takes the only copy of a
// failure's reason with it (reported live 2026-09-17).
func TestStepOutputIsKeptBesideTheRun(t *testing.T) {
	script := filepath.Join(t.TempDir(), "worker")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho '工程の出力です'\necho 'worker: 失敗しました' >&2\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := runtime.Config{WorkerBin: script, ConsumerConfigPath: "consumer.json"}
	config.Identity.EngineSHA = strings.Repeat("ab", 20)
	pipeline := &Pipeline{Config: config, Workspace: t.TempDir(), Logger: trailTestLogger{}}
	if code, err := pipeline.worker(context.Background(), "baseline", []string{"baseline"}); err != nil || code != 0 {
		t.Fatalf("step returned %d, %v", code, err)
	}
	raw, err := os.ReadFile(LiveLogPath(pipeline.Workspace, "baseline"))
	if err != nil {
		t.Fatalf("the step's output was not kept: %v", err)
	}
	for _, want := range []string{"工程の出力です", "worker: 失敗しました"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("kept output %q lacks %q", raw, want)
		}
	}
}
