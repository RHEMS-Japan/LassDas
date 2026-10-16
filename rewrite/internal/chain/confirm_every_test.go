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

// alwaysAskFlow is confirmFlow whose stage that confirms the change asks the
// requester about every change.
func alwaysAskFlow(t *testing.T) *Workflow {
	t.Helper()
	flow := confirmFlow(t)
	for i := range flow.Stages {
		if flow.Stages[i].Name == "confirm_change" {
			flow.Stages[i].AlwaysAsk = true
		}
	}
	return flow
}

// deliverWhenOffered stands in for a decision service that goes on to the
// delivery whenever it may: after the change was read it chooses the next
// stage if that is offered, and asks the requester only when it is not. A
// requester's reply that asks for a correction sends the work back to the
// first stage. Every decision after the change was read is kept with what it
// was offered.
func deliverWhenOffered(offered *[][]string) testJudge {
	return func(_ context.Context, state State, _ string, choices map[string]string) (string, error) {
		if _, confirmation := choices["ask_requester"]; !confirmation || state.Step != "confirm_change" {
			return "work", nil
		}
		var names []string
		for name := range choices {
			names = append(names, name)
		}
		sort.Strings(names)
		*offered = append(*offered, names)
		// A reply read here is one given since the work last went back to
		// requirements.
		for i := len(state.History) - 1; i >= 0 && state.History[i].Role != "elicit"; i-- {
			if state.History[i].Speaker == "requester" {
				if strings.Contains(state.History[i].Output, "直して") {
					return "elicit", nil
				}
				break
			}
		}
		if _, ok := choices["deliver"]; ok {
			return "deliver", nil
		}
		return "ask_requester", nil
	}
}

func alwaysAskEngine(flow *Workflow, store *memoryStore, offered *[][]string, order *[]string) Chain {
	return Chain{Store: store, Workflow: flow, WaitAfter: "ask_requester", RetryDelay: time.Millisecond,
		Router: StageRouter{Entrance: DecisionRouter{Judge: deliverWhenOffered(offered), Roles: confirmPurposes()}},
		Executor: testExecutor(func(_ context.Context, a Assignment, _ State) []Result {
			*order = append(*order, a.Role)
			// The stage that reads the change says no operation, screen or
			// public API changes, the case where a question was not asked.
			output := "Ordinary prose about " + a.Role + "."
			if a.Role == "confirm_change" {
				output = "The change alters no operation, screen or public API.\n依頼者の確認: なし"
			}
			return []Result{{Role: a.Role, Speaker: "fixture", Output: output}}
		})}
}

func reply(store *memoryStore, words string) {
	store.state.History = append(store.state.History, Result{Role: "ask_requester", Speaker: "requester", Output: words, FinishedAt: time.Now().UTC()})
	store.state.Waiting, store.state.WaitingWithoutQuestion = false, false
}

// With always_ask, the requester sees every change before it is delivered:
// after the change was read, the next stage is not offered until the
// requester has replied about this change, even when the reading says nothing
// a person sees changes and the decision service would deliver at once. An
// acceptance then offers it, and the change is delivered.
func TestAlwaysAskOffersTheDeliveryOnlyAfterTheRequestersReply(t *testing.T) {
	store := &memoryStore{state: State{Request: "Rename an internal helper."}}
	var offered [][]string
	var order []string
	engine := alwaysAskEngine(alwaysAskFlow(t), store, &offered, &order)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := engine.Run(ctx); !errors.Is(err, ErrWaiting) || !store.state.Waiting {
		t.Fatalf("the run did not wait for the requester: %v (launches %v)", err, order)
	}
	if slices.Contains(order, "deliver") || !slices.Contains(order, "ask_requester") {
		t.Fatalf("the change went on without the requester: %v", order)
	}
	if len(offered) != 1 || slices.Contains(offered[0], "deliver") || !slices.Equal(offered[0], []string{"ask_requester", "elicit"}) {
		t.Fatalf("the decision after the reading was offered %v", offered)
	}
	reply(store, "このまま納品してください。")
	if err := engine.Run(ctx); err != nil || !store.state.Done {
		t.Fatalf("the acceptance did not carry the change to the delivery: %v (launches %v)", err, order)
	}
	if last := offered[len(offered)-1]; !slices.Contains(last, "deliver") {
		t.Fatalf("the delivery was not offered after the requester's reply: %v", offered)
	}
	if asked := slices.Index(order, "ask_requester"); asked < 0 || slices.Index(order, "deliver") < asked {
		t.Fatalf("the delivery ran before the question: %v", order)
	}
}

// The runtime itself refuses the next stage before the reply: a decision that
// names it all the same is a routing failure, and nothing is delivered.
func TestAlwaysAskRefusesTheNextStageANamedDecisionChooses(t *testing.T) {
	flow := alwaysAskFlow(t)
	store := &memoryStore{state: State{Request: "Rename an internal helper.", Workflow: flow.clone(), Step: "confirm_change",
		History: []Result{{Role: "elicit", Speaker: "fixture"}, {Role: "work", Speaker: "fixture"}, {Role: "review", Speaker: "fixture"},
			{Role: "confirm_change", Speaker: "fixture", Output: "No operation, screen or public API changes."}}}}
	var order []string
	engine := Chain{Store: store, Workflow: flow, WaitAfter: "ask_requester", RetryDelay: time.Millisecond,
		Router: testRouter(func(context.Context, State) (Assignment, error) { return Assignment{Role: "deliver"}, nil }),
		Executor: testExecutor(func(_ context.Context, a Assignment, _ State) []Result {
			order = append(order, a.Role)
			return []Result{{Role: a.Role, Speaker: "fixture"}}
		})}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := engine.Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the run ended: %v", err)
	}
	if len(order) != 0 {
		t.Fatalf("a refused choice was launched: %v", order)
	}
	refused := false
	for _, record := range store.state.History {
		refused = refused || record.Role == "router" && strings.Contains(record.Error, `action "deliver" is not connected`)
	}
	if !refused {
		t.Fatalf("the refusal of the next stage was not recorded: %+v", store.state.History)
	}
}

// A correction sends the work back to the first stage. The requester's reply
// was about the change before it, so the change made again is asked about
// again before it can be delivered.
func TestAlwaysAskAsksAgainAboutAChangeMadeAfterTheReply(t *testing.T) {
	store := &memoryStore{state: State{Request: "Rename an internal helper."}}
	var offered [][]string
	var order []string
	engine := alwaysAskEngine(alwaysAskFlow(t), store, &offered, &order)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := engine.Run(ctx); !errors.Is(err, ErrWaiting) {
		t.Fatalf("the run did not wait for the requester: %v", err)
	}
	reply(store, "ここを直してください。")
	if err := engine.Run(ctx); !errors.Is(err, ErrWaiting) {
		t.Fatalf("the corrected change was not asked about: %v (launches %v)", err, order)
	}
	if slices.Contains(order, "deliver") {
		t.Fatalf("the corrected change was delivered without a reply about it: %v", order)
	}
	questions := 0
	for _, role := range order {
		if role == "ask_requester" {
			questions++
		}
	}
	elicited := slices.Index(order[1:], "elicit")
	if questions != 2 || elicited < 0 {
		t.Fatalf("the work did not go back to requirements and ask again: %v", order)
	}
	if last := offered[len(offered)-1]; slices.Contains(last, "deliver") {
		t.Fatalf("the decision about the corrected change was offered the delivery: %v", offered)
	}
	reply(store, "このまま納品してください。")
	if err := engine.Run(ctx); err != nil || !store.state.Done {
		t.Fatalf("the acceptance of the corrected change was not delivered: %v (launches %v)", err, order)
	}
}

// Only a reply to the question put after the change was read lets the change
// go on. A requester's comment taken in while the question role was out of
// the choices after the first stage, arriving once the change was read, is
// not a reply about this change: the requester is still asked about it.
func TestAlwaysAskCountsOnlyAReplyToTheQuestionAboutTheChange(t *testing.T) {
	store := &memoryStore{state: State{Request: "Rename an internal helper.",
		QuestionsWithoutPost: 2, QuestionUnavailable: "ask_requester"}}
	var offered [][]string
	var order []string
	engine := alwaysAskEngine(alwaysAskFlow(t), store, &offered, &order)
	commented := false
	engine.RequesterReply = func(context.Context, int64) (string, error) {
		if commented || !slices.Contains(order, "confirm_change") {
			return "", nil
		}
		commented = true
		return "別件の連絡です。", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := engine.Run(ctx); !errors.Is(err, ErrWaiting) {
		t.Fatalf("the run did not wait for a reply about the change: %v (launches %v)", err, order)
	}
	if !commented || slices.Contains(order, "deliver") || !slices.Contains(order, "ask_requester") {
		t.Fatalf("a comment that was no reply about the change let it go on: commented=%v launches %v", commented, order)
	}
	for _, choices := range offered {
		if slices.Contains(choices, "deliver") {
			t.Fatalf("the delivery was offered before a reply about the change: %v", offered)
		}
	}
}

// A reply counts for the stage whose question it answered. With two stages
// that confirm the change one after the other, the reply to the first one's
// question does not let the second one's decision go on.
func TestAlwaysAskCountsAReplyForTheStageWhoseQuestionItAnswered(t *testing.T) {
	flow := confirmFlow(t)
	flow.Stages = append(flow.Stages[:4:4], append([]Stage{{Name: "confirm_again", Kind: ModelStage, Confirm: true, AlwaysAsk: true}}, flow.Stages[4:]...)...)
	flow.Stages[3].AlwaysAsk = true
	state := State{Request: "r", Workflow: flow, History: []Result{
		{Role: "elicit", Speaker: "fixture"}, {Role: "work", Speaker: "fixture"}, {Role: "review", Speaker: "fixture"},
		{Role: "confirm_change", Speaker: "fixture"}, {Role: "ask_requester", Speaker: "questioner"},
		{Role: "ask_requester", Speaker: "requester", Output: "このまま納品してください。"},
		{Role: "confirm_change", Speaker: "fixture"}, {Role: "confirm_again", Speaker: "fixture"}}}
	if !state.changeAnswered(3) {
		t.Fatal("the reply to the first stage's question was not counted for it")
	}
	if state.changeAnswered(4) {
		t.Fatal("the reply to the first stage's question was counted for the second")
	}
	if actions := state.stageActions(); slices.Contains(actions, "deliver") {
		t.Fatalf("the second stage's decision was offered the next stage: %v", actions)
	}
}

// A question that posted nothing, or whose launch failed, still holds the
// request for the requester, as every question after the reading does: the
// question is not launched over and over for want of the next stage.
func TestAlwaysAskWaitsAfterAQuestionThatPostedNothing(t *testing.T) {
	for _, scenario := range []string{"nothing-posted", "launch-failed"} {
		t.Run(scenario, func(t *testing.T) {
			store := &memoryStore{state: State{Request: "Rename an internal helper."}}
			var offered [][]string
			var order []string
			engine := alwaysAskEngine(alwaysAskFlow(t), store, &offered, &order)
			executor := engine.Executor
			engine.Executor = testExecutor(func(ctx context.Context, a Assignment, s State) []Result {
				if a.Role == "ask_requester" && scenario == "launch-failed" {
					order = append(order, a.Role)
					return []Result{{Role: a.Role, Speaker: "questioner", Error: "comment submission not confirmed"}}
				}
				return executor.Execute(ctx, a, s)
			})
			engine.QuestionPosted = func(context.Context) (QuestionObservation, error) { return QuestionObservation{}, nil }
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := engine.Run(ctx); !errors.Is(err, ErrWaiting) || !store.state.Waiting || !store.state.WaitingWithoutQuestion {
				t.Fatalf("the request did not wait for the requester: %v waiting=%v unseen=%v (launches %v)", err, store.state.Waiting, store.state.WaitingWithoutQuestion, order)
			}
			questions := 0
			for _, role := range order {
				if role == "ask_requester" {
					questions++
				}
			}
			if questions != 1 || slices.Contains(order, "deliver") {
				t.Fatalf("the question was launched %d times: %v", questions, order)
			}
		})
	}
}

// Without always_ask nothing changes: the decision after the reading is
// offered the next stage, and a change the reading says nobody sees is
// delivered without a question.
func TestWithoutAlwaysAskTheDeliveryStaysOffered(t *testing.T) {
	store := &memoryStore{state: State{Request: "Rename an internal helper."}}
	var offered [][]string
	var order []string
	engine := alwaysAskEngine(confirmFlow(t), store, &offered, &order)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := engine.Run(ctx); err != nil || !store.state.Done {
		t.Fatalf("the run did not deliver: %v (launches %v)", err, order)
	}
	if slices.Contains(order, "ask_requester") || len(offered) != 1 || !slices.Equal(offered[0], []string{"ask_requester", "deliver", "elicit"}) {
		t.Fatalf("the run without the setting changed: offered %v, launches %v", offered, order)
	}
}

// What the runtime says with always_ask: the decision service is told that
// every change is shown to the requester and that the next stage waits for a
// reply about this change; the stage that reads the change is not told to
// say that no confirmation is needed; the question shows the change whatever
// it alters. Without the setting every one of these words is as before.
func TestAlwaysAskTellsTheRolesThatEveryChangeIsShown(t *testing.T) {
	read := func(flow *Workflow) State {
		return State{Request: "r", Workflow: flow, Step: "review", History: []Result{
			{Role: "elicit", Speaker: "fixture"}, {Role: "work", Speaker: "fixture"}, {Role: "review", Speaker: "fixture"}}}
	}
	asked := func(flow *Workflow) State {
		state := read(flow)
		state.Step, state.History = "confirm_change", append(state.History, Result{Role: "confirm_change", Speaker: "fixture"})
		return state
	}
	always := alwaysAskFlow(t)
	stage := read(always).stageInstruction("confirm_change")
	confirmationWordsPresent(t, "the stage's instruction", stage, "the change actually made in the checkout",
		"every change before it is delivered", "the next stage (deliver) is offered only after the requester has replied about this same change",
		"covers only the change it was given about")
	if strings.Contains(stage, "依頼者の確認: なし") || strings.Contains(stage, "confirmation is not needed and why") {
		t.Fatalf("the stage is still told to say no confirmation is needed:\n%s", stage)
	}
	confirmationWordsPresent(t, "the question's instruction", asked(always).stageInstruction("ask_requester"),
		"shows the requester what the change does", "whatever it alters", "deliver it as it is", "name what to change", "not to deliver it", "Nothing is delivered before")
	decision := decisionInstructions(asked(always))
	confirmationWordsPresent(t, "the decision instructions", decision,
		"the requester sees every change before it is delivered",
		"Choose the question role when the requester has not replied about this same change",
		"Choose the next stage only when the requester's reply about this same change accepts it",
		"A reply given about an earlier change does not accept a later one")
	if strings.Contains(decision, "only when it changes how a person operates the product") || strings.Contains(decision, "establishes from the change it read that none of the three is altered") {
		t.Fatalf("the decision is still told to deliver a change nobody sees without asking:\n%s", decision)
	}
	// Without the setting, the words are those of the run before it.
	plain := confirmFlow(t)
	if got := read(plain).stageInstruction("confirm_change"); !strings.Contains(got, "依頼者の確認: なし") {
		t.Fatalf("the stage's instruction changed without the setting:\n%s", got)
	}
	if got := asked(plain).stageInstruction("ask_requester"); got != confirmationQuestionInstruction {
		t.Fatalf("the question's instruction changed without the setting:\n%s", got)
	}
	if got := decisionInstructions(asked(plain)); !strings.HasSuffix(got, "\n"+confirmationRouting) {
		t.Fatalf("the decision instructions changed without the setting")
	}
}

// always_ask belongs to a stage that confirms the change, whose question it
// is about; anywhere else it is refused, with the stage it was written on.
func TestAlwaysAskIsOnlyForAStageThatConfirmsTheChange(t *testing.T) {
	if err := alwaysAskFlow(t).Validate(confirmPurposes()); err != nil {
		t.Fatalf("always_ask on the stage that confirms the change was refused: %v", err)
	}
	flow := confirmFlow(t)
	flow.Stages[1].AlwaysAsk = true // work: a model stage that does not confirm
	err := flow.Validate(confirmPurposes())
	if err == nil || !strings.Contains(err.Error(), `stage "work"`) || !strings.Contains(err.Error(), "always_ask") {
		t.Fatalf("always_ask on a stage that does not confirm the change was not refused where it is: %v", err)
	}
	accepted := alwaysAskFlow(t).clone()
	if _, at := stageAt(accepted.Stages, "confirm_change"); at < 0 || !accepted.Stages[at].AlwaysAsk {
		t.Fatal("the accepted run's copy lost always_ask")
	}
}
