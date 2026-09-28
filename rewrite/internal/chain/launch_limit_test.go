package chain

import (
	"context"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// A reviewer that always sends the draft back and a router that always
// follows it would go on all night. The operator caps the role the work is
// sent back to: at the cap that role leaves the offered choices, the router
// hears why in the runtime's own words, and the run goes on to the next
// connected action instead of ending.
func TestALaunchLimitTakesTheRoleTheWorkIsSentBackToOutOfTheChoices(t *testing.T) {
	workflow := &Workflow{
		Start:       []string{"draft"},
		After:       map[string][]string{"draft": {"review"}, "review": {"draft", "post"}, "post": {"done"}},
		Recover:     map[string][]string{"draft": {"draft"}, "review": {"review"}, "post": {"post"}},
		LaunchLimit: map[string]int{"draft": 2},
	}
	roles := map[string]string{"draft": "write the report", "review": "review the report", "post": "post the report"}
	if err := workflow.Validate(roles); err != nil {
		t.Fatal(err)
	}
	var offeredAtReview [][]string
	judge := testJudge(func(_ context.Context, s State, _ string, choices map[string]string) (string, error) {
		names := make([]string, 0, len(choices))
		for name := range choices {
			names = append(names, name)
		}
		sort.Strings(names)
		switch s.Step {
		case "review":
			offeredAtReview = append(offeredAtReview, names)
			if _, ok := choices["draft"]; ok {
				return "draft", nil // the reviewer's objection, followed every time
			}
			return "post", nil
		case "post":
			return "done", nil
		}
		return names[0], nil
	})
	store := &memoryStore{state: State{Request: "post the report"}}
	engine := Chain{Store: store, Workflow: workflow, RetryDelay: time.Millisecond,
		Router: DecisionRouter{Roles: roles, Judge: judge},
		Executor: testExecutor(func(_ context.Context, a Assignment, _ State) []Result {
			return []Result{{Role: a.Role, Output: "ordinary prose"}}
		}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := engine.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if !store.state.Done {
		t.Fatal("the run did not reach done")
	}
	var launched []string
	notes := 0
	for _, r := range store.state.History {
		if r.Speaker == "runtime" {
			if r.Role == "draft" && strings.Contains(r.Output, "launch limit") {
				notes++
			}
			continue
		}
		launched = append(launched, r.Role)
	}
	if want := []string{"draft", "review", "draft", "review", "post"}; !slices.Equal(launched, want) {
		t.Fatalf("launched %v, want %v", launched, want)
	}
	if len(offeredAtReview) != 2 || !slices.Equal(offeredAtReview[0], []string{"draft", "post"}) || !slices.Equal(offeredAtReview[1], []string{"post"}) {
		t.Fatalf("offered at the review decisions: %v", offeredAtReview)
	}
	if notes != 1 {
		t.Fatalf("runtime notes about the limit: %d, want exactly 1", notes)
	}
}

// When the capped role is the only action connected after a step, it stays
// offered: a cap changes what is offered and never leaves the decision empty.
func TestALaunchLimitNeverLeavesADecisionWithoutARole(t *testing.T) {
	workflow := &Workflow{
		Start:       []string{"fix"},
		After:       map[string][]string{"fix": {"check"}, "check": {"fix", "done"}},
		Recover:     map[string][]string{"fix": {"fix"}, "check": {"check"}},
		LaunchLimit: map[string]int{"check": 1},
	}
	roles := map[string]string{"fix": "fix", "check": "check"}
	checks := 0
	judge := testJudge(func(_ context.Context, s State, _ string, choices map[string]string) (string, error) {
		switch s.Step {
		case "":
			return "fix", nil
		case "fix":
			if _, ok := choices["check"]; !ok {
				t.Fatalf("after fix nothing but check is connected, yet it was not offered: %v", choices)
			}
			return "check", nil
		}
		checks++
		if checks == 1 {
			return "fix", nil
		}
		return "done", nil
	})
	store := &memoryStore{state: State{Request: "fix it"}}
	engine := Chain{Store: store, Workflow: workflow, RetryDelay: time.Millisecond,
		Router: DecisionRouter{Roles: roles, Judge: judge},
		Executor: testExecutor(func(_ context.Context, a Assignment, _ State) []Result {
			return []Result{{Role: a.Role, Output: "ordinary prose"}}
		}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := engine.Run(ctx); err != nil {
		t.Fatal(err)
	}
	var launched []string
	for _, r := range store.state.History {
		if r.Speaker == "runtime" {
			if r.Role == "router" {
				t.Fatalf("a routing failure was recorded: %s", r.Error)
			}
			continue
		}
		launched = append(launched, r.Role)
	}
	if want := []string{"fix", "check", "fix", "check"}; !slices.Equal(launched, want) || !store.state.Done {
		t.Fatalf("launched %v (done=%v), want %v and done", launched, store.state.Done, want)
	}
}

// A launch is a run of consecutive records for one role (a parallel group
// returns several at once); the runtime's notes are not launches, and the
// requester's answer starts the count again.
func TestLaunchesCountRunsOfRecordsSinceTheRequestersAnswer(t *testing.T) {
	before := State{History: []Result{{Role: "review"}, {Role: "review"}, {Role: "draft"}, {Role: "review"}}}
	if got := before.launches("review"); got != 2 {
		t.Fatalf("review launches before the answer: %d, want 2", got)
	}
	if got := before.launches("draft"); got != 1 {
		t.Fatalf("draft launches: %d, want 1", got)
	}
	after := State{History: append(slices.Clone(before.History),
		Result{Role: "ask", Speaker: "requester", Output: "the answer"},
		Result{Role: "review"},
		Result{Role: "review", Speaker: "runtime", Output: "a note"})}
	if got := after.launches("review"); got != 1 {
		t.Fatalf("review launches after the answer: %d, want 1", got)
	}
	if got := after.launches("draft"); got != 0 {
		t.Fatalf("draft launches after the answer: %d, want 0", got)
	}
}

func TestLaunchLimitIsCheckedWithTheWorkflow(t *testing.T) {
	roles := map[string]string{"draft": "", "review": ""}
	base := func() *Workflow {
		return &Workflow{Start: []string{"draft"},
			After:   map[string][]string{"draft": {"review"}, "review": {"draft", "done"}},
			Recover: map[string][]string{"draft": {"draft"}, "review": {"review"}}}
	}
	for name, limits := range map[string]map[string]int{
		"an unconfigured role": {"nobody": 2},
		"a cap below one":      {"draft": 0},
		"done":                 {"done": 1},
	} {
		w := base()
		w.LaunchLimit = limits
		if err := w.Validate(roles); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	staged := &Workflow{Stages: []Stage{{Name: "draft", Kind: ModelStage}, {Name: "check", Kind: CommandStage, OnFailure: "draft"}},
		LaunchLimit: map[string]int{"draft": 1}}
	if err := staged.Validate(map[string]string{"draft": "", "check": ""}); err == nil || !strings.Contains(err.Error(), "ordered run") {
		t.Fatalf("an ordered run accepted a launch limit: %v", err)
	}
	w := base()
	w.LaunchLimit = map[string]int{"draft": 2}
	if err := w.Validate(roles); err != nil {
		t.Fatal(err)
	}
	if !w.clone().atLaunchLimitFor("draft", 2) {
		t.Fatal("the clone lost the launch limit")
	}
}

// helper for the clone check above: a workflow with the limit and that many launches is at its cap.
func (w *Workflow) atLaunchLimitFor(role string, launches int) bool {
	s := State{Workflow: w}
	for i := 0; i < launches; i++ {
		s.History = append(s.History, Result{Role: role}, Result{Role: "other"})
	}
	return s.atLaunchLimit(role)
}
