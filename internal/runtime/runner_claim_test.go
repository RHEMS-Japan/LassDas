package runtime

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
	"automation.internal/ticket-ingress/internal/state"
)

func runnerPullRequest(envelope hook.DispatchEnvelope, owner int64) hook.PullClaimRequest {
	s := envelope.Snapshot
	return hook.PullClaimRequest{
		SpaceKey: s.SpaceKey, ProjectID: s.ProjectID, ProjectKey: s.ProjectKey,
		AllowedCreatorID: s.CreatorID, AllowedActivityType: s.ActivityType, Target: s.Target,
		Owner: hook.PullOwner{RepositoryID: s.Target.RepositoryID,
			RepositorySHA256:  hook.HashIdentity("example/automation-receiver"),
			WorkflowRefSHA256: s.Target.WorkflowRefSHA256, WorkflowSHA: strings.Repeat("d", 40),
			WorkflowRunID: owner, RunAttempt: 1},
		IssuedAt: time.Now().UTC(), ClaimedAt: time.Now().UTC(), ClockSkew: 2 * time.Minute,
	}
}

func writeRunnerEnvelope(t *testing.T, workspace string, envelope hook.DispatchEnvelope) {
	t.Helper()
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "ticket-envelope.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Reproduce a stale queued row whose project pending slot has advanced to a
// newer ticket. Only this disposable test ledger is altered to seed that state.
func twoRunnerDeliveries(t *testing.T) (*state.LocalStore, hook.DispatchEnvelope, hook.DispatchEnvelope) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.db")
	store, err := state.NewLocalStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	old := syncEnvelope(t)
	ctx := context.Background()
	queue := func(envelope hook.DispatchEnvelope) {
		t.Helper()
		disposition, err := store.Enqueue(ctx, hook.QueueRequest{Envelope: envelope, QueuedAt: time.Now().Add(-time.Hour)})
		if err != nil || disposition != hook.QueueCreated {
			t.Fatalf("enqueue = %s, %v", disposition, err)
		}
	}
	queue(old)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("DELETE FROM ledger WHERE pk LIKE 'pending#%'"); err != nil {
		t.Fatal(err)
	}
	snapshot := old.Snapshot
	snapshot.ActivityID++
	snapshot.IssueID++
	snapshot.IssueKeyID++
	snapshot.IssueKey = "TICKET-502"
	snapshot.RunID = "run_20260802_beta"
	newer, err := hook.SealSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	queue(newer)
	return store, old, newer
}

func TestPullTaskDoesNotConsumeAnotherCardsPendingTicket(t *testing.T) {
	ctx := context.Background()
	store, old, newer := twoRunnerDeliveries(t)
	bin, _, tasksFile := stubHermes(t)
	oldDir, newDir := t.TempDir(), t.TempDir()
	setTasks(t, tasksFile, []BoardTask{
		{ID: "old", Status: "running", IdempotencyKey: old.DeliveryID, WorkspacePath: oldDir},
		{ID: "new", Status: "running", IdempotencyKey: newer.DeliveryID, WorkspacePath: newDir},
	})
	h := NewHermes(Config{HermesBin: bin, HermesProfile: "runner"})
	for _, tc := range []struct {
		task, workspace string
		envelope        hook.DispatchEnvelope
		owner           int64
	}{{"old", oldDir, old, 10}, {"new", newDir, newer, 11}} {
		request := runnerPullRequest(tc.envelope, tc.owner)
		request.IssuedAt = time.Now().Add(-20 * time.Minute).UTC()
		request.ClaimedAt = request.IssuedAt
		got, disposition, err := h.PullTask(ctx, store, tc.task, tc.workspace, request)
		if err != nil || disposition != hook.PullAcquired || got.DeliveryID != tc.envelope.DeliveryID {
			t.Fatalf("%s claimed %s (%s, %v), want %s", tc.task, got.DeliveryID, disposition, err, tc.envelope.DeliveryID)
		}
		writeRunnerEnvelope(t, tc.workspace, got)
	}
	// Older than the recovery grace, but both correctly bound cards are alive.
	// An attendant pass must not requeue either run or dispatch it twice.
	runSync(t, &Services{Store: store}, h)
	runs, err := store.ScanRuns(ctx)
	if err != nil || len(runs) != 2 {
		t.Fatalf("runs = %+v, %v", runs, err)
	}
	for _, run := range runs {
		if run.State != "claimed" {
			t.Fatalf("live delivery was requeued: %+v", run)
		}
	}
	_, disposition, err := h.PullTask(ctx, store, "new", newDir, runnerPullRequest(newer, 12))
	if err != nil || disposition != hook.PullClaimed {
		t.Fatalf("duplicate worker = %s, %v", disposition, err)
	}
}

func TestPullTaskCannotUseSameRunIDFromAnotherProject(t *testing.T) {
	_, store, envelope := syncServices(t)
	snapshot := envelope.Snapshot
	snapshot.ProjectID++
	other, err := hook.SealSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Enqueue(context.Background(), hook.QueueRequest{Envelope: other, QueuedAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	bin, _, tasksFile := stubHermes(t)
	workspace := t.TempDir()
	setTasks(t, tasksFile, []BoardTask{{ID: "task", Status: "running", IdempotencyKey: other.DeliveryID, WorkspacePath: workspace}})
	h := NewHermes(Config{HermesBin: bin})
	if _, _, err := h.PullTask(context.Background(), store, "task", workspace, runnerPullRequest(envelope, 40)); err == nil {
		t.Fatal("cross-project card claimed this project's same-named run")
	}
	runs, err := store.ScanRuns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		if run.State != "queued" {
			t.Fatalf("cross-project binding changed a claim: %+v", run)
		}
	}
}

func TestPullTaskRefusesBadBindingsWithoutClaimingOrErasing(t *testing.T) {
	for _, failure := range []string{"missing-task", "missing-key", "missing-run", "archived", "blocked", "ambiguous-task", "wrong-workspace", "no-workspace", "wrong-run", "foreign-envelope", "invalid-envelope", "linked-envelope", "oversize-envelope", "list-failed"} {
		t.Run(failure, func(t *testing.T) {
			_, store, envelope := syncServices(t)
			bin, _, tasksFile := stubHermes(t)
			workspace := t.TempDir()
			card := BoardTask{ID: "task", Status: "running", IdempotencyKey: envelope.DeliveryID, WorkspacePath: workspace}
			h := NewHermes(Config{HermesBin: bin, HermesProfile: "runner"})
			request := runnerPullRequest(envelope, 20)
			sentinel := filepath.Join(workspace, "history.txt")
			if err := os.WriteFile(sentinel, []byte("preserve evidence"), 0o600); err != nil {
				t.Fatal(err)
			}
			writeRunnerEnvelope(t, workspace, envelope)
			envelopePath := filepath.Join(workspace, "ticket-envelope.json")
			switch failure {
			case "missing-task":
				card.ID = "other"
			case "missing-key":
				card.IdempotencyKey = ""
			case "missing-run":
				card.IdempotencyKey = "delivery_" + strings.Repeat("a", 32)
			case "archived", "blocked":
				card.Status = failure
			case "wrong-workspace":
				card.WorkspacePath = t.TempDir()
			case "no-workspace":
				card.WorkspacePath = ""
			case "wrong-run":
				request.RunID = "other-run"
			case "foreign-envelope":
				snapshot := envelope.Snapshot
				snapshot.ActivityID++
				other, err := hook.SealSnapshot(snapshot)
				if err != nil {
					t.Fatal(err)
				}
				writeRunnerEnvelope(t, workspace, other)
			case "invalid-envelope":
				if err := os.WriteFile(envelopePath, []byte("{broken"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "linked-envelope":
				if err := os.Remove(envelopePath); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(sentinel, envelopePath); err != nil {
					t.Fatal(err)
				}
			case "oversize-envelope":
				if err := os.Truncate(envelopePath, 4*1024*1024+1); err != nil {
					t.Fatal(err)
				}
			case "list-failed":
				h.bin = filepath.Join(t.TempDir(), "missing-hermes")
			}
			tasks := []BoardTask{card}
			if failure == "ambiguous-task" {
				tasks = append(tasks, card)
			}
			setTasks(t, tasksFile, tasks)
			before, err := os.ReadFile(envelopePath)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := h.PullTask(context.Background(), store, "task", workspace, request); err == nil || !strings.Contains(err.Error(), "runner card binding:") {
				t.Fatalf("bad binding not explained: %v", err)
			}
			if got := runState(t, store); got != "queued" {
				t.Fatalf("refused binding claimed a run: %s", got)
			}
			after, err := os.ReadFile(envelopePath)
			if err != nil || string(after) != string(before) {
				t.Fatal("refused binding changed envelope")
			}
			if data, err := os.ReadFile(sentinel); err != nil || string(data) != "preserve evidence" {
				t.Fatal("refused binding changed history")
			}
		})
	}
}

func TestPullTaskAllowsSameDeliveryRetryAndCanonicalWorkspaceAlias(t *testing.T) {
	_, store, envelope := syncServices(t)
	bin, _, tasksFile := stubHermes(t)
	root := t.TempDir()
	workspace := filepath.Join(root, envelope.DeliveryID)
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "workspace")
	if err := os.Symlink(workspace, alias); err != nil {
		t.Fatal(err)
	}
	writeRunnerEnvelope(t, workspace, envelope)
	setTasks(t, tasksFile, []BoardTask{{ID: "task", Status: "running", IdempotencyKey: envelope.DeliveryID, WorkspacePath: workspace}})
	h := NewHermes(Config{HermesBin: bin, Chain: ChainConfig{RunsRoot: root}})
	// The board remains authoritative for existing cards after a runs-root
	// configuration change; do not guess their workspace from today's config.
	h.runsRoot = t.TempDir()
	request := runnerPullRequest(envelope, 30)
	for i := 0; i < 2; i++ {
		got, disposition, err := h.PullTask(context.Background(), store, "task", alias, request)
		if err != nil || disposition != hook.PullAcquired || got.DeliveryID != envelope.DeliveryID {
			t.Fatalf("same-owner retry = %s, %v", disposition, err)
		}
	}
	// A genuinely dead claim can still be recovered, then claimed by a new
	// dispatch of this same card. No new binding file or configuration needed.
	runs, err := store.ScanRuns(context.Background())
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %+v, %v", runs, err)
	}
	if err := store.RecoverLostClaim(context.Background(), runs[0].Key, runs[0].ClaimedAt, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, disposition, err := h.PullTask(context.Background(), store, "task", alias, runnerPullRequest(envelope, 31))
	if err != nil || disposition != hook.PullAcquired || got.DeliveryID != envelope.DeliveryID {
		t.Fatalf("recovered retry = %s, %v", disposition, err)
	}
}
