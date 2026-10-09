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
	for _, tc := range []struct {
		name, membership, memory, cpu, wantMemory, wantCPU string
	}{
		{"limited", "0::/slice/task\n", "67108864\n", "150000 100000\n", "64 MiB", "1.5 CPUs"},
		{"unaligned", "0::/\n", "1000000\n", "200000 100000\n", "1000000 bytes", "2 CPUs"},
		{"max", "0::/\n", "max\n", "max 100000\n", "none set", "none set"},
		{"absent", "", "", "", "unknown", "unknown"},
		{"legacy", "7:memory:/task\n", "67108864", "150000 100000", "unknown", "unknown"},
		{"malformed", "0::/\n", "not a limit", "150000 0", "unknown", "unknown"},
		{"outside", "0::/../../other\n", "67108864", "150000 100000", "unknown", "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			directory := root
			if tc.name == "limited" {
				directory = filepath.Join(root, "slice", "task")
			}
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
			if strings.Contains(text, "Offer concrete alternatives only for that authority decision.") ||
				!strings.Contains(text, "After handoff, offer concrete alternatives only for questions permitted by") {
				t.Error("the initial feasibility choices conflict with an unrestricted authority-only instruction")
			}
			for _, phrase := range []string{"can run to its end within the execution environment facts", "or you cannot tell", "question role before implementation",
				"numbered choices with a recommended answer", "try the heaviest step once", "have the operator enlarge or prepare the environment, then continue this same request",
				"leave the work to a person", "Recommend the trial when only the demand is unknown", "the operator's preparation when a fact already shows the environment falls short",
				"do not recommend leaving the work to a person", "When the workflow offers no question role, take the recommended answer as settled", "does not change limits"} {
				if !strings.Contains(text, phrase) {
					t.Errorf("required resource guidance %q did not reach a role", phrase)
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
