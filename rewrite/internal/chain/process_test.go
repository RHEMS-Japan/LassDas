package chain

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestProcessGetsOriginalAndAssignmentWithoutShellInterpolation(t *testing.T) {
	const original = "Use a filename with spaces. Literal $(exit 9), `exit 8`, and \"quotes\"."
	process := Process{Name: "implementer", Command: []string{"/bin/sh", "-c", "cat; printf 'diagnostic only' >&2"}}
	result := process.run(context.Background(), Role{Name: "implement", Purpose: "implement the change"}, Assignment{Role: "implement", Instruction: "Fix the observed argument splitting."}, State{Request: original, History: []Result{{Role: "review", Output: "Ordinary prose with an unknown field: {\"extra\":true}."}}})
	if result.Error != "" || !strings.Contains(result.Output, original) || !strings.Contains(result.Output, "Fix the observed argument splitting.") {
		t.Fatalf("result=%#v", result)
	}
	if strings.Contains(result.Output, "diagnostic only") || result.Diagnostics != "diagnostic only" {
		t.Fatal("diagnostic output was mixed with the report")
	}
}

func TestProcessKeepsFailureReasonAndOnlyReceivesNamedCredentials(t *testing.T) {
	t.Setenv("UNRELATED_TEST_CREDENTIAL", "not-for-this-role")
	t.Setenv("FIXTURE_ROLE_SOURCE", "synthetic-test-value")
	process := Process{Name: "worker", Command: []string{"/bin/sh", "-c", `test -z "$UNRELATED_TEST_CREDENTIAL" || exit 7; printf '%s' "$ROLE_KEY"; printf 'actual build reason: %s' "$ROLE_KEY" >&2; exit 3`}, Secrets: map[string]string{"ROLE_KEY": "FIXTURE_ROLE_SOURCE"}}
	result := process.run(context.Background(), Role{Name: "implement"}, Assignment{Role: "implement"}, State{Request: "original"})
	if !strings.Contains(result.Error, "exit status 3") || !strings.Contains(result.Error, "actual build reason") {
		t.Fatalf("reason was lost: %#v", result)
	}
	if strings.Contains(result.Output+result.Error+result.Diagnostics, "synthetic-test-value") {
		t.Fatal("credential echoed into history")
	}
	if result.Output != "[credential]" {
		t.Fatalf("output=%q", result.Output)
	}
}

func TestProcessTimeoutReturnsAnObservationAndStopIsPrompt(t *testing.T) {
	process := Process{Name: "worker", Command: []string{"/bin/sh", "-c", "sleep 60"}, Timeout: 30 * time.Millisecond}
	started := time.Now()
	result := process.run(context.Background(), Role{Name: "implement"}, Assignment{Role: "implement"}, State{Request: "original"})
	if !strings.Contains(result.Error, "deadline exceeded") || result.FinishedAt.IsZero() {
		t.Fatalf("result=%#v", result)
	}
	if time.Since(started) > time.Second {
		t.Fatal("process group outlived the cancellation")
	}
}

func TestIndependentProcessesReceiveNoPeerAnswer(t *testing.T) {
	processes := Processes{Roles: map[string]Role{"review": {Name: "review", Processes: []Process{
		{Name: "a", Command: []string{"/bin/sh", "-c", "cat; printf '\nunique answer A'"}},
		{Name: "b", Command: []string{"/bin/sh", "-c", "cat; printf '\nunique answer B'"}},
	}}}}
	results := processes.Execute(context.Background(), Assignment{Role: "review"}, State{Request: "original", History: []Result{{Role: "implement", Output: "same implementation"}}})
	if len(results) != 2 || strings.Contains(results[0].Output, "unique answer B") || strings.Contains(results[1].Output, "unique answer A") {
		t.Fatalf("peer report leaked: %#v", results)
	}
	for _, result := range results {
		if !strings.Contains(result.Output, "same implementation") || result.Error != "" {
			t.Fatalf("result=%#v", result)
		}
	}
}
