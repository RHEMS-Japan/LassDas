package chain

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
// that confirm the change one after the other, the second asking about every
// change, the reply to the first one's question does not let the second
// one's decision go on.
func TestAlwaysAskCountsAReplyForTheStageWhoseQuestionItAnswered(t *testing.T) {
	flow := confirmFlow(t)
	flow.Stages = append(flow.Stages[:4:4], append([]Stage{{Name: "confirm_again", Kind: ModelStage, Confirm: true, AlwaysAsk: true}}, flow.Stages[4:]...)...)
	purposes := confirmPurposes()
	purposes["confirm_again"] = "Read the change again."
	if err := flow.Validate(purposes); err != nil {
		t.Fatal(err)
	}
	state := State{Request: "r", Workflow: flow, History: []Result{
		{Role: "elicit", Speaker: "fixture"}, {Role: "work", Speaker: "fixture"}, {Role: "review", Speaker: "fixture"},
		{Role: "confirm_change", Speaker: "fixture"}, {Role: "ask_requester", Speaker: "questioner"},
		{Role: "ask_requester", Speaker: "requester", Output: "このまま納品してください。"},
		{Role: "confirm_change", Speaker: "fixture"}, {Role: "confirm_again", Speaker: "fixture"}}}
	// At the first stage's own decision, after it read the reply.
	if first := (State{Request: "r", Workflow: flow, History: state.History[:7]}); !first.changeAnswered(3) {
		t.Fatal("the reply to the first stage's question was not counted for it")
	}
	if state.changeAnswered(4) {
		t.Fatal("the reply to the first stage's question was counted for the second")
	}
	if actions := state.stageActions(); slices.Contains(actions, "deliver") {
		t.Fatalf("the second stage's decision was offered the next stage: %v", actions)
	}
}

// A stage after the one that asks, run after the requester's reply, also has
// the change asked about again: a delivery whose failure returns the work to
// the asking stage itself leaves a change the reply was not about. The check
// refuses that shape before work is accepted (below); the count holds as well,
// should a record ever take it. The shape the shipped examples use, a failure
// sent back to the first stage, does the same through the first stage.
func TestAlwaysAskAsksAgainAfterALaterStageRanSinceTheReply(t *testing.T) {
	model := func(name string) Stage { return Stage{Name: name, Kind: ModelStage} }
	ran := func(role string) Result { return Result{Role: role, Speaker: "worker", Output: "done"} }
	failed := func(role string) Result { return Result{Role: role, Speaker: "worker", Error: "exit status 1"} }
	asked := Result{Role: "ask", Speaker: "questioner", Output: "asked"}
	accepted := Result{Role: "ask", Speaker: "requester", Output: "deliver it"}
	for _, shape := range []struct {
		name      string
		onFailure string
		history   []Result
	}{
		{"failure-back-to-the-asking-stage", "c", []Result{ran("e"), ran("w"), ran("c"), asked, accepted, ran("c"), failed("d"), ran("c")}},
		{"failure-back-to-the-first-stage", "e", []Result{ran("e"), ran("w"), ran("c"), asked, accepted, ran("c"), failed("d"), ran("e"), ran("w"), ran("c")}},
	} {
		t.Run(shape.name, func(t *testing.T) {
			flow := &Workflow{Question: "ask", Stages: []Stage{model("e"), model("w"),
				{Name: "c", Kind: ModelStage, Confirm: true, AlwaysAsk: true}, {Name: "d", Kind: CommandStage, OnFailure: shape.onFailure}}}
			if err := flow.validateStages(map[string]string{"e": "", "w": "", "c": "", "d": "", "ask": ""}); (err == nil) != (shape.onFailure == "e") {
				t.Fatalf("the check of the shape: %v", err)
			}
			state := State{Request: "r", Workflow: flow, History: shape.history}
			if actions := state.stageActions(); !slices.Equal(actions, []string{"e", "ask"}) {
				t.Fatalf("after a later stage ran since the reply the decision is offered %v", actions)
			}
			// Before the failure the reply let the change go on.
			before := State{Request: "r", Workflow: flow, History: shape.history[:6]}
			if actions := before.stageActions(); !slices.Contains(actions, "d") {
				t.Fatalf("the reply about the change did not offer the delivery: %v", actions)
			}
		})
	}
}

// When the first stage has reached the confirmation rework limit, the note
// says what is left: with always_ask and no reply about this change, only the
// question, since the next stage is not offered either.
func TestAlwaysAskReworkLimitNoteNamesOnlyTheQuestion(t *testing.T) {
	cycle := []Result{{Role: "elicit", Speaker: "fixture"}, {Role: "work", Speaker: "fixture"}, {Role: "review", Speaker: "fixture"}, {Role: "confirm_change", Speaker: "fixture"}}
	var history []Result
	for i := 0; i < 3; i++ {
		history = append(history, cycle...)
	}
	for _, setting := range []string{"always-ask", "without"} {
		t.Run(setting, func(t *testing.T) {
			flow := confirmFlow(t)
			if setting == "always-ask" {
				flow = alwaysAskFlow(t)
			}
			state := State{Request: "r", Workflow: flow, Step: "confirm_change", History: append([]Result(nil), history...)}
			notes := state.launchLimitNotes()
			if len(notes) != 1 || !strings.Contains(notes[0].Output, "confirmation rework limit") {
				t.Fatalf("no note of the reached limit: %+v (actions %v)", notes, state.stageActions())
			}
			nextStage := strings.Contains(notes[0].Output, "go on to the next stage")
			if setting == "always-ask" && (nextStage || !strings.Contains(notes[0].Output, "Ask the requester, without weakening the original requirements.")) {
				t.Fatalf("the note offers the next stage, which is not offered: %q", notes[0].Output)
			}
			if setting == "without" && !nextStage {
				t.Fatalf("the note without the setting changed: %q", notes[0].Output)
			}
		})
	}
}

// The setting is written only where it is on: a run without it saves the same
// copy of its workflow as before the setting existed.
func TestAlwaysAskIsLeftOutOfTheCopyWhenItIsOff(t *testing.T) {
	off, err := json.Marshal(confirmFlow(t).clone())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(off), "always_ask") {
		t.Fatalf("a workflow without the setting writes it: %s", off)
	}
	on, err := json.Marshal(alwaysAskFlow(t).clone())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(on), `"confirm":true,"always_ask":true`) {
		t.Fatalf("a workflow with the setting does not write it: %s", on)
	}
}

// A model stage right after a stage that asks about every change could change
// the checkout after the requester's reply, and nothing would ask about that:
// such a run is refused, naming both stages. A command stage right after it,
// as in the shipped examples, whose later model stage writes the report after
// the delivery, is accepted, and so is the refused shape without the setting.
func TestAlwaysAskRefusesAModelStageRightAfterIt(t *testing.T) {
	model := func(name string) Stage { return Stage{Name: name, Kind: ModelStage} }
	confirm := func(always bool) Stage { return Stage{Name: "c", Kind: ModelStage, Confirm: true, AlwaysAsk: always} }
	command := func(name, onFailure string) Stage { return Stage{Name: name, Kind: CommandStage, OnFailure: onFailure} }
	roles := map[string]string{"e": "", "w": "", "c": "", "m": "", "d": "", "r": "", "k": "", "ask": ""}
	for _, shape := range []struct {
		name    string
		stages  []Stage
		refused bool
	}{
		{"model-stage-right-after", []Stage{model("e"), model("w"), confirm(true), model("m"), command("d", "e")}, true},
		{"command-stage-right-after", []Stage{model("e"), model("w"), confirm(true), command("d", "e")}, false},
		{"model-stage-after-the-delivery", []Stage{model("e"), model("w"), confirm(true), command("d", "e"), model("r"), command("k", "r")}, false},
		{"model-stage-right-after-without-the-setting", []Stage{model("e"), model("w"), confirm(false), model("m"), command("d", "e")}, false},
	} {
		t.Run(shape.name, func(t *testing.T) {
			err := (&Workflow{Question: "ask", Stages: shape.stages}).validateStages(roles)
			if !shape.refused {
				if err != nil {
					t.Fatalf("the run was refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), `stage "c"`) || !strings.Contains(err.Error(), `"m"`) || !strings.Contains(err.Error(), "always_ask") {
				t.Fatalf("the model stage right after the stage that asks about every change was not refused by name: %v", err)
			}
		})
	}
	shippedExamplesAcceptAlwaysAsk(t)
}

// shippedExamplesAcceptAlwaysAsk checks the two shipped ordered runs with
// always_ask turned on at their stage that confirms the change.
func shippedExamplesAcceptAlwaysAsk(t *testing.T) {
	t.Helper()
	for _, file := range []string{"operator-stages.json", "operator-github-stages.json"} {
		t.Run(file, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "..", "examples", file))
			if err != nil {
				t.Fatal(err)
			}
			var shipped struct {
				Workflow Workflow
				Intake   struct {
					QuestionRole string `json:"question_role"`
				}
				Roles []struct{ Name string }
			}
			if err := json.Unmarshal(raw, &shipped); err != nil {
				t.Fatal(err)
			}
			named := map[string]string{}
			for _, role := range shipped.Roles {
				named[role.Name] = ""
			}
			flow := shipped.Workflow
			flow.Question = shipped.Intake.QuestionRole
			turned := 0
			for i := range flow.Stages {
				if flow.Stages[i].Confirm {
					flow.Stages[i].AlwaysAsk = true
					turned++
				}
			}
			if turned != 1 {
				t.Fatalf("the example has %d stages that confirm the change", turned)
			}
			if err := flow.validateStages(named); err != nil {
				t.Fatalf("the shipped example with always_ask was refused: %v", err)
			}
		})
	}
}

// The delivery right after a stage that asks about every change sends its
// failure back to a stage before that one. A failure sent to a later model
// stage, such as the report, would run that stage and then deliver what it
// changed with nothing asking about it; one sent to the asking stage itself
// leaves the same gap for the count to close. Both are refused, naming the
// stages. A failure sent to a stage before it, the shipped examples, and the
// refused shape without the setting are accepted.
func TestAlwaysAskRefusesADeliveryFailingPastIt(t *testing.T) {
	model := func(name string) Stage { return Stage{Name: name, Kind: ModelStage} }
	confirm := func(always bool) Stage { return Stage{Name: "c", Kind: ModelStage, Confirm: true, AlwaysAsk: always} }
	command := func(name, onFailure string) Stage { return Stage{Name: name, Kind: CommandStage, OnFailure: onFailure} }
	roles := map[string]string{"e": "", "w": "", "c": "", "d": "", "r": "", "k": "", "ask": ""}
	for _, shape := range []struct {
		name    string
		stages  []Stage
		refused string
	}{
		{"failure-to-a-later-model-stage", []Stage{model("e"), model("w"), confirm(true), command("d", "r"), model("r"), command("k", "r")}, `"r"`},
		{"failure-to-the-asking-stage", []Stage{model("e"), model("w"), confirm(true), command("d", "c")}, `"c"`},
		{"failure-to-a-stage-before-it", []Stage{model("e"), model("w"), confirm(true), command("d", "w"), model("r"), command("k", "r")}, ""},
		{"failure-to-a-later-model-stage-without-the-setting", []Stage{model("e"), model("w"), confirm(false), command("d", "r"), model("r"), command("k", "r")}, ""},
	} {
		t.Run(shape.name, func(t *testing.T) {
			err := (&Workflow{Question: "ask", Stages: shape.stages}).validateStages(roles)
			if shape.refused == "" {
				if err != nil {
					t.Fatalf("the run was refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), `stage "c"`) || !strings.Contains(err.Error(), `stage "d"`) ||
				!strings.Contains(err.Error(), shape.refused) || !strings.Contains(err.Error(), "on_failure") {
				t.Fatalf("the delivery failing past the stage that asks about every change was not refused by name: %v", err)
			}
		})
	}
	shippedExamplesAcceptAlwaysAsk(t)
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
