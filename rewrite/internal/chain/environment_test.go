package chain

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCgroupFactsDistinguishObservedLimitsMaxAndUnknown(t *testing.T) {
	observations := ExecutionEnvironment("Time limit for this request: unknown")
	for _, phrase := range []string{"Memory limit:", "memory.max of the controller's own cgroup v2", "limits of enclosing cgroups are not measured",
		"CPUs:", "Time limit for this request: unknown", "inside these same limits together with the controller",
		"Where the role launcher's memory guard runs, it stops the largest role process before the limit is reached",
		"without it, going over the memory limit can stop the controller and every role at once"} {
		if !strings.Contains(observations, phrase) {
			t.Errorf("observation scope is missing %q", phrase)
		}
	}
	// at is where the files are written, relative to the mount: the outside
	// case writes readable limits where its escaping path would lead.
	for _, tc := range []struct {
		name, membership, at, memory, cpu, wantMemory, wantCPU string
	}{
		{"limited", "0::/slice/task\n", "slice/task", "67108864\n", "150000 100000\n", "64 MiB", "1.5 CPUs"},
		{"unaligned", "0::/\n", "", "1000000\n", "200000 100000\n", "1000000 bytes", "2 CPUs"},
		{"max", "0::/\n", "", "max\n", "max 100000\n", "none set", "none set"},
		{"absent", "", "", "", "", "unknown", "unknown"},
		{"legacy", "7:memory:/task\n", "", "67108864", "150000 100000", "unknown", "unknown"},
		{"malformed", "0::/\n", "", "not a limit", "150000 0", "unknown", "unknown"},
		{"outside", "0::/../../other\n", "../../other", "67108864", "150000 100000", "unknown", "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "mount", "cgroup")
			directory := filepath.Join(root, tc.at)
			if err := os.MkdirAll(directory, 0700); err != nil {
				t.Fatal(err)
			}
			for name, value := range map[string]string{"memory.max": tc.memory, "cpu.max": tc.cpu} {
				if value != "" {
					if err := os.WriteFile(filepath.Join(directory, name), []byte(value), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			memory, cpu := cgroupFacts(root, tc.membership)
			if memory != tc.wantMemory || cpu != tc.wantCPU {
				t.Fatalf("memory=%q cpu=%q, want %q / %q", memory, cpu, tc.wantMemory, tc.wantCPU)
			}
		})
	}
}

func TestUnknownFeasibilityCanAskBeforeAnyImplementation(t *testing.T) {
	flow := stagesWorkflow()
	flow.Question = "ask_requester"
	store := &memoryStore{state: State{Request: "Build the requested tool and verify it here."}}
	var ran []string
	judge := testJudge(func(_ context.Context, state State, instructions string, choices map[string]string) (string, error) {
		// This stand-in checks wiring, not an actual model's semantic judgment.
		for _, text := range []string{instructions, processPrompt(Role{Name: "elicit"}, Process{}, Assignment{}, state)} {
			if strings.Contains(text, "Offer concrete alternatives only") || !strings.Contains(text, alternativesAfterHandoff) {
				t.Error("the initial feasibility choices conflict with an unrestricted authority-only instruction")
			}
			// This run has no stage that confirms the change: it is told nothing about one.
			for _, words := range []string{"confirms the change", "change-confirmation", alternativesOrChangeAfterHandoff} {
				if strings.Contains(text, words) {
					t.Errorf("a run without a stage that confirms the change was told %q", words)
				}
			}
			for _, phrase := range []string{"can run to its end within the execution environment facts", "or you cannot tell", "question role before implementation",
				"two numbered choices with a recommended answer", "run the heaviest part of the verification once as a trial and continue only if it fits",
				"or have the operator enlarge or prepare the environment, then continue this same request",
				"Recommend the trial when only the demand is unknown", "the operator's preparation when a fact already shows the environment falls short",
				"Offer handing the work to a person only when the requester explicitly wants that",
				"When the workflow offers no question role, nobody is asked: open the requirements report with the missing or insufficient fact and what the operator would have to prepare",
				"have the heaviest part of the verification run once first", "remove or lower verification", "does not change limits"} {
				if !strings.Contains(text, phrase) {
					t.Errorf("required resource guidance %q did not reach a role", phrase)
				}
			}
			// A person is not a standing choice, and nothing goes on as if a
			// recommendation to prepare the environment had been accepted.
			for _, phrase := range []string{"or leave the work to a person", "take the recommended answer as settled"} {
				if strings.Contains(text, phrase) {
					t.Errorf("the resource guidance still says %q", phrase)
				}
			}
		}
		if _, ok := choices["ask_requester"]; !ok {
			t.Fatal("no question choice")
		}
		return "ask_requester", nil
	})
	engine := Chain{Store: store, Workflow: flow,
		Router:    StageRouter{Entrance: DecisionRouter{Judge: judge, Roles: stagePurposes()}},
		WaitAfter: "ask_requester", RetryDelay: time.Millisecond,
		Executor: testExecutor(func(_ context.Context, assignment Assignment, _ State) []Result {
			ran = append(ran, assignment.Role)
			return []Result{{Role: assignment.Role, Speaker: "fixture", Output: "The tool exists but build-memory demand is unknown. Offer an authorized measured trial or operator preparation."}}
		})}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := engine.Run(ctx); !errors.Is(err, ErrWaiting) {
		t.Fatalf("not waiting: %v", err)
	}
	if strings.Join(ran, ",") != "elicit,ask_requester" || store.state.Done || !store.state.Waiting {
		t.Fatalf("unknown feasibility reached implementation: ran=%v state=%#v", ran, store.state)
	}
}

func TestEveryRoleReceivesControllerFactsWithoutChangingTheRequest(t *testing.T) {
	const facts = "Execution environment facts:\nMemory: unknown\nCPU: observed quota 1.5 CPUs\nActive-work cap: 17 minutes\nLocal child roles share the controller resource budget."
	executor := Processes{EnvironmentFacts: facts, Roles: map[string]Role{}}
	for _, name := range []string{"requirements", "work", "review", "report"} {
		executor.Roles[name] = Role{Name: name, Processes: []Process{{Name: "reader", Command: []string{"/bin/cat"}}}}
		state := State{Request: "Keep the original scope and delivery."}
		result := executor.Execute(context.Background(), Assignment{Role: name}, state)
		if len(result) != 1 || result[0].Error != "" || strings.Count(result[0].Output, facts) != 1 || !strings.Contains(result[0].Output, state.Request) {
			t.Fatalf("%s did not receive facts and original request: %#v", name, result)
		}
	}
}

func TestOnlyAConfirmingRunWidensTheAlternativesAfterHandoff(t *testing.T) {
	confirming := State{Request: "r", Workflow: confirmFlow(t)}
	for what, text := range map[string]string{"decision": decisionInstructions(confirming), "role": processPrompt(Role{}, Process{}, Assignment{}, confirming)} {
		if !strings.Contains(text, alternativesOrChangeAfterHandoff) || strings.Contains(text, alternativesAfterHandoff) {
			t.Errorf("the %s text does not offer alternatives for the change shown before delivery after handoff", what)
		}
	}
	plain := State{Request: "r", Workflow: stagesWorkflow()}
	for what, text := range map[string]string{"decision": decisionInstructions(plain), "role": processPrompt(Role{}, Process{}, Assignment{}, plain)} {
		if strings.Count(text, alternativesAfterHandoff) != 1 || strings.Contains(text, alternativesOrChangeAfterHandoff) {
			t.Errorf("the %s text of a run without that stage changed its alternatives after handoff", what)
		}
	}
}
