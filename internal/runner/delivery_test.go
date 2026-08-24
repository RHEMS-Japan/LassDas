package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/runtime"
)

type trailTestLogger struct{}

func (trailTestLogger) Info(string, ...any)  {}
func (trailTestLogger) Error(string, ...any) {}

// trailPipeline is the smallest pipeline that can run EnsureTrail: a
// workspace, a stand-in worker binary, and the config fields the trail
// arguments name.
func trailPipeline(t *testing.T, workerBin string) *Pipeline {
	t.Helper()
	config := runtime.Config{WorkerBin: workerBin, ConsumerConfigPath: "consumer.json"}
	config.Identity.EngineSHA = strings.Repeat("ab", 20)
	return &Pipeline{Config: config, Workspace: t.TempDir(), Logger: trailTestLogger{}}
}

// A file that merely exists was written by nobody this run trusts — a model
// agent can reach ../m1-trail.txt unobserved — so it is replaced, never
// attached.
func TestEnsureTrailReplacesAFileItDidNotWrite(t *testing.T) {
	pipeline := trailPipeline(t, "false")
	if err := os.WriteFile(pipeline.path("m1-trail.txt"), []byte("forged trail\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.EnsureTrail(context.Background()); err != nil {
		t.Fatalf("EnsureTrail() error = %v", err)
	}
	content, err := os.ReadFile(pipeline.path("m1-trail.txt"))
	if err != nil || strings.Contains(string(content), "forged") {
		t.Fatalf("a pre-placed trail survived: %q, %v", content, err)
	}
	if !strings.Contains(string(content), "証跡の自動生成に失敗した") {
		t.Fatalf("the fallback line was not written over the squatter: %q", content)
	}
}

// The trail this run composed is reused by a later terminal in the same
// process — the post-publish failure report — without recomposing.
func TestEnsureTrailSkipsOnlyWhatThisRunWrote(t *testing.T) {
	directory := t.TempDir()
	composer := filepath.Join(directory, "stand-in-composer")
	script := "#!/bin/sh\nout=\"\"\nwhile [ $# -gt 0 ]; do\n  if [ \"$1\" = \"--out\" ]; then out=\"$2\"; fi\n  shift\ndone\nprintf 'COMPOSED\\n' > \"$out\"\n"
	if err := os.WriteFile(composer, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	pipeline := trailPipeline(t, composer)
	if err := pipeline.EnsureTrail(context.Background()); err != nil {
		t.Fatalf("EnsureTrail() error = %v", err)
	}
	// A second call must not run the worker again: a failing binary proves it.
	pipeline.Config.WorkerBin = "false"
	if err := pipeline.EnsureTrail(context.Background()); err != nil {
		t.Fatalf("EnsureTrail() second call error = %v", err)
	}
	content, err := os.ReadFile(pipeline.path("m1-trail.txt"))
	if err != nil || string(content) != "COMPOSED\n" {
		t.Fatalf("the composed trail was not kept across calls: %q, %v", content, err)
	}
}

func TestEnsureTrailWritesTheFallbackWhenComposeFails(t *testing.T) {
	pipeline := trailPipeline(t, "false")
	if err := pipeline.EnsureTrail(context.Background()); err != nil {
		t.Fatalf("EnsureTrail() error = %v", err)
	}
	content, err := os.ReadFile(pipeline.path("m1-trail.txt"))
	if err != nil || !strings.Contains(string(content), "証跡の自動生成に失敗した") {
		t.Fatalf("the fallback line was not written: %q, %v", content, err)
	}
}

func TestEnsureTrailUsesWhatTheComposerWrote(t *testing.T) {
	directory := t.TempDir()
	composer := filepath.Join(directory, "stand-in-composer")
	script := "#!/bin/sh\nout=\"\"\nwhile [ $# -gt 0 ]; do\n  if [ \"$1\" = \"--out\" ]; then out=\"$2\"; fi\n  shift\ndone\nprintf 'COMPOSED\\n' > \"$out\"\n"
	if err := os.WriteFile(composer, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	pipeline := trailPipeline(t, composer)
	if err := pipeline.EnsureTrail(context.Background()); err != nil {
		t.Fatalf("EnsureTrail() error = %v", err)
	}
	content, err := os.ReadFile(pipeline.path("m1-trail.txt"))
	if err != nil || string(content) != "COMPOSED\n" {
		t.Fatalf("the composed trail was not kept: %q, %v", content, err)
	}
}

func TestAttemptedImplementationFollowsTheHistoryDirectory(t *testing.T) {
	pipeline := trailPipeline(t, "false")
	pipeline.prepared = true
	if pipeline.AttemptedImplementation() {
		t.Fatal("an empty workspace counted as an attempted implementation")
	}
	if err := os.MkdirAll(filepath.Join(pipeline.Workspace, "history", "stage-1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if !pipeline.AttemptedImplementation() {
		t.Fatal("a sealed stage-1 directory did not count as an attempted implementation")
	}
}

// A stage directory in a workspace Prepare never cleared could be an earlier
// dispatch's leftover; this run must not render someone else's history.
func TestAttemptedImplementationRequiresAPreparedWorkspace(t *testing.T) {
	pipeline := trailPipeline(t, "false")
	if err := os.MkdirAll(filepath.Join(pipeline.Workspace, "history", "stage-1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if pipeline.AttemptedImplementation() {
		t.Fatal("an unprepared workspace's history counted as this run's")
	}
}
