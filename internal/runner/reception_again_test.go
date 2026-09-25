package runner

import (
	"os"
	"path/filepath"
	"testing"
)

// The preparation clears the run directory so that a restarted delivery
// re-derives everything from the ticket. One file survives it besides the
// launcher's lend lock: the note saying this delivery's reception has
// already been run a second time. It is the bound on that regeneration, and
// a bound the rebuild erases is no bound at all.
func TestPrepareKeepsTheReceptionAgainNote(t *testing.T) {
	workspace := t.TempDir()
	for _, name := range []string{ReceptionAgainFile, "ticket-draft.json"} {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(workspace, "history", "readiness"), 0o700); err != nil {
		t.Fatal(err)
	}

	pipeline := &Pipeline{Workspace: workspace}
	if err := pipeline.Prepare(); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(workspace, ReceptionAgainFile)); err != nil {
		t.Fatalf("the regeneration note did not survive the preparation: %v", err)
	}
	for _, cleared := range []string{"ticket-draft.json", "history"} {
		if _, err := os.Stat(filepath.Join(workspace, cleared)); !os.IsNotExist(err) {
			t.Fatalf("%s survived the preparation: %v", cleared, err)
		}
	}
}
