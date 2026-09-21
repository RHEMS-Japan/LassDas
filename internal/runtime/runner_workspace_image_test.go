package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/worker"
)

// Run the cross-compiled test binary in the runtime image as the engine
// user, without a network or credentials, with RUNNER_IMAGE_TEST=1. This
// exercises the installed launcher's actual capabilities and UID switch;
// the ordinary CLI stub alone cannot test access after that switch.
func TestRunnerWorkspaceLaunchInImage(t *testing.T) {
	if os.Getenv("RUNNER_IMAGE_TEST") != "1" {
		t.Skip("requires isolated runtime image with its installed launcher")
	}
	if runtime.GOOS != "linux" || os.Getuid() != 1000 {
		t.Fatal("run as the runtime image's engine user")
	}
	base, err := os.MkdirTemp("", "runner-launch-")
	if err != nil {
		t.Fatal(err)
	}
	// Do not use t.TempDir's enclosing 0700 directory as a workspace
	// ancestor: it would mask the production directory permissions.
	t.Cleanup(func() {
		if err := os.RemoveAll(base); err != nil {
			t.Error(err)
		}
	})
	if err := os.Chmod(base, 0o711); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "runs")
	delivery := filepath.Join(root, "delivery_test")
	repo := filepath.Join(delivery, "target-repo")
	home := filepath.Join(delivery, "manual-home")
	for _, dir := range []string{repo, home} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	reclaimHome := func() {
		t.Helper()
		if output, err := exec.Command("/usr/local/bin/agentexec", "--reclaim", home).CombinedOutput(); err != nil {
			t.Fatalf("return agent home: %s, %v", output, err)
		}
	}
	t.Cleanup(reclaimHome)
	secret := filepath.Join(delivery, "sealed.json")
	if err := os.WriteFile(secret, []byte("engine-only"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LASSDAS_AGENT_TREE_ROOT", root)
	launch := func() ([]byte, error) {
		return exec.Command("/usr/local/bin/agentexec", "--uid", "2001", "--workspace", repo, "--home", home,
			"--", "/bin/sh", "-ec", `
test "$(id -u)" = 2001
test -w .
test ! -r "$1"
test ! -w "$1"
test ! -r "$2"
test ! -w "$2"
test ! -r "$3"
test ! -w "$3"
printf 'own-workspace' > result.txt
printf 'agent-launch-ok\n'
`, "workspace-check", root, delivery, secret).CombinedOutput()
	}
	if output, err := launch(); err == nil || !strings.Contains(string(output), "permission denied") {
		t.Fatalf("closed ancestor did not reproduce launch failure: %s, %v", output, err)
	}
	reclaimHome()
	bin, _, _ := stubHermes(t)
	h := NewHermes(Config{HermesBin: bin, Chain: ChainConfig{RunsRoot: root}})
	if _, err := h.CreateCard(context.Background(), "delivery_test", "ticket", "body"); err != nil {
		t.Fatal(err)
	}
	if output, err := launch(); err != nil || strings.TrimSpace(string(output)) != "agent-launch-ok" {
		t.Fatalf("prepared workspace cannot launch safely: %s, %v", output, err)
	}
	if data, err := os.ReadFile(filepath.Join(repo, "result.txt")); err != nil || string(data) != "own-workspace" {
		t.Fatal("agent's output was not returned to the engine")
	}
	// Exercise the worker's complete lend/run/reclaim/cleanup path too:
	// immutable tool caches must not accumulate after successful launches.
	t.Setenv(worker.AgentLauncherEnv, "/usr/local/bin/agentexec")
	t.Setenv("LASSDAS_STATE_DIR", base)
	config := worker.AgentConfig{ID: "cache-fixture", Command: "sh", TimeoutSeconds: 60,
		Args: []string{"-ec", `test "$(id -u)" = 2001; mkdir -p "$HOME/cache/module"; printf cache > "$HOME/cache/module/source.go"; chmod 555 "$HOME/cache/module"; printf 'cache-launch-ok\n'`}}
	for i := 0; i < 2; i++ {
		outcome, err := worker.RunReviewingAgentWithHomeFiles(context.Background(), config, repo, "fixture", nil, "")
		if err != nil || outcome.ExitCode != 0 || strings.TrimSpace(outcome.Transcript) != "cache-launch-ok" {
			t.Fatalf("worker launch: %+v, %v", outcome, err)
		}
		entries, err := os.ReadDir(filepath.Join(delivery, "agent-home"))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.Name() != ".agent-lend.lock" {
				t.Fatalf("worker left launch home behind: %s", entry.Name())
			}
		}
	}
}
