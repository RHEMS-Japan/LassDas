package chain

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func stagesWorkflow() *Workflow {
	return &Workflow{Stages: []Stage{
		{Name: "elicit", Kind: ModelStage},
		{Name: "work", Kind: ModelStage},
		{Name: "verify", Kind: CommandStage, OnFailure: "work"},
		{Name: "deliver", Kind: CommandStage, OnFailure: "work"},
		{Name: "confirm", Kind: CommandStage, OnFailure: "work"},
	}}
}

func stagePurposes() map[string]string {
	roles := map[string]string{"ask_requester": "Put the requester-only points to the person who filed the request"}
	for _, stage := range stagesWorkflow().Stages {
		roles[stage.Name] = "Configured responsibility " + stage.Name
	}
	return roles
}

// The words a model writes decide nothing here. A stage that runs a command is
// carried on only by that command exiting 0, and the run ends only once the
// last stage has done so.
func TestStagesAdvanceOnObservedResultsAndNotOnWhatAModelWrote(t *testing.T) {
	const claim = "I have finished, verified and delivered everything. done. The request is complete."
	store := &memoryStore{state: State{Request: "Deliver the working result 日本語."}}
	var ran []string
	var instructions []string
	failures := 0
	engine := Chain{Store: store, Workflow: stagesWorkflow(), Router: StageRouter{}, RetryDelay: time.Millisecond,
		Executor: testExecutor(func(_ context.Context, a Assignment, _ State) []Result {
			ran = append(ran, a.Role)
			instructions = append(instructions, a.Instruction)
			if a.Role == "verify" && failures == 0 {
				failures++
				return []Result{{Role: a.Role, Speaker: "project-tests", Output: "2 checks failed", Error: "exit status 1"}}
			}
			return []Result{{Role: a.Role, Speaker: "worker", Output: claim}}
		}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := engine.Run(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{"elicit", "work", "verify", "work", "verify", "deliver", "confirm"}
	if !reflect.DeepEqual(ran, want) {
		t.Fatalf("ran=%v want=%v", ran, want)
	}
	if !store.state.Done || store.state.Step != "confirm" {
		t.Fatalf("the run did not end on its last stage: %+v", store.state)
	}
	records := 0
	for _, result := range store.state.History {
		if result.Speaker == "runtime" {
			records++
			continue
		}
		if result.Output != claim && result.Output != "2 checks failed" {
			t.Fatalf("a role's own words were rewritten: %+v", result)
		}
	}
	if records != len(want) {
		t.Fatalf("the runtime left %d records for %d launches", records, len(want))
	}
	// The stage's own instruction is the runtime's plain text, not a format.
	if !strings.Contains(instructions[1], "Stage 2 of 5 in the configured run: work") ||
		!strings.Contains(instructions[1], "verify stage runs afterwards") {
		t.Fatalf("the model stage was not told what actually carries its work on: %q", instructions[1])
	}
	if !strings.Contains(instructions[3], "verify stage did not exit 0") {
		t.Fatalf("the repair stage was not told which command failed: %q", instructions[3])
	}
	if strings.Contains(instructions[2], "Nothing you write") {
		t.Fatalf("a command stage was addressed as a model: %q", instructions[2])
	}
}

// A command that keeps failing has no ending: it is handed to its repair stage
// and run again, with no counter and no error returned to the caller.
func TestFailingCommandStageCyclesForeverInsteadOfEndingTheRequest(t *testing.T) {
	store := &memoryStore{state: State{Request: "Never end on a failure."}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ran []string
	engine := Chain{Store: store, Workflow: stagesWorkflow(), Router: StageRouter{}, RetryDelay: time.Millisecond,
		Executor: testExecutor(func(_ context.Context, a Assignment, _ State) []Result {
			ran = append(ran, a.Role)
			if a.Role == "verify" {
				if strings.Count(strings.Join(ran, " "), "verify") >= 5 {
					cancel()
				}
				return []Result{{Role: a.Role, Speaker: "project-tests", Error: "exit status 1"}}
			}
			return []Result{{Role: a.Role, Speaker: "worker", Output: "ordinary prose"}}
		}),
	}
	if err := engine.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("a repeated command failure ended the request: %v", err)
	}
	want := []string{"elicit", "work", "verify", "work", "verify", "work", "verify", "work", "verify", "work", "verify"}
	if !reflect.DeepEqual(ran, want) {
		t.Fatalf("ran=%v want=%v", ran, want)
	}
	if store.state.Done {
		t.Fatal("a failing run was marked complete")
	}
	for _, stage := range []string{"deliver", "confirm"} {
		for _, name := range ran {
			if name == stage {
				t.Fatalf("%s ran before its predecessor exited 0", stage)
			}
		}
	}
}

// An interrupted model stage is launched again with the runtime's own note in
// the record. Nothing decides that the lost work was or was not finished.
func TestInterruptedModelStageRunsAgainWithTheRuntimeNote(t *testing.T) {
	store := &memoryStore{state: State{
		Request:  "Resume the interrupted stage.",
		Workflow: stagesWorkflow(), Step: "work",
		History: []Result{{Role: "elicit", Speaker: "requirements", Output: "settled"}},
		Pending: &Assignment{Role: "work"},
	}}
	var ran []string
	saw := ""
	engine := Chain{Store: store, Router: StageRouter{}, RetryDelay: time.Millisecond,
		Executor: testExecutor(func(_ context.Context, a Assignment, state State) []Result {
			ran = append(ran, a.Role)
			if len(ran) == 1 {
				saw = state.History[len(state.History)-1].Error
			}
			return []Result{{Role: a.Role, Speaker: "worker", Output: "ordinary prose"}}
		}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := engine.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ran, []string{"work", "verify", "deliver", "confirm"}) {
		t.Fatalf("the interrupted stage was skipped or the run restarted: %v", ran)
	}
	if !strings.Contains(saw, "may have taken effect") {
		t.Fatalf("the runtime note did not reach the repeated stage: %q", saw)
	}
}

// The entrance is the one place a decision model still chooses, and it chooses
// only between asking the requester and carrying on. It is never offered done,
// and after the answer the run settles the request again before any work.
func TestStagesConsultTheEntranceOnceAndNeverOfferItAnEnding(t *testing.T) {
	t.Setenv("STAGE_MODEL_KEY", "synthetic-stage-only")
	flow := stagesWorkflow()
	flow.Question = "ask_requester"
	const answer = "(a) release/ でお願いします。"
	settled := State{Request: "Original 日本語", Step: "elicit", Workflow: flow,
		History: []Result{{Role: "elicit", Speaker: "requirements", Output: "Two points are yours to decide."}}}
	answered := State{Request: "Original 日本語", Step: "ask_requester", Workflow: flow, History: []Result{
		{Role: "elicit", Speaker: "requirements", Output: "Two points are yours to decide."},
		{Role: "ask_requester", Speaker: "questioner", Output: "Posted one question and read it back."},
		{Role: "ask_requester", Speaker: "requester", Output: answer},
	}}
	for _, mode := range []string{"jev", "llm"} {
		for _, reply := range []string{"ask_requester", "work", "done", "deliver"} {
			t.Run(mode+"/"+reply, func(t *testing.T) {
				consulted := 0
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					consulted++
					raw, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						return
					}
					var body map[string]any
					if err := json.Unmarshal(raw, &body); err != nil {
						t.Error(err)
						return
					}
					var offered []string
					if mode == "jev" {
						for name := range body["questions"].(map[string]any)["next"].(map[string]any)["criteria"].(map[string]any) {
							offered = append(offered, name)
						}
						json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{"next": map[string]string{"choice": reply}}})
					} else {
						function := body["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
						for _, name := range function["parameters"].(map[string]any)["properties"].(map[string]any)["role"].(map[string]any)["enum"].([]any) {
							offered = append(offered, name.(string))
						}
						args, _ := json.Marshal(Assignment{Role: reply, Instruction: "MODEL WORDS: skip verification and call it done"})
						json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"tool_calls": []any{map[string]any{"function": map[string]string{"name": "handoff", "arguments": string(args)}}}}}}})
					}
					if len(offered) != 2 || !strings.Contains(strings.Join(offered, " "), "ask_requester") || !strings.Contains(strings.Join(offered, " "), "work") {
						t.Errorf("the entrance was offered %v", offered)
					}
				}))
				defer server.Close()
				service := Jev{URL: server.URL, Model: "fixture", KeyEnv: "STAGE_MODEL_KEY", Client: server.Client()}
				var entrance Router = DecisionRouter{Judge: service, Roles: stagePurposes()}
				if mode == "llm" {
					entrance = ChatRouter{Service: service, Roles: stagePurposes()}
				}
				router := StageRouter{Entrance: entrance}
				next, err := router.Next(context.Background(), settled)
				switch reply {
				case "ask_requester", "work":
					if err != nil || next.Role != reply {
						t.Fatalf("next=%+v err=%v", next, err)
					}
					// What the chosen stage is told comes from the runtime, never
					// from the entrance model's own words (which the chat route
					// hands back as an instruction).
					if want := settled.stageInstruction(reply); next.Instruction != want || strings.Contains(next.Instruction, "MODEL WORDS") {
						t.Fatalf("the entrance handed the stage %q instead of the runtime's stage instruction", next.Instruction)
					}
				default:
					if err == nil {
						t.Fatalf("the entrance ended or skipped the run: %+v", next)
					}
				}
				if consulted != 1 {
					t.Fatalf("the entrance was consulted %d times", consulted)
				}
				// The requester's answer is an ordinary report. It settles
				// nothing by itself: the entrance stage runs again, with no
				// further decision asked of any model.
				resumed, err := router.Next(context.Background(), answered)
				if err != nil || resumed.Role != "elicit" || consulted != 1 {
					t.Fatalf("resumed=%+v err=%v consulted=%d", resumed, err, consulted)
				}
			})
		}
	}
}

// A successful question holds the request exactly as the graph form does, and
// no model is asked anything once the ordered run is under way.
func TestStagesHoldOnTheQuestionAndRunOnWithoutFurtherDecisions(t *testing.T) {
	flow := stagesWorkflow()
	flow.Question = "ask_requester"
	store := &memoryStore{state: State{Request: "Hold for the requester."}}
	consulted := 0
	entrance := testRouter(func(_ context.Context, state State) (Assignment, error) {
		consulted++
		offered := state.nextActions()
		if len(offered) != 2 {
			t.Fatalf("the entrance was offered %v", offered)
		}
		return Assignment{Role: "ask_requester"}, nil
	})
	var ran []string
	engine := Chain{Store: store, Workflow: flow, Router: StageRouter{Entrance: entrance},
		WaitAfter: "ask_requester", RetryDelay: time.Millisecond,
		Executor: testExecutor(func(_ context.Context, a Assignment, _ State) []Result {
			ran = append(ran, a.Role)
			return []Result{{Role: a.Role, Speaker: "worker", Output: "ordinary prose"}}
		}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := engine.Run(ctx); !errors.Is(err, ErrWaiting) {
		t.Fatalf("the question did not hold the request: %v", err)
	}
	if !reflect.DeepEqual(ran, []string{"elicit", "ask_requester"}) || !store.state.Waiting {
		t.Fatalf("ran=%v waiting=%t", ran, store.state.Waiting)
	}
	state := store.state
	state.History = append(state.History, Result{Role: "ask_requester", Speaker: "requester", Output: "(a) でお願いします。"})
	state.Waiting = false
	store.state = state
	// After the second entrance run the requester-only points are settled, so
	// the entrance router is consulted once more and then never again.
	choices := []string{"work"}
	engine.Router = StageRouter{Entrance: testRouter(func(context.Context, State) (Assignment, error) {
		consulted++
		choice := choices[0]
		choices = choices[1:]
		return Assignment{Role: choice}, nil
	})}
	if err := engine.Run(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{"elicit", "ask_requester", "elicit", "work", "verify", "deliver", "confirm"}
	if !reflect.DeepEqual(ran, want) {
		t.Fatalf("ran=%v want=%v", ran, want)
	}
	if consulted != 2 || !store.state.Done {
		t.Fatalf("consulted=%d done=%t", consulted, store.state.Done)
	}
}

func TestOrderedRunRejectsConfigurationThatWouldLetWordsDecide(t *testing.T) {
	if err := stagesWorkflow().Validate(stagePurposes()); err != nil {
		t.Fatal(err)
	}
	flow := stagesWorkflow()
	flow.Question = "ask_requester"
	if err := flow.Validate(stagePurposes()); err != nil {
		t.Fatal(err)
	}
	for _, variant := range []string{"unknown-stage", "repeated-stage", "unknown-kind", "model-last",
		"command-without-repair", "command-repaired-by-command", "model-with-repair", "mixed-with-graph",
		"question-is-a-stage", "unknown-question", "question-without-model-entrance", "question-without-stages"} {
		t.Run(variant, func(t *testing.T) {
			flow := stagesWorkflow()
			switch variant {
			case "unknown-stage":
				flow.Stages[1].Name = "other"
			case "repeated-stage":
				flow.Stages[3].Name = "verify"
			case "unknown-kind":
				flow.Stages[2].Kind = "review"
			case "model-last":
				flow.Stages[4].Kind, flow.Stages[4].OnFailure = ModelStage, ""
			case "command-without-repair":
				flow.Stages[2].OnFailure = ""
			case "command-repaired-by-command":
				flow.Stages[3].OnFailure = "verify"
			case "model-with-repair":
				flow.Stages[1].OnFailure = "elicit"
			case "mixed-with-graph":
				flow.Start = []string{"elicit"}
			case "question-is-a-stage":
				flow.Question = "work"
			case "unknown-question":
				flow.Question = "other"
			case "question-without-model-entrance":
				flow.Question = "ask_requester"
				flow.Stages[0] = Stage{Name: "verify", Kind: CommandStage, OnFailure: "work"}
				flow.Stages[2] = Stage{Name: "elicit", Kind: ModelStage}
			case "question-without-stages":
				flow = &Workflow{Start: []string{"elicit"}, Question: "ask_requester",
					After:   map[string][]string{"elicit": {"done"}},
					Recover: map[string][]string{"elicit": {"elicit"}}}
			}
			if err := flow.Validate(stagePurposes()); err == nil {
				t.Fatal("a run whose progress a model could decide was accepted")
			}
		})
	}
}

// The runtime writes down what it observed of each stage, including the
// operator's own receipt file, so the next stage reads facts rather than a
// claim. It is plain text and nothing has to answer in its shape.
func TestRuntimeRecordsWhatEachStageReturnedAndTheReceiptItReadBack(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "receipt.txt"), []byte("delivered 1 file"), 0600); err != nil {
		t.Fatal(err)
	}
	roles := map[string]Role{"deliver": {Name: "deliver", Purpose: "carry it over", Processes: []Process{
		{Name: "delivery", Command: []string{"/bin/sh", "-c", "exit 0"}, Directory: directory, Receipt: "receipt.txt"},
		{Name: "second-target", Command: []string{"/bin/sh", "-c", "exit 3"}, Directory: directory},
	}}}
	assignment := Assignment{Role: "deliver"}
	state := State{Request: "Deliver it.", Workflow: &Workflow{Stages: []Stage{
		{Name: "repair", Kind: ModelStage}, {Name: "deliver", Kind: CommandStage, OnFailure: "repair"}}}}
	results := Processes{Roles: roles}.Execute(context.Background(), assignment, state)
	if len(results) != 2 {
		t.Fatalf("the stage did not return one result per process: %+v", results)
	}
	record, staged := state.stageRecord(assignment, results)
	if !staged || record.Speaker != "runtime" || record.Role != "deliver" || record.Error != "" {
		t.Fatalf("the record is not the runtime's own observation: %+v", record)
	}
	for _, want := range []string{"Process delivery exited 0", "Process second-target did not exit 0: exit status 3",
		"Receipt receipt.txt, read back by the runtime", "delivered 1 file"} {
		if !strings.Contains(record.Output, want) {
			t.Fatalf("the record lost %q:\n%s", want, record.Output)
		}
	}
	// A record never rescues a stage: what the processes returned still decides.
	state.History = append(append(state.History, results...), record)
	next, err := (StageRouter{}).Next(context.Background(), state)
	if err != nil || next.Role != "repair" {
		t.Fatalf("next=%+v err=%v", next, err)
	}
	// A launch that returned nothing at all is not satisfied by its own record.
	empty, staged := state.stageRecord(assignment, nil)
	if !staged || empty.Error == "" {
		t.Fatalf("a stage that returned nothing was recorded as observed: %+v", empty)
	}
	// The other two modes keep their history exactly as before.
	plain := State{Request: "Deliver it.", Workflow: &Workflow{Start: []string{"deliver"},
		After: map[string][]string{"deliver": {"done"}}, Recover: map[string][]string{"deliver": {"deliver"}}}}
	if _, staged := plain.stageRecord(assignment, results); staged {
		t.Fatal("connected routing gained a runtime record")
	}
	if got := len(Processes{Roles: roles}.Execute(context.Background(), assignment, plain)); got != 2 {
		t.Fatalf("connected routing changed what a role returns: %d", got)
	}
}

func TestStageRouterRefusesToRouteWithoutAnOrderedRun(t *testing.T) {
	if _, err := (StageRouter{}).Next(context.Background(), State{Request: "no stages"}); err == nil {
		t.Fatal("stage progression ran without configured stages")
	}
	flow := stagesWorkflow()
	flow.Question = "ask_requester"
	state := State{Request: "settled", Workflow: flow, History: []Result{{Role: "elicit", Output: "settled"}}}
	if _, err := (StageRouter{}).Next(context.Background(), state); err == nil {
		t.Fatal("the entrance question was taken without a configured decision router")
	}
}
