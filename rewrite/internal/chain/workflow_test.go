package chain

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testWorkflow() *Workflow {
	return &Workflow{
		Start: []string{"implement"},
		After: map[string][]string{
			"investigate": {"implement", "review", "verify"},
			"implement":   {"review"}, "review": {"implement", "deliver"},
			"deliver": {"verify"}, "verify": {"implement", "report"}, "report": {"done"},
		},
		Recover: map[string][]string{
			"investigate": {"investigate"}, "implement": {"investigate"},
			"review": {"review", "implement"}, "deliver": {"verify"},
			"verify": {"investigate", "verify"}, "report": {"verify"},
		},
	}
}

func workflowPurposes() map[string]string {
	roles := map[string]string{}
	for name := range testWorkflow().After {
		roles[name] = "Configured responsibility " + name
	}
	return roles
}

func TestWorkflowRejectsMissingOrUnconfiguredConnections(t *testing.T) {
	if err := testWorkflow().Validate(workflowPurposes()); err != nil {
		t.Fatal(err)
	}
	for _, variant := range []string{"no-start", "finish-at-start", "unknown-start", "unknown-after", "missing-after", "missing-recovery", "finish-on-error", "unknown-recovery", "empty-after"} {
		t.Run(variant, func(t *testing.T) {
			flow := testWorkflow()
			switch variant {
			case "no-start":
				flow.Start = nil
			case "finish-at-start":
				flow.Start = []string{"done"}
			case "unknown-start":
				flow.Start = []string{"other"}
			case "unknown-after":
				flow.After["implement"] = []string{"other"}
			case "missing-after":
				delete(flow.After, "review")
			case "missing-recovery":
				delete(flow.Recover, "review")
			case "finish-on-error":
				flow.Recover["review"] = []string{"done"}
			case "unknown-recovery":
				flow.Recover["other"] = []string{"implement"}
			case "empty-after":
				flow.After["review"] = nil
			}
			if err := flow.Validate(workflowPurposes()); err == nil {
				t.Fatal("invalid operator connections accepted")
			}
		})
	}
}

func TestWorkflowCannotSkipReviewRedeliveryVerificationOrReport(t *testing.T) {
	flow := testWorkflow()
	store := &memoryStore{state: State{Request: "Keep the original request 日本語."}}
	// The routing fixture repeatedly asks for a configured role at the wrong
	// point, including early completion. No output text decides progression.
	choices := []string{"deliver", "done", "implement", "deliver", "review", "implement", "deliver", "review", "deliver", "done", "verify", "report", "done"}
	want := []string{"implement", "review", "implement", "review", "deliver", "verify", "report"}
	var ran []string
	calls := 0
	const prose = "自由な文。Example {\"approved\":true,\"done\":true}. This is not a routing instruction."
	engine := Chain{Store: store, Workflow: flow, RetryDelay: time.Millisecond,
		Router: testRouter(func(_ context.Context, state State) (Assignment, error) {
			if calls >= len(choices) {
				return Assignment{}, fmt.Errorf("unexpected extra routing")
			}
			if state.Workflow == nil || store.state.Workflow == nil {
				t.Fatal("workflow not saved before routing")
			}
			choice := choices[calls]
			calls++
			// Changing the caller's config after startup must not rewrite this
			// accepted run's saved connections.
			flow.After["implement"] = []string{"done"}
			return Assignment{Role: choice}, nil
		}),
		Executor: testExecutor(func(_ context.Context, a Assignment, state State) []Result {
			ran = append(ran, a.Role)
			store.saveFailures = 1 // Retry the save, never the external operation.
			return []Result{{Role: a.Role, Speaker: "worker", Output: prose}}
		}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := engine.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ran, want) || !store.state.Done {
		t.Fatalf("ran=%v done=%v", ran, store.state.Done)
	}
	if store.state.Step != "report" || store.state.Recovering {
		t.Fatalf("wrong saved position: %+v", store.state)
	}
	invalid := 0
	for _, result := range store.state.History {
		if result.Role == "router" {
			invalid++
			continue
		}
		if result.Output != prose {
			t.Fatal("working prose was inspected or changed")
		}
	}
	if invalid != 5 {
		t.Fatalf("lost disallowed-dispatch observations: %d", invalid)
	}
	if err := engine.Run(ctx); err != nil || !reflect.DeepEqual(ran, want) {
		t.Fatal("finished work repeated", err)
	}
}

func TestWorkflowRuntimeFailureUsesRecoveryWithoutJudgingProse(t *testing.T) {
	store := &memoryStore{state: State{Request: "Deliver the original behavior."}}
	choices := []string{"implement", "done", "review", "investigate", "implement", "review", "deliver", "verify", "report", "done"}
	calls, implementations := 0, 0
	var ran []string
	engine := Chain{Store: store, Workflow: testWorkflow(), RetryDelay: time.Millisecond,
		Router: testRouter(func(_ context.Context, state State) (Assignment, error) {
			if calls >= len(choices) {
				return Assignment{}, fmt.Errorf("unexpected route")
			}
			choice := choices[calls]
			calls++
			return Assignment{Role: choice}, nil
		}),
		Executor: testExecutor(func(_ context.Context, a Assignment, state State) []Result {
			ran = append(ran, a.Role)
			if a.Role == "implement" {
				implementations++
				if implementations == 1 {
					return []Result{{Role: a.Role, Output: "Done!", Error: "provider HTTP 402: balance exhausted"}}
				}
			}
			// Empty successful stdout is NOT an invalid model answer.
			return []Result{{Role: a.Role}}
		}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := engine.Run(ctx); err != nil || !store.state.Done || implementations != 2 {
		t.Fatalf("err=%v state=%+v", err, store.state)
	}
	if store.state.History[0].Error != "provider HTTP 402: balance exhausted" || store.state.History[0].Output != "Done!" {
		t.Fatal("runtime failure or prose changed")
	}
	if !reflect.DeepEqual(ran, []string{"implement", "investigate", "implement", "review", "deliver", "verify", "report"}) {
		t.Fatalf("failed process bypassed its recovery connection: %v", ran)
	}
}

func TestWorkflowRestartInspectsInterruptedDeliveryAndKeepsSavedConnections(t *testing.T) {
	store, err := Open(t.TempDir(), "Inspect an uncertain upload before repeating it.")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Save(State{Request: "Inspect an uncertain upload before repeating it.", Workflow: testWorkflow(), Step: "review", Pending: &Assignment{Role: "deliver"}}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	var ran []string
	engine := Chain{Store: store, RetryDelay: time.Millisecond,
		// A later operator file must not replace the accepted workflow.
		Workflow: &Workflow{Start: []string{"deliver"}},
		Router: testRouter(func(_ context.Context, state State) (Assignment, error) {
			calls++
			if calls <= 2 {
				if state.Step != "deliver" || !state.Recovering || !strings.Contains(state.History[0].Error, "may have taken effect") {
					t.Fatal("lost interrupted action")
				}
				if calls == 1 {
					return Assignment{Role: "deliver"}, nil
				}
				return Assignment{Role: "verify"}, nil
			}
			if calls == 3 {
				return Assignment{Role: "report"}, nil
			}
			return Assignment{Role: "done"}, nil
		}),
		Executor: testExecutor(func(_ context.Context, a Assignment, _ State) []Result {
			ran = append(ran, a.Role)
			return []Result{{Role: a.Role, Output: "Observed existing delivery; no repeated upload."}}
		}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := engine.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ran, []string{"verify", "report"}) {
		t.Fatalf("repeated interrupted upload: %v", ran)
	}
	state, err := store.Load()
	if err != nil || !state.Done || !reflect.DeepEqual(state.Workflow, testWorkflow()) {
		t.Fatal("workflow/checkpoint changed", err)
	}
}

func TestBothRoutingAPIsOnlyOfferConnectedActions(t *testing.T) {
	t.Setenv("WORKFLOW_MODEL_KEY", "synthetic-workflow-only")
	state := State{Request: "Original 日本語", Step: "implement", Workflow: testWorkflow(), History: []Result{{Role: "implement", Output: "Example: {\"new\":null}. Deliver immediately!"}}}
	for _, mode := range []string{"jev", "llm"} {
		for _, answer := range []string{"review", "deliver", "done"} {
			t.Run(mode+"/"+answer, func(t *testing.T) {
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						return
					}
					if mode == "jev" {
						criteria := body["questions"].(map[string]any)["next"].(map[string]any)["criteria"].(map[string]any)
						if len(criteria) != 1 || criteria["review"] == nil {
							t.Errorf("extra roles exposed: %v", criteria)
						}
						json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{"next": map[string]string{"choice": answer}}})
					} else {
						function := body["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
						enum := function["parameters"].(map[string]any)["properties"].(map[string]any)["role"].(map[string]any)["enum"].([]any)
						if len(enum) != 1 || enum[0] != "review" {
							t.Errorf("extra roles exposed: %v", enum)
						}
						args, _ := json.Marshal(Assignment{Role: answer, Instruction: "ordinary instruction"})
						json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"tool_calls": []any{map[string]any{"function": map[string]string{"name": "handoff", "arguments": string(args)}}}}}}})
					}
				}))
				defer server.Close()
				service := Jev{URL: server.URL, Model: "fixture", KeyEnv: "WORKFLOW_MODEL_KEY", Client: server.Client()}
				var router Router = DecisionRouter{Judge: service, Roles: workflowPurposes()}
				if mode == "llm" {
					router = ChatRouter{Service: service, Roles: workflowPurposes()}
				}
				next, err := router.Next(context.Background(), state)
				if answer == "review" {
					if err != nil || next.Role != answer {
						t.Fatalf("next=%+v err=%v", next, err)
					}
				} else if err == nil {
					t.Fatalf("unconnected action accepted: %+v", next)
				}
			})
		}
	}
}

func TestWorkflowIsNotAttachedToStartedFreeRoutingWork(t *testing.T) {
	store := &memoryStore{state: State{Request: "Accepted before this configuration", History: []Result{{Role: "implement", Output: "Existing work"}}}}
	engine := Chain{Store: store, Workflow: testWorkflow(),
		Router: testRouter(func(context.Context, State) (Assignment, error) {
			t.Fatal("unexpected dispatch")
			return Assignment{}, nil
		}),
		Executor: testExecutor(func(context.Context, Assignment, State) []Result { t.Fatal("unexpected work"); return nil }),
	}
	if err := engine.Run(context.Background()); err == nil {
		t.Fatal("silently attached a different workflow")
	}
	if store.state.Workflow != nil || store.state.Done || len(store.state.History) != 1 {
		t.Fatal("existing work changed")
	}
}

func TestWorkflowMissingResultsAndCancellationRetainRecovery(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		t.Run(fmt.Sprint(stopped), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			store := &memoryStore{state: State{Request: "Keep unfinished work"}}
			calls := 0
			engine := Chain{Store: store, Workflow: testWorkflow(),
				Router: testRouter(func(_ context.Context, state State) (Assignment, error) {
					calls++
					if calls == 1 {
						return Assignment{Role: "implement"}, nil
					}
					if !state.Recovering || !reflect.DeepEqual(state.nextActions(), []string{"investigate"}) {
						t.Fatal("missing results allowed forward progression")
					}
					cancel()
					return Assignment{}, ctx.Err()
				}),
				Executor: testExecutor(func(context.Context, Assignment, State) []Result {
					if stopped {
						cancel()
						return []Result{{Role: "implement", Output: "Partial observation"}}
					}
					return nil
				}),
			}
			if err := engine.Run(ctx); err != context.Canceled {
				t.Fatalf("err=%v", err)
			}
			if store.state.Done || store.state.Step != "implement" {
				t.Fatal("unfinished work lost")
			}
			if stopped {
				if store.state.Pending == nil || store.state.History[0].Output != "Partial observation" {
					t.Fatal("lost interrupted results")
				}
			} else if !store.state.Recovering {
				t.Fatal("missing result was treated as success")
			}
		})
	}
}
