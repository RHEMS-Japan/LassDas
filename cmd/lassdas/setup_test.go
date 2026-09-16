package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/initsmoke"
	"automation.internal/ticket-ingress/internal/initwizard"
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
	err := runSetup(context.Background(), "setup check", "", root, t.TempDir(), "", localrun.Manager{}, &out)
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
	err = runSetup(context.Background(), "setup check", "", root, t.TempDir(), "", localrun.Manager{}, &out)
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
	err := runSetup(context.Background(), "setup apply", "sample", root, t.TempDir(), "", localrun.Manager{}, &out)
	if err == nil || !strings.Contains(err.Error(), "回答が足りません") {
		t.Fatalf("apply with gaps: %v", err)
	}
	for _, command := range []string{"setup apply", "setup smoke", "setup secrets"} {
		if err := runSetup(context.Background(), command, "", root, t.TempDir(), "", localrun.Manager{}, &out); err == nil || !strings.Contains(err.Error(), "--project") {
			t.Fatalf("%s without a project: %v", command, err)
		}
	}
	if err := runSetup(context.Background(), "setup check", "", t.TempDir(), t.TempDir(), "", localrun.Manager{}, &out); err == nil || !strings.Contains(err.Error(), "git repo") {
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

// apply stops where the person takes over with the exit code that means
// "pending", not "failed".
func TestApplyStopsAsPendingNotFailed(t *testing.T) {
	if !errors.Is(errSmokePending, initsmoke.ErrPending) {
		t.Fatal("the smoke hand-over must be the pending exit, not a failure")
	}
}

// The keys `setup secrets` collects are exactly the keys the wizard will
// look for: three when one key is shared; the intake key and one per role
// - design reviewers included - when they are separate. The mode is
// written down, never inferred from which keys happen to exist.
func TestSecretPlanFollowsTheFilesChoices(t *testing.T) {
	root := t.TempDir()
	write := func(body string) initwizard.Answers {
		if err := os.MkdirAll(filepath.Join(root, ".lassdas"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".lassdas", "setup.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		answers, err := initwizard.LoadAnswers(root)
		if err != nil {
			t.Fatal(err)
		}
		return answers
	}
	shared := write(`{"answers":{}}`)
	if mode, design := keyMode(shared); mode != initwizard.ModelKeysShared || design {
		t.Fatalf("default mode: %s %v", mode, design)
	}
	if plan := secretPlan(shared); len(plan) != 3 || plan[2].name != "LASSDAS_INTAKE_TARGET_KEY" {
		t.Fatalf("shared plan: %+v", plan)
	}
	separate := write(`{"answers":{"separate-model-keys":true,"separate-design":true}}`)
	if mode, design := keyMode(separate); mode != initwizard.ModelKeysSeparate || !design {
		t.Fatalf("separate mode: %s %v", mode, design)
	}
	plan := secretPlan(separate)
	names := map[string]bool{}
	for _, entry := range plan {
		names[entry.name] = true
	}
	for _, want := range []string{"TARGET_GITHUB_TOKEN", "BACKLOG_API_KEY", "LASSDAS_INTAKE_TARGET_KEY", "LASSDAS_IMPLEMENTER_KEY", "LASSDAS_REVIEW_A_KEY", "LASSDAS_APPLIER_KEY", "LASSDAS_DESIGN_REVIEW_A_KEY", "LASSDAS_DESIGN_REVIEW_B_KEY"} {
		if !names[want] {
			t.Fatalf("separate plan lacks %s: %+v", want, plan)
		}
	}
	if len(plan) != 3+7+2 {
		t.Fatalf("separate plan has %d entries", len(plan))
	}
}
