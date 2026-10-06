package chain

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// confirmFlow is an ordered run with a stage that reads the change between the
// review and the delivery, written as an operator writes it.
func confirmFlow(t *testing.T) *Workflow {
	t.Helper()
	var flow Workflow
	if err := json.Unmarshal([]byte(`{"stages":[
		{"name":"elicit","kind":"model"},
		{"name":"work","kind":"model"},
		{"name":"review","kind":"command","on_failure":"elicit"},
		{"name":"confirm_change","kind":"model","confirm":true},
		{"name":"deliver","kind":"command","on_failure":"elicit"},
		{"name":"report","kind":"model"},
		{"name":"confirm_report","kind":"command","on_failure":"report"}]}`), &flow); err != nil {
		t.Fatal(err)
	}
	flow.Question = "ask_requester"
	return &flow
}

func confirmPurposes() map[string]string {
	return map[string]string{"elicit": "Settle the requirements.", "work": "Make the change.", "review": "Review the change.",
		"confirm_change": "Read the change before it is delivered.", "deliver": "Deliver the reviewed change.",
		"report": "Write the report.", "confirm_report": "Read the report back.", "ask_requester": "Ask the requester."}
}

// confirmDecision is one call of the stand-in decision service: the role that
// had just run, and the choices it was offered.
type confirmDecision struct {
	after   string
	offered []string
}

// confirmJudge stands in for the decision service. After the first stage it
// hands the work on. After the stage that reads the change it asks the
// requester, and delivers only once a requester comment is in the record. The
// choices are scripted: what is under test is what the runtime lets happen.
func confirmJudge(decisions *[]confirmDecision) testJudge {
	return func(_ context.Context, state State, _ string, choices map[string]string) (string, error) {
		var offered []string
		for name := range choices {
			offered = append(offered, name)
		}
		sort.Strings(offered)
		*decisions = append(*decisions, confirmDecision{after: state.Step, offered: offered})
		if state.Step != "confirm_change" {
			return "work", nil
		}
		for _, result := range state.History {
			if result.Speaker == "requester" {
				return "deliver", nil
			}
		}
		if _, ok := choices["ask_requester"]; ok {
			return "ask_requester", nil
		}
		return "deliver", nil
	}
}

// The requester is to see the change before it is delivered. Whatever becomes
// of the question, nothing but the requester's own words lets the delivery
// run: not a question that posted nothing, a launch that failed or was cut by
// a restart, a tracker that cannot be read, nor a question role that the limit
// after the first stage had already taken out of the choices.
func TestNoDeliveryWithoutTheRequestersWordsAfterAConfirmationQuestion(t *testing.T) {
	for _, scenario := range []string{"posted", "nothing-posted", "launch-failed", "no-result", "interrupted", "unreadable", "no-post-limit-reached"} {
		t.Run(scenario, func(t *testing.T) {
			flow := confirmFlow(t)
			store := &memoryStore{state: State{Request: "Show the item count on the list screen."}}
			if scenario == "no-post-limit-reached" {
				store.state.QuestionsWithoutPost, store.state.QuestionUnavailable = 2, "ask_requester"
			}
			var decisions []confirmDecision
			launches := map[string]int{}
			var order []string
			var cancel context.CancelFunc
			engine := Chain{Store: store, Workflow: flow, WaitAfter: "ask_requester", RetryDelay: time.Millisecond,
				QuestionPosted: func(context.Context) (QuestionObservation, error) {
					switch scenario {
					case "unreadable":
						return QuestionObservation{}, errors.New("issue comments could not be read for the assigned issue")
					case "nothing-posted":
						return QuestionObservation{}, nil
					}
					return QuestionObservation{Posted: true}, nil
				},
				Router: StageRouter{Entrance: DecisionRouter{Judge: confirmJudge(&decisions), Roles: confirmPurposes()}},
				Executor: testExecutor(func(_ context.Context, a Assignment, _ State) []Result {
					launches[a.Role]++
					order = append(order, a.Role)
					if a.Role == "ask_requester" {
						switch scenario {
						case "launch-failed":
							return []Result{{Role: a.Role, Speaker: "questioner", Error: "comment submission not confirmed"}}
						case "no-result":
							return nil // An executor that returned nothing leaves no record of the launch.
						case "interrupted":
							cancel() // The engine stops while the question is being put.
							return []Result{{Role: a.Role, Speaker: "questioner", Error: "signal: killed", Interrupted: true}}
						}
					}
					return []Result{{Role: a.Role, Speaker: "fixture", Output: "Ordinary prose about " + a.Role + "."}}
				}),
			}
			timeout := 2 * time.Second
			if scenario == "unreadable" {
				timeout = 300 * time.Millisecond
			}
			ctx, stop := context.WithTimeout(context.Background(), timeout)
			cancel = stop
			err := engine.Run(ctx)
			stop()
			if scenario == "interrupted" && errors.Is(err, context.Canceled) {
				// The restart: the same request, read back from the store.
				ctx, stop = context.WithTimeout(context.Background(), timeout)
				cancel = stop
				err = engine.Run(ctx)
				stop()
			}
			requester := 0
			for _, result := range store.state.History {
				if result.Speaker == "requester" {
					requester++
				}
			}
			if launches["deliver"] != 0 || store.state.Done || requester != 0 {
				t.Fatalf("the delivery ran %d times with %d requester comments in the record (done=%v, decisions after %v, launches %v)",
					launches["deliver"], requester, store.state.Done, decisionPlaces(decisions), order)
			}
			if scenario == "unreadable" {
				if !errors.Is(err, context.DeadlineExceeded) || store.state.Waiting {
					t.Fatalf("an unreadable tracker settled the question: err=%v waiting=%v", err, store.state.Waiting)
				}
				return
			}
			if !errors.Is(err, ErrWaiting) || !store.state.Waiting {
				t.Fatalf("the request does not wait for the requester: err=%v waiting=%v launches %v", err, store.state.Waiting, order)
			}
			// The requester's words arrive the way the conversation's owner adds
			// them: unchanged, as the requester's own record, clearing the hold.
			store.state.History = append(store.state.History, Result{Role: "ask_requester", Speaker: "requester", Output: "このままで納品してください。", FinishedAt: time.Now().UTC()})
			store.state.Waiting = false
			replied := len(store.state.History)
			ctx, stop = context.WithTimeout(context.Background(), 2*time.Second)
			defer stop()
			if err := engine.Run(ctx); err != nil {
				t.Fatal(err)
			}
			if launches["deliver"] != 1 || !store.state.Done {
				t.Fatalf("the reply did not carry the change on to one delivery: deliver=%d done=%v", launches["deliver"], store.state.Done)
			}
			// The stage that asked read the reply before anything was delivered.
			reread := false
			for _, result := range store.state.History[replied:] {
				if result.Role == "deliver" {
					break
				}
				reread = reread || result.Role == "confirm_change" && result.Speaker != "runtime"
			}
			if !reread {
				t.Fatalf("the delivery ran before the stage that asked read the reply: %v", order)
			}
			t.Logf("%s: launches %v; decisions after %v", scenario, order, decisionPlaces(decisions))
		})
	}
}

func decisionPlaces(decisions []confirmDecision) []string {
	var places []string
	for _, decision := range decisions {
		places = append(places, decision.after)
	}
	return places
}

// The limits that bound the choices after the first stage are counted there
// and apply there only. A question role that posted nothing at elicitation,
// and requirements passes repeated there, leave the choices after the stage
// that reads the change as they are.
func TestTheLimitsAfterTheFirstStageDoNotReachTheConfirmation(t *testing.T) {
	for _, limit := range []string{"question-no-post-limit", "entrance-rework-limit"} {
		t.Run(limit, func(t *testing.T) {
			store := &memoryStore{state: State{Request: "Change the help text of the list command."}}
			if limit == "question-no-post-limit" {
				store.state.QuestionsWithoutPost, store.state.QuestionUnavailable = 2, "ask_requester"
			}
			var decisions []confirmDecision
			judge := confirmJudge(&decisions)
			entrance := 0
			engine := Chain{Store: store, Workflow: confirmFlow(t), WaitAfter: "ask_requester", RetryDelay: time.Millisecond,
				Router: StageRouter{Entrance: DecisionRouter{Roles: confirmPurposes(),
					Judge: testJudge(func(ctx context.Context, state State, text string, choices map[string]string) (string, error) {
						choice, err := judge(ctx, state, text, choices)
						if _, offered := choices["elicit"]; limit == "entrance-rework-limit" && state.Step == "elicit" && offered {
							// Requirements again, as long as it stays offered.
							entrance++
							return "elicit", nil
						}
						return choice, err
					})}},
				Executor: testExecutor(func(_ context.Context, a Assignment, _ State) []Result {
					return []Result{{Role: a.Role, Speaker: "fixture", Output: "Ordinary prose."}}
				}),
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := engine.Run(ctx); !errors.Is(err, ErrWaiting) {
				t.Fatalf("the run did not end waiting for the requester: %v (decisions after %v)", err, decisionPlaces(decisions))
			}
			confirmations := 0
			for _, decision := range decisions {
				switch decision.after {
				case "elicit":
					if limit == "question-no-post-limit" && slices.Contains(decision.offered, "ask_requester") {
						t.Fatalf("the no-post limit stopped applying after the first stage: %v", decision.offered)
					}
				case "confirm_change":
					confirmations++
					if want := []string{"ask_requester", "deliver", "elicit"}; !slices.Equal(decision.offered, want) {
						t.Fatalf("after the change was read the decision was offered %v, not %v", decision.offered, want)
					}
				}
			}
			if confirmations != 1 {
				t.Fatalf("the decision after the change was read was asked %d times (decisions after %v)", confirmations, decisionPlaces(decisions))
			}
			if limit == "entrance-rework-limit" && entrance != 2 {
				t.Fatalf("requirements were chosen again %d times at elicitation, not the default two", entrance)
			}
			t.Logf("%s: decisions after %v", limit, decisionPlaces(decisions))
		})
	}
}

// What the runtime tells the decision service and the stage, in its own
// words, about the confirmation. It is wording: nothing here measures what a
// model makes of it.
func confirmationWordsPresent(t *testing.T, name, text string, phrases ...string) {
	t.Helper()
	for _, phrase := range phrases {
		if !strings.Contains(text, phrase) {
			t.Errorf("%s lacks %q", name, phrase)
		}
	}
}

// A confirmation question that reached nobody is not a question asked after
// the first stage: it waits for the requester instead of continuing, and it
// is not counted toward the limit for questions that posted nothing.
func TestAConfirmationQuestionThatReachedNobodyWaitsApartFromTheNoPostLimit(t *testing.T) {
	for _, scenario := range []string{"posted", "nothing-posted", "launch-failed", "no-result", "interrupted"} {
		t.Run(scenario, func(t *testing.T) {
			store := &memoryStore{state: State{Request: "Add an option to the list command."}}
			var decisions []confirmDecision
			var cancel context.CancelFunc
			engine := Chain{Store: store, Workflow: confirmFlow(t), WaitAfter: "ask_requester", RetryDelay: time.Millisecond, QuestionNoPostLimit: 1,
				QuestionPosted: func(context.Context) (QuestionObservation, error) {
					return QuestionObservation{Posted: scenario == "posted"}, nil
				},
				Router: StageRouter{Entrance: DecisionRouter{Judge: confirmJudge(&decisions), Roles: confirmPurposes()}},
				Executor: testExecutor(func(_ context.Context, a Assignment, _ State) []Result {
					if a.Role == "ask_requester" && scenario == "launch-failed" {
						return []Result{{Role: a.Role, Speaker: "questioner", Error: "exit status 1"}}
					}
					if a.Role == "ask_requester" && scenario == "no-result" {
						return nil
					}
					if a.Role == "ask_requester" && scenario == "interrupted" {
						cancel()
						return []Result{{Role: a.Role, Speaker: "questioner", Error: "signal: killed", Interrupted: true}}
					}
					return []Result{{Role: a.Role, Speaker: "fixture", Output: "Ordinary prose."}}
				}),
			}
			var err error
			for attempt := 0; attempt < 2; attempt++ {
				ctx, stop := context.WithTimeout(context.Background(), 2*time.Second)
				cancel = stop
				err = engine.Run(ctx)
				stop()
				if !errors.Is(err, context.Canceled) {
					break
				}
			}
			if !errors.Is(err, ErrWaiting) || !store.state.Waiting {
				t.Fatalf("err=%v waiting=%v", err, store.state.Waiting)
			}
			unseen := scenario != "posted"
			if store.state.WaitingWithoutQuestion != unseen {
				t.Fatalf("waiting without a seen question = %v, want %v", store.state.WaitingWithoutQuestion, unseen)
			}
			if store.state.QuestionsWithoutPost != 0 || store.state.QuestionUnavailable != "" {
				t.Fatalf("the confirmation question was counted toward the no-post limit: %d %q",
					store.state.QuestionsWithoutPost, store.state.QuestionUnavailable)
			}
			last := store.state.History[len(store.state.History)-1]
			if unseen != (last.Role == "router" && last.Speaker == "runtime" && last.Error == "" && last.Output != "") {
				t.Fatalf("the record does not say why the request waits: %+v", last)
			}
			t.Logf("%s: waiting=%v without a seen question=%v; last record %q", scenario, store.state.Waiting, store.state.WaitingWithoutQuestion, last.Output)
		})
	}
}

// The requester's reply goes back to the stage that asked: a stage that reads
// the change takes it up with that change, and a question asked after the
// first stage, or a new comment read while the question role was unavailable,
// goes back to requirements as before.
func TestTheReplyGoesBackToTheStageThatAsked(t *testing.T) {
	stage := func(role string) []Result {
		return []Result{{Role: role, Speaker: "fixture", Output: "prose"}, {Role: role, Speaker: "runtime", Output: "record"}}
	}
	history := func(roles ...string) []Result {
		var results []Result
		for _, role := range roles {
			results = append(results, stage(role)...)
		}
		return results
	}
	reply := Result{Role: "ask_requester", Speaker: "requester", Output: "このままで納品してください。"}
	question := Result{Role: "ask_requester", Speaker: "questioner", Output: "Posted one question."}
	for name, tc := range map[string]struct {
		records []Result
		want    string
	}{
		"after-the-first-stage":     {append(history("elicit"), question, reply), "elicit"},
		"after-the-confirmation":    {append(history("elicit", "work", "review", "confirm_change"), question, reply), "confirm_change"},
		"comment-while-unavailable": {append(history("elicit", "work"), reply), "elicit"},
	} {
		t.Run(name, func(t *testing.T) {
			state := State{Request: "r", Workflow: confirmFlow(t), Step: "ask_requester", History: tc.records}
			if got := state.stageActions(); !slices.Equal(got, []string{tc.want}) {
				t.Fatalf("the reply goes to %v, not %s", got, tc.want)
			}
		})
	}
}

// A decision service that keeps sending the change back to requirements from
// the confirmation would run the whole work again and again. The limit takes
// that choice out there and leaves the question and the delivery, so it never
// forces a delivery; a reply from the requester starts the count over.
func TestRequirementsChosenAfterTheConfirmationStopAtTheirLimit(t *testing.T) {
	for _, tc := range []struct {
		name           string
		limit, want    int
		failedDelivery bool
	}{{"default", 0, 2, false}, {"configured", 1, 1, false}, {"limit-kept-after-a-failed-delivery", 0, 2, true}} {
		t.Run(tc.name, func(t *testing.T) {
			flow := confirmFlow(t)
			flow.ConfirmationReworkLimit = tc.limit
			store := &memoryStore{state: State{Request: "Rename the menu entry."}}
			returns, deliveries := 0, 0
			var offered [][]string
			judge := testJudge(func(_ context.Context, state State, _ string, choices map[string]string) (string, error) {
				if state.Step != "confirm_change" {
					return "work", nil
				}
				var names []string
				for name := range choices {
					names = append(names, name)
				}
				sort.Strings(names)
				offered = append(offered, names)
				if _, ok := choices["elicit"]; ok {
					returns++
					return "elicit", nil
				}
				return "deliver", nil
			})
			engine := Chain{Store: store, Workflow: flow, WaitAfter: "ask_requester", RetryDelay: time.Millisecond,
				Router: StageRouter{Entrance: DecisionRouter{Judge: judge, Roles: confirmPurposes()}},
				Executor: testExecutor(func(_ context.Context, a Assignment, _ State) []Result {
					if a.Role == "deliver" {
						deliveries++
						if tc.failedDelivery && deliveries == 1 {
							// Back through requirements to the confirmation, still at the limit.
							return []Result{{Role: a.Role, Speaker: "delivery", Error: "exit status 1"}}
						}
					}
					return []Result{{Role: a.Role, Speaker: "fixture", Output: "Ordinary prose."}}
				}),
			}
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			if err := engine.Run(ctx); err != nil {
				t.Fatalf("the run did not finish within 300 ms: %v (returns %d)", err, returns)
			}
			wantDeliveries := 1
			if tc.failedDelivery {
				wantDeliveries = 2
			}
			if returns != tc.want || deliveries != wantDeliveries || len(offered) != tc.want+wantDeliveries {
				t.Fatalf("returns=%d deliveries=%d decisions after the confirmation=%d", returns, deliveries, len(offered))
			}
			if last := offered[len(offered)-1]; !slices.Equal(last, []string{"ask_requester", "deliver"}) {
				t.Fatalf("at the limit the decision was offered %v", last)
			}
			notes := 0
			for _, result := range store.state.History {
				if result.Speaker == "runtime" && strings.Contains(result.Output, "confirmation rework limit") {
					notes++
				}
			}
			if notes != 1 {
				t.Fatalf("%d runtime notes about the limit", notes)
			}
			t.Logf("%s: %d returns to requirements in 300 ms, then %v offered; %d deliveries, %d limit note", tc.name, returns, offered[len(offered)-1], deliveries, notes)
		})
	}
	// For comparison, the same decision service with a limit too high to reach:
	// in the same 300 ms it sends the work round again and again.
	t.Run("measured-without-an-effective-limit", func(t *testing.T) {
		flow := confirmFlow(t)
		flow.ConfirmationReworkLimit = 1 << 30
		store := &memoryStore{state: State{Request: "Rename the menu entry."}}
		returns, works := 0, 0
		engine := Chain{Store: store, Workflow: flow, WaitAfter: "ask_requester", RetryDelay: time.Millisecond,
			Router: StageRouter{Entrance: DecisionRouter{Roles: confirmPurposes(), Judge: testJudge(func(_ context.Context, state State, _ string, _ map[string]string) (string, error) {
				if state.Step == "confirm_change" {
					returns++
					return "elicit", nil
				}
				return "work", nil
			})}},
			Executor: testExecutor(func(_ context.Context, a Assignment, _ State) []Result {
				if a.Role == "work" {
					works++
				}
				return []Result{{Role: a.Role, Speaker: "fixture", Output: "Ordinary prose."}}
			}),
		}
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		if err := engine.Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("an unbounded choice ended the run: %v", err)
		}
		if returns <= 2 {
			t.Fatalf("only %d returns in 300 ms; the comparison shows nothing", returns)
		}
		t.Logf("without an effective limit: %d returns to requirements and %d work launches in 300 ms", returns, works)
	})
}

// confirmScript stands in for what the roles make of a reply. The runtime
// does not read the words; the script does, to show where each reading leads.
func confirmScript(state State, choices map[string]string) string {
	if state.Step != "confirm_change" {
		return "work"
	}
	lastWork, lastReply := -1, -1
	for i, result := range state.History {
		if result.Role == "work" && result.Speaker != "runtime" {
			lastWork = i
		}
		if result.Speaker == "requester" {
			lastReply = i
		}
	}
	switch {
	case lastReply < lastWork:
		// No reply about this change yet: show it to the requester.
		return "ask_requester"
	case strings.Contains(state.History[lastReply].Output, "このまま"):
		return "deliver"
	}
	// A correction or a refusal: back to requirements, not to delivery.
	return "elicit"
}

func TestACorrectionOrARefusalLeadsBackToRequirementsAndTheNewChangeIsAskedAbout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		replies []string
		want    int
	}{
		{"correction-then-acceptance", []string{"一覧の見た目は変えないでください。", "このままで納品してください。"}, 1},
		{"refusal", []string{"この変更は納品しないでください。"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &memoryStore{state: State{Request: "Show the item count on the list screen."}}
			var order []string
			deliveries := 0
			engine := Chain{Store: store, Workflow: confirmFlow(t), WaitAfter: "ask_requester", RetryDelay: time.Millisecond,
				Router: StageRouter{Entrance: DecisionRouter{Roles: confirmPurposes(), Judge: testJudge(func(_ context.Context, state State, _ string, choices map[string]string) (string, error) {
					return confirmScript(state, choices), nil
				})}},
				Executor: testExecutor(func(_ context.Context, a Assignment, state State) []Result {
					order = append(order, a.Role)
					if a.Role == "deliver" {
						deliveries++
					}
					if a.Role == "confirm_change" {
						// The stage that asked receives the reply as it was written.
						for i := len(state.History) - 1; i >= 0; i-- {
							if state.History[i].Speaker == "requester" {
								if !slices.Contains(tc.replies, state.History[i].Output) {
									t.Errorf("the reply reached the stage changed: %q", state.History[i].Output)
								}
								break
							}
						}
					}
					return []Result{{Role: a.Role, Speaker: "fixture", Output: "Ordinary prose."}}
				}),
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			for _, reply := range tc.replies {
				if err := engine.Run(ctx); !errors.Is(err, ErrWaiting) {
					t.Fatalf("no wait before the reply %q: %v (%v)", reply, err, order)
				}
				if deliveries != 0 {
					t.Fatalf("delivered before the reply %q: %v", reply, order)
				}
				store.state.History = append(store.state.History, Result{Role: "ask_requester", Speaker: "requester", Output: reply})
				store.state.Waiting = false
			}
			err := engine.Run(ctx)
			if tc.want == 0 {
				if !errors.Is(err, ErrWaiting) || deliveries != 0 {
					t.Fatalf("a refusal was delivered or ended the wait: err=%v deliveries=%d %v", err, deliveries, order)
				}
			} else if err != nil || deliveries != 1 || !store.state.Done {
				t.Fatalf("err=%v deliveries=%d done=%v %v", err, deliveries, store.state.Done, order)
			}
			if works := strings.Count(strings.Join(order, " "), "work"); works != 2 {
				t.Fatalf("the reply did not lead to new work once: %v", order)
			}
			t.Logf("%s: %v", tc.name, order)
		})
	}
}

// An accepted change that has to change again, here after the delivery
// failed, is read and decided on again before it is delivered: the earlier
// acceptance was about the earlier change.
func TestAnAcceptedChangeIsConfirmedAgainAfterItChanges(t *testing.T) {
	store := &memoryStore{state: State{Request: "Show the item count on the list screen."}}
	var order []string
	deliveries := 0
	engine := Chain{Store: store, Workflow: confirmFlow(t), WaitAfter: "ask_requester", RetryDelay: time.Millisecond,
		Router: StageRouter{Entrance: DecisionRouter{Roles: confirmPurposes(), Judge: testJudge(func(_ context.Context, state State, _ string, choices map[string]string) (string, error) {
			return confirmScript(state, choices), nil
		})}},
		Executor: testExecutor(func(_ context.Context, a Assignment, _ State) []Result {
			order = append(order, a.Role)
			if a.Role == "deliver" {
				deliveries++
				if deliveries == 1 {
					return []Result{{Role: a.Role, Speaker: "delivery", Error: "exit status 1", Output: "the integration branch moved; the merge conflicts"}}
				}
			}
			return []Result{{Role: a.Role, Speaker: "fixture", Output: "Ordinary prose."}}
		}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	answer := func(words string) {
		if err := engine.Run(ctx); !errors.Is(err, ErrWaiting) {
			t.Fatalf("no wait before %q: %v (%v)", words, err, order)
		}
		store.state.History = append(store.state.History, Result{Role: "ask_requester", Speaker: "requester", Output: words})
		store.state.Waiting = false
	}
	answer("このままで納品してください。")
	answer("このままで納品してください。直した後もこれで。")
	if err := engine.Run(ctx); err != nil || deliveries != 2 || !store.state.Done {
		t.Fatalf("err=%v deliveries=%d %v", err, deliveries, order)
	}
	// Between the failed delivery and the next one, the change was made again
	// and read again, and the requester was asked about it.
	failed := slices.Index(order, "deliver")
	between := order[failed+1 : failed+1+slices.Index(order[failed+1:], "deliver")]
	for _, role := range []string{"work", "confirm_change", "ask_requester"} {
		if !slices.Contains(between, role) {
			t.Fatalf("%s did not run between the two deliveries: %v", role, order)
		}
	}
	t.Logf("launches: %v", order)
}

// The runtime's own words to the stage, to the question role and to the
// decision service: what to read, what counts as the public API, when to ask,
// and that a reply covers only the change it was given about.
func TestTheRuntimeSaysWhatTheConfirmationReadsAndWhenItAsks(t *testing.T) {
	flow := confirmFlow(t)
	read := State{Request: "r", Workflow: flow, Step: "review", History: []Result{
		{Role: "elicit", Speaker: "fixture"}, {Role: "work", Speaker: "fixture"}, {Role: "review", Speaker: "fixture"}}}
	confirmationWordsPresent(t, "the stage's instruction", read.stageInstruction("confirm_change"),
		"the change actually made in the checkout", "settled requirements", "project's knowledge defines the public API",
		"HTTP routes", "command arguments and options and the output other programs read", "exported functions and types",
		"configuration keys and file formats", "Name the material you read and what you could not read",
		"too long to read whole is not an internal one", "When you cannot tell", "依頼者の確認: なし",
		"covers only the change it was given about", "the first stage again, a question to the requester, or the next stage (deliver)")
	asked := read
	asked.Step, asked.History = "confirm_change", append(append([]Result{}, read.History...), Result{Role: "confirm_change", Speaker: "fixture"})
	confirmationWordsPresent(t, "the question's instruction", asked.stageInstruction("ask_requester"),
		"what the change does", "deliver it as it is", "name what to change", "not to deliver it", "Nothing is delivered before")
	if text := (State{Request: "r", Workflow: flow, Step: "elicit", History: []Result{{Role: "elicit", Speaker: "fixture"}}}).stageInstruction("ask_requester"); text != "" {
		t.Fatalf("a question after the first stage gained an instruction: %q", text)
	}
	confirmationWordsPresent(t, "the routing instructions", routingInstructions,
		"after a stage that confirms the change before delivery, about that change",
		"After a stage that confirms the change before delivery, read its report",
		"how a person operates the product, what a screen shows or does, or the public API",
		"or when the report cannot tell, including when the change could not be read whole",
		"A reply given about an earlier change does not accept a later one",
		"a question that was not posted, a tracker that could not be read or a reached limit is not a reply")
	confirmationWordsPresent(t, "every role's prompt", processPrompt(Role{}, Process{}, Assignment{}, State{}),
		"after a stage that confirms the change before delivery, about that change")
}

func TestAConfirmationThatCouldOnlyDeliverIsRefused(t *testing.T) {
	if err := confirmFlow(t).Validate(confirmPurposes()); err != nil {
		t.Fatal(err)
	}
	for _, variant := range []string{"command-stage", "first-stage", "no-question-role", "negative-limit", "limit-without-confirmation", "limit-on-connections"} {
		t.Run(variant, func(t *testing.T) {
			flow := confirmFlow(t)
			switch variant {
			case "command-stage":
				flow.Stages[2].Confirm = true
			case "first-stage":
				flow.Stages[0].Confirm = true
			case "no-question-role":
				flow.Question = ""
			case "negative-limit":
				flow.ConfirmationReworkLimit = -1
			case "limit-without-confirmation":
				flow.Stages[3].Confirm, flow.ConfirmationReworkLimit = false, 1
			case "limit-on-connections":
				flow = &Workflow{Start: []string{"elicit"}, ConfirmationReworkLimit: 1,
					After:   map[string][]string{"elicit": {"done"}},
					Recover: map[string][]string{"elicit": {"elicit"}}}
			}
			if err := flow.Validate(confirmPurposes()); err == nil {
				t.Fatal("a confirmation that cannot ask, or a limit with nothing to bound, was accepted")
			} else {
				t.Log(err)
			}
		})
	}
}
