package probe

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMeasurementChainVerifiesPrefixes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "measurements.jsonl")
	recorder, err := OpenRecorder(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	var chains []string
	for i := 0; i < 3; i++ {
		m, err := recorder.Append(Measurement{Probe: "repo.list", StartedAt: now, EndedAt: now.Add(time.Second), Output: strings.Repeat("x", i+1)})
		if err != nil {
			t.Fatal(err)
		}
		if m.ID != measurementID(i+1) {
			t.Errorf("id = %q", m.ID)
		}
		chains = append(chains, m.ChainSHA256)
	}
	// A report sealed after two lines verifies against the file after a third was appended.
	chain, err := VerifyPrefix(path, 2)
	if err != nil || chain != chains[1] {
		t.Fatalf("VerifyPrefix(2) = %q, %v; want %q", chain, err, chains[1])
	}
	if _, err := VerifyPrefix(path, 4); !errors.Is(err, ErrChainBroken) {
		t.Errorf("VerifyPrefix(4) on a 3-line file: %v", err)
	}
	// Reopening continues the chain rather than restarting it.
	reopened, err := OpenRecorder(path)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Count() != 3 || reopened.Chain() != chains[2] || reopened.NextID() != "m-0004" {
		t.Errorf("reopened recorder: count %d chain %q next %q", reopened.Count(), reopened.Chain(), reopened.NextID())
	}
	// Editing a line in the middle breaks the verification of every prefix that includes it.
	raw, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	lines[1] = strings.Replace(lines[1], `"output":"xx"`, `"output":"xy"`, 1)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyPrefix(path, 1); err != nil {
		t.Errorf("prefix before the edit should still verify: %v", err)
	}
	if _, err := VerifyPrefix(path, 2); !errors.Is(err, ErrChainBroken) {
		t.Errorf("edited line accepted: %v", err)
	}
	if _, err := OpenRecorder(path); !errors.Is(err, ErrChainBroken) {
		t.Errorf("recorder reopened a broken file: %v", err)
	}
	// Reordering lines is caught by the ids.
	lines[1], lines[2] = lines[2], lines[1]
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyPrefix(path, 3); !errors.Is(err, ErrChainBroken) {
		t.Errorf("reordered lines accepted: %v", err)
	}
}

func TestReadPrefixReturnsVerifiedMeasurements(t *testing.T) {
	path := filepath.Join(t.TempDir(), "measurements.jsonl")
	recorder, err := OpenRecorder(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, output := range []string{"a", "b"} {
		if _, err := recorder.Append(Measurement{Probe: "repo.read", StartedAt: now, EndedAt: now, Output: output}); err != nil {
			t.Fatal(err)
		}
	}
	measurements, err := ReadPrefix(path, 1)
	if err != nil || len(measurements) != 1 || measurements[0].Output != "a" || measurements[0].OutputSHA256 != digestHex([]byte("a")) {
		t.Fatalf("ReadPrefix(1) = %+v, %v", measurements, err)
	}
}
