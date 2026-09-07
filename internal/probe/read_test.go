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

// Neither the excerpt nor a window splits a character: both cuts back off
// to a boundary, the next window starts where the previous one ended, and
// the excerpt plus the windows are the stored output byte for byte.
func TestExcerptAndWindowsCutOnCharacterBoundaries(t *testing.T) {
	content := strings.Repeat("あ", 20) // 60 bytes, 3 per character
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "long.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	catalog, _ := NewCatalog(nil)
	recorder, err := OpenRecorder(filepath.Join(t.TempDir(), "measurements.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	// A 10-byte limit falls inside the fourth character: the excerpt is 9.
	session := &Session{Catalog: catalog, Recorder: recorder, RepoRoot: root, Limits: Limits{MaxProbes: 5, MaxTotalBytes: 1 << 20, ExcerptBytes: 10, MaxReads: 20}}
	outcome, err := session.Run(context.Background(), Request{Probe: "repo.read", Args: map[string]string{"path": "long.txt"}})
	if err != nil || outcome.Excerpt != strings.Repeat("あ", 3) || outcome.Measurement.ExcerptBytes != 9 {
		t.Fatalf("excerpt = %q (%d bytes), %v; want three whole characters", outcome.Excerpt, outcome.Measurement.ExcerptBytes, err)
	}
	assembled := outcome.Excerpt
	offset := outcome.Measurement.ExcerptBytes
	for offset < len(content) {
		window, err := session.Read(outcome.Measurement.ID, offset)
		if err != nil {
			t.Fatalf("read at %d: %v", offset, err)
		}
		if window.Bytes%3 != 0 || window.Bytes == 0 {
			t.Fatalf("window at %d cut a character: %+v", offset, window)
		}
		assembled += window.Text
		offset = window.NextOffset
	}
	if assembled != content {
		t.Fatalf("excerpt + windows differ from the stored output")
	}
	// An offset inside a character is refused, not realigned.
	if _, err := session.Read(outcome.Measurement.ID, 10); !errors.Is(err, ErrReadRefused) || !strings.Contains(err.Error(), "inside a character") {
		t.Fatalf("offset inside a character: %v", err)
	}
	// A limit too small for one character would give an empty window: refused.
	session.Limits.ExcerptBytes = 1
	if _, err := session.Read(outcome.Measurement.ID, 9); !errors.Is(err, ErrReadRefused) || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty window: %v", err)
	}
}

// A measurement the probe's own cap cut is readable to what was stored,
// says so, and refuses an offset in the tail that exists nowhere.
func TestReadOfATruncatedMeasurementStopsAtWhatWasStored(t *testing.T) {
	recorder, err := OpenRecorder(filepath.Join(t.TempDir(), "measurements.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recorder.Append(Measurement{Probe: "k8s.events", Output: strings.Repeat("e", 30), OutputBytes: 100, Truncated: true, ExcerptBytes: 10}); err != nil {
		t.Fatal(err)
	}
	session := &Session{Recorder: recorder, Limits: Limits{ExcerptBytes: 10, MaxReads: 5}}
	window, err := session.Read("m-0001", 10)
	if err != nil || !window.Truncated || window.StoredBytes != 30 || window.Remaining != 10 {
		t.Fatalf("window of a truncated measurement = %+v, %v", window, err)
	}
	if _, err := session.Read("m-0001", 50); !errors.Is(err, ErrReadRefused) {
		t.Fatalf("an offset in the lost tail was served: %v", err)
	}
}

// Reads are refused, never guessed: an unknown id, an offset outside the
// record, a refused measurement, and the read budget — which counts every
// request, refusals included, as the probe budget does.
func TestReadRefusesWhatItCannotShow(t *testing.T) {
	session, measurement := readSession(t, strings.Repeat("x", 30), 10)
	if _, err := session.Read("m-0009", 0); !errors.Is(err, ErrReadRefused) || !strings.Contains(err.Error(), "not a recorded measurement") {
		t.Fatalf("unknown id: %v", err)
	}
	for _, bogus := range []string{"bogus", "m-1", "m--001", "m-+001", "m-0001x", " m-0001", "m-0000", "m-10000"} {
		session.Reads = 0
		if _, err := session.Read(bogus, 0); !errors.Is(err, ErrReadRefused) {
			t.Fatalf("a malformed id %q was accepted: %v", bogus, err)
		}
	}
	session.Reads = 0
	if _, err := session.Read(measurement.ID, 30); !errors.Is(err, ErrReadRefused) || !strings.Contains(err.Error(), "outside the stored output") {
		t.Fatalf("offset at the end: %v", err)
	}
	// The refusal spent one of the five; four more windows fit, the sixth
	// request is refused.
	for i := 0; i < 4; i++ {
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
