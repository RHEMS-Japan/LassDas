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
