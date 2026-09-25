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

// Exercise the real reception tick, not just classification with a merge
// record pre-placed by the test. Only the external CLIs are fixtures.
// The configuration digest the delivery recorded while it was running. The
// reader is held to this one rather than to the destination's configuration
// as it stands now, which may well have changed since.
const observedRunDigest = "3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c"

func TestResidentObservesHumanMerge(t *testing.T) {
	configPath, boardPath, _ := observationFixture(t)
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
	ticket := filepath.Join(dir, "history", "stage-1", "ticket.json")
	if err := os.MkdirAll(filepath.Dir(ticket), 0700); err != nil {
		t.Fatal(err)
	}
	write(ticket, `{}`, 0600)
	// The published pull request artifact carries the digest the run
	// recorded; the reader is told it, so a run sealed before the
	// destination's configuration was last changed is still readable
	// (live 2026-09-24).
	write(filepath.Join(dir, "feature-pr.json"),
		`{"binding":{"config_sha256":"`+observedRunDigest+`"},"payload":{"pull_request":{"Number":29}}}`, 0600)
	// The destination credential comes from the sealed file; a token left
	// in the environment must never be the one that is used.
	chain := config["chain"].(map[string]any)
	t.Setenv("TARGET_GITHUB_TOKEN", "must-not-use-the-environment")
	write(chain["target_token_path"].(string), "test-token", 0600)
	controller := filepath.Join(root, "controller")
	config["controller_bin"] = controller
	response := filepath.Join(root, "merge-response")
	calls := filepath.Join(root, "merge-calls")
	t.Setenv("MERGE_TEST_RESPONSE", response)
	t.Setenv("MERGE_TEST_CALLS", calls)
	t.Setenv("MERGE_TEST_TICKET", ticket)
	t.Setenv("MERGE_TEST_CONFIG", config["consumer_config_path"].(string))
	t.Setenv("MERGE_TEST_DIGEST", observedRunDigest)
	write(controller, `#!/bin/sh
[ "$#" = 11 ] && [ "$1" = read-merged ] && [ "$2" = --config ] && [ "$3" = "$MERGE_TEST_CONFIG" ] && [ "$4" = --ticket ] && [ "$5" = "$MERGE_TEST_TICKET" ] && [ "$6" = --config-sha256 ] && [ "$7" = "$MERGE_TEST_DIGEST" ] && [ "$8" = --number ] && [ "$9" = 29 ] && [ "${10}" = --out ] && [ "$TARGET_GITHUB_TOKEN" = test-token ] || exit 94
printf 'read\n' >> "$MERGE_TEST_CALLS"
[ -f "$MERGE_TEST_RESPONSE" ] || exit 1
shift 10
cp "$MERGE_TEST_RESPONSE" "$1"
`, 0700)
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
	// Failed reads, malformed responses and an unmerged close do not claim
	// success. The next tick must still be able to retry.
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
	// Restart with the network unavailable: sealed success stays visible,
	// and no further controller call is made.
	if err := os.Remove(response); err != nil {
		t.Fatal(err)
	}
	poll("done")
	after, _ := os.ReadFile(calls)
	if string(after) != string(before) {
		t.Fatal("known merge was fetched again")
	}
}
