package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/attendant"
)

// Exercise the real mode selection, not just classification with a merge
// record pre-placed by the test. Only the external CLIs are fixtures.
func TestResidentObservesHumanMerge(t *testing.T) {
	for _, mode := range []string{"runner", "cards"} {
		for _, legacy := range []bool{false, true} {
			if mode == "cards" && legacy {
				continue
			}
			name := mode + "/persistent"
			if legacy {
				name = mode + "/canonical-workspace"
			}
			t.Run(name, func(t *testing.T) {
				configPath, boardPath, _ := observationFixture(t, mode)
				var config map[string]any
				raw, err := os.ReadFile(configPath)
				if err != nil || json.Unmarshal(raw, &config) != nil {
					t.Fatal("read fixture config", err)
				}
				root := filepath.Dir(configPath)
				write := func(path, content string, mode os.FileMode) {
					t.Helper()
					if err := os.WriteFile(path, []byte(content), mode); err != nil {
						t.Fatal(err)
					}
				}
				dir := filepath.Join(root, "runs", "delivery-example")
				if legacy {
					dir = filepath.Join(root, "canonical-workspace")
				}
				ticket := filepath.Join(dir, "history", "stage-1", "ticket.json")
				if err := os.MkdirAll(filepath.Dir(ticket), 0700); err != nil {
					t.Fatal(err)
				}
				write(ticket, `{}`, 0600)
				write(filepath.Join(dir, "feature-pr.json"), `{"payload":{"pull_request":{"Number":29}}}`, 0600)
				chain := config["chain"].(map[string]any)
				write(chain["target_token_path"].(string), "test-token", 0600)
				controller := filepath.Join(root, "controller")
				config["controller_bin"] = controller
				response := filepath.Join(root, "merge-response")
				calls := filepath.Join(root, "merge-calls")
				t.Setenv("MERGE_TEST_RESPONSE", response)
				t.Setenv("MERGE_TEST_CALLS", calls)
				t.Setenv("MERGE_TEST_TICKET", ticket)
				t.Setenv("MERGE_TEST_CONFIG", config["consumer_config_path"].(string))
				write(controller, `#!/bin/sh
[ "$#" = 9 ] && [ "$1" = read-merged ] && [ "$2" = --config ] && [ "$3" = "$MERGE_TEST_CONFIG" ] && [ "$4" = --ticket ] && [ "$5" = "$MERGE_TEST_TICKET" ] && [ "$6" = --number ] && [ "$7" = 29 ] && [ "$8" = --out ] && [ "$TARGET_GITHUB_TOKEN" = test-token ] || exit 94
printf 'read\n' >> "$MERGE_TEST_CALLS"
[ -f "$MERGE_TEST_RESPONSE" ] || exit 1
cp "$MERGE_TEST_RESPONSE" "$9"
`, 0700)
				// A terminal runner task must not be re-created or mutated.
				if mode == "runner" {
					tasks, _ := json.Marshal([]map[string]string{{"id": "task-example", "idempotency_key": "delivery-example", "status": "done", "workspace_path": dir}})
					taskFile := filepath.Join(root, "tasks.json")
					write(taskFile, string(tasks), 0600)
					t.Setenv("MERGE_TEST_TASKS", taskFile)
					write(config["hermes_bin"].(string), "#!/bin/sh\n[ \"$1\" = kanban ] && [ \"$2\" = list ] || exit 94\ncat \"$MERGE_TEST_TASKS\"\n", 0700)
				}
				raw, err = json.Marshal(config)
				if err != nil {
					t.Fatal(err)
				}
				write(configPath, string(raw), 0600)
				db, err := sql.Open("sqlite", config["ledger_path"].(string))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				row := `{"record_type":"run","state":"terminal","terminal_code":"success","run_id":"run-example","delivery_id":"delivery-example"}`
				if _, err := db.Exec("UPDATE ledger SET attrs = ? WHERE pk = ?", row, "run#example"); err != nil {
					t.Fatal(err)
				}
				poll := func(want string) {
					t.Helper()
					done, _ := startObservationFixture(t, configPath, "--once", "--observe-interval", "0")
					select {
					case <-done:
					case <-time.After(20 * time.Second):
						t.Fatal("resident did not finish its tick")
					}
					var snapshot attendant.BoardSnapshot
					raw, err := os.ReadFile(boardPath)
					if err != nil || json.Unmarshal(raw, &snapshot) != nil || len(snapshot.Runs) != 1 {
						t.Fatalf("no run snapshot: %s, %v", raw, err)
					}
					got := snapshot.Runs[0]
					if got.Step != want || (want == "done" && (got.StepTitle != "マージ済み" || !strings.Contains(got.Detail, "abcdef1"))) {
						t.Fatalf("step = %s / %s / %s, want %s", got.Step, got.StepTitle, got.Detail, want)
					}
				}
				// Failed reads, malformed responses and an unmerged close do
				// not claim success. The next tick must still be able to retry.
				poll("confirm")
				for _, value := range []string{`broken`, `{"merged":false,"state":"open"}`, `{"merged":false,"state":"closed"}`} {
					write(response, value, 0600)
					poll("confirm")
					if _, err := os.Stat(filepath.Join(dir, "feature-merged.json")); !os.IsNotExist(err) {
						t.Fatal("an unconfirmed merge was recorded")
					}
				}
				write(response, `{"merged":true,"state":"closed","merge_commit_sha":"abcdef1234567890"}`, 0600)
				poll("done")
				before, err := os.ReadFile(calls)
				if err != nil || strings.Count(string(before), "\n") != 5 {
					t.Fatalf("wrong number of controller reads: %q, %v", before, err)
				}
				// Restart with the network unavailable: sealed success stays
				// visible, and no further controller call is made.
				if err := os.Remove(response); err != nil {
					t.Fatal(err)
				}
				poll("done")
				after, _ := os.ReadFile(calls)
				if string(after) != string(before) {
					t.Fatal("known merge was fetched again")
				}
			})
		}
	}
}
