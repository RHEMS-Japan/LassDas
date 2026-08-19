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

// stubHermes writes a fake `hermes` CLI that records every argv line and
// answers from canned files. Driving the real command layer keeps the
// argument shapes honest — the block reason regression (a flag where the
// canonical CLI takes a positional) was exactly the kind of defect no
// mocked interface would have caught.
func stubHermes(t *testing.T) (bin string, callLog string, statusFile string) {
	t.Helper()
	directory := t.TempDir()
	callLog = filepath.Join(directory, "calls.log")
	statusFile = filepath.Join(directory, "status")
	bin = filepath.Join(directory, "hermes")
	script := `#!/bin/sh
{ printf '%s|' "$@"; echo; } >> "` + callLog + `"
case "$2" in
  create) printf '{"id":"t_stub01"}\n' ;;
  show)   printf '{"status":"%s"}\n' "$(cat "` + statusFile + `")" ;;
  *) : ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statusFile, []byte("todo"), 0o600); err != nil {
		t.Fatal(err)
	}
	return bin, callLog, statusFile
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

func calls(t *testing.T, callLog string) []string {
	t.Helper()
	raw, err := os.ReadFile(callLog)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	// One record per invocation: args joined by "|" with a trailing "|\n".
	// Argument values may embed newlines (the card body does), so records
	// are split on the terminator, not on newlines.
	records := strings.Split(strings.TrimSuffix(string(raw), "|\n"), "|\n")
	for index := range records {
		records[index] = strings.TrimSuffix(records[index], "|")
	}
	return records
}

func TestSyncCardsCreatesUnblocksAndCompletes(t *testing.T) {
	bin, callLog, statusFile := stubHermes(t)
	services, store, envelope := syncServices(t)
	config := Config{HermesBin: bin, HermesBoard: "lassdas", HermesProfile: "lassdas-runner"}
	hermes := NewHermes(config)
	cards, err := LoadCardLedger(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx := context.Background()

	// Pass 1: a queued run gets its card, created idempotently by delivery.
	if err := SyncCards(ctx, services, cards, hermes, logger); err != nil {
		t.Fatalf("SyncCards() error = %v", err)
	}
	got := calls(t, callLog)
	if len(got) != 1 || !strings.HasPrefix(got[0], "kanban|create|") ||
		!strings.Contains(got[0], "--idempotency-key|"+envelope.DeliveryID) ||
		!strings.Contains(got[0], "--assignee|lassdas-runner|") {
		t.Fatalf("create pass calls = %q", got)
	}
	if cards.Cards[envelope.DeliveryID] != "t_stub01" {
		t.Fatalf("card map = %v", cards.Cards)
	}

	// Pass 2: the run is still queued and the card is blocked (a resumed
	// run) — the attendant unblocks it, with the positional CLI shape.
	if err := os.WriteFile(statusFile, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SyncCards(ctx, services, cards, hermes, logger); err != nil {
		t.Fatalf("SyncCards() error = %v", err)
	}
	got = calls(t, callLog)
	last := got[len(got)-1]
	if last != "kanban|unblock|t_stub01" {
		t.Fatalf("unblock pass tail = %q", last)
	}

	// Pass 3: a stale claim with a non-running card is returned to the
	// queue by the recovery transition.
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
		IssuedAt:  time.Date(2026, 8, 2, 3, 4, 6, 0, time.UTC),
		ClaimedAt: time.Date(2026, 8, 2, 3, 4, 7, 0, time.UTC),
		ClockSkew: 2 * time.Minute,
	}
	if _, disposition, err := store.Pull(ctx, pull); err != nil || disposition != hook.PullAcquired {
		t.Fatalf("Pull() = %s, %v", disposition, err)
	}
	if err := os.WriteFile(statusFile, []byte("todo"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SyncCards(ctx, services, cards, hermes, logger); err != nil {
		t.Fatalf("SyncCards() error = %v", err)
	}
	runs, err := store.ScanRuns(ctx)
	if err != nil || len(runs) != 1 {
		t.Fatalf("ScanRuns() = %v, %v", runs, err)
	}
	if runs[0].State != "queued" {
		t.Fatalf("stale claim not recovered: state = %s", runs[0].State)
	}
}

func TestCardLedgerQuarantinesCorruptFileAndSavesAtomically(t *testing.T) {
	directory := t.TempDir()
	ledgerPath := filepath.Join(directory, "ledger.db")
	cardsPath := filepath.Join(directory, "cards.json")
	if err := os.WriteFile(cardsPath, []byte("{truncated"), 0o600); err != nil {
		t.Fatal(err)
	}
	cards, err := LoadCardLedger(ledgerPath)
	if err != nil {
		t.Fatalf("LoadCardLedger() error = %v", err)
	}
	if len(cards.Cards) != 0 {
		t.Fatalf("corrupt load produced %v", cards.Cards)
	}
	if _, err := os.Stat(cardsPath + ".corrupt"); err != nil {
		t.Fatalf("corrupt file was not quarantined: %v", err)
	}
	cards.Cards["delivery_x"] = "t_1"
	if err := cards.save(); err != nil {
		t.Fatalf("save() error = %v", err)
	}
	raw, err := os.ReadFile(cardsPath)
	if err != nil {
		t.Fatal(err)
	}
	reloaded := CardLedger{}
	if err := json.Unmarshal(raw, &reloaded); err != nil || reloaded.Cards["delivery_x"] != "t_1" {
		t.Fatalf("saved cards.json unreadable: %v %v", err, reloaded.Cards)
	}
}
