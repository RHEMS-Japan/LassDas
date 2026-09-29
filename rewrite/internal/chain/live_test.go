package chain

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLiveWriterReplacesACredentialSplitAcrossWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := newLiveWriter(file, []string{"SECRET-VALUE-123", "short"})
	for _, part := range []string{"start SECRET-VAL", "UE-123 middle sho", "rt end"} {
		if n, err := writer.Write([]byte(part)); err != nil || n != len(part) {
			t.Fatalf("write %q: %d %v", part, n, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "start [credential] middle [credential] end" {
		t.Fatalf("on disk: %q", raw)
	}
}

func TestLiveWriterHoldsBackNothingWithoutCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := newLiveWriter(file, nil)
	writer.Write([]byte("visible at once"))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "visible at once" {
		t.Fatalf("before close: %q", raw)
	}
	writer.Close()
}

func TestAProcessWritesItsLiveCopyWhileRunningAndRemovesItAfter(t *testing.T) {
	live := filepath.Join(t.TempDir(), "live")
	process := Process{Name: "p", Command: []string{"/bin/sh", "-c", "echo first; sleep 1; echo second; echo note >&2"}, Live: live}
	done := make(chan Result, 1)
	go func() {
		done <- process.run(context.Background(), Role{Name: "r"}, Assignment{Role: "r", Instruction: "i"}, State{})
	}()
	deadline := time.Now().Add(5 * time.Second)
	var seen string
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(filepath.Join(live, "r-p.stdout")); err == nil && strings.Contains(string(raw), "first") {
			seen = string(raw)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if seen == "" {
		t.Fatal("the live copy never showed the first line while the process ran")
	}
	if raw, err := os.ReadFile(filepath.Join(live, "r-p.json")); err != nil || !strings.Contains(string(raw), `"role":"r"`) {
		t.Fatalf("live record: %v %q", err, raw)
	}
	result := <-done
	if result.Error != "" || result.Output != "first\nsecond\n" || result.Diagnostics != "note\n" {
		t.Fatalf("record: %+v", result)
	}
	for _, suffix := range []string{".json", ".stdout", ".stderr"} {
		if _, err := os.Stat(filepath.Join(live, "r-p"+suffix)); err == nil {
			t.Errorf("the live copy %s survived the completed record", suffix)
		}
	}
}

func TestALiveDirectoryThatCannotBeCreatedIsNotedNotFatal(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	process := Process{Name: "p", Command: []string{"/bin/sh", "-c", "echo ran"}, Live: filepath.Join(blocker, "live")}
	result := process.run(context.Background(), Role{Name: "r"}, Assignment{Role: "r"}, State{})
	if result.Error != "" || result.Output != "ran\n" {
		t.Fatalf("the run was affected: %+v", result)
	}
	if !strings.Contains(result.Diagnostics, "live output was not written") {
		t.Fatalf("the failure to show live output was hidden: %q", result.Diagnostics)
	}
}
