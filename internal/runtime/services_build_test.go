package runtime

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildServicesOpensTheLedgerAndCloses(t *testing.T) {
	t.Setenv("BACKLOG_API_KEY", "test-key")
	raw := validRuntimeConfigMap()
	raw["ledger_path"] = filepath.Join(t.TempDir(), "ledger.db")
	config, err := Load(writeRuntimeConfig(t, raw))
	if err != nil {
		t.Fatal(err)
	}
	services, err := BuildServices(config, slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatalf("BuildServices() error = %v", err)
	}
	if services.Store == nil || services.Tick == nil {
		t.Fatalf("services incomplete: %+v", services)
	}
	if err := services.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestBuildServicesRequiresTheTrackerKey(t *testing.T) {
	t.Setenv("BACKLOG_API_KEY", "")
	raw := validRuntimeConfigMap()
	raw["ledger_path"] = filepath.Join(t.TempDir(), "ledger.db")
	config, err := Load(writeRuntimeConfig(t, raw))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BuildServices(config, nil); err == nil {
		t.Fatal("BuildServices() accepted a missing tracker key")
	}
}

func TestOwnerAndTargetDeriveFromTheFixedIdentity(t *testing.T) {
	raw := validRuntimeConfigMap()
	config, err := Load(writeRuntimeConfig(t, raw))
	if err != nil {
		t.Fatal(err)
	}
	owner := config.Owner(42)
	if owner.WorkflowRunID != 42 || owner.RepositoryID != config.Identity.RepositoryID ||
		owner.WorkflowSHA != config.Identity.EngineSHA || owner.RunAttempt != 1 {
		t.Fatalf("owner = %+v", owner)
	}
	if config.Target().RepositoryID != config.Identity.RepositoryID {
		t.Fatalf("target = %+v", config.Target())
	}
}

func TestArchiveGoesThroughTheCanonicalCLI(t *testing.T) {
	bin, callLog, _ := stubHermes(t)
	hermes := NewHermes(Config{HermesBin: bin, HermesBoard: "lassdas"})
	if err := hermes.Archive(context.Background(), "t1"); err != nil {
		t.Fatalf("Archive() error = %v", err)
	}
	records := calls(t, callLog)
	if len(records) != 1 || !strings.HasPrefix(records[0], "kanban|archive|t1") {
		t.Fatalf("canonical transitions = %v", records)
	}
}
