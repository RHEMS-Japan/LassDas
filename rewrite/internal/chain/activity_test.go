package chain

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A launch cut by a forced exit leaves nothing of its own. The note written
// for it after the restart says what the role was running then, and the
// launch that follows is told how the previous one ended and how many in a
// row ended so, instead of walking into the same end without knowing.
func TestALaunchAfterAForcedExitIsToldWhatThePreviousOneWasRunning(t *testing.T) {
	const doing = "Last command: terminal: cargo build --release (it had not returned). Running in the background: cargo build --release."
	began := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	store := &memoryStore{state: State{Request: "Build it.", Workflow: stagesWorkflow(), Step: "work",
		Pending: &Assignment{Role: "work", Instruction: "earlier"}, PendingSince: began,
		History: []Result{
			{Role: "elicit", Speaker: "requirements", Output: "settled", StartedAt: began.Add(-time.Hour)},
			{Role: "elicit", Speaker: "runtime", Output: "Process requirements exited 0.", StartedAt: began.Add(-time.Hour)},
		}}}
	var told []string
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	engine := Chain{Store: store, Workflow: stagesWorkflow(), Router: StageRouter{}, RetryDelay: time.Millisecond,
		Activity: func(role string) string {
			if role != "work" {
				t.Errorf("the activity of %q was read for a cut launch of work", role)
			}
			return doing
		},
		// Each launch taken up is cut short again; the test lays down the
		// forced exit itself below.
		Executor: testExecutor(func(_ context.Context, a Assignment, _ State) []Result {
			told = append(told, a.Instruction)
			cancel()
			return nil
		}),
	}
	_ = engine.Run(ctx)
	note := store.state.History[2]
	if note.Speaker != "runtime" || !note.Interrupted || !note.Forced || note.Activity != doing {
		t.Fatalf("the note for the cut launch is %+v", note)
	}
	if !strings.Contains(note.Output, "killed") || !strings.Contains(note.Output, doing) {
		t.Fatalf("the note's own words do not say what was going on: %q", note.Output)
	}
	if len(told) != 1 {
		t.Fatalf("launches: %q", told)
	}
	for _, want := range []string{
		"The previous launch of work (attempt 1 in a row that did not end cleanly) was cut off when the runtime itself was killed (lack of memory is one cause).",
		doing,
		"Do not repeat what it did unchanged: suspect the cause (memory, time or wrong arguments) and change the plan.",
	} {
		if !strings.Contains(told[0], want) {
			t.Fatalf("the launch after the forced exit was not told %q:\n%s", want, told[0])
		}
	}

	// The launch taken up is cut by a second forced exit, with nothing saved.
	store.state.Pending, store.state.PendingSince = &Assignment{Role: "work"}, time.Now().UTC().Add(-time.Second)
	store.state.History = store.state.History[:3]
	engine.Activity = func(string) string { return "" }
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	told = nil
	_ = engine.Run(ctx)
	if len(told) != 1 || !strings.Contains(told[0], "(attempt 2 in a row that did not end cleanly) was cut off when the runtime itself was killed") {
		t.Fatalf("the second cut in a row was not counted: %q", told)
	}
	// No record of the second launch's commands is said as unknown, and the
	// launch goes ahead all the same.
	if !strings.Contains(told[0], "Its last command is unknown: the role left no record of it.") {
		t.Fatalf("a missing record was not said as unknown:\n%s", told[0])
	}
	if !strings.Contains(store.state.History[3].Output, "Its last command is unknown") {
		t.Fatalf("the second note left its words empty: %+v", store.state.History[3])
	}
}

// A launch the runtime stopped itself, its results saved with the action still
// pending, is not a forced exit, and its saved results and the note written
// after the restart are one launch, not two.
func TestAStoppedLaunchIsOneLaunchAndNotAForcedExit(t *testing.T) {
	began := time.Now().UTC().Add(-time.Minute)
	store := &memoryStore{state: State{Request: "Build it.", Workflow: stagesWorkflow(), Step: "work",
		Pending: &Assignment{Role: "work"}, PendingSince: began,
		History: []Result{
			{Role: "elicit", Speaker: "requirements", Output: "settled", StartedAt: began.Add(-time.Hour)},
			{Role: "elicit", Speaker: "runtime", Output: "Process requirements exited 0.", StartedAt: began.Add(-time.Hour)},
			{Role: "work", Speaker: "worker", Error: "context canceled", Interrupted: true, Activity: "Last command: terminal: make (it had not returned). Nothing was running in the background.", StartedAt: began.Add(time.Second)},
			{Role: "work", Speaker: "runtime", Output: "Process worker did not exit 0: context canceled", StartedAt: began.Add(2 * time.Second)},
		}}}
	var told []string
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	engine := Chain{Store: store, Workflow: stagesWorkflow(), Router: StageRouter{}, RetryDelay: time.Millisecond,
		Activity: func(string) string { return "" },
		Executor: testExecutor(func(_ context.Context, a Assignment, _ State) []Result {
			told = append(told, a.Instruction)
			cancel()
			return nil
		})}
	_ = engine.Run(ctx)
	if note := store.state.History[4]; !note.Interrupted || note.Forced {
		t.Fatalf("a stopped launch was noted as a forced exit: %+v", note)
	}
	if len(told) != 1 || !strings.Contains(told[0], "(attempt 1 in a row that did not end cleanly) was cut off when the runtime stopped. Last command: terminal: make") {
		t.Fatalf("the stopped launch was told as %q", told)
	}
}

// A model stage whose process does not exit 0 is launched again, and that
// launch is told the error and what the role last ran; a failed command
// stage hands its own words to its on_failure stage and is not told this way.
func TestAFailedLaunchOfTheSameStageIsToldWhatItRanLast(t *testing.T) {
	store := &memoryStore{state: State{Request: "Build it."}}
	var work, verify []string
	failures := 0
	engine := Chain{Store: store, Workflow: stagesWorkflow(), Router: StageRouter{}, RetryDelay: time.Millisecond,
		Executor: testExecutor(func(_ context.Context, a Assignment, _ State) []Result {
			switch a.Role {
			case "work":
				work = append(work, a.Instruction)
				if failures < 2 {
					failures++
					return []Result{{Role: a.Role, Speaker: "worker", Error: "exit status 137", StartedAt: time.Now().UTC().Add(-time.Minute), FinishedAt: time.Now().UTC(),
						Activity: "Last command: terminal: cargo build (it had not returned). Nothing was running in the background."}}
				}
			case "verify":
				verify = append(verify, a.Instruction)
				if len(verify) == 1 {
					return []Result{{Role: a.Role, Speaker: "project-tests", Error: "exit status 1"}}
				}
			}
			return []Result{{Role: a.Role, Speaker: "worker", Output: "ok"}}
		})}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := engine.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(work) != 4 {
		t.Fatalf("work ran %d times", len(work))
	}
	if strings.Contains(work[0], "The previous launch") {
		t.Fatalf("the first launch was told of a previous one:\n%s", work[0])
	}
	for i, attempt := range []string{"1", "2"} {
		want := "The previous launch of work (attempt " + attempt + " in a row that did not end cleanly) ended with a process error, which is in the record below. Last command: terminal: cargo build (it had not returned)."
		if !strings.Contains(work[i+1], want) {
			t.Fatalf("launch %d of work was not told %q:\n%s", i+2, want, work[i+1])
		}
	}
	if strings.Contains(work[3], "The previous launch") {
		t.Fatalf("the launch after the failed check was told of a failed launch of its own:\n%s", work[3])
	}
}

// The record is the harness's, read back plainly: a missing, oversized or
// unreadable one says nothing, and what the runtime knows as a credential is
// replaced before the words reach the history.
func TestTheActivityRecordIsReadBackBoundedAndWithoutCredentials(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "task-activity.json")
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := readActivity(path, nil); got != "" {
		t.Fatalf("a missing record said %q", got)
	}
	for _, text := range []string{"not json", `{"last":""}`, `{"last":"x","background":` + strings.Repeat(" ", activityBytes) + `[]}`} {
		write(text)
		if got, _ := readActivity(path, nil); got != "" {
			t.Fatalf("record %.40q said %q", text, got)
		}
	}
	write(`{"last":"terminal: curl -H 'token: synthetic-secret' https://example.invalid\n` + strings.Repeat("y", 300) + `","returned":true,"background":["a","b","c","d","e","f"]}`)
	got, repeated := readActivity(path, []string{"synthetic-secret"})
	if strings.Contains(got, "synthetic-secret") || !strings.Contains(got, "[credential]") {
		t.Fatalf("a credential reached the words: %q", got)
	}
	if !strings.HasPrefix(got, "Last command: terminal: curl") || !strings.Contains(got, "… (it had returned). Running in the background: a; b; c; d; e.") || repeated != 0 {
		t.Fatalf("the record was read as %q, %d", got, repeated)
	}
	// The bridge cuts each command at 199 characters, not bytes: a record of
	// six such commands in characters of four bytes is still read.
	wide := strings.Repeat("😀", 199)
	write(`{"last":"` + wide + `","background":["` + strings.Join([]string{wide, wide, wide, wide, wide}, `","`) + `"]}`)
	if got, _ := readActivity(path, nil); !strings.HasPrefix(got, "Last command: 😀") || !strings.Contains(got, "Running in the background: 😀") {
		t.Fatalf("a record of wide characters was read as %.80q", got)
	}
	write(`{"last":"read_file: README.md"}`)
	if got, _ := readActivity(path, nil); got != "Last command: read_file: README.md. Nothing was running in the background." {
		t.Fatalf("a record without the returned flag was read as %q", got)
	}
	// A role its harness ended for one call that kept failing says so, with
	// the count kept apart; another rule is named by its plain letters only.
	for _, shape := range []struct {
		record, want string
		repeated     int
	}{
		{`{"last":"process: wait","returned":true,"background":["cargo build"],"halted":{"code":"repeated_identical_failure","count":5}}`,
			"The role stopped itself after one tool call failed the same way 5 times in a row. Last command: process: wait (it had returned). Running in the background: cargo build.", 5},
		{`{"last":"","halted":{"code":"same_tool_failure_halt\n<b>","count":2}}`,
			"The role's tool-call guardrail ended it (same_tool_failure_haltb). Its last command is unknown. Nothing was running in the background.", 0},
	} {
		write(shape.record)
		if got, repeated := readActivity(path, nil); got != shape.want || repeated != shape.repeated {
			t.Fatalf("a halted record was read as %q, %d", got, repeated)
		}
	}
}

// The runtime removes the previous launch's record before each launch, and
// reads the one the role wrote back only when the launch did not end cleanly.
func TestALaunchReadsBackOnlyItsOwnRecordAndOnlyWhenItFailed(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "task-activity.json")
	if err := os.WriteFile(path, []byte(`{"last":"stale: from an earlier launch"}`), 0600); err != nil {
		t.Fatal(err)
	}
	role := Role{Name: "work"}
	run := func(script string) Result {
		t.Helper()
		process := Process{Name: "worker", Command: []string{"/bin/sh", "-c", script}, Env: map[string]string{ActivityEnv: path}}
		return process.run(context.Background(), role, Assignment{Role: "work"}, State{})
	}
	if result := run("exit 3"); result.Error == "" || result.Activity != "" {
		t.Fatalf("a launch that wrote nothing read an earlier launch's record: %+v", result)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the earlier record was left in place: %v", err)
	}
	result := run(`printf '{"last":"terminal: cargo build","returned":false,"background":[]}' > "$TASK_ACTIVITY"; exit 3`)
	if result.Activity != "Last command: terminal: cargo build (it had not returned). Nothing was running in the background." || result.RepeatedFailures != 0 {
		t.Fatalf("the failed launch read back %q", result.Activity)
	}
	result = run(`printf '{"last":"process: wait","halted":{"code":"repeated_identical_failure","count":5}}' > "$TASK_ACTIVITY"; exit 1`)
	if result.RepeatedFailures != 5 || !strings.HasPrefix(result.Activity, "The role stopped itself") {
		t.Fatalf("the launch its harness ended read back %+v", result)
	}
	if result := run(`printf '{"last":"terminal: true"}' > "$TASK_ACTIVITY"`); result.Error != "" || result.Activity != "" {
		t.Fatalf("a launch that exited 0 carried a record: %+v", result)
	}
	processes := Processes{Roles: map[string]Role{"work": {Name: "work", Processes: []Process{
		{Name: "worker", Env: map[string]string{ActivityEnv: path}},
		{Name: "no-record"},
	}}}}
	if got := processes.LastActivity("work"); got != "Process worker: Last command: terminal: true. Nothing was running in the background." {
		t.Fatalf("the role's activity was read as %q", got)
	}
	if got := processes.LastActivity("missing"); got != "" {
		t.Fatalf("a role without processes said %q", got)
	}
}

// How a launch ended is read from what the runtime recorded of it: its time
// limit, a process that could not select a model, a role its harness ended
// for one call that kept failing, and a plain process error.
func TestTheEndingOfALaunchIsReadFromItsRecords(t *testing.T) {
	record := Result{Role: "work", Speaker: "runtime", Output: "Runtime record."}
	for _, shape := range []struct {
		name    string
		process Result
		check   func(Ending) bool
		says    string
	}{
		{"its time limit", Result{Error: "context deadline exceeded\nlast words"}, func(e Ending) bool { return e.TimedOut }, "was stopped at its time limit"},
		{"no model", Result{Error: selectionFailure + "catalog HTTP 503"}, func(e Ending) bool { return e.NoModel }, "could not select a current model for every process"},
		{"the same call failing", Result{Error: "exit status 1", RepeatedFailures: 5}, func(e Ending) bool { return e.RepeatedFailures == 5 }, "ended with a process error"},
		{"an error", Result{Error: "exit status 1"}, func(e Ending) bool { return !e.TimedOut && !e.NoModel && !e.Interrupted }, "ended with a process error"},
	} {
		t.Run(shape.name, func(t *testing.T) {
			shape.process.Role, shape.process.Speaker = "work", "worker"
			ending, ok := State{History: []Result{shape.process, record}}.LastEnding()
			if !ok || ending.Role != "work" || ending.Attempt != 1 || !shape.check(ending) || !strings.Contains(ending.instruction(), shape.says) {
				t.Fatalf("the ending was read as %+v: %s", ending, ending.instruction())
			}
		})
	}
	if _, ok := (State{History: []Result{{Role: "work", Speaker: "worker"}, record}}).LastEnding(); ok {
		t.Fatal("a launch that ended cleanly was read as one that did not")
	}
}

// The role can put anything at the record's name. A named pipe there must not
// hold the launch, its stop or the note written after a restart, waiting for a
// writer that never comes; a link must not lead the runtime to read a file
// outside the role's home; a directory or a second name for another file says
// nothing either. Each of them reads as no record: the last command unknown.
func TestARecordTheRoleReplacedIsNeitherWaitedOnNorFollowed(t *testing.T) {
	// within fails the test instead of hanging it, and opens the pipe for
	// writing so a reader that did block is let go.
	within := func(t *testing.T, path string, read func() string) string {
		t.Helper()
		done := make(chan string, 1)
		go func() { done <- read() }()
		select {
		case got := <-done:
			return got
		case <-time.After(5 * time.Second):
			if file, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
				file.Close()
			}
			t.Fatal("reading the record waited on what the role put there")
			return ""
		}
	}
	directory := t.TempDir()
	outside := filepath.Join(directory, "outside.json")
	if err := os.WriteFile(outside, []byte(`{"last":"a file outside the home"}`), 0600); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(directory, "home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "task-activity.json")
	for _, shape := range []struct {
		name string
		make func() error
	}{
		{"a named pipe", func() error { return syscall.Mkfifo(path, 0600) }},
		// A pipe whose writer stays open and writes nothing.
		{"a named pipe held open", func() error {
			if err := syscall.Mkfifo(path, 0600); err != nil {
				return err
			}
			writer, err := os.OpenFile(path, os.O_RDWR, 0)
			if err == nil {
				t.Cleanup(func() { writer.Close() })
			}
			return err
		}},
		{"a link to a file outside", func() error { return os.Symlink(outside, path) }},
		{"a second name of a file outside", func() error { return os.Link(outside, path) }},
		{"a directory", func() error { return os.Mkdir(path, 0700) }},
	} {
		t.Run(shape.name, func(t *testing.T) {
			os.RemoveAll(path)
			if err := shape.make(); err != nil {
				t.Fatal(err)
			}
			if got := within(t, path, func() string { got, _ := readActivity(path, nil); return got }); got != "" {
				t.Fatalf("the record was read as %q", got)
			}
			processes := Processes{Roles: map[string]Role{"work": {Name: "work", Processes: []Process{{Name: "worker", Env: map[string]string{ActivityEnv: path}}}}}}
			if got := within(t, path, func() string { return processes.LastActivity("work") }); got != "" {
				t.Fatalf("the note after a restart read %q", got)
			}
		})
	}
	// A role that makes its record a named pipe and exits 3: the launch
	// returns, with its error and no record.
	os.RemoveAll(path)
	process := Process{Name: "worker", Command: []string{"/bin/sh", "-c", `mkfifo "$TASK_ACTIVITY" && exit 3`}, Env: map[string]string{ActivityEnv: path}}
	var result Result
	within(t, path, func() string {
		result = process.run(context.Background(), Role{Name: "work"}, Assignment{Role: "work"}, State{})
		return ""
	})
	if !strings.Contains(result.Error, "exit status 3") || result.Activity != "" {
		t.Fatalf("the launch whose role left a named pipe returned %+v", result)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("the role did not leave a named pipe, so nothing was tried: %v %v", info, err)
	}
	// Positive control: an ordinary file at the same name is read.
	os.RemoveAll(path)
	if err := os.WriteFile(path, []byte(`{"last":"terminal: true"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if got, _ := readActivity(path, nil); got != "Last command: terminal: true. Nothing was running in the background." {
		t.Fatalf("an ordinary record was read as %q", got)
	}
}

// A command stage cut by a forced exit hands the work to its on_failure model
// stage, which is told how the check ended though it did not run it itself.
func TestTheStageAfterACutCommandStageIsToldHowItEnded(t *testing.T) {
	began := time.Now().UTC().Add(-time.Minute)
	store := &memoryStore{state: State{Request: "Build it.", Workflow: stagesWorkflow(), Step: "verify",
		Pending: &Assignment{Role: "verify"}, PendingSince: began,
		History: []Result{
			{Role: "elicit", Speaker: "requirements", Output: "settled", StartedAt: began.Add(-time.Hour)},
			{Role: "elicit", Speaker: "runtime", Output: "Process requirements exited 0.", StartedAt: began.Add(-time.Hour)},
			{Role: "work", Speaker: "worker", Output: "built", StartedAt: began.Add(-time.Hour)},
			{Role: "work", Speaker: "runtime", Output: "Process worker exited 0.", StartedAt: began.Add(-time.Hour)},
		}}}
	var told []string
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	engine := Chain{Store: store, Workflow: stagesWorkflow(), Router: StageRouter{}, RetryDelay: time.Millisecond,
		Activity: func(string) string { return "" },
		Executor: testExecutor(func(_ context.Context, a Assignment, _ State) []Result {
			told = append(told, a.Role+": "+a.Instruction)
			cancel()
			return nil
		})}
	_ = engine.Run(ctx)
	if len(told) != 1 || !strings.HasPrefix(told[0], "work: ") ||
		!strings.Contains(told[0], "The previous launch of verify (attempt 1 in a row that did not end cleanly) was cut off when the runtime itself was killed") {
		t.Fatalf("the on_failure stage after the cut check was told %q", told)
	}
}

// The launches in a row that did not end cleanly are counted by how each
// ended, so a run of mixed endings is not said as if all ended like the last.
func TestLaunchesInARowAreCountedByHowEachEnded(t *testing.T) {
	at := time.Now().UTC()
	failed := func(result Result) []Result {
		result.Role, result.Speaker, result.StartedAt = "work", "worker", at
		return []Result{result, {Role: "work", Speaker: "runtime", Output: "Runtime record.", StartedAt: at}}
	}
	forced := []Result{{Role: "work", Speaker: "runtime", Interrupted: true, Forced: true, Error: "stopped", StartedAt: at}}
	errored := failed(Result{Error: "exit status 1"})
	noModel := failed(Result{Error: selectionFailure + "catalog HTTP 503"})
	join := func(parts ...[]Result) (all []Result) {
		for _, part := range parts {
			all = append(all, part...)
		}
		return all
	}
	for _, shape := range []struct {
		name    string
		history []Result
		kinds   EndingKinds
		says    string
	}{
		{"a forced exit, then an error", join(forced, errored), EndingKinds{Forced: 1, Errors: 1},
			"(attempt 2 in a row that did not end cleanly: 1 forced exit, 1 process error) ended with a process error"},
		{"no model, then an error", join(noModel, errored), EndingKinds{NoModel: 1, Errors: 1},
			"(attempt 2 in a row that did not end cleanly: 1 launch without a model, 1 process error) ended with a process error"},
		{"errors around a forced exit", join(errored, errored, forced, errored), EndingKinds{Forced: 1, Errors: 3},
			"(attempt 4 in a row that did not end cleanly: 1 forced exit, 3 process errors) ended with a process error"},
		{"errors only", join(errored, errored), EndingKinds{Errors: 2},
			"(attempt 2 in a row that did not end cleanly) ended with a process error"},
	} {
		t.Run(shape.name, func(t *testing.T) {
			ending, ok := State{History: shape.history}.LastEnding()
			if !ok || ending.Kinds != shape.kinds || !strings.Contains(ending.instruction(), shape.says) {
				t.Fatalf("read as %+v: %s", ending, ending.instruction())
			}
		})
	}
}
