package attendant

import (
	"os"
	"path/filepath"
	"testing"
)

// The agents run as another user with the run directory as their working
// area's parent (the target clone lives inside it): the attendant's own
// records there must not be readable by that user, and the directory must
// let it pass without letting it list.
func TestRunRecordsStayClosedToOtherUsers(t *testing.T) {
	runDir := filepath.Join(t.TempDir(), "delivery_test")
	writeLadderRecord(runDir, "validate", 1, ladderRecord{Attempts: 1}, &pendingTestLogger{})
	sealBoardOutcome(runDir, "checks", "pass", "note")
	writeBoardPhase(runDir, "delivered", &pendingTestLogger{}, "TKT-1")

	info, err := os.Stat(runDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o711 {
		t.Fatalf("run directory mode = %o, want 711 (enter, not list)", got)
	}
	// The ladder's own record sits one directory down, and that directory
	// is held to the same terms: the agent user may pass through it and
	// must not be able to list it or read what is inside.
	if got := modeOf(t, filepath.Dir(ladderRecordFile(runDir, "validate", 1))); got != 0o711 {
		t.Fatalf("ladder record directory mode = %o, want 711 (enter, not list)", got)
	}
	if got := modeOf(t, ladderRecordFile(runDir, "validate", 1)); got != 0o600 {
		t.Fatalf("ladder record mode = %o, want 600", got)
	}
	for _, name := range []string{boardOutcomeFile, boardPhaseFile} {
		info, err := os.Stat(filepath.Join(runDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("%s mode = %o, want 600", name, got)
		}
	}
}

func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

// A design-shape run needs the applier's launch; the attendant sees the
// gap before creating any card.
func TestConsumerHasApplierReadsTheLaunch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "consumer.json")
	for content, want := range map[string]bool{
		`{"agents":{"implementer":{"command":"hermes"}}}`:                                false,
		`{"agents":{"implementer":{"command":"hermes"},"applier":{"command":"hermes"}}}`: true,
		`{"agents":{"applier":{}}}`:                                                      false,
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := consumerHasApplier(path)
		if err != nil || got != want {
			t.Fatalf("%s: got %v, %v; want %v", content, got, err, want)
		}
	}
	if _, err := consumerHasApplier(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("an unreadable configuration passed")
	}
}
