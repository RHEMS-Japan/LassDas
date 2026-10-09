package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ticket-runner/internal/chain"
)

func TestTimeFactsUseTheSavedCapAndSayUnknownWhenUnreadable(t *testing.T) {
	directory := t.TempDir()
	if got := requestTimeFacts(directory); !strings.Contains(got, "none was saved") {
		t.Fatal(got)
	}
	if err := acceptWorkLimit(directory, 17, 3); err != nil {
		t.Fatal(err)
	}
	if got := requestTimeFacts(directory); !strings.Contains(got, "17 minutes of active work") || !strings.Contains(got, "0s of it already used") {
		t.Fatal(got)
	}
	if err := os.WriteFile(filepath.Join(directory, workLimitFile), []byte("broken record"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := requestTimeFacts(directory); !strings.Contains(got, "unknown") || strings.Contains(got, "none was saved") {
		t.Fatal(got)
	}
}

func TestWatchedRequestPassesSavedEnvironmentToDecisionAndRole(t *testing.T) {
	for _, mode := range []string{"jev", "llm"} {
		t.Run(mode, func(t *testing.T) {
			cfg := watchConfiguration(t)
			cfg.Router.Mode = mode
			if mode == "llm" {
				cfg.Router.LLM = cfg.Router.Decision
				cfg.Router.Decision = chain.Jev{}
			}
			cfg.Intake.MaxActiveMinutes = 99 // A new setting must not replace this request's saved 17.
			cfg.Roles[0].Processes[0].Command = []string{"/bin/cat"}
			root, directory := noticeJob(t, chain.State{})
			if err := acceptWorkLimit(directory, 17, 3); err != nil {
				t.Fatal(err)
			}
			decisions := 0
			useWatchTransport(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Host == "watch-tracker.example" {
					return selectionReply(r, 200, []any{}), nil
				}
				var body struct {
					State     chain.State
					Questions map[string]struct{ Instructions string }
					Messages  []struct{ Role, Content string }
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					return nil, err
				}
				instructions := body.Questions["next"].Instructions
				if mode == "llm" {
					for _, message := range body.Messages {
						if message.Role == "system" {
							instructions = message.Content
						}
						if message.Role == "user" {
							if err := json.Unmarshal([]byte(message.Content), &body.State); err != nil {
								return nil, err
							}
						}
					}
				}
				decisions++
				for _, phrase := range []string{"Execution environment facts", "17 minutes", "Memory limit:", "CPUs:"} {
					if !strings.Contains(instructions, phrase) {
						t.Errorf("decision model did not directly receive %q", phrase)
					}
				}
				if strings.Contains(instructions, "99 minutes") {
					t.Error("current configuration replaced the saved cap")
				}
				choice := "implement"
				if len(body.State.History) > 0 {
					choice = "done"
				}
				if mode == "llm" {
					return routingSelectionReply(r, chain.Assignment{Role: choice}), nil
				}
				return selectionReply(r, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": choice}}}), nil
			})
			finish := startStopQueue(t, cfg, root, 10*time.Millisecond, io.Discard)
			waitFor(t, func() bool { state, err := loadWatchState(root, 51); return err == nil && state.Done })
			finish()
			state, err := loadWatchState(root, 51)
			if err != nil || !state.Done || len(state.History) != 1 || decisions != 2 {
				t.Fatalf("state=%#v decisions=%d error=%v", state, decisions, err)
			}
			for _, phrase := range []string{"Execution environment facts", "Memory limit:", "CPUs:", "17 minutes", "inside these same limits together with the controller", "unknown", noticeRequest} {
				if !strings.Contains(state.History[0].Output, phrase) {
					t.Errorf("role did not receive %q", phrase)
				}
			}
			if _, err := os.Stat(filepath.Join(directory, "run", workLimitFile)); !os.IsNotExist(err) {
				t.Fatalf("fixture or implementation placed the limit beside the history: %v", err)
			}
		})
	}
}

func TestDirectRunDoesNotBorrowAParentsTimeLimit(t *testing.T) {
	directory := t.TempDir()
	if err := acceptWorkLimit(directory, 17, 3); err != nil {
		t.Fatal(err)
	}
	request := filepath.Join(directory, "request.txt")
	environmentWrite(t, request, []byte("Observe only this request."))
	var cfg config
	cfg.Router.Mode = "single"
	cfg.Roles = []chain.Role{{Name: "observe", Processes: []chain.Process{{Name: "reader", Command: []string{"/bin/cat"}}}}}
	configuration := filepath.Join(directory, "operator.json")
	environmentJSON(t, configuration, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	runDir := filepath.Join(directory, "run")
	if err := run(ctx, []string{"--config", configuration, "--request", request, "--run-dir", runDir}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	store, err := chain.Open(runDir, "Observe only this request.")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	state, err := store.Load()
	if err != nil || len(state.History) != 1 {
		t.Fatalf("state=%#v error=%v", state, err)
	}
	text := state.History[0].Output
	if !strings.Contains(text, "Time limit for this run: none applies") || strings.Contains(text, "17 minutes") {
		t.Fatalf("standalone run misreported a watch cap: %s", text)
	}
}
