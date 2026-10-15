package chain

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// previewFlow is confirmFlow with the operator's two preview roles named.
func previewFlow(t *testing.T) *Workflow {
	t.Helper()
	flow := confirmFlow(t)
	flow.Preview = &Preview{Up: "preview_up", Down: "preview_down"}
	return flow
}

func previewPurposes() map[string]string {
	purposes := confirmPurposes()
	purposes["preview_up"] = "Put the change up for the requester to see."
	purposes["preview_down"] = "Take that page down."
	return purposes
}

// askAfterConfirmation stands in for the decision service: it hands the work
// on everywhere, and after the change was read it asks the requester until
// the requester's words are in the record, then delivers. Every call is kept
// with the choices it was offered.
func askAfterConfirmation(decisions *[]confirmDecision) testJudge {
	return func(_ context.Context, state State, _ string, choices map[string]string) (string, error) {
		var offered []string
		for name := range choices {
			offered = append(offered, name)
		}
		sort.Strings(offered)
		*decisions = append(*decisions, confirmDecision{after: state.Step, offered: offered})
		if _, confirmation := choices["deliver"]; !confirmation {
			return "work", nil
		}
		for _, result := range state.History {
			if result.Speaker == "requester" {
				return "deliver", nil
			}
		}
		return "ask_requester", nil
	}
}

// previewCall is one call of the stand-in preview: where the run stood and
// how many launches had been made when it was called.
type previewCall struct {
	role, step string
	launched   int
}

// The decision after the stage that read the change chose to ask the
// requester. Right before the question is launched, and only then, the page
// is put up once. The runtime keeps one record of its own of what came of it,
// with nothing in its error field, and the question is told the words the
// preview returned. Whether the page came up or not, nothing else about the
// run changes: no stage runs again and the choices after the requester's
// reply are those of a run without the setting.
func TestAPreviewIsPutUpOnceRightBeforeTheConfirmationQuestion(t *testing.T) {
	baseline := func() ([]confirmDecision, []string) {
		var decisions []confirmDecision
		var order []string
		store := &memoryStore{state: State{Request: "Show the item count on the list screen."}}
		engine := Chain{Store: store, Workflow: confirmFlow(t), WaitAfter: "ask_requester", RetryDelay: time.Millisecond,
			Router: StageRouter{Entrance: DecisionRouter{Judge: askAfterConfirmation(&decisions), Roles: confirmPurposes()}},
			Executor: testExecutor(func(_ context.Context, a Assignment, _ State) []Result {
				order = append(order, a.Role)
				return []Result{{Role: a.Role, Speaker: "fixture", Output: "Ordinary prose about " + a.Role + "."}}
			})}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := engine.Run(ctx); !errors.Is(err, ErrWaiting) {
			t.Fatalf("the run without the setting did not wait: %v", err)
		}
		store.state.History = append(store.state.History, Result{Role: "ask_requester", Speaker: "requester", Output: "このまま納品してください。", FinishedAt: time.Now().UTC()})
		store.state.Waiting = false
		if err := engine.Run(ctx); err != nil || !store.state.Done {
			t.Fatalf("the run without the setting did not deliver: %v", err)
		}
		return decisions, order
	}
	wantDecisions, wantOrder := baseline()
	for _, outcome := range []struct{ name, note, addition string }{
		{"up", "確認用の画面を用意した: https://sample-1.preview.example.invalid/", "The runtime has posted the address.\n"},
		{"failed", "確認用の画面を用意できなかった: exit status 1", "Say in one line that the page could not be prepared.\n"},
	} {
		t.Run(outcome.name, func(t *testing.T) {
			var decisions []confirmDecision
			var order []string
			var calls []previewCall
			instructions := map[string]string{}
			store := &memoryStore{state: State{Request: "Show the item count on the list screen."}}
			engine := Chain{Store: store, Workflow: previewFlow(t), WaitAfter: "ask_requester", RetryDelay: time.Millisecond,
				Router: StageRouter{Entrance: DecisionRouter{Judge: askAfterConfirmation(&decisions), Roles: previewPurposes()}},
				Executor: testExecutor(func(_ context.Context, a Assignment, _ State) []Result {
					order = append(order, a.Role)
					instructions[a.Role] = a.Instruction
					return []Result{{Role: a.Role, Speaker: "fixture", Output: "Ordinary prose about " + a.Role + "."}}
				}),
				PreviewUp: func(_ context.Context, role string, state State) (string, string) {
					calls = append(calls, previewCall{role: role, step: state.Step, launched: len(order)})
					return outcome.note, outcome.addition
				},
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := engine.Run(ctx); !errors.Is(err, ErrWaiting) || !store.state.Waiting {
				t.Fatalf("the run did not wait for the requester: %v (launches %v)", err, order)
			}
			if len(calls) != 1 || calls[0].role != "preview_up" || calls[0].step != "confirm_change" {
				t.Fatalf("the page was not put up once after the change was read: %+v (launches %v)", calls, order)
			}
			if asked := slices.Index(order, "ask_requester"); asked < 0 || calls[0].launched != asked || order[asked-1] != "confirm_change" {
				t.Fatalf("the page was not put up between the reading and the question: %+v (launches %v)", calls, order)
			}
			if slices.Contains(order, "preview_up") || slices.Contains(order, "preview_down") {
				t.Fatalf("the chain launched a preview role as a stage: %v", order)
			}
			if got := instructions["ask_requester"]; got != confirmationQuestionInstruction+outcome.addition {
				t.Fatalf("the question was not told what the preview returned:\n%q", got)
			}
			var notes []Result
			question := -1
			for i, result := range store.state.History {
				if result.Role == "router" && result.Speaker == "runtime" && result.Output == outcome.note {
					notes = append(notes, result)
				}
				if result.Role == "ask_requester" && question < 0 {
					question = i
				}
			}
			if len(notes) != 1 || notes[0].Error != "" || notes[0].FinishedAt.IsZero() || question < 1 || store.state.History[question-1].Output != outcome.note {
				t.Fatalf("the runtime's record of the preview is not one record without an error right before the question: %+v", store.state.History)
			}
			store.state.History = append(store.state.History, Result{Role: "ask_requester", Speaker: "requester", Output: "このまま納品してください。", FinishedAt: time.Now().UTC()})
			store.state.Waiting = false
			if err := engine.Run(ctx); err != nil || !store.state.Done {
				t.Fatalf("the reply did not carry the change on to the delivery: %v", err)
			}
			if len(calls) != 1 {
				t.Fatalf("the page was put up again without a new question: %+v", calls)
			}
			if !slices.Equal(order, wantOrder) {
				t.Fatalf("the launches differ from a run without the setting:\n got %v\nwant %v", order, wantOrder)
			}
			if len(decisions) != len(wantDecisions) {
				t.Fatalf("the decisions differ from a run without the setting:\n got %v\nwant %v", decisions, wantDecisions)
			}
			for i := range decisions {
				if decisions[i].after != wantDecisions[i].after || !slices.Equal(decisions[i].offered, wantDecisions[i].offered) {
					t.Fatalf("decision %d differs from a run without the setting: got %+v want %+v", i, decisions[i], wantDecisions[i])
				}
			}
		})
	}
}

// Only the question chosen after the change was read puts the page up: not a
// question at requirements, not the next stage, not a request accepted before
// the setting was added, and not an engine that has nothing to put it up with.
func TestNoPreviewWhereNoConfirmationQuestionIsChosen(t *testing.T) {
	for _, scenario := range []string{"question-at-requirements", "next-stage-after-reading", "accepted-before-the-setting", "no-way-to-put-it-up"} {
		t.Run(scenario, func(t *testing.T) {
			var decisions []confirmDecision
			ask := askAfterConfirmation(&decisions)
			judge := testJudge(func(ctx context.Context, state State, text string, choices map[string]string) (string, error) {
				switch {
				case scenario == "question-at-requirements" && state.Step == "elicit":
					return "ask_requester", nil
				case scenario == "next-stage-after-reading" && state.Step == "confirm_change":
					return "deliver", nil
				}
				return ask(ctx, state, text, choices)
			})
			store := &memoryStore{state: State{Request: "Show the item count on the list screen."}}
			if scenario == "accepted-before-the-setting" {
				// The run was accepted, and its workflow saved, before the
				// operator added the preview roles.
				store.state.Workflow = confirmFlow(t).clone()
			}
			calls := 0
			var order []string
			engine := Chain{Store: store, Workflow: previewFlow(t), WaitAfter: "ask_requester", RetryDelay: time.Millisecond,
				Router: StageRouter{Entrance: DecisionRouter{Judge: judge, Roles: previewPurposes()}},
				Executor: testExecutor(func(_ context.Context, a Assignment, _ State) []Result {
					order = append(order, a.Role)
					return []Result{{Role: a.Role, Speaker: "fixture", Output: "Ordinary prose about " + a.Role + "."}}
				}),
				PreviewUp: func(context.Context, string, State) (string, string) {
					calls++
					return "put up", "posted\n"
				},
			}
			if scenario == "no-way-to-put-it-up" {
				engine.PreviewUp = nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			err := engine.Run(ctx)
			if scenario == "next-stage-after-reading" {
				if err != nil || !store.state.Done {
					t.Fatalf("the run did not go on to the delivery: %v (launches %v)", err, order)
				}
			} else if !errors.Is(err, ErrWaiting) || !slices.Contains(order, "ask_requester") {
				t.Fatalf("the question was not asked: %v (launches %v)", err, order)
			}
			if calls != 0 {
				t.Fatalf("the page was put up %d times (launches %v)", calls, order)
			}
			for _, result := range store.state.History {
				if result.Role == "router" {
					t.Fatalf("the runtime recorded a preview it did not put up: %+v", result)
				}
			}
			if scenario == "accepted-before-the-setting" && store.state.Workflow.Preview != nil {
				t.Fatal("the setting reached a request accepted before it")
			}
		})
	}
}

// A request accepted after the setting keeps the role that puts its page up:
// the role is saved with the request, and naming another one in the
// operator's setting afterwards does not change which role puts its page up.
// Nothing else of the setting is kept; the queue takes pages down with the
// operator's current setting.
func TestTheAcceptedRunKeepsItsOwnCopyOfThePreviewRoles(t *testing.T) {
	flow := previewFlow(t)
	store := &memoryStore{state: State{Request: "Show the item count on the list screen."}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The request is accepted and its workflow saved; the run then stops at
	// its first decision.
	engine := Chain{Store: store, Workflow: flow, RetryDelay: time.Millisecond,
		Router: testRouter(func(context.Context, State) (Assignment, error) {
			cancel()
			return Assignment{}, errors.New("stopped at the first decision")
		}),
		Executor: testExecutor(func(context.Context, Assignment, State) []Result { return nil })}
	if err := engine.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	saved := store.state.Workflow
	if saved == nil || saved.Preview == nil || *saved.Preview != (Preview{Up: "preview_up"}) {
		t.Fatalf("the accepted run does not keep the up role, and only it: %+v", saved)
	}
	if clone := confirmFlow(t).clone(); clone.Preview != nil {
		t.Fatal("a run without the setting gained one in its copy")
	}
	// The operator names another up role; the accepted run goes on to its
	// question with the one it was accepted with.
	flow.Preview.Up = "changed_later"
	var decisions []confirmDecision
	var roles []string
	engine.Router = StageRouter{Entrance: DecisionRouter{Judge: askAfterConfirmation(&decisions), Roles: previewPurposes()}}
	engine.Executor = testExecutor(func(_ context.Context, a Assignment, _ State) []Result {
		return []Result{{Role: a.Role, Speaker: "fixture", Output: "Ordinary prose about " + a.Role + "."}}
	})
	engine.WaitAfter = "ask_requester"
	engine.PreviewUp = func(_ context.Context, role string, _ State) (string, string) {
		roles = append(roles, role)
		return "put up", "posted\n"
	}
	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := engine.Run(ctx); !errors.Is(err, ErrWaiting) {
		t.Fatalf("the accepted run did not reach its question: %v", err)
	}
	if len(roles) != 1 || roles[0] != "preview_up" || saved.Preview.Up != "preview_up" {
		t.Fatalf("the accepted run put its page up with %v, not the role it was accepted with", roles)
	}
}

// The runtime is stopped while the page is being put up, before anything of
// the question was saved. The next start finds no pending question: it asks
// the decision again, and when the question is chosen again the page is put
// up again before it.
func TestAPreviewCutByARestartIsPutUpAgainAfterTheDecision(t *testing.T) {
	var decisions []confirmDecision
	store := &memoryStore{state: State{Request: "Show the item count on the list screen."}}
	var order []string
	calls := 0
	var stop context.CancelFunc
	engine := Chain{Store: store, Workflow: previewFlow(t), WaitAfter: "ask_requester", RetryDelay: time.Millisecond,
		Router: StageRouter{Entrance: DecisionRouter{Judge: askAfterConfirmation(&decisions), Roles: previewPurposes()}},
		Executor: testExecutor(func(_ context.Context, a Assignment, _ State) []Result {
			order = append(order, a.Role)
			return []Result{{Role: a.Role, Speaker: "fixture", Output: "Ordinary prose about " + a.Role + "."}}
		}),
		PreviewUp: func(context.Context, string, State) (string, string) {
			calls++
			if calls == 1 {
				stop() // The runtime stops while the page is going up.
			}
			return "put up", "posted\n"
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	stop = cancel
	if err := engine.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("the stop did not end the run: %v", err)
	}
	saved := store.state
	if saved.Pending != nil || slices.Contains(order, "ask_requester") || saved.Step != "confirm_change" {
		t.Fatalf("a question was left pending or launched by a run stopped while the page went up: pending=%+v step=%q launches %v", saved.Pending, saved.Step, order)
	}
	for _, result := range saved.History {
		if result.Role == "router" {
			t.Fatalf("a preview cut short was recorded as one that returned: %+v", result)
		}
	}
	before := len(decisions)
	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := engine.Run(ctx); !errors.Is(err, ErrWaiting) {
		t.Fatalf("the restarted run did not ask and wait: %v (launches %v)", err, order)
	}
	if len(decisions) != before+1 || decisions[before].after != "confirm_change" || calls != 2 || !slices.Contains(order, "ask_requester") {
		t.Fatalf("the restart did not decide again and put the page up again: decisions %v calls %d launches %v", decisionPlaces(decisions), calls, order)
	}
}

// The setting names two different roles that exist and are neither a stage
// nor the question role, in an ordered run that has a stage which confirms
// the change: without such a stage nothing would ever put the page up.
func TestThePreviewSettingNamesTwoSeparateRolesOutsideTheRun(t *testing.T) {
	if err := previewFlow(t).Validate(previewPurposes()); err != nil {
		t.Fatalf("a valid setting was refused: %v", err)
	}
	for _, variant := range []struct{ name, want string }{
		{"up-missing", "preview_up"}, {"down-missing", "preview_down"},
		{"up-empty", "workflow.preview.up"}, {"down-empty", "workflow.preview.down"},
		{"up-is-a-stage", "stage"}, {"down-is-a-stage", "stage"},
		{"up-is-the-question", "question"}, {"down-is-the-question", "question"},
		{"same-role", "different"}, {"done", "done"},
		{"no-confirming-stage", "confirm"}, {"connected-roles", "ordered run"},
	} {
		t.Run(variant.name, func(t *testing.T) {
			flow, purposes := previewFlow(t), previewPurposes()
			switch variant.name {
			case "up-missing":
				delete(purposes, "preview_up")
			case "down-missing":
				delete(purposes, "preview_down")
			case "up-empty":
				flow.Preview.Up = ""
			case "down-empty":
				flow.Preview.Down = ""
			case "up-is-a-stage":
				flow.Preview.Up = "review"
			case "down-is-a-stage":
				flow.Preview.Down = "deliver"
			case "up-is-the-question":
				flow.Preview.Up = "ask_requester"
			case "down-is-the-question":
				flow.Preview.Down = "ask_requester"
			case "same-role":
				flow.Preview.Down = "preview_up"
			case "done":
				flow.Preview.Down, purposes["done"] = "done", "finish"
			case "no-confirming-stage":
				for i := range flow.Stages {
					flow.Stages[i].Confirm = false
				}
			case "connected-roles":
				flow = testWorkflow()
				flow.Preview = &Preview{Up: "preview_up", Down: "preview_down"}
				purposes = workflowPurposes()
				purposes["preview_up"], purposes["preview_down"] = "up", "down"
			}
			err := flow.Validate(purposes)
			if err == nil || !strings.Contains(err.Error(), variant.want) {
				t.Fatalf("the setting was not refused for %s: %v", variant.name, err)
			}
		})
	}
}
