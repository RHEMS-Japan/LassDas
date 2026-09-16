package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/localrun"
)

func gitRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	command := exec.Command("git", "init", "-q", root)
	if err := command.Run(); err != nil {
		t.Skip("git is not available")
	}
	return root
}

// `setup check` says what the answers file lacks and runs nothing; with a
// complete file it says whose turn is next.
func TestSetupCheckNamesTheGapsWithoutRunningAnything(t *testing.T) {
	root := gitRepo(t)
	var out bytes.Buffer
	err := runSetup(context.Background(), "setup check", "", root, t.TempDir(), localrun.Manager{}, &out)
	if err == nil || !strings.Contains(out.String(), ".lassdas/setup.json がありません") {
		t.Fatalf("no file: err=%v out=%q", err, out.String())
	}
	if err := os.MkdirAll(filepath.Join(root, ".lassdas"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".lassdas", "setup.json"), []byte(`{"answers":{"repository":"example/app"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	err = runSetup(context.Background(), "setup check", "", root, t.TempDir(), localrun.Manager{}, &out)
	if err == nil || !strings.Contains(out.String(), "回答がありません: branch") || !strings.Contains(out.String(), "agreement.md がありません") {
		t.Fatalf("gaps: err=%v out=%q", err, out.String())
	}
	if strings.Contains(out.String(), "TOKEN") || strings.Contains(out.String(), "鍵を入力") {
		t.Fatalf("check must not ask for a key: %q", out.String())
	}
}

// `setup apply` refuses to start with an incomplete file, and `setup
// secrets` / `apply` / `smoke` need a project name.
func TestSetupApplyRefusesAnIncompleteFile(t *testing.T) {
	root := gitRepo(t)
	if err := os.MkdirAll(filepath.Join(root, ".lassdas"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".lassdas", "setup.json"), []byte(`{"answers":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := runSetup(context.Background(), "setup apply", "sample", root, t.TempDir(), localrun.Manager{}, &out)
	if err == nil || !strings.Contains(err.Error(), "回答が足りません") {
		t.Fatalf("apply with gaps: %v", err)
	}
	for _, command := range []string{"setup apply", "setup smoke", "setup secrets"} {
		if err := runSetup(context.Background(), command, "", root, t.TempDir(), localrun.Manager{}, &out); err == nil || !strings.Contains(err.Error(), "--project") {
			t.Fatalf("%s without a project: %v", command, err)
		}
	}
	if err := runSetup(context.Background(), "setup check", "", t.TempDir(), t.TempDir(), localrun.Manager{}, &out); err == nil || !strings.Contains(err.Error(), "git repo") {
		t.Fatalf("outside a repo: %v", err)
	}
}

func TestHelpMentionsSetup(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"--help"}, &out); err != nil || !strings.Contains(out.String(), "lassdas setup check") {
		t.Fatalf("help: %v %q", err, out.String())
	}
	if err := run(context.Background(), []string{"setup"}, &out); err == nil || !strings.Contains(err.Error(), "setup の操作名") {
		t.Fatalf("setup without an operation: %v", err)
	}
	if err := run(context.Background(), []string{"setup", "unknown"}, &out); err == nil {
		t.Fatal("an unknown setup operation must be refused")
	}
}
