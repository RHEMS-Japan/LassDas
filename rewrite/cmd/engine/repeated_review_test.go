package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ticket-runner/internal/chain"
)

// This child process scripts the semantic judgment. The test establishes
// instruction/history transport and saved reporting, not real-model accuracy.
func TestRepeatedReviewReportHelper(t *testing.T) {
	if os.Getenv("REPEATED_REPORT_FIXTURE") != "1" {
		return
	}
	prompt, err := io.ReadAll(os.Stdin)
	if err != nil {
		t.Fatal(err)
	}
	for _, phrase := range []string{"second independent occurrence", "not a new occurrence", "Do not create the follow-up PR", "TASK_HISTORY"} {
		if !strings.Contains(string(prompt), phrase) {
			t.Fatalf("reporter lost %q", phrase)
		}
	}
	raw, err := os.ReadFile(os.Getenv("TASK_HISTORY"))
	if err != nil {
		t.Fatal(err)
	}
	var state chain.State
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	observations := map[string]bool{}
	for _, result := range state.History {
		if result.Role == "review" && result.Error != "" {
			observations[result.Output] = true
		}
	}
	fmt.Println("The delivered change and its verification are described separately.")
	if observations["Review round 1: empty input loses the header."] && observations["Review round 2 after repair: empty input still loses the header."] {
		fmt.Println("Proposed follow-up PR (not created): add an empty-input regression check in the project's approved test location. Review rounds 1 and 2 observed the same lost header after a repair. Pass when empty input preserves the header byte for byte.")
	}
}

func TestReportProposesPreventionOnlyForAnObservedRecurrence(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"operator.json", "operator-gateway.json", "operator-github.json", "operator-stages.json", "operator-github-stages.json"} {
		cfg := loadExample(t, "../../examples/"+name)
		for _, role := range cfg.Roles {
			if role.Name != "report" && role.Name != "draft_report" {
				continue
			}
			for _, scenario := range []string{"first", "replayed-first", "second-after-repair"} {
				t.Run(name+"/"+scenario, func(t *testing.T) {
					store, err := chain.Open(t.TempDir(), "Repair empty-input handling within the approved project.")
					if err != nil {
						t.Fatal(err)
					}
					defer store.Close()
					state, err := store.Load()
					if err != nil {
						t.Fatal(err)
					}
					first := chain.Result{Role: "review", Speaker: "fixture", Output: "Review round 1: empty input loses the header.", Error: "exit status 1"}
					state.History = []chain.Result{first, {Role: "work", Speaker: "fixture", Output: "Attempted the header repair."}}
					if scenario == "replayed-first" {
						state.History = append(state.History, first)
					} else if scenario == "second-after-repair" {
						state.History = append(state.History, chain.Result{Role: "review", Speaker: "fixture", Output: "Review round 2 after repair: empty input still loses the header.", Error: "exit status 1"})
					}
					if err := store.Save(state); err != nil {
						t.Fatal(err)
					}
					process := chain.Process{Name: "scripted-report", Instructions: role.Processes[0].Instructions,
						Command: []string{binary, "-test.run=^TestRepeatedReviewReportHelper$"}, Env: map[string]string{"REPEATED_REPORT_FIXTURE": "1"}}
					executor := chain.Processes{Roles: map[string]chain.Role{role.Name: {Name: role.Name, Processes: []chain.Process{process}}}, HistoryPath: filepath.Join(store.Dir, "history.json")}
					results := executor.Execute(context.Background(), chain.Assignment{Role: role.Name}, state)
					if len(results) != 1 || results[0].Error != "" {
						t.Fatalf("report did not run: %+v", results)
					}
					if proposed := strings.Contains(results[0].Output, "Proposed follow-up PR"); proposed != (scenario == "second-after-repair") {
						t.Fatalf("wrong recurrence proposal: %s", results[0].Output)
					}
					state.History = append(state.History, results...)
					if err := store.Save(state); err != nil {
						t.Fatal(err)
					}
					saved, err := store.Load()
					if err != nil || saved.History[len(saved.History)-1].Output != results[0].Output {
						t.Fatal("the actual report was not saved unchanged", err)
					}
					t.Logf("%s: %s", scenario, strings.TrimSpace(results[0].Output))
				})
			}
		}
	}
}
