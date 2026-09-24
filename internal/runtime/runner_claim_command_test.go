package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/state"
)

// Exercise the shipped command wiring, not just the helper. A mutation that
// restores cmd/runner's unscoped Store.Pull must fail these assertions too.
func TestRunnerCommandRejectsUnboundDispatchBeforeTouchingHistory(t *testing.T) {
	binary := os.Getenv("RUNNER_BINDING_TEST_BIN")
	if binary == "" {
		binary = filepath.Join(t.TempDir(), "runner")
		build := exec.Command("go", "build", "-o", binary, "../../cmd/runner")
		if output, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build runner: %v\n%s", err, output)
		}
	}
	for _, failure := range []string{"foreign-envelope", "different-card", "different-workspace"} {
		t.Run(failure, func(t *testing.T) {
			workspace := t.TempDir()
			bin, _, tasksFile := stubHermes(t)
			raw := validRuntimeConfigMap()
			raw["ledger_path"] = filepath.Join(t.TempDir(), "ledger.db")
			raw["hermes_bin"] = bin
			// A failed mutation must not execute an actual stage binary.
			raw["worker_bin"] = filepath.Join(t.TempDir(), "missing-worker")
			raw["controller_bin"] = filepath.Join(t.TempDir(), "missing-controller")
			configPath := writeRuntimeConfig(t, raw)
			config, err := Load(configPath)
			if err != nil {
				t.Fatal(err)
			}
			store, err := state.NewLocalStore(config.LedgerPath)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			snapshot := syncEnvelope(t).Snapshot
			snapshot.ProjectID, snapshot.ProjectKey = config.Tracker.ProjectID, config.Tracker.ProjectKey
			snapshot.IssueKey = config.Tracker.ProjectKey + "-501"
			snapshot.CreatorID = config.Tracker.AllowedCreatorID
			snapshot.Target = config.Target()
			envelope, err := hook.SealSnapshot(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Enqueue(context.Background(), hook.QueueRequest{Envelope: envelope, QueuedAt: time.Now().Add(-time.Minute)}); err != nil {
				t.Fatal(err)
			}
			card := BoardTask{ID: "task", Status: "running", IdempotencyKey: envelope.DeliveryID, WorkspacePath: workspace}
			prior := envelope
			switch failure {
			case "foreign-envelope":
				snapshot.ActivityID++
				prior, err = hook.SealSnapshot(snapshot)
				if err != nil {
					t.Fatal(err)
				}
			case "different-card":
				card.ID = "other-task"
			case "different-workspace":
				card.WorkspacePath = t.TempDir()
			}
			setTasks(t, tasksFile, []BoardTask{card})
			writeRunnerEnvelope(t, workspace, prior)
			for _, name := range []string{"history.txt", "current-step.json", "spend.json"} {
				if err := os.WriteFile(filepath.Join(workspace, name), []byte("preserve evidence"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, binary, "--config", configPath)
			// No real credentials or network destinations inherited. The proxy
			// is deliberately closed so even a failing old binary cannot post.
			command.Env = []string{"PATH=" + os.Getenv("PATH"), "BACKLOG_API_KEY=fixture-only",
				"HERMES_KANBAN_TASK=task", "HERMES_KANBAN_WORKSPACE=" + workspace, "HERMES_KANBAN_RUN_ID=123",
				"HTTPS_PROXY=http://127.0.0.1:1", "HTTP_PROXY=http://127.0.0.1:1", "NO_PROXY="}
			output, err := command.CombinedOutput()
			if err == nil || !strings.Contains(string(output), "runner card binding:") {
				t.Errorf("command did not explain refusal: %v\n%s", err, output)
			}
			if got := runState(t, store); got != "queued" {
				t.Errorf("command claimed a ticket before binding dispatch: %s", got)
			}
			for _, name := range []string{"history.txt", "current-step.json", "spend.json"} {
				if data, err := os.ReadFile(filepath.Join(workspace, name)); err != nil || string(data) != "preserve evidence" {
					t.Errorf("command altered %s before binding dispatch", name)
				}
			}
			if err := checkRunnerWorkspaceEnvelope(workspace, prior.DeliveryID); err != nil {
				t.Errorf("command changed prior envelope: %v", err)
			}
		})
	}
}

// Run in the isolated runtime image with RUNNER_BINDING_HERMES_BIN set to
// its installed CLI. No gateway, models, tracker or dispatcher is started.
func TestPullTaskWithInstalledHermes(t *testing.T) {
	bin := os.Getenv("RUNNER_BINDING_HERMES_BIN")
	if bin == "" {
		t.Skip("requires the runtime image's installed Hermes CLI")
	}
	for _, persistent := range []bool{true, false} {
		name := "scratch"
		if persistent {
			name = "persistent"
		}
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			t.Setenv("HERMES_HOME", directory)
			t.Setenv("HERMES_KANBAN_DB", filepath.Join(directory, "kanban.db"))
			t.Setenv("HERMES_KANBAN_BOARD", "binding-test")
			config := Config{HermesBin: bin, HermesBoard: "binding-test", HermesProfile: "runner"}
			if persistent {
				config.Chain.RunsRoot = filepath.Join(directory, "runs")
			}
			h := NewHermes(config)
			store, old, newer := twoRunnerDeliveries(t)
			for i, envelope := range []hook.DispatchEnvelope{old, newer} {
				id, err := h.CreateCard(context.Background(), envelope.DeliveryID, envelope.Snapshot.IssueKey, "binding fixture")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := h.run(context.Background(), "claim", id); err != nil {
					t.Fatal(err)
				}
				tasks, err := h.ListTasks(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				workspace := ""
				for _, task := range tasks {
					if task.ID == id {
						workspace = task.WorkspacePath
					}
				}
				got, disposition, err := h.PullTask(context.Background(), store, id, workspace, runnerPullRequest(envelope, int64(50+i)))
				if err != nil || disposition != hook.PullAcquired || got.DeliveryID != envelope.DeliveryID {
					t.Fatalf("real CLI card %s claimed %s (%s, %v), want %s", id, got.DeliveryID, disposition, err, envelope.DeliveryID)
				}
				writeRunnerEnvelope(t, workspace, got)
				t.Logf("%s: real card %s retained its own delivery", name, id)
			}
			runSync(t, &Services{Store: store}, h)
		})
	}
}
