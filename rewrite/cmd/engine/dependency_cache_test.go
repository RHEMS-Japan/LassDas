package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDependencyCacheIsOptInAndRequestLocal(t *testing.T) {
	cfg := watchConfiguration(t)
	cfg.Roles[0].Processes = append(cfg.Roles[0].Processes, cfg.Roles[0].Processes[0])
	before, err := bindRequestConfig(cfg, "/fixture/one", "EXAMPLE-1")
	if err != nil {
		t.Fatal(err)
	}
	baseline, _ := json.Marshal(before)
	cfg.Intake.DependencyCache = false
	off, err := bindRequestConfig(cfg, "/fixture/one", "EXAMPLE-1")
	if err != nil {
		t.Fatal(err)
	}
	serialized, _ := json.Marshal(off)
	if !bytes.Equal(baseline, serialized) || bytes.Contains(serialized, []byte("dependency_cache")) || bytes.Contains(serialized, []byte("TASK_DEPENDENCY_CACHE")) {
		t.Fatal("disabled cache changed the serialized request")
	}
	cfg.Intake.DependencyCache = true
	first, err := bindRequestConfig(cfg, "/fixture/one", "EXAMPLE-1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := bindRequestConfig(cfg, "/fixture/two", "EXAMPLE-2")
	if err != nil {
		t.Fatal(err)
	}
	for _, process := range first.Roles[0].Processes {
		if process.Env["TASK_DEPENDENCY_CACHE"] != "/fixture/one/dependency-cache" {
			t.Fatal("roles did not share the request cache")
		}
	}
	if second.Roles[0].Processes[0].Env["TASK_DEPENDENCY_CACHE"] == first.Roles[0].Processes[0].Env["TASK_DEPENDENCY_CACHE"] {
		t.Fatal("different requests share a cache")
	}
	cfg.Roles[0].Processes[0].Env = map[string]string{"TASK_DEPENDENCY_CACHE": "/fixture/two/dependency-cache"}
	if _, err := bindRequestConfig(cfg, "/fixture/one", "EXAMPLE-1"); err == nil {
		t.Fatal("configuration replaced the owned cache path")
	}
}

func TestFinishedSharedCacheIsRemovedAfterDisablingAndAfterEarlierTrim(t *testing.T) {
	for _, alreadyTrimmed := range []bool{false, true} {
		root := t.TempDir()
		cache := filepath.Join(root, "dependency-cache", "go-mod")
		if err := os.MkdirAll(cache, 0700); err != nil {
			t.Fatal(err)
		}
		if alreadyTrimmed {
			if err := os.MkdirAll(filepath.Join(root, "homes"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "homes", ".trimmed"), []byte("earlier cleanup\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := trimFinished(root); err != nil {
			t.Fatal(err)
		}
		_, err := os.Stat(cache)
		if !os.IsNotExist(err) {
			t.Fatalf("alreadyTrimmed=%t: cache stat %v", alreadyTrimmed, err)
		}
	}
}

func TestDisabledCacheAddsNoCleanupWritesOrRemovals(t *testing.T) {
	root := t.TempDir()
	kept := filepath.Join(root, "unrelated")
	if err := os.WriteFile(kept, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := trimFinished(root); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "unrelated" {
		t.Fatal("disabled cleanup created or removed files")
	}
	content, err := os.ReadFile(kept)
	if err != nil || string(content) != "unchanged" {
		t.Fatalf("unrelated file changed: %v", err)
	}
}
