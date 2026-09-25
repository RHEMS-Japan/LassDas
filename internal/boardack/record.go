// Package boardack is the board's record of which finished runs a person
// has cleared away.
//
// It is the board's own fact, not the engine's: it reaches no tracker,
// needs no requester credential, and says only that somebody looked at a
// finished card and took it off the list. It lives beside the snapshot in
// the status directory because two programs need it — the board's server
// writes it and draws the lanes from it, and the attendant reads it to know
// which finished rows it may leave out of the snapshot. Written out twice,
// the two would drift and the board would quietly stop showing cards
// nobody had cleared.
package boardack

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// FileName is the record, in the status directory. It is the board's own
// file, disjoint from the attendant's board.json and events.jsonl, so the
// single-writer rule holds per file.
const FileName = "acknowledged.json"

// MaxEntries bounds the record. Cleared cards leave the board for good, so
// entries past this many are for runs no board will show again; the oldest
// go first.
const MaxEntries = 500

// maxFileBytes bounds one reading, the way every other file the board reads
// is bounded.
const maxFileBytes = 1 << 20

// Entry is one person's "I have seen this one, take it off the list", with
// when and who, because being able to say when is the whole point.
type Entry struct {
	At       time.Time `json:"at"`
	User     string    `json:"user,omitempty"`
	ClientIP string    `json:"client_ip,omitempty"`
}

type record struct {
	SchemaVersion int              `json:"schema_version"`
	Runs          map[string]Entry `json:"runs"`
}

// Read returns what has been cleared away, keyed by delivery. An absent,
// oversized or unreadable record is no acknowledgement at all: every
// finished card then stays in the running lane, which is the state that
// asks a person to look rather than the one that hides things.
func Read(statusDir string) map[string]Entry {
	raw, err := os.ReadFile(filepath.Join(statusDir, FileName))
	if err != nil || len(raw) > maxFileBytes {
		return map[string]Entry{}
	}
	var stored record
	if json.Unmarshal(raw, &stored) != nil || stored.Runs == nil {
		return map[string]Entry{}
	}
	for id, entry := range stored.Runs {
		if id == "" || entry.At.IsZero() {
			delete(stored.Runs, id)
		}
	}
	return stored.Runs
}

// Write replaces the record with these entries, oldest dropped past
// MaxEntries. The whole file is written and then renamed over the old one,
// so a reader arriving mid-write sees one complete record or the other,
// never half.
func Write(statusDir string, runs map[string]Entry) error {
	if len(runs) > MaxEntries {
		ids := make([]string, 0, len(runs))
		for id := range runs {
			ids = append(ids, id)
		}
		// Oldest first, and the id breaks a tie so two entries stamped in
		// the same instant are dropped in a fixed order.
		sort.Slice(ids, func(a, b int) bool {
			if runs[ids[a]].At.Equal(runs[ids[b]].At) {
				return ids[a] < ids[b]
			}
			return runs[ids[a]].At.Before(runs[ids[b]].At)
		})
		for _, id := range ids[:len(runs)-MaxEntries] {
			delete(runs, id)
		}
	}
	encoded, err := json.Marshal(record{SchemaVersion: 1, Runs: runs})
	if err != nil {
		return err
	}
	temp := filepath.Join(statusDir, FileName+".tmp")
	if err := os.WriteFile(temp, encoded, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temp, filepath.Join(statusDir, FileName)); err != nil {
		_ = os.Remove(temp)
		return err
	}
	return nil
}
