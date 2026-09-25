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

// CreatedResource is one thing an agent declared it made. The engine does
// not check that it exists: it has no standing to ask a provider about an
// account it reaches only through a credential the destination handed over,
// and a card that died after provisioning something could not have
// declared it either. So the list is what was claimed, not what was
// verified, and every field of it is untrusted text written by a model.
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
	// Refused marks a kind the destination did not allow. The declaration
	// is kept rather than dropped: an agent that said it made something is
	// the only evidence anyone has that it may exist, and a record that
	// silently discarded it would leave a resource nobody knows to look
	// for. It is never reported as created.
	Refused bool `json:"refused,omitempty"`
}

// AgentResourcesFile is what an agent declares its creations in, at the
// root of its working copy — the one place it can write. The card collects
// the file, stamps each line and removes it, so it never reaches the sealed
// candidate as a change to the destination's repository. Named here because
// the instruction that asks for it and the collector that reads it must
// name the same file.
const AgentResourcesFile = "lassdas-resources.jsonl"

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
	allowed, refused := splitRefusedResources(created)
	return composeResourceList("\n### この依頼で作った資源 (repo の外・削除されません。本体の確認ではなく実装役の申告です)\n", allowed) +
		composeResourceList("\n### 許可されていない種類として退けた宣言 (作られている可能性があります)\n", refused)
}

// splitRefusedResources keeps a declaration the destination did not allow
// out of what the run reports as created.
func splitRefusedResources(created []CreatedResource) (allowed, refused []CreatedResource) {
	for _, record := range created {
		if record.Refused {
			refused = append(refused, record)
			continue
		}
		allowed = append(allowed, record)
	}
	return allowed, refused
}

func composeResourceList(heading string, created []CreatedResource) string {
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
	builder.WriteString(heading)
	for _, record := range sorted {
		builder.WriteString("- " + plainResourceText(record.Kind, 64) + ": " + plainResourceText(record.Identifier, 256))
		if record.Provider != "" {
			builder.WriteString(" (" + plainResourceText(record.Provider, 64) + ")")
		}
		if record.Stage != "" {
			builder.WriteString(" — " + plainResourceText(record.Stage, 64) + " の工程")
		}
		if !record.CreatedAt.IsZero() {
			builder.WriteString(" " + record.CreatedAt.UTC().Format("2006-01-02 15:04 UTC"))
		}
		builder.WriteString("\n")
	}
	return builder.String()
}

// plainResourceText renders one declared field as text and not as markup.
//
// The record is a model's own words, and the two places it is shown — a
// pull request body and a tracker comment — both render markup. An
// identifier written as `x](https://elsewhere.invalid)` would come out as a
// link somebody might follow, believing the engine put it there. Bounding
// the field and stripping control characters, which the collection already
// does, does nothing about that: every character here is printable.
//
// Escaped rather than removed. What is in the field is how the resource is
// found again, and a name with a bracket in it is still that resource's
// name; a reader needs to see it as it is.
func plainResourceText(value string, limit int) string {
	clipped := trailClip(value, limit)
	var builder strings.Builder
	builder.Grow(len(clipped))
	for _, r := range clipped {
		if strings.ContainsRune(markupCharacters, r) {
			builder.WriteByte('\\')
		}
		builder.WriteRune(r)
	}
	return builder.String()
}

// markupCharacters are what builds a link, a code span, a table cell, an
// emphasis or a tag: the constructs that make text into something other
// than itself. Nothing a provider allows in an identifier is in the set —
// names are letters, digits and -_.:/ — so a declaration of a real resource
// comes out exactly as it went in, and the escape is only ever visible on
// one that was trying to be markup.
//
// Deliberately not the whole of Markdown's punctuation. A hyphen or a full
// stop escaped here would show as a backslash wherever the text is read as
// plain text, and the tracker comment is read that way: every resource
// name in every report would be wrong, to defend against a construct
// neither character can build on its own.
const markupCharacters = "\\`[]()<>|*~"
