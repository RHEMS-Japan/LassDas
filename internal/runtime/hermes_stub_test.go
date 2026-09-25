package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubHermes writes a fake `hermes` CLI that records every argv record and
// answers `list` from a canned file. Driving the real command layer keeps
// the argument shapes honest — the block-reason regression (a flag where
// the canonical CLI takes a positional) is exactly the kind of defect no
// mocked interface would have caught.
func stubHermes(t *testing.T) (bin string, callLog string, tasksFile string) {
	t.Helper()
	directory := t.TempDir()
	callLog = filepath.Join(directory, "calls.log")
	tasksFile = filepath.Join(directory, "tasks.json")
	bin = filepath.Join(directory, "hermes")
	script := `#!/bin/sh
{ printf '%s|' "$@"; echo; } >> "` + callLog + `"
case "$2" in
  list)   cat "` + tasksFile + `" ;;
  create) printf '{"id":"t_new"}\n' ;;
  *) : ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	setTasks(t, tasksFile, nil)
	return bin, callLog, tasksFile
}

func setTasks(t *testing.T, tasksFile string, tasks []BoardTask) {
	t.Helper()
	if tasks == nil {
		tasks = []BoardTask{}
	}
	encoded, err := json.Marshal(tasks)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tasksFile, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func calls(t *testing.T, callLog string) []string {
	t.Helper()
	raw, err := os.ReadFile(callLog)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	records := strings.Split(strings.TrimSuffix(string(raw), "|\n"), "|\n")
	for index := range records {
		records[index] = strings.TrimSuffix(records[index], "|")
	}
	return records
}
