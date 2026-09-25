package attendant

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"

	_ "modernc.org/sqlite"
)

// The digest one finished run recorded, and a merge commit distinct from it
// so a value travelling the wrong way is visible in a failure message.
var (
	recordedRunDigest = strings.Repeat("ab", 32)
	observedMergeSHA  = strings.Repeat("7f", 20)
)

// A delivery whose pull request was merged after the destination's
// configuration had been changed used to sit at "waiting for merge" for as
// long as the run was kept: the reader was handed the live configuration,
// refused a ticket sealed under the previous one, and the refusal was
// swallowed as "not merged yet" (live 2026-09-24, more than ten hours; a run
// started after the change was recorded within seconds). The reader is now
// told which digest the run recorded, and the merge is written down.
func TestAFinishedRunsMergeIsRecordedAfterTheConfigurationChanged(t *testing.T) {
	for _, mode := range []string{"runner", "cards"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			config := mergeObservationConfig(t, root, mode)
			runDir := seedDeliveredRun(t, config, "delivery_"+strings.Repeat("c", 32), recordedRunDigest)
			writeFakeReader(t, config.ControllerBin, recordedRunDigest)
			logger := &recordingLogger{}

			recordFeatureMerge(context.Background(), config, state.RunOverview{RunID: "TICKET-77"}, runDir, logger)

			merge, known := readFeatureMerge(runDir)
			if !known || merge.MergeCommitSHA != observedMergeSHA {
				t.Fatalf("merge = %+v known = %v log = %v", merge, known, logger.lines)
			}
		})
	}
}

// The same, reached the way the runner-mode reception tick reaches it: a real
// ledger row, an empty board, and no run directory but the configured one.
func TestSyncRunnerMergesRecordsAMergeOfARunSealedUnderAnotherConfiguration(t *testing.T) {
	root := t.TempDir()
	config := mergeObservationConfig(t, root, "runner")
	store, deliveryID := seedTerminalSuccessRun(t, root)
	defer store.Close()
	runDir := seedDeliveredRun(t, config, deliveryID, recordedRunDigest)
	writeFakeReader(t, config.ControllerBin, recordedRunDigest)

	hermesBin := filepath.Join(root, "hermes")
	if err := os.WriteFile(hermesBin, []byte("#!/bin/sh\ncase \"$2\" in list) echo '[]' ;; *) : ;; esac\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	hermes := runtime.NewHermes(runtime.Config{HermesBin: hermesBin, HermesBoard: "board"})
	logger := &recordingLogger{}

	if err := SyncRunnerMerges(context.Background(), config, &runtime.Services{Store: store}, hermes, logger); err != nil {
		t.Fatal(err)
	}
	if merge, known := readFeatureMerge(runDir); !known || merge.MergeCommitSHA != observedMergeSHA {
		t.Fatalf("merge = %+v known = %v log = %v", merge, known, logger.lines)
	}
}

// A reader that refuses the run's own records will refuse them again every
// minute for as long as the run is kept. The operator is told once — and
// told at all, which is the half that was missing.
func TestAFinishedRunWhoseRecordsCannotBeReadIsSaidOnce(t *testing.T) {
	root := t.TempDir()
	config := mergeObservationConfig(t, root, "runner")
	runDir := seedDeliveredRun(t, config, "delivery_"+strings.Repeat("d", 32), recordedRunDigest)
	// A reader bound to a digest this run did not record: every wake-up ends
	// the same way.
	writeFakeReader(t, config.ControllerBin, strings.Repeat("ef", 32))
	logger := &recordingLogger{}

	for range 3 {
		recordFeatureMerge(context.Background(), config, state.RunOverview{RunID: "TICKET-78"}, runDir, logger)
	}

	if _, known := readFeatureMerge(runDir); known {
		t.Fatal("a refused reading was written down as a merge")
	}
	said := 0
	for _, line := range logger.lines {
		if strings.Contains(line, "could not be read") {
			said++
		}
	}
	if said != 1 {
		t.Fatalf("said %d times, want once: %v", said, logger.lines)
	}
	if !strings.Contains(strings.Join(logger.lines, "\n"), "ticket_artifact_invalid") {
		t.Fatalf("the reason was not said: %v", logger.lines)
	}
}

// A run that published a pull request whose record cannot be read never
// reaches the reader at all, so it carries no refusal of its own. Passing
// over it silently leaves the delivery at waiting-for-merge for ever with
// nothing said — the same shape this reader exists to close — so it is said
// once, and the tick goes on.
func TestAPublishedDeliveryWithAnUnreadableRecordIsSaidOnce(t *testing.T) {
	tests := map[string]func(*testing.T, string){
		"no binding at all": func(t *testing.T, runDir string) {
			writeRunFile(t, runDir, "feature-pr.json", `{"payload":{"pull_request":{"Number":76}}}`)
		},
		"malformed digest": func(t *testing.T, runDir string) {
			writeRunFile(t, runDir, "feature-pr.json",
				`{"binding":{"config_sha256":"NOT-A-DIGEST"},"payload":{"pull_request":{"Number":76}}}`)
		},
		"no round ticket": func(t *testing.T, runDir string) {
			if err := os.RemoveAll(filepath.Join(runDir, "history")); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, breakIt := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			config := mergeObservationConfig(t, root, "runner")
			runDir := seedDeliveredRun(t, config, "delivery_"+strings.Repeat("a", 32), recordedRunDigest)
			writeFakeReader(t, config.ControllerBin, recordedRunDigest)
			breakIt(t, runDir)
			logger := &recordingLogger{}

			for range 3 {
				recordFeatureMerge(context.Background(), config, state.RunOverview{RunID: "TICKET-80"}, runDir, logger)
			}

			said := 0
			for _, line := range logger.lines {
				if strings.Contains(line, "could not be read") {
					said++
				}
			}
			if said != 1 {
				t.Fatalf("said %d times, want once: %v", said, logger.lines)
			}
			if !strings.Contains(strings.Join(logger.lines, "\n"), mergeDeliveryRecordCode) {
				t.Fatalf("the reason was not said: %v", logger.lines)
			}
			if _, known := readFeatureMerge(runDir); known {
				t.Fatal("an unreadable delivery was written down as a merge")
			}
		})
	}
}

// A finished run that published no pull request at all is the ordinary case,
// and most finished runs are it. Saying something about each of them every
// minute would bury the runs that do need looking at.
func TestAFinishedRunThatPublishedNothingIsNotSaid(t *testing.T) {
	root := t.TempDir()
	config := mergeObservationConfig(t, root, "runner")
	runDir := seedDeliveredRun(t, config, "delivery_"+strings.Repeat("b", 32), recordedRunDigest)
	if err := os.Remove(filepath.Join(runDir, "feature-pr.json")); err != nil {
		t.Fatal(err)
	}
	logger := &recordingLogger{}

	recordFeatureMerge(context.Background(), config, state.RunOverview{RunID: "TICKET-81"}, runDir, logger)

	if len(logger.lines) != 0 {
		t.Fatalf("a run that delivered nothing was said: %v", logger.lines)
	}
	if _, err := os.Stat(filepath.Join(runDir, mergeUnreadableFile)); !os.IsNotExist(err) {
		t.Fatalf("a run that delivered nothing was recorded: %v", err)
	}
}

// A refusal that says nothing about the run's own records — the destination
// unreachable, say — stays quiet: it is indistinguishable from a pull request
// that is simply not merged yet, and the next wake-up may well succeed.
func TestATransientRefusalIsNotSaid(t *testing.T) {
	root := t.TempDir()
	config := mergeObservationConfig(t, root, "runner")
	runDir := seedDeliveredRun(t, config, "delivery_"+strings.Repeat("e", 32), recordedRunDigest)
	script := "#!/bin/sh\necho 'controller: read_merged: 503 from api' >&2\necho 'controller: read_merged_failed' >&2\nexit 1\n"
	if err := os.WriteFile(config.ControllerBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	logger := &recordingLogger{}

	recordFeatureMerge(context.Background(), config, state.RunOverview{RunID: "TICKET-79"}, runDir, logger)

	if len(logger.lines) != 0 {
		t.Fatalf("a transient refusal was said: %v", logger.lines)
	}
	if _, err := os.Stat(filepath.Join(runDir, mergeUnreadableFile)); !os.IsNotExist(err) {
		t.Fatalf("a transient refusal was recorded: %v", err)
	}
}

// Two things have to be right for the reason to be the run's actual ending.
// A code named inside a detail line is a detail, and where two codes are
// printed bare the LAST one is the ending — reading the first would let an
// earlier code answer for the failure below it.
func TestEndingCodeIsTheLastBareCode(t *testing.T) {
	detailed := "controller: read_merged: ticket_artifact_invalid named in a detail\n" +
		"controller: read_merged_failed\n"
	if code := endingCode(detailed); code != "read_merged_failed" {
		t.Fatalf("ending = %q, want the bare code", code)
	}
	twice := "controller: ticket_artifact_invalid\ncontroller: read_merged_failed\n"
	if code := endingCode(twice); code != "read_merged_failed" {
		t.Fatalf("ending = %q, want the last of two bare codes", code)
	}
	if code := endingCode(""); code != "" {
		t.Fatalf("empty stderr ending = %q", code)
	}
}

// mergeObservationConfig is one reception's configuration in the orchestration
// under test. The two modes reach the same reader by different routes: cards
// seals the destination token into a file, runner keeps it in the environment.
func mergeObservationConfig(t *testing.T, root, mode string) runtime.Config {
	t.Helper()
	config := runtime.Config{
		ControllerBin:      filepath.Join(root, "controller"),
		ConsumerConfigPath: filepath.Join(root, "m1-consumer.json"),
		Chain:              runtime.ChainConfig{RunsRoot: filepath.Join(root, "runs")},
	}
	if mode == "cards" {
		config.Orchestration = "cards"
		config.Chain.TargetTokenPath = filepath.Join(root, "target-token")
		if err := os.WriteFile(config.Chain.TargetTokenPath, []byte("token-from-file\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	} else {
		t.Setenv("TARGET_GITHUB_TOKEN", "token-from-environment")
	}
	return config
}

// seedDeliveredRun writes the run directory of a delivery that published a
// pull request and then ended: the sealed pull request artifact, and the
// implementation round's ticket the reader is handed.
func seedDeliveredRun(t *testing.T, config runtime.Config, deliveryID, recorded string) string {
	t.Helper()
	runDir := runDirectory(config, deliveryID)
	stage := filepath.Join(runDir, "history", "stage-1")
	if err := os.MkdirAll(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	pull := map[string]any{
		"schema_version": 1,
		"kind":           "m1-feature-pull-request",
		"binding":        map[string]any{"config_sha256": recorded},
		"payload":        map[string]any{"pull_request": map[string]any{"Number": 76}},
	}
	encoded, err := json.Marshal(pull)
	if err != nil {
		t.Fatal(err)
	}
	writeRunFile(t, runDir, "feature-pr.json", string(encoded))
	// The reader validates the ticket itself; here it only has to exist, so
	// that the one path under test is which digest the reader is told to
	// hold it to.
	if err := os.WriteFile(filepath.Join(stage, "ticket.json"), []byte(`{"schema_version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return runDir
}

// writeRunFile writes one record into a run directory.
func writeRunFile(t *testing.T, runDir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(runDir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeFakeReader stands in for the delivery binary's read-merged verb: it
// answers "merged" only when told the digest the run recorded, and refuses
// the ticket otherwise — which is what the real reader does when it is held
// to a configuration the run was not sealed under.
func writeFakeReader(t *testing.T, path, accepts string) {
	t.Helper()
	script := "#!/bin/sh\n" +
		"recorded=''\n" +
		"out=''\n" +
		"while [ $# -gt 0 ]; do\n" +
		"  case \"$1\" in\n" +
		"    --config-sha256) recorded=\"$2\"; shift 2 ;;\n" +
		"    --out) out=\"$2\"; shift 2 ;;\n" +
		"    *) shift ;;\n" +
		"  esac\n" +
		"done\n" +
		"if [ \"$recorded\" != '" + accepts + "' ]; then\n" +
		"  echo 'controller: ticket_artifact_invalid' >&2\n" +
		"  exit 1\n" +
		"fi\n" +
		"printf '%s' '{\"state\":\"closed\",\"merged\":true,\"merge_commit_sha\":\"" + observedMergeSHA + "\"}' > \"$out\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// seedTerminalSuccessRun puts one finished, successful delivery in a real
// ledger and answers with its delivery id.
func seedTerminalSuccessRun(t *testing.T, root string) (*state.LocalStore, string) {
	t.Helper()
	ledger := filepath.Join(root, "ledger.db")
	store, err := state.NewLocalStore(ledger)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := hook.SealSnapshot(hook.TicketSnapshot{
		SchemaVersion: hook.SnapshotSchemaVersion, SpaceKey: "example", ActivityID: 9001, ActivityType: 1,
		ProjectID: 42, ProjectKey: "TICKET", IssueID: 30, IssueKey: "TICKET-77", IssueKeyID: 77, CreatorID: 7,
		RunID: "TICKET-77", CreatedAt: time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC),
		Target: runtime.Config{
			Identity: runtime.IdentityConfig{RepositoryID: 1, Repository: "o/r", WorkflowRef: "o/r/wf@main", EngineSHA: strings.Repeat("a", 40)},
		}.Target(),
		Untrusted: hook.UntrustedTicketData{Summary: "s", Description: "d"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Enqueue(context.Background(), hook.QueueRequest{Envelope: envelope, QueuedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", ledger+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE ledger SET attrs = json_set(attrs, '$.state', 'terminal', '$.terminal_code', ?, '$.terminal_completed_at', ?) WHERE pk LIKE 'run#%'`,
		string(hook.TerminalSuccess), time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	runs, err := store.ScanRuns(context.Background())
	if err != nil || len(runs) != 1 || runs[0].State != "terminal" {
		t.Fatalf("seeded run = %+v err = %v", runs, err)
	}
	return store, envelope.DeliveryID
}
