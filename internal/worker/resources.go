package worker

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"time"
)

// What the engine creates outside the repository has to be written down, or
// it is not findable afterwards. A change delivered as a pull request is
// reviewable by reading it; a queue, a bucket or a database the engine
// brought into existence so that change would work is invisible in the
// diff, outlives the delivery, and costs money until somebody knows it is
// there.
//
// The run records each one as it is made (internal/runner), and the trail
// carries the list into the pull request body and the ticket's closing
// comment, beside everything else the run decided on its own.

// CreatedResource is one thing that now exists because a run made it.
type CreatedResource struct {
	// Kind is the sort of thing, in the provider's own word for it — the
	// same vocabulary a destination's infrastructure block declares.
	Kind string `json:"kind"`
	// Identifier is what it is called where it lives: the name, the ARN,
	// the URL. It is what somebody types to find it again.
	Identifier string `json:"identifier"`
	// Provider is where it lives, when the record says so.
	Provider string `json:"provider,omitempty"`
	// Stage is the card that created it and CreatedAt when. Both are
	// stamped by the card, never taken from what an agent wrote.
	Stage     string    `json:"stage"`
	CreatedAt time.Time `json:"created_at"`
}

// MaxCreatedResourcesBytes bounds the record file a trail reads back.
const MaxCreatedResourcesBytes = 256 * 1024

// LoadCreatedResources reads a run's record, one JSON object per line. A
// missing file means the run created nothing, which is the ordinary case; a
// line that cannot be read is skipped rather than losing the rest, because
// the list exists so that what was made can still be found.
func LoadCreatedResources(path string) []CreatedResource {
	if path == "" {
		return nil
	}
	raw, err := readFileWithin(path, MaxCreatedResourcesBytes)
	if err != nil {
		return nil
	}
	var created []CreatedResource
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var record CreatedResource
		if json.Unmarshal([]byte(line), &record) != nil {
			continue
		}
		if record.Kind == "" || record.Identifier == "" {
			continue
		}
		created = append(created, record)
	}
	return created
}

// readFileWithin reads a regular file no larger than the bound. A symlink
// is not one: the record sits in a run directory an agent can reach.
func readFileWithin(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, os.ErrInvalid
	}
	return os.ReadFile(path)
}

// composeCreatedResources renders the list for the trail. Sorted by kind
// and then identifier rather than by the order they were made: the reader
// is looking for one thing, and two runs of the same request should read
// the same way.
func composeCreatedResources(created []CreatedResource) string {
	if len(created) == 0 {
		return ""
	}
	sorted := append([]CreatedResource(nil), created...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Kind != sorted[j].Kind {
			return sorted[i].Kind < sorted[j].Kind
		}
		return sorted[i].Identifier < sorted[j].Identifier
	})
	var builder strings.Builder
	builder.WriteString("\n### この依頼で作った資源 (repo の外・削除されません)\n")
	for _, record := range sorted {
		builder.WriteString("- " + trailClip(record.Kind, 64) + ": " + trailClip(record.Identifier, 256))
		if record.Provider != "" {
			builder.WriteString(" (" + trailClip(record.Provider, 64) + ")")
		}
		if record.Stage != "" {
			builder.WriteString(" — " + trailClip(record.Stage, 64) + " の工程")
		}
		if !record.CreatedAt.IsZero() {
			builder.WriteString(" " + record.CreatedAt.UTC().Format("2006-01-02 15:04 UTC"))
		}
		builder.WriteString("\n")
	}
	return builder.String()
}
