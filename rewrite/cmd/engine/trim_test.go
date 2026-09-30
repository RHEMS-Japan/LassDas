package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAFinishedRequestLosesItsCachesAndKeepsWhatPeopleRead(t *testing.T) {
	directory := t.TempDir()
	for _, path := range []string{"homes/2-0/.cache/go-build/x", "homes/2-0/go/pkg/mod/y", "homes/2-0/logs", "homes/3-0/lsp/z", "homes/3-0/logs", "workspace/src"} {
		if err := os.MkdirAll(filepath.Join(directory, path), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"homes/2-0/logs/agent.log", "homes/2-0/transcript.json", "homes/2-0/state.db", "homes/3-0/logs/errors.log", "workspace/src/main.go", "homes/2-0/.cache/go-build/x/blob"} {
		if err := os.WriteFile(filepath.Join(directory, path), []byte("kept?"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := trimFinished(directory); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"homes/2-0/.cache", "homes/2-0/go", "homes/3-0/lsp"} {
		if _, err := os.Stat(filepath.Join(directory, gone)); err == nil {
			t.Errorf("%s survived the trim", gone)
		}
	}
	for _, kept := range []string{"homes/2-0/logs/agent.log", "homes/2-0/transcript.json", "homes/2-0/state.db", "homes/3-0/logs/errors.log", "workspace/src/main.go", "homes/.trimmed"} {
		if _, err := os.Stat(filepath.Join(directory, kept)); err != nil {
			t.Errorf("%s was removed or not written: %v", kept, err)
		}
	}
	// A second pass is a no-op, and a request without homes needs nothing.
	if err := trimFinished(directory); err != nil {
		t.Fatal(err)
	}
	if err := trimFinished(t.TempDir()); err != nil {
		t.Fatal(err)
	}
}
