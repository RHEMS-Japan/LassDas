package runtime

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/state"
)

// stubHermes writes a fake `hermes` CLI that records every argv record and
// answers `list` from a canned file. Driving the real command layer keeps
// the argument shapes honest — the block-reason regression (a flag where
// the canonical CLI takes a positional) is exactly the kind of defect no
// mocked interface would have caught.
func stubHermes(t *testing.T) (bin string, callLog string, tasksFile string) {
	t.Helper()
	directory := t.TempDir()
	callLog = filepath.Join(directory, "calls.log")
	tasksFile = filepath.Join(directory, "tasks.json")
	bin = filepath.Join(directory, "hermes")
	script := `#!/bin/sh
{ printf '%s|' "$@"; echo; } >> "` + callLog + `"
case "$2" in
  list)   cat "` + tasksFile + `" ;;
  create) printf '{"id":"t_new"}\n' ;;
  *) : ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	setTasks(t, tasksFile, nil)
	return bin, callLog, tasksFile
}

func setTasks(t *testing.T, tasksFile string, tasks []BoardTask) {
	t.Helper()
	if tasks == nil {
		tasks = []BoardTask{}
	}
	encoded, err := json.Marshal(tasks)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tasksFile, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func calls(t *testing.T, callLog string) []string {
	t.Helper()
	raw, err := os.ReadFile(callLog)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	records := strings.Split(strings.TrimSuffix(string(raw), "|\n"), "|\n")
	for index := range records {
		records[index] = strings.TrimSuffix(records[index], "|")
	}
	return records
}

func lastNonList(t *testing.T, callLog string) string {
	t.Helper()
	records := calls(t, callLog)
	for index := len(records) - 1; index >= 0; index-- {
		if !strings.HasPrefix(records[index], "kanban|list|") {
			return records[index]
		}
	}
	return ""
}

func syncEnvelope(t *testing.T) hook.DispatchEnvelope {
	t.Helper()
	envelope, err := hook.SealSnapshot(hook.TicketSnapshot{
		SchemaVersion: hook.SnapshotSchemaVersion,
		SpaceKey:      "example", ActivityID: 9001, ActivityType: 1,
		ProjectID: 909057, ProjectKey: "TICKET",
		IssueID: 8001, IssueKey: "TICKET-501", IssueKeyID: 501,
		CreatorID: 9903853, RunID: "run_20260802_alpha",
		CreatedAt: time.Date(2026, 8, 2, 1, 2, 3, 0, time.UTC),
		Target:    hook.DeliveryTarget{RepositoryID: 4242, WorkflowRefSHA256: hook.HashIdentity("example/automation-receiver/.github/workflows/x.yml@refs/heads/main")},
		Untrusted: hook.UntrustedTicketData{Summary: "stub summary", Description: "body"},
	})
	if err != nil {
		t.Fatalf("SealSnapshot() error = %v", err)
	}
	return envelope
}

func syncServices(t *testing.T) (*Services, *state.LocalStore, hook.DispatchEnvelope) {
	t.Helper()
	store, err := state.NewLocalStore(t.TempDir() + "/ledger.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	envelope := syncEnvelope(t)
	queuedAt := time.Date(2026, 8, 2, 3, 4, 5, 0, time.UTC)
	if _, err := store.Enqueue(context.Background(), hook.QueueRequest{Envelope: envelope, QueuedAt: queuedAt}); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	return &Services{Store: store}, store, envelope
}

func claimStale(t *testing.T, store *state.LocalStore, envelope hook.DispatchEnvelope) {
	t.Helper()
	pull := hook.PullClaimRequest{
		SpaceKey: "example", ProjectID: 909057, ProjectKey: "TICKET",
		AllowedCreatorID: 9903853, AllowedActivityType: 1,
		RunID:  envelope.Snapshot.RunID,
		Target: envelope.Snapshot.Target,
		Owner: hook.PullOwner{
			RepositoryID: 4242, RepositorySHA256: hook.HashIdentity("example/automation-receiver"),
			WorkflowRefSHA256: envelope.Snapshot.Target.WorkflowRefSHA256,
			WorkflowSHA:       strings.Repeat("d", 40), WorkflowRunID: 7, RunAttempt: 1,
		},
		// 2026-08-02 is far past the recovery grace relative to wall clock.
		IssuedAt:  time.Date(2026, 8, 2, 3, 4, 6, 0, time.UTC),
		ClaimedAt: time.Date(2026, 8, 2, 3, 4, 7, 0, time.UTC),
		ClockSkew: 2 * time.Minute,
	}
	if _, disposition, err := store.Pull(context.Background(), pull); err != nil || disposition != hook.PullAcquired {
		t.Fatalf("Pull() = %s, %v", disposition, err)
	}
}

func runSync(t *testing.T, services *Services, hermes *Hermes) {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := SyncCards(context.Background(), services, hermes, logger); err != nil {
		t.Fatalf("SyncCards() error = %v", err)
	}
}

func runState(t *testing.T, store *state.LocalStore) string {
	t.Helper()
	runs, err := store.ScanRuns(context.Background())
	if err != nil || len(runs) != 1 {
		t.Fatalf("ScanRuns() = %v, %v", runs, err)
	}
	return runs[0].State
}

func TestSyncCardsQueuedRunLifecycle(t *testing.T) {
	bin, callLog, tasksFile := stubHermes(t)
	services, _, envelope := syncServices(t)
	hermes := NewHermes(Config{HermesBin: bin, HermesBoard: "lassdas", HermesProfile: "lassdas-runner"})

	// No card on the board -> create, idempotent by delivery id.
	runSync(t, services, hermes)
	last := lastNonList(t, callLog)
	if !strings.HasPrefix(last, "kanban|create|") ||
		!strings.Contains(last, "--idempotency-key|"+envelope.DeliveryID) ||
		!strings.Contains(last, "--assignee|lassdas-runner") {
		t.Fatalf("create call = %q", last)
	}

	// Blocked card under a queued run (a resumed run) -> resolve-unblock.
	setTasks(t, tasksFile, []BoardTask{{ID: "t_1", Status: "blocked", IdempotencyKey: envelope.DeliveryID}})
	runSync(t, services, hermes)
	if got := lastNonList(t, callLog); got != "kanban|unblock|--resolve|t_1" {
		t.Fatalf("unblock call = %q", got)
	}

	// Done card still holds the idempotency key in the kanban -> archive it
	// (recreating without archiving would loop on the dedup forever).
	setTasks(t, tasksFile, []BoardTask{{ID: "t_1", Status: "done", IdempotencyKey: envelope.DeliveryID}})
	runSync(t, services, hermes)
	if got := lastNonList(t, callLog); got != "kanban|archive|t_1" {
		t.Fatalf("archive call = %q", got)
	}

	// Scheduled card = operator parking; the attendant must not touch it.
	setTasks(t, tasksFile, []BoardTask{{ID: "t_1", Status: "scheduled", IdempotencyKey: envelope.DeliveryID}})
	before := len(calls(t, callLog))
	runSync(t, services, hermes)
	after := calls(t, callLog)
	if len(after) != before+1 || !strings.HasPrefix(after[len(after)-1], "kanban|list|") {
		t.Fatalf("scheduled card was touched: %q", after[before:])
	}
}

func TestSyncCardsClaimRecoveryRespectsLiveness(t *testing.T) {
	bin, _, tasksFile := stubHermes(t)
	services, store, envelope := syncServices(t)
	hermes := NewHermes(Config{HermesBin: bin, HermesProfile: "lassdas-runner"})
	claimStale(t, store, envelope)

	// A running card is a living worker: never recovered, however old.
	setTasks(t, tasksFile, []BoardTask{{ID: "t_1", Status: "running", IdempotencyKey: envelope.DeliveryID}})
	runSync(t, services, hermes)
	if got := runState(t, store); got != "claimed" {
		t.Fatalf("live claim was recovered: state = %s", got)
	}

	// The supervisor translated a crash into blocked -> recover to queued.
	setTasks(t, tasksFile, []BoardTask{{ID: "t_1", Status: "blocked", IdempotencyKey: envelope.DeliveryID}})
	runSync(t, services, hermes)
	if got := runState(t, store); got != "queued" {
		t.Fatalf("dead claim not recovered: state = %s", got)
	}
}

func TestSyncCardsRecoversClaimWithNoCardAtAll(t *testing.T) {
	bin, _, tasksFile := stubHermes(t)
	services, store, envelope := syncServices(t)
	hermes := NewHermes(Config{HermesBin: bin, HermesProfile: "lassdas-runner"})
	claimStale(t, store, envelope)

	// The listing is the authority: nothing holds this delivery, so the
	// card provably does not exist (not merely "not in a cache").
	setTasks(t, tasksFile, nil)
	runSync(t, services, hermes)
	if got := runState(t, store); got != "queued" {
		t.Fatalf("cardless dead claim not recovered: state = %s", got)
	}
}
