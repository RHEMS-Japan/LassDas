package chain

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestProcessesUseFreshChoicesWithoutMutatingConfigOrSharingReports(t *testing.T) {
	initial := map[string]string{"MODEL": "old/configured"}
	var exclusions [][]string
	processes := Processes{Roles: map[string]Role{"review": {Name: "review", Processes: []Process{
		{Name: "a", ModelEnv: "MODEL", Env: initial, Command: []string{"/bin/sh", "-c", `printf '%s\n' "$MODEL"; cat; printf '\nanswer-a'`}},
		{Name: "b", ModelEnv: "MODEL", Env: initial, Command: []string{"/bin/sh", "-c", `printf '%s\n' "$MODEL"; cat; printf '\nanswer-b'`}},
	}}}, SelectModel: func(ctx context.Context, role Role, process Process, state State, selected []string) (string, error) {
		if role.Name != "review" || state.Request != "original" {
			t.Error("selection got another responsibility/request")
		}
		exclusions = append(exclusions, append([]string(nil), selected...))
		return "publisher-" + process.Name + "/current", nil
	}}
	results := processes.Execute(context.Background(), Assignment{Role: "review"}, State{Request: "original"})
	if initial["MODEL"] != "old/configured" || !reflect.DeepEqual(exclusions, [][]string{nil, {"publisher-a/current"}}) {
		t.Fatalf("configuration mutated or selection not separated: env=%v selected=%v", initial, exclusions)
	}
	for i, result := range results {
		want := []string{"publisher-a/current", "publisher-b/current"}[i]
		if result.Error != "" || result.Model != want || !strings.HasPrefix(result.Output, want+"\n") {
			t.Fatalf("model not delivered: %#v", result)
		}
		if strings.Contains(result.Output, []string{"answer-b", "answer-a"}[i]) {
			t.Fatal("current peer review leaked")
		}
	}
}

func TestSelectionFailureDoesNotLaunchPinnedProcess(t *testing.T) {
	process := Process{Name: "worker", ModelEnv: "MODEL", Env: map[string]string{"MODEL": "old/pinned"}, Command: []string{"/bin/sh", "-c", "printf 'must not run'"}}
	processes := Processes{Roles: map[string]Role{"implement": {Name: "implement", Processes: []Process{process}}}, SelectModel: func(context.Context, Role, Process, State, []string) (string, error) {
		return "", errors.New("catalog HTTP 503: actual reason")
	}}
	result := processes.Execute(context.Background(), Assignment{Role: "implement"}, State{Request: "original"})[0]
	if result.Output != "" || result.Model != "" || !strings.Contains(result.Error, "catalog HTTP 503: actual reason") || result.FinishedAt.IsZero() {
		t.Fatalf("failure hidden or stale worker launched: %#v", result)
	}
	processes.SelectModel = nil
	if result = processes.Execute(context.Background(), Assignment{Role: "implement"}, State{})[0]; result.Output != "" || !strings.Contains(result.Error, "no current-model selector") {
		t.Fatalf("missing selector launched stale model: %#v", result)
	}
}

func TestModelSelectionCannotReplaceASecretAndHonorsStop(t *testing.T) {
	process := Process{Name: "worker", ModelEnv: "MODEL", Secrets: map[string]string{"MODEL": "PRIVATE_KEY_SOURCE"}, Command: []string{"/bin/sh", "-c", "printf 'must not run'"}}
	called := false
	processes := Processes{Roles: map[string]Role{"implement": {Name: "implement", Processes: []Process{process}}}, SelectModel: func(context.Context, Role, Process, State, []string) (string, error) {
		called = true
		return "maker/current", nil
	}}
	result := processes.Execute(context.Background(), Assignment{Role: "implement"}, State{})[0]
	if called || result.Output != "" || !strings.Contains(result.Error, "overlaps a credential") {
		t.Fatalf("selection replaced a credential: called=%v result=%#v", called, result)
	}
	process.Secrets = nil
	processes.Roles["implement"] = Role{Name: "implement", Processes: []Process{process}}
	processes.SelectModel = func(ctx context.Context, _ Role, _ Process, _ State, _ []string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	started := time.Now()
	result = processes.Execute(ctx, Assignment{Role: "implement"}, State{})[0]
	if result.Output != "" || !strings.Contains(result.Error, "deadline exceeded") || time.Since(started) > time.Second {
		t.Fatalf("stopped selection launched a process or did not return: %#v", result)
	}
}

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
