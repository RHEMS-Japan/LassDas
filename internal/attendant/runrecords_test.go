package attendant

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The agents run as another user with the run directory as their working
// area's parent (the target clone lives inside it): the attendant's own
// records there must not be readable by that user, and the directory must
// let it pass without letting it list.
func TestRunRecordsStayClosedToOtherUsers(t *testing.T) {
	runDir := filepath.Join(t.TempDir(), "delivery_test")
	recordStreakCheck(runDir, time.Now())
	sealBoardOutcome(runDir, "checks", "pass", "note")

	info, err := os.Stat(runDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o711 {
		t.Fatalf("run directory mode = %o, want 711 (enter, not list)", got)
	}
	for _, name := range []string{streakCheckFile, boardOutcomeFile} {
		info, err := os.Stat(filepath.Join(runDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("%s mode = %o, want 600", name, got)
		}
	}
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
