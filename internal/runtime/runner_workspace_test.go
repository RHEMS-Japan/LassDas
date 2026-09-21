package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunnerCardUsesConfiguredPersistentWorkspace(t *testing.T) {
	bin, log, _ := stubHermes(t)
	root := filepath.Join(t.TempDir(), "runs")
	h := NewHermes(Config{HermesBin: bin, HermesProfile: "runner", Chain: ChainConfig{RunsRoot: root}})
	for i := 0; i < 2; i++ {
		id, err := h.CreateCard(context.Background(), "delivery_test", "ticket", "body")
		if err != nil || id != "t_new" {
			t.Fatalf("CreateCard = %s, %v", id, err)
		}
		workspace := filepath.Join(root, "delivery_test")
		if info, err := os.Stat(workspace); err != nil || !info.IsDir() {
			t.Fatalf("workspace not created: %v", err)
		}
		for _, directory := range []string{root, workspace} {
			if info, err := os.Stat(directory); err != nil || info.Mode().Perm() != 0o711 {
				t.Fatalf("workspace ancestor must allow traversal only: %s, %v", directory, err)
			}
		}
		if i == 0 {
			if err := os.WriteFile(filepath.Join(workspace, "evidence.txt"), []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			// Existing directories from the older runner need repair too.
			for _, directory := range []string{root, workspace} {
				if err := os.Chmod(directory, 0o700); err != nil {
					t.Fatal(err)
				}
			}
		}
		if !strings.Contains(lastNonList(t, log), "--workspace|dir:"+workspace) {
			t.Fatal("canonical card did not receive persistent workspace")
		}
	}
	if data, err := os.ReadFile(filepath.Join(root, "delivery_test", "evidence.txt")); err != nil || string(data) != "keep" {
		t.Fatal("idempotent card creation erased records")
	}
	if info, err := os.Stat(filepath.Join(root, "delivery_test", "evidence.txt")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("repair widened the evidence file's permissions")
	}
}

func TestRunnerCardRefusesLinkedRootWithoutChangingTarget(t *testing.T) {
	bin, log, _ := stubHermes(t)
	target := t.TempDir()
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "linked-runs")
	if err := os.Symlink(target, root); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{root, root + "/", root + "/.", string(filepath.Separator)} {
		h := NewHermes(Config{HermesBin: bin, Chain: ChainConfig{RunsRoot: root}})
		if _, err := h.CreateCard(context.Background(), "delivery_test", "ticket", "body"); err == nil {
			t.Fatal("accepted unsafe runs root")
		}
	}
	after, err := os.Stat(target)
	if err != nil || after.Mode() != before.Mode() {
		t.Fatal("linked target mode changed")
	}
	if _, err := os.Stat(filepath.Join(target, "delivery_test")); !os.IsNotExist(err) {
		t.Fatal("created workspace through linked root")
	}
	if len(calls(t, log)) != 0 {
		t.Fatal("unsafe root reached Hermes")
	}
}

func TestRunnerCardWithoutRootKeepsLegacyScratch(t *testing.T) {
	bin, log, _ := stubHermes(t)
	h := NewHermes(Config{HermesBin: bin, HermesProfile: "runner"})
	if _, err := h.CreateCard(context.Background(), "delivery_test", "ticket", "body"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(lastNonList(t, log), "--workspace") {
		t.Fatal("legacy scratch default changed")
	}
}

func TestRunnerCardRefusesEscapingOrLinkedWorkspace(t *testing.T) {
	bin, log, _ := stubHermes(t)
	root := t.TempDir()
	h := NewHermes(Config{HermesBin: bin, Chain: ChainConfig{RunsRoot: root}})
	for _, delivery := range []string{"", ".", "..", "../escape", "/absolute"} {
		if _, err := h.CreateCard(context.Background(), delivery, "ticket", "body"); err == nil {
			t.Fatalf("accepted %q", delivery)
		}
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "delivery_link")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.CreateCard(context.Background(), "delivery_link", "ticket", "body"); err == nil {
		t.Fatal("accepted symlink workspace")
	}
	if len(calls(t, log)) != 0 {
		t.Fatal("unsafe workspace reached Hermes")
	}
}
