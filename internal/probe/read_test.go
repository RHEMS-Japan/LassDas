package probe

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readSession records one repo.read of a file larger than the excerpt.
func readSession(t *testing.T, content string, excerpt int) (*Session, Measurement) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "long.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	catalog, err := NewCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := OpenRecorder(filepath.Join(t.TempDir(), "measurements.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{Catalog: catalog, Recorder: recorder, RepoRoot: root, Limits: Limits{MaxProbes: 5, MaxTotalBytes: 1 << 20, ExcerptBytes: excerpt, MaxReads: 5}}
	outcome, err := session.Run(context.Background(), Request{Probe: "repo.read", Args: map[string]string{"path": "long.txt"}})
	if err != nil || outcome.Measurement.Refused {
		t.Fatalf("repo.read: %+v, %v", outcome.Measurement, err)
	}
	if len(outcome.Excerpt) != excerpt || outcome.Measurement.ExcerptBytes != excerpt {
		t.Fatalf("excerpt = %d bytes, want %d", len(outcome.Excerpt), excerpt)
	}
	return session, outcome.Measurement
}

// A window continues exactly where the excerpt ended, the next where that
// window ended, and the last says nothing remains; together they are the
// stored output byte for byte.
func TestReadPagesThroughTheStoredOutput(t *testing.T) {
	content := strings.Repeat("0123456789", 5) // 50 bytes
	session, measurement := readSession(t, content, 20)
	first, err := session.Read(measurement.ID, measurement.ExcerptBytes)
	if err != nil || first.Offset != 20 || first.Text != content[20:40] || first.NextOffset != 40 || first.Remaining != 10 || first.StoredBytes != 50 {
		t.Fatalf("first window = %+v, %v", first, err)
	}
	last, err := session.Read(measurement.ID, first.NextOffset)
	if err != nil || last.Text != content[40:] || last.Remaining != 0 || last.NextOffset != 50 {
		t.Fatalf("last window = %+v, %v", last, err)
	}
	if session.Reads != 2 || session.Used != 1 {
		t.Fatalf("reads/used = %d/%d, want 2 reads and the one probe", session.Reads, session.Used)
	}
}

// A window never splits a character: the cut backs off to a boundary and
// the next window starts there.
func TestReadCutsOnACharacterBoundary(t *testing.T) {
	content := strings.Repeat("あ", 20) // 60 bytes, 3 per character
	session, measurement := readSession(t, content, 9)
	window, err := session.Read(measurement.ID, 9)
	if err != nil {
		t.Fatal(err)
	}
	if window.Bytes != 9 || window.Text != strings.Repeat("あ", 3) {
		t.Fatalf("aligned window = %+v", window)
	}
	// An offset inside a character still ends on a boundary.
	session.Limits.ExcerptBytes = 10
	window, err = session.Read(measurement.ID, 9)
	if err != nil || window.Bytes != 9 || window.NextOffset != 18 {
		t.Fatalf("window with a 10-byte limit = %+v, %v; want 9 bytes ending on a boundary", window, err)
	}
}

// Reads are refused, never guessed: an unknown id, an offset outside the
// record, a refused measurement, and the read budget — which counts every
// request, refusals included, as the probe budget does.
func TestReadRefusesWhatItCannotShow(t *testing.T) {
	session, measurement := readSession(t, strings.Repeat("x", 30), 10)
	if _, err := session.Read("m-0009", 0); err == nil || !strings.Contains(err.Error(), "not a recorded measurement") {
		t.Fatalf("unknown id: %v", err)
	}
	if _, err := session.Read("bogus", 0); err == nil {
		t.Fatal("a malformed id was accepted")
	}
	if _, err := session.Read(measurement.ID, 30); err == nil || !strings.Contains(err.Error(), "outside the stored output") {
		t.Fatalf("offset at the end: %v", err)
	}
	// Three refusals spent three of the five; two more windows fit, the
	// sixth request is refused.
	for i := 0; i < 2; i++ {
		if _, err := session.Read(measurement.ID, 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := session.Read(measurement.ID, 0); !errors.Is(err, ErrReadBudgetExhausted) {
		t.Fatalf("over the read budget: %v", err)
	}
	if session.Reads != 5 {
		t.Fatalf("reads = %d, want every request counted", session.Reads)
	}
	refused, err := session.Run(context.Background(), Request{Probe: "no.such.probe"})
	if err != nil || !refused.Measurement.Refused {
		t.Fatalf("a refusal was expected: %+v, %v", refused.Measurement, err)
	}
	session.Reads = 0
	if _, err := session.Read(refused.Measurement.ID, 0); err == nil || !strings.Contains(err.Error(), "was refused") {
		t.Fatalf("reading a refused measurement: %v", err)
	}
}
