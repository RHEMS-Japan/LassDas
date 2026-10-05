package chain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRoleAndRouterInstructionsAllowConfiguredRecoveryQuestions(t *testing.T) {
	for _, instructions := range []string{routingInstructions, processPrompt(Role{}, Process{}, Assignment{}, State{})} {
		for _, phrase := range []string{"only when the workflow offers that role", "failed check may return to requirements", "concrete alternatives", "no answer itself widens those permissions", "newly required expansion of authority", "unknown cause", "initial elicitation only"} {
			if !strings.Contains(instructions, phrase) {
				t.Errorf("recovery guidance omits %q", phrase)
			}
		}
		if strings.Contains(instructions, "before the work is handed over;") {
			t.Fatal("shared guidance still forbids recovery questions")
		}
	}
}

func TestLaunchPreparationFailureReleasesAndReturnsAnObservation(t *testing.T) {
	var attempts, released atomic.Int32
	process := Process{Name: "worker", Command: []string{"/bin/sh", "-c", `printf '%s' "$LOCAL_KEY"; printf '%s' "$LOCAL_KEY" >&2`}}
	p := Processes{Roles: map[string]Role{"report": {Name: "report", Processes: []Process{process}}},
		Prepare: func(ctx context.Context, p Process) (Process, func(), error) {
			release := func() { released.Add(1) }
			if attempts.Add(1) == 1 {
				return p, release, errors.New("local service could not listen")
			}
			p.Credentials = map[string]string{"LOCAL_KEY": "synthetic-controller-issued-access"}
			return p, release, nil
		}}
	first := p.Execute(context.Background(), Assignment{Role: "report"}, State{Request: "original"})
	if len(first) != 1 || !strings.Contains(first[0].Error, "local service could not listen") || first[0].Output != "" || released.Load() != 1 {
		t.Fatalf("preparation error was hidden: %#v release=%d", first, released.Load())
	}
	second := p.Execute(context.Background(), Assignment{Role: "report"}, State{Request: "original", History: first})
	if second[0].Error != "" || second[0].Output != "[credential]" || second[0].Diagnostics != "[credential]" || released.Load() != 2 {
		t.Fatalf("second launch failed or leaked access: %#v release=%d", second, released.Load())
	}
	encoded, err := json.Marshal(Process{Credentials: map[string]string{"LOCAL_KEY": "synthetic-controller-issued-access"}})
	if err != nil || strings.Contains(string(encoded), "synthetic") || strings.Contains(string(encoded), "LOCAL_KEY") {
		t.Fatalf("ephemeral access serialized: %s %v", encoded, err)
	}
}

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
	if strings.Contains(result.Output, "Requester comments (original words;") {
		t.Fatal("a run without requester comments received an empty comments section")
	}
}

// Exercise the runtime's actual prompt and the shipped review command. The
// local model returns a scripted verdict; this checks transport, not judgment.
func TestRequesterAnswersReachReviewModelBeyondProcessDiagnostics(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("../../harnesses/adversarial_review.py")
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	file := filepath.Join(workspace, "decision.txt")
	if err := os.WriteFile(file, []byte("Existing decision.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitEnv := []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_SYSTEM=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0"}
	for _, args := range [][]string{{"init", "-q"}, {"add", "decision.txt"},
		{"-c", "user.name=Fixture", "-c", "user.email=fixture", "commit", "-qm", "initial"}} {
		command := exec.Command("git", args...)
		command.Dir, command.Env = workspace, gitEnv
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("fixture git: %s %v", output, err)
		}
	}
	if err := os.WriteFile(file, []byte("Use dist/.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sent := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Messages []struct{ Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || len(input.Messages) != 2 {
			t.Errorf("review model input: %+v %v", input, err)
			http.Error(w, "bad fixture request", http.StatusBadRequest)
			return
		}
		select {
		case sent <- input.Messages[1].Content:
		default:
			t.Error("unexpected extra model request")
		}
		fmt.Fprint(w, `{"choices":[{"message":{"tool_calls":[{"function":{"name":"verdict","arguments":"{\"blocking\":false,\"findings\":\"fixture verdict\"}"}}]}}]}`)
	}))
	defer server.Close()
	t.Setenv("PROMPT_REVIEW_FIXTURE_KEY", "synthetic-prompt-review-key")
	process := Process{Name: "reviewer", Command: []string{python, "-B", script}, Directory: workspace, Timeout: 15 * time.Second,
		Secrets: map[string]string{"REVIEW_API_KEY": "PROMPT_REVIEW_FIXTURE_KEY"},
		Env: map[string]string{"TASK_WORKSPACE": workspace, "TASK_HOME": t.TempDir(),
			"REVIEW_MODEL_URL": server.URL, "REVIEW_MODEL": "fixture/reviewer", "REVIEW_TEST_COMMANDS": "",
			"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_SYSTEM": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1"}}
	const answer = "Use release/. Keep exactly three entries."
	state := State{Request: "Use the requester's chosen destination and retain it with the implementation.", History: []Result{
		{Role: "elicit", Speaker: "planner", Output: strings.Repeat("requirements ", 250), Diagnostics: strings.Repeat("progress ", 800)},
		{Role: "ask", Speaker: "questioner", Output: strings.Repeat("context ", 300) + "Which destination?", Diagnostics: strings.Repeat("progress ", 800)},
		{Role: "ask", Speaker: "requester", Output: answer},
		{Role: "work", Speaker: "builder", Output: strings.Repeat("work report ", 650), Diagnostics: strings.Repeat("progress ", 800)},
		{Role: "verify", Speaker: "check", Output: strings.Repeat("checks ", 350), Diagnostics: strings.Repeat("progress ", 800)},
	}}
	for _, older := range []int{0, 65} {
		t.Run(fmt.Sprint(older), func(t *testing.T) {
			current := state
			current.History = append([]Result(nil), state.History...)
			for i := 0; i < older; i++ {
				current.History = append(current.History, Result{Role: "verify", Speaker: "check", Output: strings.Repeat("later check ", 150)})
			}
			role, assignment := Role{Name: "review"}, Assignment{Role: "review"}
			prompt := processPrompt(role, process, assignment, current)
			if len(prompt) < 30000 {
				t.Fatal("fixture does not exercise a long runtime prompt")
			}
			result := process.run(context.Background(), role, assignment, current)
			if result.Error != "" || !strings.Contains(result.Output, "PASSED") {
				t.Fatalf("review command did not complete: %+v", result)
			}
			select {
			case input := <-sent:
				if !strings.Contains(input, answer) || !strings.Contains(input, "Use dist/.") {
					t.Fatalf("review model lost the answer or the contradicting diff: answer=%t diff=%t prompt_bytes=%d", strings.Contains(input, answer), strings.Contains(input, "Use dist/."), len(prompt))
				}
				t.Logf("prompt_bytes=%d requester_position=%d later_records=%d answer_in_model=true contradictory_diff_in_model=true", len(prompt), strings.Index(prompt, answer), older)
			default:
				t.Fatal("no actual model request was captured")
			}
		})
	}
}

func TestSuccessfulProcessDiagnosticsReachNextRoleWithoutBecomingAVerdict(t *testing.T) {
	t.Setenv("HANDOFF_TEST_SECRET", "synthetic-handoff-secret")
	worker := Process{Name: "worker", Command: []string{"/bin/sh", "-c",
		`printf 'My part is ready.'; printf 'Earlier tool attempt failed: %s\nLater terminal command returned zero.\n' "$ROLE_KEY" >&2`},
		Secrets: map[string]string{"ROLE_KEY": "HANDOFF_TEST_SECRET"}}
	state := State{Request: "Deliver the original request. Unknown {\"field\":true}."}
	result := worker.run(context.Background(), Role{Name: "implement"}, Assignment{Role: "implement"}, state)
	if result.Error != "" || result.Output != "My part is ready." {
		t.Fatalf("ordinary successful exit was turned into a failure: %#v", result)
	}
	const diagnostics = "Earlier tool attempt failed: [credential]\nLater terminal command returned zero.\n"
	if result.Diagnostics != diagnostics {
		t.Fatalf("redacted diagnostics changed: %q", result.Diagnostics)
	}
	state.History = []Result{result}
	reader := Process{Name: "next", Command: []string{"/bin/cat"}}
	next := reader.run(context.Background(), Role{Name: "review"}, Assignment{Role: "review"}, state)
	if next.Error != "" || !strings.Contains(next.Output, state.Request) || !strings.Contains(next.Output, result.Output) ||
		!strings.Contains(next.Output, diagnostics) || strings.Contains(next.Output, "synthetic-handoff-secret") {
		t.Fatalf("next role lost observations or received a credential: %#v", next)
	}
	if state.History[0].Error != "" || state.History[0].Diagnostics != diagnostics {
		t.Fatal("handoff reclassified or changed the earlier result")
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

func TestOnlyTheOperatorsMinutesBoundALaunch(t *testing.T) {
	if got := (Process{}).timeLimit(); got != 0 {
		t.Fatalf("a launch has a limit nobody configured: %v", got)
	}
	if got := (Process{TimeoutMinutes: 240}).timeLimit(); got != 4*time.Hour {
		t.Fatalf("operator minutes ignored: %v", got)
	}
	if got := (Process{TimeoutMinutes: 240, Timeout: time.Second}).timeLimit(); got != time.Second {
		t.Fatalf("a caller's duration lost to the operator's minutes: %v", got)
	}
	var decoded Process
	if err := json.Unmarshal([]byte(`{"name":"p","command":["true"],"timeout_minutes":90}`), &decoded); err != nil || decoded.timeLimit() != 90*time.Minute {
		t.Fatalf("timeout_minutes not read from configuration: %v %v", decoded.timeLimit(), err)
	}
}

func TestALaunchIsBoundedOnlyByAConfiguredLimit(t *testing.T) {
	ctx, cancel := (Process{}).launchContext(context.Background())
	defer cancel()
	if _, bounded := ctx.Deadline(); bounded {
		t.Fatal("a launch with no configured limit was given a deadline")
	}
	ctx, cancel = (Process{TimeoutMinutes: 2}).launchContext(context.Background())
	defer cancel()
	if deadline, bounded := ctx.Deadline(); !bounded || time.Until(deadline) > 2*time.Minute || time.Until(deadline) < time.Minute {
		t.Fatalf("the configured limit was not applied: %v %v", deadline, bounded)
	}
}

func TestOneLaunchKeepsOnlyTheTailOfAnEndlessStream(t *testing.T) {
	var b boundedBuffer
	chunk := bytes.Repeat([]byte("x"), 1<<20)
	for i := 0; i < 10; i++ {
		if n, err := b.Write(chunk); err != nil || n != len(chunk) {
			t.Fatalf("write %d: %d %v", i, n, err)
		}
	}
	b.Write([]byte("the end"))
	kept := b.String()
	if !strings.HasPrefix(kept, "[the first 2097159 bytes of this stream are not kept") || !strings.HasSuffix(kept, "the end") || len(kept) > keptBytes+200 {
		t.Fatalf("kept %d bytes, head %q", len(kept), kept[:80])
	}
	var small boundedBuffer
	small.Write([]byte("all of it"))
	if small.String() != "all of it" {
		t.Fatalf("a small stream was altered: %q", small.String())
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

func TestProcessCancellationLetsHarnessCleanUpBeforeReturning(t *testing.T) {
	dir := t.TempDir()
	process := Process{Name: "cleanup-aware", Directory: dir, Command: []string{"/bin/sh", "-c", `trap 'printf cleaned > cleaned; exit 0' TERM; printf 'partial work'; printf ready > ready; while :; do sleep 0.05; done`}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan Result, 1)
	go func() {
		result <- process.run(ctx, Role{Name: "implement"}, Assignment{Role: "implement"}, State{Request: "original"})
	}()
	waitProcessFile(t, filepath.Join(dir, "ready"))
	cancel()
	var got Result
	select {
	case got = <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("cooperative harness did not stop")
	}
	if _, err := os.Stat(filepath.Join(dir, "cleaned")); err != nil {
		t.Fatal("harness was killed without running its cleanup")
	}
	if got.Output != "partial work" || !strings.Contains(got.Error, "context canceled") {
		t.Fatalf("cancellation erased partial work or looked successful: %#v", got)
	}
}

func TestProcessCancellationForcesAnUnresponsiveHarnessToStop(t *testing.T) {
	dir := t.TempDir()
	// Finite even when an escalation mutation breaks the cleanup under test.
	process := Process{Name: "ignores-term", Directory: dir, Command: []string{"/bin/sh", "-c", `trap '' TERM; printf ready > ready; i=0; while [ "$i" -lt 160 ]; do sleep 0.05; i=$((i+1)); done`}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan Result, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		result <- process.run(ctx, Role{Name: "implement"}, Assignment{Role: "implement"}, State{Request: "original"})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Error("bounded shutdown fixture was not reaped")
		}
	})
	waitProcessFile(t, filepath.Join(dir, "ready"))
	started := time.Now()
	cancel()
	select {
	case got := <-result:
		if !strings.Contains(got.Error, "context canceled") || time.Since(started) < 2800*time.Millisecond || time.Since(started) > 4500*time.Millisecond {
			t.Fatalf("shutdown did not allow the cleanup period: elapsed=%s result=%#v", time.Since(started), got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("unresponsive harness survived the shutdown period")
	}
}

func waitProcessFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("known harness did not become ready")
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

func TestALaterRolesPromptCarriesOnlyTheTailOfLongDiagnostics(t *testing.T) {
	long := strings.Repeat("step line\n", 1000) + "LAST-LINE"
	state := State{Request: "r", History: []Result{{Role: "implement", Speaker: "p", Output: "done", Diagnostics: long, Error: "exit status 1\n" + long}}}
	prompt := processPrompt(Role{Name: "verify"}, Process{Name: "v"}, Assignment{Role: "verify"}, state)
	if strings.Count(prompt, "step line") >= 1000 {
		t.Fatal("the whole diagnostics travelled in the prompt")
	}
	if !strings.Contains(prompt, "LAST-LINE") || !strings.Contains(prompt, "earlier characters are in the record, not in this prompt") {
		t.Fatalf("the tail and the marker are missing: %.300s", prompt)
	}
	if strings.Count(prompt, "earlier characters are in the record") != 2 {
		t.Fatalf("both the diagnostics and the error should be cut once each: %d", strings.Count(prompt, "earlier characters are in the record"))
	}
	if promptTail("short") != "short" {
		t.Fatal("a short text must pass unchanged")
	}
}

func TestAProcessIsToldWhichOfItsVariablesAreCredentials(t *testing.T) {
	t.Setenv("PROCESS_TEST_SOURCE", "source-value-1")
	process := Process{Name: "p", Command: []string{"/bin/sh", "-c", "printf %s \"$TASK_CREDENTIAL_NAMES\""},
		Secrets: map[string]string{"ROLE_KEY": "PROCESS_TEST_SOURCE"}, Credentials: map[string]string{"TASK_TRACKER_KEY": "issued-value"}}
	result := process.run(context.Background(), Role{Name: "r"}, Assignment{Role: "r"}, State{})
	if result.Error != "" || result.Output != "ROLE_KEY:TASK_TRACKER_KEY" {
		t.Fatalf("%+v", result)
	}
	plain := Process{Name: "p", Command: []string{"/bin/sh", "-c", "printf %s \"${TASK_CREDENTIAL_NAMES-unset}\""}}
	if result := plain.run(context.Background(), Role{Name: "r"}, Assignment{Role: "r"}, State{}); result.Output != "unset" {
		t.Fatalf("a process without credentials got %q", result.Output)
	}
}

func TestAPromptCollapsesRepeatedFailuresAndCapsTheRecords(t *testing.T) {
	var history []Result
	history = append(history, Result{Role: "elicit", Speaker: "p", Output: "settled"})
	for i := 0; i < 150; i++ {
		history = append(history, Result{Role: "draft_report", Speaker: "p", Error: "exit status 1\nTraceback: the same"})
		history = append(history, Result{Role: "draft_report", Speaker: "runtime", Output: "Runtime record"})
	}
	carried := promptHistory(history)
	if len(carried) != 3 {
		t.Fatalf("a staged run of one failure must carry the elicit record, the newest failure and its stage record: %d", len(carried))
	}
	if !strings.HasPrefix(carried[1].Error, "(this failure repeated 150 times in a row; this is the latest)") {
		t.Fatalf("the count is missing: %q", carried[1].Error)
	}
	if history[1].Error != "exit status 1\nTraceback: the same" {
		t.Fatal("the record on disk must not change")
	}
	different := []Result{
		{Role: "r", Speaker: "p", Error: "exit status 1\nbwrap: no such path", Diagnostics: "d1"},
		{Role: "r", Speaker: "runtime", Output: "stage"},
		{Role: "r", Speaker: "p", Error: "exit status 1\nKeyError: HOME", Diagnostics: "d2"},
		{Role: "r", Speaker: "runtime", Output: "stage"},
	}
	if got := promptHistory(different); len(got) != 4 || got[2].Error != "exit status 1\nKeyError: HOME" {
		t.Fatalf("two different failures must both travel: %+v", got)
	}
	newest := []Result{
		{Role: "r", Speaker: "p", Error: "same", Output: "first attempt", Diagnostics: "old"},
		{Role: "r", Speaker: "p", Error: "same", Output: "second attempt", Diagnostics: "new"},
		{Role: "r", Speaker: "p", Output: "worked"},
		{Role: "r", Speaker: "p", Error: "same"},
	}
	got := promptHistory(newest)
	if len(got) != 3 || got[0].Diagnostics != "new" || got[0].Output != "second attempt" || !strings.HasPrefix(got[0].Error, "(this failure repeated 2 times") || got[2].Error != "same" {
		t.Fatalf("the newest failure of a run travels and a success ends the run: %+v", got)
	}
	var long []Result
	for i := 0; i < 100; i++ {
		long = append(long, Result{Role: "r", Speaker: "p", Output: fmt.Sprintf("work %d", i)})
	}
	capped := promptHistory(long)
	if len(capped) != promptRecords+1 || !strings.Contains(capped[0].Output, "40 earlier records are in the request's history") {
		t.Fatalf("the cap and its note: %d %q", len(capped), capped[0].Output)
	}
	prompt := processPrompt(Role{Name: "confirm"}, Process{Name: "c"}, Assignment{Role: "confirm"}, State{History: history})
	if strings.Count(prompt, "Traceback: the same") != 1 {
		t.Fatalf("the prompt repeats the failure %d times", strings.Count(prompt, "Traceback: the same"))
	}
}

// The runtime tells the person who filed the request which model took their
// work on, at the moment the stage begins. The history cannot answer that
// yet, so the choice is handed over before the child runs, and a selection
// that failed hands over nothing because nothing ran.
func TestTheChosenModelIsHandedOverBeforeTheChildRuns(t *testing.T) {
	directory := t.TempDir()
	marker := filepath.Join(directory, "ran.txt")
	var told []string
	childHadRun := false
	processes := Processes{
		Roles: map[string]Role{"work": {Name: "work", Processes: []Process{
			{Name: "worker", Directory: directory, ModelEnv: "MODEL",
				Command: []string{"/bin/sh", "-c", `printf '%s' "$MODEL" > ran.txt`}},
		}}},
		ModelPrefix: "gateway/",
		SelectModel: func(context.Context, Role, Process, State, []string) (string, error) {
			return "maker/current", nil
		},
		Chosen: func(role, process, model string, launch int) {
			told = append(told, fmt.Sprintf("%s %s %s %d", role, process, model, launch))
			if _, err := os.Stat(marker); err == nil {
				childHadRun = true
			}
		},
	}
	result := processes.Execute(context.Background(), Assignment{Role: "work"}, State{})[0]
	if result.Error != "" || result.Model != "maker/current" || result.ModelPrefix != "gateway/" {
		t.Fatalf("launch: %#v", result)
	}
	if len(told) != 1 || told[0] != "work worker maker/current 0" {
		t.Fatalf("the chosen model was handed over as %v", told)
	}
	if childHadRun {
		t.Fatal("the child had already run when the chosen model was handed over")
	}
	// The harness is reached through the gateway; what a person is told is the
	// catalog id, the same id the record keeps.
	if data, err := os.ReadFile(marker); err != nil || string(data) != "gateway/maker/current" {
		t.Fatalf("the harness was given %q (%v)", data, err)
	}
	// A later launch begins after more records, so its choice is told apart
	// from the first one's.
	told = nil
	processes.Execute(context.Background(), Assignment{Role: "work"}, State{History: make([]Result, 3)})
	if len(told) != 1 || told[0] != "work worker maker/current 3" {
		t.Fatalf("a later launch was handed over as %v", told)
	}
	processes.SelectModel = func(context.Context, Role, Process, State, []string) (string, error) {
		return "", errors.New("catalog HTTP 503: actual reason")
	}
	told = nil
	if result := processes.Execute(context.Background(), Assignment{Role: "work"}, State{})[0]; result.Error == "" || len(told) != 0 {
		t.Fatalf("a failed selection handed over %v: %#v", told, result)
	}
}
