package chain

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type testJudge func(context.Context, State, string, map[string]string) (string, error)

func (f testJudge) Choose(ctx context.Context, s State, text string, roles map[string]string) (string, error) {
	return f(ctx, s, text, roles)
}

func TestRouterPreservesFullReportsAndEarlierObjections(t *testing.T) {
	s := State{Request: "Deliver the actual requested behavior, not a report of failure."}
	for i := 0; i < 15; i++ {
		s.History = append(s.History, Result{Role: "review", Output: "opening\n" + strings.Repeat("ordinary prose. ", 100) + "unresolved: the published artifact is still the old one\n" + strings.Repeat("detail. ", 100),
			Diagnostics: "Earlier tool attempt: upload receipt missing.\nThis is not a verdict about the final state."})
	}
	r := DecisionRouter{Roles: map[string]string{"investigate": "inspect the artifact"}, Judge: testJudge(func(_ context.Context, got State, instructions string, _ map[string]string) (string, error) {
		if !reflect.DeepEqual(got, s) {
			t.Error("the router lost earlier reports, diagnostics, or the middle of a report")
		}
		if !strings.Contains(instructions, "not an earlier implementation") || !strings.Contains(instructions, "not independent reviews of the changed work") {
			t.Error("the current-work review instruction did not reach the decision model")
		}
		return "investigate", nil
	})}
	if _, err := r.Next(context.Background(), s); err != nil {
		t.Fatal(err)
	}
}

type testRouter func(context.Context, State) (Assignment, error)

func (f testRouter) Next(ctx context.Context, s State) (Assignment, error) { return f(ctx, s) }

type testExecutor func(context.Context, Assignment, State) []Result

func (f testExecutor) Execute(ctx context.Context, a Assignment, s State) []Result {
	return f(ctx, a, s)
}

type memoryStore struct {
	state                      State
	loadFailures, saveFailures int
}

func (s *memoryStore) Load() (State, error) {
	if s.loadFailures > 0 {
		s.loadFailures--
		return State{}, errors.New("temporary read failure")
	}
	return s.state, nil
}
func (s *memoryStore) Save(state State) error {
	if s.saveFailures > 0 {
		s.saveFailures--
		return errors.New("temporary write failure")
	}
	s.state = state
	return nil
}

func TestTemporaryStorageOutageDoesNotAbandonAcceptedRequest(t *testing.T) {
	store := &memoryStore{state: State{Request: "original request"}, loadFailures: 2, saveFailures: 2}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	launches := 0
	engine := Chain{Store: store, RetryDelay: time.Millisecond,
		Router: testRouter(func(_ context.Context, s State) (Assignment, error) {
			if len(s.History) == 0 {
				return Assignment{Role: "implement"}, nil
			}
			return Assignment{Role: "done"}, nil
		}),
		Executor: testExecutor(func(_ context.Context, a Assignment, s State) []Result {
			launches++
			return []Result{{Role: a.Role, Output: "ordinary text"}}
		}),
	}
	if err := engine.Run(ctx); err != nil {
		t.Fatalf("abandoned accepted request: %v", err)
	}
	if launches != 1 || !store.state.Done {
		t.Fatalf("launches=%d, done=%v", launches, store.state.Done)
	}
}

func TestFailuresAndFreeFormAnswersDoNotEndTheRequest(t *testing.T) {
	const request = "Keep the original scope and deliver the working result."
	store := &memoryStore{state: State{Request: request}}
	answers := []Result{
		{Output: "", Error: "provider unavailable"},
		{Output: "Example: {}\nThis is prose, not a candidate object.", Error: "exit status 2"},
		{Output: "I could not run the check.", Error: "command timed out"},
		{Output: "I found another approach and ran it."},
	}
	launched := 0
	engine := Chain{Store: store, RetryDelay: time.Millisecond,
		Router: testRouter(func(_ context.Context, state State) (Assignment, error) {
			if state.Request != request {
				t.Fatal("original request changed")
			}
			if len(state.History) == len(answers) {
				return Assignment{Role: "done"}, nil
			}
			return Assignment{Role: "implement", Instruction: "Use the actual failure to choose another approach."}, nil
		}),
		Executor: testExecutor(func(_ context.Context, assignment Assignment, state State) []Result {
			if store.state.Pending == nil || store.state.Pending.Instruction != assignment.Instruction {
				t.Fatal("assignment not saved before launch")
			}
			result := answers[launched]
			launched++
			// A failed save after an external action must not launch it again.
			store.saveFailures = 2
			return []Result{result}
		}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := engine.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if launched != 4 || !store.state.Done || len(store.state.History) != 4 {
		t.Fatalf("unexpected state: %#v", store.state)
	}
	for i, expected := range answers {
		if store.state.History[i].Output != expected.Output || store.state.History[i].Error != expected.Error {
			t.Fatalf("answer %d was interpreted or rewritten", i)
		}
	}
}

func TestRoutingFailureSurvivesStorageRetryAndRestart(t *testing.T) {
	const reason = "routing service HTTP 503: temporarily unavailable"
	store := &memoryStore{state: State{Request: "Deliver the original request."}}
	ctx, cancel := context.WithCancel(context.Background())
	routes := 0
	engine := Chain{Store: store, RetryDelay: time.Millisecond,
		Router: testRouter(func(_ context.Context, state State) (Assignment, error) {
			routes++
			if routes == 1 {
				store.saveFailures = 2
				return Assignment{}, errors.New(reason)
			}
			if len(state.History) != 1 || state.History[0].Error != reason || !reflect.DeepEqual(state, store.state) {
				t.Errorf("retry lost the saved routing failure: state=%+v saved=%+v", state, store.state)
			}
			cancel()
			return Assignment{}, ctx.Err()
		}),
		Executor: testExecutor(func(context.Context, Assignment, State) []Result {
			t.Fatal("no working role was assigned")
			return nil
		}),
	}
	if err := engine.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("stop did not interrupt recovery: %v", err)
	}
	if len(store.state.History) != 1 || store.state.Done || store.state.Pending != nil {
		t.Fatalf("routing failure was discarded or became a completed/pending action: %+v", store.state)
	}
	observation := store.state.History[0]
	if observation.Role != "router" || observation.Speaker != "runtime" || observation.Error != reason || observation.Output != "" || observation.StartedAt.IsZero() || observation.FinishedAt.Before(observation.StartedAt) {
		t.Fatalf("not an actual runtime failure observation: %+v", observation)
	}
	// A later authorized restart must have the same observation. Cancellation
	// is not itself a failed model call and must not add another result.
	engine.Router = testRouter(func(_ context.Context, state State) (Assignment, error) {
		if !reflect.DeepEqual(state.History, []Result{observation}) || state.Request != store.state.Request {
			t.Fatalf("restart lost or rewrote the observation: %+v", state)
		}
		return Assignment{Role: "done"}, nil
	})
	if err := engine.Run(context.Background()); err != nil || !store.state.Done {
		t.Fatalf("restart failed: %v state=%+v", err, store.state)
	}
}

func TestRoutingAlternativeRetainsBothUnavailableReasons(t *testing.T) {
	primaryFailure := errors.New("primary routing service unavailable")
	alternativeFailure := errors.New("alternative routing service unavailable")
	for _, alternativeFails := range []bool{false, true} {
		router := Alternate{
			Primary: testRouter(func(context.Context, State) (Assignment, error) {
				return Assignment{}, primaryFailure
			}),
			Secondary: testRouter(func(context.Context, State) (Assignment, error) {
				if alternativeFails {
					return Assignment{}, alternativeFailure
				}
				return Assignment{Role: "investigate"}, nil
			}),
		}
		next, err := router.Next(context.Background(), State{Request: "original"})
		if alternativeFails {
			if !errors.Is(err, primaryFailure) || !errors.Is(err, alternativeFailure) || next.Role != "" {
				t.Fatalf("one of the routing failures was lost: next=%+v err=%v", next, err)
			}
		} else if err != nil || next.Role != "investigate" {
			t.Fatalf("successful alternative treated as unavailable: next=%+v err=%v", next, err)
		}
	}
}

func TestRestartObservesInterruptedActionBeforeAnyRepeat(t *testing.T) {
	directory := t.TempDir()
	store, err := Open(directory, "Publish once, after checking the destination.")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(State{Request: "Publish once, after checking the destination.", Pending: &Assignment{Role: "deliver", Instruction: "Upload the release."}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(directory, "Publish once, after checking the destination.")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var ran []string
	engine := Chain{Store: store, Router: testRouter(func(_ context.Context, state State) (Assignment, error) {
		if state.Pending != nil {
			t.Fatal("pending action was blindly retained for dispatch")
		}
		if len(state.History) == 1 {
			if !strings.Contains(state.History[0].Error, "may have taken effect") {
				t.Fatal("lost interrupted-write warning")
			}
			return Assignment{Role: "verify"}, nil
		}
		return Assignment{Role: "done"}, nil
	}), Executor: testExecutor(func(_ context.Context, assignment Assignment, _ State) []Result {
		ran = append(ran, assignment.Role)
		return []Result{{Role: assignment.Role, Output: "The existing release is present; no second upload."}}
	})}
	if err := engine.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ran, []string{"verify"}) {
		t.Fatalf("ran %v", ran)
	}
	state, err := store.Load()
	if err != nil || !state.Done {
		t.Fatalf("state=%#v err=%v", state, err)
	}
	if err := engine.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 1 {
		t.Fatal("completed request ran again")
	}
}

// A launch keeps its start while it runs and drops it when it returns. One cut
// by a restart leaves no record of its own, so the note written for it says
// when it began: a stage taken up again is not read as one that began at the
// restart. A state saved before the start was kept still loads, and its note
// has no start, which no reader can place after anything.
func TestTheNoteForAnInterruptedLaunchSaysWhenItBegan(t *testing.T) {
	const request = "Take the stage up again after a restart."
	run := func(t *testing.T, directory string) (State, State) {
		t.Helper()
		store, err := Open(directory, request)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		var during State
		engine := Chain{Store: store, Router: testRouter(func(_ context.Context, state State) (Assignment, error) {
			for _, result := range state.History {
				if result.Speaker == "worker" {
					return Assignment{Role: "done"}, nil
				}
			}
			return Assignment{Role: "work"}, nil
		}), Executor: testExecutor(func(context.Context, Assignment, State) []Result {
			during, _ = store.Load()
			return []Result{{Role: "work", Speaker: "worker", Output: "worked"}}
		})}
		if err := engine.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		after, err := store.Load()
		if err != nil {
			t.Fatal(err)
		}
		return during, after
	}
	t.Run("a launch keeps its start while it runs", func(t *testing.T) {
		directory := t.TempDir()
		before := time.Now().UTC()
		during, after := run(t, directory)
		if during.Pending == nil || during.PendingSince.Before(before) || during.PendingSince.After(time.Now().UTC()) {
			t.Fatalf("the running launch kept %v as its start", during.PendingSince)
		}
		if after.Pending != nil || !after.PendingSince.IsZero() {
			t.Fatalf("a returned launch kept its start: %+v", after)
		}
		if raw, _ := os.ReadFile(filepath.Join(directory, "history.json")); strings.Contains(string(raw), "pending_since") {
			t.Fatalf("a state with nothing pending was saved with a start: %s", raw)
		}
	})
	began := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
	for _, shape := range []struct {
		name  string
		saved string
		start time.Time
	}{
		{"cut under a runtime that keeps the start", `{"request":"` + request + `","history":[],"pending":{"role":"work"},"pending_since":"` + began.Format(time.RFC3339) + `","done":false}`, began},
		{"cut under a runtime that did not", `{"request":"` + request + `","history":[],"pending":{"role":"work"},"done":false}`, time.Time{}},
	} {
		t.Run(shape.name, func(t *testing.T) {
			directory := t.TempDir()
			if err := os.WriteFile(filepath.Join(directory, "history.json"), []byte(shape.saved), 0600); err != nil {
				t.Fatal(err)
			}
			_, after := run(t, directory)
			note := after.History[0]
			if note.Speaker != "runtime" || note.Role != "work" || !note.StartedAt.Equal(shape.start) || note.FinishedAt.Before(began) {
				t.Fatalf("the note for the cut launch is %+v", note)
			}
			if after.Pending != nil || !after.PendingSince.IsZero() || !after.Done {
				t.Fatalf("the request after the restart: %+v", after)
			}
		})
	}
}

func TestStopInterruptsUnavailableRouterAndStoreWait(t *testing.T) {
	for _, storage := range []bool{false, true} {
		t.Run(map[bool]string{false: "router", true: "store"}[storage], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			store := &memoryStore{}
			if storage {
				store.loadFailures = 1000
			}
			engine := Chain{Store: store, RetryDelay: time.Hour,
				Router:   testRouter(func(context.Context, State) (Assignment, error) { return Assignment{}, errors.New("offline") }),
				Executor: testExecutor(func(context.Context, Assignment, State) []Result { t.Error("unexpected launch"); return nil }),
				Observe:  func(string) { cancel() },
			}
			started := time.Now()
			if err := engine.Run(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("err=%v", err)
			}
			if time.Since(started) > time.Second {
				t.Fatal("stop waited for retry delay")
			}
		})
	}
}

func TestFileStoreExclusiveOwnerAndOriginalRequest(t *testing.T) {
	directory := t.TempDir()
	first, err := Open(directory, "request A")
	if err != nil {
		t.Fatal(err)
	}
	if second, err := Open(directory, "request A"); err == nil {
		second.Close()
		t.Fatal("second owner was allowed")
	}
	first.Close()
	if other, err := Open(directory, "request B"); err == nil {
		other.Close()
		t.Fatal("another request reused the history")
	}
	data, err := os.ReadFile(filepath.Join(directory, "history.json"))
	if err != nil || !strings.Contains(string(data), "request A") {
		t.Fatal("original history was changed")
	}
}

func TestCancellationRetainsReturnedReportsAndRestartWarning(t *testing.T) {
	const request = "Publish only once and inspect the actual external result."
	dir := t.TempDir()
	store, err := Open(dir, request)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	launches, routes := 0, 0
	partial := []Result{{Role: "deliver", Speaker: "first", Model: "fixture/model", Output: "Uploaded part of the release.\n{\"unknown\":true}", Diagnostics: "The remote receipt was interrupted.", Error: "context canceled", StartedAt: time.Now().UTC(), FinishedAt: time.Now().UTC()},
		{Role: "deliver", Speaker: "second", Output: "", Error: "receipt unavailable"}}
	engine := Chain{Store: store, Router: testRouter(func(context.Context, State) (Assignment, error) {
		routes++
		return Assignment{Role: "deliver", Instruction: "Keep the original destination."}, nil
	}),
		Executor: testExecutor(func(context.Context, Assignment, State) []Result {
			launches++
			cancel()
			return append([]Result(nil), partial...)
		})}
	if err := engine.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("stop not returned: %v", err)
	}
	store.Close()
	store, err = Open(dir, request)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	saved, err := store.Load()
	if err != nil || saved.Done || saved.Request != request || len(saved.History) != 2 || saved.Pending == nil || saved.Pending.Role != "deliver" || routes != 1 || launches != 1 {
		t.Fatalf("stopped results discarded/finished: %#v err=%v routes=%d launches=%d", saved, err, routes, launches)
	}
	for i, r := range partial {
		r.Instruction = "Keep the original destination."
		if !reflect.DeepEqual(saved.History[i], r) {
			t.Fatalf("report changed: %#v", saved.History[i])
		}
	}
	// This explicitly simulates operator-authorized resumption. Watch's saved
	// stop still prevents an ordinary restart from resuming stopped work.
	var resumed []string
	engine = Chain{Store: store, Router: testRouter(func(_ context.Context, s State) (Assignment, error) {
		if len(s.History) == 3 {
			if s.Pending != nil || s.History[2].Speaker != "runtime" || !strings.Contains(s.History[2].Error, "may have taken effect") || s.History[0].Output != partial[0].Output {
				t.Fatal("restart lost reports or interrupted-write warning")
			}
			return Assignment{Role: "verify"}, nil
		}
		return Assignment{Role: "done"}, nil
	}), Executor: testExecutor(func(_ context.Context, a Assignment, _ State) []Result {
		resumed = append(resumed, a.Role)
		return []Result{{Role: a.Role, Output: "Inspected the existing destination; did not repeat publication."}}
	})}
	if err := engine.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(resumed, []string{"verify"}) {
		t.Fatalf("publication repeated: %v", resumed)
	}
}

type cancellationStore struct {
	memoryStore
	cancel       context.CancelFunc
	reportWrites int
	failLast     bool
}

func (s *cancellationStore) Save(state State) error {
	if len(state.History) > 0 {
		s.reportWrites++
		if s.reportWrites == 1 {
			s.cancel()
			return errors.New("storage interrupted while saving the report")
		}
		if s.failLast {
			return errors.New("storage still unavailable on shutdown")
		}
	}
	return s.memoryStore.Save(state)
}

func TestCancellationDuringSaveAttemptsLocalRetentionWithoutRepeatingWork(t *testing.T) {
	for _, failLast := range []bool{false, true} {
		t.Run(map[bool]string{false: "recovered", true: "still-unavailable"}[failLast], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			store := &cancellationStore{memoryStore: memoryStore{state: State{Request: "original"}}, cancel: cancel, failLast: failLast}
			launches, routes := 0, 0
			var observations []string
			engine := Chain{Store: store, RetryDelay: time.Hour, Observe: func(s string) { observations = append(observations, s) },
				Router: testRouter(func(context.Context, State) (Assignment, error) { routes++; return Assignment{Role: "report"}, nil }),
				Executor: testExecutor(func(context.Context, Assignment, State) []Result {
					launches++
					return []Result{{Role: "report", Output: "The comment may already be stored."}}
				})}
			started := time.Now()
			err := engine.Run(ctx)
			if !errors.Is(err, context.Canceled) || time.Since(started) > time.Second || launches != 1 || routes != 1 || store.reportWrites != 2 {
				t.Fatalf("shutdown retried work/waited/lost local attempt: %v launches=%d routes=%d writes=%d", err, launches, routes, store.reportWrites)
			}
			if store.state.Done || store.state.Pending == nil || store.state.Pending.Role != "report" {
				t.Fatalf("ambiguous action lost: %#v", store.state)
			}
			if failLast {
				if len(store.state.History) != 0 || !strings.Contains(err.Error(), "storage still unavailable on shutdown") || !strings.Contains(strings.Join(observations, "\n"), "storage still unavailable on shutdown") {
					t.Fatalf("shutdown storage reason lost: %v state=%#v observations=%v", err, store.state, observations)
				}
			} else if len(store.state.History) != 1 || store.state.History[0].Output != "The comment may already be stored." {
				t.Fatalf("returned report lost: %#v", store.state)
			}
		})
	}
}

const requesterAnswer = "(a) release/ でお願いします。"

// A question put to the person who filed the request is not a failure and not
// a completion. The chain stops there, says so, and leaves the conversation to
// whoever owns it; it never decides what an answer has to look like.
func TestAQuestionToTheRequesterHoldsTheRequestWithoutEndingIt(t *testing.T) {
	store := &memoryStore{state: State{Request: "Settle what this asks for before starting."}}
	routed, launched := 0, 0
	engine := Chain{Store: store, RetryDelay: time.Millisecond, WaitAfter: "ask_requester",
		Router: testRouter(func(_ context.Context, state State) (Assignment, error) {
			routed++
			for _, result := range state.History {
				if result.Speaker == "requester" {
					return Assignment{Role: "done"}, nil
				}
			}
			return Assignment{Role: "ask_requester"}, nil
		}),
		Executor: testExecutor(func(_ context.Context, assignment Assignment, _ State) []Result {
			launched++
			return []Result{{Role: assignment.Role, Output: "Posted one question and read it back."}}
		}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := engine.Run(ctx); !errors.Is(err, ErrWaiting) {
		t.Fatalf("the question did not hold the request: %v", err)
	}
	if !store.state.Waiting || store.state.Done || store.state.Step != "ask_requester" || len(store.state.History) != 1 {
		t.Fatalf("held state: %+v", store.state)
	}
	if err := engine.Run(ctx); !errors.Is(err, ErrWaiting) || routed != 1 || launched != 1 {
		t.Fatalf("a waiting request was dispatched again: routed=%d launched=%d err=%v", routed, launched, err)
	}
	// Only the owner of the conversation appends the reply and clears the hold.
	store.state.History = append(store.state.History, Result{Role: "ask_requester", Speaker: "requester", Output: requesterAnswer})
	store.state.Waiting = false
	if err := engine.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if !store.state.Done || store.state.Waiting || routed != 2 || launched != 1 {
		t.Fatalf("the answer did not carry the request on: routed=%d launched=%d state=%+v", routed, launched, store.state)
	}
}

// An unsuccessful question asked nobody anything, so waiting for an answer
// would end the request at a person. It is ordinary recovery instead.
func TestAnUnsuccessfulQuestionRecoversInsteadOfWaiting(t *testing.T) {
	store := &memoryStore{state: State{Request: "Settle what this asks for before starting."}}
	routed := 0
	engine := Chain{Store: store, RetryDelay: time.Millisecond, WaitAfter: "ask_requester", Workflow: entranceWorkflow(),
		Router: testRouter(func(_ context.Context, state State) (Assignment, error) {
			routed++
			switch routed {
			case 1:
				return Assignment{Role: "elicit"}, nil
			case 2:
				return Assignment{Role: "ask_requester"}, nil
			case 3:
				if !state.Recovering || !reflect.DeepEqual(state.nextActions(), []string{"elicit"}) {
					t.Fatalf("an unsuccessful question did not recover: %+v", state)
				}
				return Assignment{Role: "elicit"}, nil
			case 4:
				return Assignment{Role: "investigate"}, nil
			}
			return Assignment{Role: "done"}, nil
		}),
		Executor: testExecutor(func(_ context.Context, assignment Assignment, _ State) []Result {
			if assignment.Role == "ask_requester" {
				return []Result{{Role: assignment.Role, Error: "comment submission not confirmed"}}
			}
			return []Result{{Role: assignment.Role, Output: "ordinary prose"}}
		}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := engine.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if store.state.Waiting {
		t.Fatal("an unsuccessful question waited for an answer that was never asked for")
	}
	if !store.state.Done || routed != 5 {
		t.Fatalf("routed=%d state=%+v", routed, store.state)
	}
}

// The requester is asked by the configured question role, at the entrance,
// instead of being told that acceptance ends their part. Neither instruction
// asks a role to check, decode or grade what anyone answered.
func TestTheQuestionRoleReplacesSilenceAfterAcceptance(t *testing.T) {
	prompt := processPrompt(Role{Name: "implement", Purpose: "Do the work"}, Process{}, Assignment{}, State{})
	for name, text := range map[string]string{"routing instructions": routingInstructions, "process prompt": prompt} {
		if strings.Contains(text, "after acceptance") {
			t.Fatalf("the %s still end the requester's part at acceptance", name)
		}
		if !strings.Contains(text, "configured question role") {
			t.Fatalf("the %s do not say who may ask the requester", name)
		}
	}
}

// What the entrance is held to, in the words every decision receives. A vague
// request is bounced back at once instead of being guessed at: proceeding with
// an open point loses a night, asking costs one reply. This is wording and
// connections; nothing here inspects or scores what a role wrote.
const byMorningStandard = "can this request be carried to a delivered, verified result by morning with nobody available to answer?"
const requesterPointTest = "A point is the requester's to decide only when the request, the repository and the operator instructions do not settle it and it changes what the delivered result does, where it goes or what the work may touch: a behaviour the request leaves open without saying you may choose, a target that cannot be told apart, access or a credential that was not given, instructions that contradict each other, or an action that cannot be undone."
const preferencesAreSettled = "Wording, naming, language, level of detail and style are never questions: take the reading closest to the request and to what the repository already does, write the choice down with its reason, and leave it to the review of the delivered result; a point once decided is settled and is not listed again as a question."
const askWhenInDoubt = "Proceeding with such an open point costs a night's work and asking costs one reply, so proceed only when every point of that kind is absent or already answered and the settled requirements state the completion condition to be held to; when you cannot tell whether a point is of that kind, ask the requester, and never proceed in order to find out."

func TestTheEntranceStandardReachesEveryDecision(t *testing.T) {
	for _, sentence := range []string{byMorningStandard, requesterPointTest, preferencesAreSettled, askWhenInDoubt} {
		if !strings.Contains(routingInstructions, sentence) {
			t.Fatalf("the routing instructions do not carry the entrance standard: %q", sentence)
		}
	}
}

func TestALaunchThatFailsWithinSecondsIsPacedBeforeTheNextAttempt(t *testing.T) {
	quick := []Result{{Speaker: "p", Error: "boom", StartedAt: time.Now(), FinishedAt: time.Now().Add(time.Second)}}
	slow := []Result{{Speaker: "p", Error: "boom", StartedAt: time.Now().Add(-time.Minute), FinishedAt: time.Now()}}
	fine := []Result{{Speaker: "p", Output: "ok", StartedAt: time.Now(), FinishedAt: time.Now().Add(time.Second)}}
	note := []Result{{Speaker: "runtime", Error: "note", StartedAt: time.Now(), FinishedAt: time.Now()}}
	if !failedFast(quick) || failedFast(slow) || failedFast(fine) || failedFast(note) {
		t.Fatalf("quick=%t slow=%t fine=%t note=%t", failedFast(quick), failedFast(slow), failedFast(fine), failedFast(note))
	}
	before := append(append([]Result{}, quick...), note...)
	before[0].Role = "report"
	if !failedFastBefore(before, "report") || failedFastBefore(before, "other") || failedFastBefore(append(append([]Result{}, slow...), note...), "") {
		t.Fatal("only a second quick failure of the same role is paced")
	}
}
