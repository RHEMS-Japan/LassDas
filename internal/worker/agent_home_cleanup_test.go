package worker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveAgentHomeCleansReadOnlyCachesWithoutFollowingLinks(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "launch")
	cache := filepath.Join(home, "cache", "module")
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "keep")
	if err := os.WriteFile(sentinel, []byte("unchanged"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(outside, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(outside, 0o700) })
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "source.go"), []byte("cache"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(cache, "external")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cache, 0o555); err != nil {
		t.Fatal(err)
	}
	// Cleanup remains possible even when a mutation restores RemoveAll.
	t.Cleanup(func() { _ = os.Chmod(cache, 0o700) })
	if err := removeAgentHome(home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(home); !os.IsNotExist(err) {
		t.Fatal("launch home remains")
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "unchanged" {
		t.Fatal("linked file changed")
	}
	if info, err := os.Stat(sentinel); err != nil || info.Mode().Perm() != 0o400 {
		t.Fatal("linked file permissions changed")
	}
	if info, err := os.Stat(outside); err != nil || info.Mode().Perm() != 0o500 {
		t.Fatal("linked directory permissions changed")
	}
	if err := removeAgentHome(home); err != nil {
		t.Fatalf("already removed home: %v", err)
	}
	if err := os.Symlink(outside, home); err != nil {
		t.Fatal(err)
	}
	if err := removeAgentHome(home); err == nil {
		t.Fatal("accepted linked launch root")
	}
}
