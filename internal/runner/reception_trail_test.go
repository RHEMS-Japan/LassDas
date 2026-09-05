package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runtime"
)

// receptionStubWorker stands in for the worker binary: the named subcommand
// prints the given stderr text and exits 1; every other subcommand succeeds
// without writing anything.
func receptionStubWorker(t *testing.T, failing, stderr string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "stand-in-worker")
	body := "#!/bin/sh\nif [ \"$1\" = \"" + failing + "\" ]; then printf '%s\\n' '" + stderr + "' >&2; exit 1; fi\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return script
}

func receptionPipeline(t *testing.T, workerBin string) *Pipeline {
	t.Helper()
	config := runtime.Config{WorkerBin: workerBin, ConsumerConfigPath: "consumer.json"}
	config.Identity.EngineSHA = strings.Repeat("ab", 20)
	return &Pipeline{Config: config, Workspace: t.TempDir(), Logger: trailTestLogger{}}
}

// A readiness answer cut off at the output allowance ends the run as
// model_failed, and the requester learns why: the run's trail carries the
// cause in their words, and the terminal report accepts it.
func TestReadinessCutOffLeavesTheRequesterTheReasonInTheTrail(t *testing.T) {
	worker := receptionStubWorker(t, "assess-readiness",
		"worker: readiness assessment failed: model response ended before a complete answer: finish_reason=length (output allowance 32768 tokens); asked again with the wider allowance and cut off again")
	pipeline := receptionPipeline(t, worker)
	outcome, err := pipeline.readinessGate(context.Background())
	if err != nil || outcome.Code != hook.TerminalModelFailed {
		t.Fatalf("readinessGate() = %+v, %v; want model_failed", outcome, err)
	}
	content, readErr := os.ReadFile(pipeline.path("m1-trail.txt"))
	if readErr != nil {
		t.Fatalf("no trail was written for the cutoff: %v", readErr)
	}
	text := string(content)
	if !strings.Contains(text, "出力の上限で途切れた") || !strings.Contains(text, "受付の判定") || !strings.Contains(text, "1 回聞き直しましたが") || !strings.Contains(text, "出し直しても同じ結果になる可能性") {
		t.Fatalf("the trail does not name the cause in the requester's words: %q", text)
	}
	if err := hook.ValidateTrailText(text); err != nil {
		t.Fatalf("the trail would be refused by the report: %v", err)
	}
	if !pipeline.trailWritten {
		t.Fatal("the trail this run wrote must be trusted by the terminal report")
	}
}

// Any other reception failure keeps the generic outcome: no trail is
// invented for a cause the runner did not see.
func TestReadinessFailureWithoutACutoffWritesNoTrail(t *testing.T) {
	worker := receptionStubWorker(t, "assess-readiness", "worker: readiness assessment failed: model invocation failed")
	pipeline := receptionPipeline(t, worker)
	outcome, err := pipeline.readinessGate(context.Background())
	if err != nil || outcome.Code != hook.TerminalModelFailed {
		t.Fatalf("readinessGate() = %+v, %v; want model_failed", outcome, err)
	}
	if _, statErr := os.Stat(pipeline.path("m1-trail.txt")); statErr == nil {
		t.Fatal("a trail was written without a cutoff")
	}
	if pipeline.trailWritten {
		t.Fatal("nothing was written, so nothing is trusted")
	}
}

// A file squatting on the trail path is replaced, never attached.
func TestReceptionTrailReplacesASquatter(t *testing.T) {
	worker := receptionStubWorker(t, "assess-readiness", "worker: readiness assessment failed: finish_reason=length")
	pipeline := receptionPipeline(t, worker)
	if err := os.WriteFile(pipeline.path("m1-trail.txt"), []byte("forged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.readinessGate(context.Background()); err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(pipeline.path("m1-trail.txt"))
	if strings.Contains(string(content), "forged") {
		t.Fatalf("the squatter survived: %q", content)
	}
}

// The note says only what happened: a widened re-ask that was cut off
// again, no re-ask because the allowance was already at the ceiling, or
// neither when the worker's words say nothing more.
func TestReceptionCutoffNoteMatchesWhatTheWorkerDid(t *testing.T) {
	again := receptionCutoffNote("契約の導出", "worker: contract derivation failed: model response ended before a complete answer: finish_reason=length (output allowance 16384 tokens); asked again with the wider allowance and cut off again")
	if !strings.Contains(again, "契約の導出") || !strings.Contains(again, "1 回聞き直しましたが") || strings.Contains(again, "最大値") {
		t.Fatalf("cut off again: %q", again)
	}
	ceiling := receptionCutoffNote("受付の確認", "worker: readiness check failed: model response ended before a complete answer: finish_reason=length (output allowance 32768 tokens); the allowance is already at the ceiling of 32768 tokens")
	if !strings.Contains(ceiling, "受付の確認") || !strings.Contains(ceiling, "聞き直しはできませんでした") || strings.Contains(ceiling, "1 回聞き直し") {
		t.Fatalf("at the ceiling: %q", ceiling)
	}
	bare := receptionCutoffNote("受付の判定", "finish_reason=length")
	if strings.Contains(bare, "聞き直し") || !strings.Contains(bare, "途切れた") {
		t.Fatalf("bare marker: %q", bare)
	}
	if receptionCutoffNote("受付の判定", "worker: readiness assessment failed: model invocation failed") != "" {
		t.Fatal("a note was rendered without a cutoff")
	}
	for _, note := range []string{again, ceiling, bare} {
		if err := hook.ValidateTrailText(note); err != nil {
			t.Fatalf("the report would refuse the note: %v", err)
		}
	}
}

// The stderr tail keeps the end of a long stream, where the worker's
// refusal line is.
func TestTailBufferKeepsTheEnd(t *testing.T) {
	tail := &tailBuffer{limit: 16}
	for _, chunk := range []string{"0123456789", "abcdefghij", "KLMNOPQRSTUV"} {
		if _, err := tail.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if tail.String() != "efghijKLMNOPQRSTUV"[2:] || len(tail.String()) != 16 {
		t.Fatalf("tail = %q", tail.String())
	}
}

// The terminal report attaches the note the way it attaches a delivery's
// trail: loadTrail returns exactly what the reception wrote.
func TestReceptionTrailIsWhatTheTerminalReportAttaches(t *testing.T) {
	worker := receptionStubWorker(t, "assess-readiness", "worker: readiness assessment failed: finish_reason=length; asked again with the wider allowance and cut off again")
	pipeline := receptionPipeline(t, worker)
	if _, err := pipeline.readinessGate(context.Background()); err != nil {
		t.Fatal(err)
	}
	terminal := NewTerminal(pipeline.Config, nil, hook.DispatchEnvelope{}, 1, pipeline.Workspace, trailTestLogger{})
	trail, err := terminal.loadTrail(hook.TerminalModelFailed)
	if err != nil || !strings.Contains(trail, "1 回聞き直しましたが") {
		t.Fatalf("loadTrail() = %q, %v", trail, err)
	}
}
