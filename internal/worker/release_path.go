package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

// A destination that asks for production needs more than a pull request.
// Something has to deploy the change, something has to record which build
// landed, and something has to open a screen afterwards and judge it. A
// destination with none of that was delivered as far as the proposal, and
// the ticket was given a line naming the settings somebody would have to go
// and write.
//
// That line asked a person to do the work, which is the one thing the
// engine is for. The gap is turned into work for the round that is about to
// run instead — the same round that implements the request — and it travels
// there the way every other reason for a round travels: a record sealed
// beside the round's own, read by the command that renders the instruction.
//
// What the engine was not handed the means to apply is named rather than
// asked for. An item carrying a means is not in the instruction and never
// becomes a request on a ticket; it is reported afterwards, by name, as
// something the delivery did not apply.

// ReleasePathSchemaVersion is this record's shape.
const ReleasePathSchemaVersion = 1

// ReleasePathFile is the plan's name inside a delivery's directory. One
// name, read by the tick that writes it and by the stage that hands it to
// the command, so the writer and the reader cannot drift apart.
const ReleasePathFile = "release-path.json"

// MaxReleasePathJSONBytes bounds the read of a sealed record: a handful of
// short items and one instruction that itself has to fit in a prompt.
const MaxReleasePathJSONBytes int64 = 64 * 1024

// MaxReleasePathItems bounds how many things one plan may name. The release
// path has a fixed number of parts; a record claiming more than this is not
// describing one.
const MaxReleasePathItems = 24

// MaxReleasePathInstructionBytes bounds the instruction text. It shares one
// prompt with the request, the boundaries and the previous rounds'
// objections, so it takes about the same share the objections do.
const MaxReleasePathInstructionBytes = 8 * 1024

// The parts of a release path, as the kinds an item is grouped under. They
// are named rather than free text because the report and the instruction
// both sort by them, and a kind invented at one end would sort nowhere at
// the other.
const (
	// ReleasePathWorkflow is the workflow that deploys, named by the
	// destination's own settings.
	ReleasePathWorkflow = "workflow"
	// ReleasePathDigestCommit is the policy that says which commit records
	// what actually landed, so a promotion can prove it moved one delivery.
	ReleasePathDigestCommit = "digest_commit"
	// ReleasePathObservation is the entry the observing browser opens, and
	// the language it asks the screen for.
	ReleasePathObservation = "observation"
	// ReleasePathOrigin is where staging and production answer.
	ReleasePathOrigin = "origin"
	// ReleasePathCards is the instance-side configuration that gives the
	// delivery its cards.
	ReleasePathCards = "cards"
)

// ReleasePathItem is one part of the release path that is not there yet.
type ReleasePathItem struct {
	// Name is what an operator would call it: the settings key, or the
	// repository path of the file that has to exist. It is the name the
	// report uses, so it is written the way it is written in the
	// configuration rather than described.
	Name string `json:"name"`
	// Kind groups the item with the other parts of its own half of the
	// path.
	Kind string `json:"kind"`
	// Detail is the one sentence that says what this part is for. It is
	// what the instruction carries, so it says what to build rather than
	// what is missing.
	Detail string `json:"detail"`
	// Means names what the engine would have had to be handed to apply this
	// part itself — a writable scope that can address the file, the cards'
	// own configuration. Empty is the ordinary case: the engine builds it.
	Means string `json:"means,omitempty"`
}

// Buildable reports whether the engine can apply this part with what it was
// handed. An item that is not buildable is reported by name and never
// asked for.
func (i ReleasePathItem) Buildable() bool { return i.Means == "" }

// ReleasePathPlan is one delivery's account of the release path its
// destination asks for and does not have.
type ReleasePathPlan struct {
	SchemaVersion int    `json:"schema_version"`
	Repository    string `json:"repository"`
	// Configured is the depth the destination asks for, restated inside the
	// record: a plan found beside a run proves nothing about which
	// destination it was decided for.
	Configured string            `json:"configured"`
	Items      []ReleasePathItem `json:"items,omitempty"`
	// WorkflowFiles are the deploy workflow files this plan says the round
	// will create, as repository paths under the workflow directory. Empty
	// unless the destination handed the means to author them, and it is
	// this list — not the configuration, and not a flag — that the path
	// gates admit for the run the plan belongs to.
	WorkflowFiles []string `json:"workflow_files,omitempty"`
	// Instruction is what the implementing round is told to build, composed
	// from the buildable items. Empty when there is nothing to build.
	Instruction string `json:"instruction,omitempty"`
	// Hold, when set, is why the delivery did not go on to production after
	// staging: the built path did not check out. It is the requester's own
	// sentence, so it says what stopped rather than what to configure.
	Hold         string    `json:"hold,omitempty"`
	DecidedAt    time.Time `json:"decided_at"`
	RecordSHA256 string    `json:"record_sha256"`
}

// Empty reports whether the destination's release path is complete. A plan
// with nothing in it is the ordinary case and changes nothing about the
// round that follows.
func (p ReleasePathPlan) Empty() bool { return len(p.Items) == 0 }

// UnappliedNames are the names of the parts the engine did not apply, for
// a report to say what the delivery left undone.
//
// Names only, and never a sentence asking for them. The report says what it
// did and what it did not; a line telling a person to go and configure
// something is the shape this whole record exists to remove.
func (p ReleasePathPlan) UnappliedNames() []string {
	unapplied := p.Unapplied()
	names := make([]string, 0, len(unapplied))
	for _, item := range unapplied {
		names = append(names, item.Name)
	}
	return names
}

// Buildable is the parts the engine applies itself.
func (p ReleasePathPlan) Buildable() []ReleasePathItem {
	return p.filter(func(item ReleasePathItem) bool { return item.Buildable() })
}

// Unapplied is the parts the engine could not apply, in the order they were
// found. These are reported by name and never asked for.
func (p ReleasePathPlan) Unapplied() []ReleasePathItem {
	return p.filter(func(item ReleasePathItem) bool { return !item.Buildable() })
}

func (p ReleasePathPlan) filter(keep func(ReleasePathItem) bool) []ReleasePathItem {
	kept := make([]ReleasePathItem, 0, len(p.Items))
	for _, item := range p.Items {
		if keep(item) {
			kept = append(kept, item)
		}
	}
	return kept
}

// Seal fills in the digest over everything else the record says, so a
// reader in another process can tell a record from a half-written one.
func (p *ReleasePathPlan) Seal() error {
	digest, err := p.digest()
	if err != nil {
		return err
	}
	p.RecordSHA256 = digest
	return nil
}

// Bound reports whether a record read back is one of ours and intact.
// Anything else is reported as not ours rather than repaired: the round
// after this one is about to be told what is in here.
func (p ReleasePathPlan) Bound() bool {
	if p.SchemaVersion != ReleasePathSchemaVersion || p.Repository == "" {
		return false
	}
	if len(p.Items) > MaxReleasePathItems || len(p.Instruction) > MaxReleasePathInstructionBytes {
		return false
	}
	// A record read back from a volume is still a record. The files it
	// names open the one hole in the path vocabulary, so a plan naming
	// anything that is not a plain workflow file reads as not ours rather
	// than having that name quietly dropped later.
	if len(p.WorkflowFiles) > MaxDeployWorkflowPaths ||
		len(NewWorkflowAllowance(p.WorkflowFiles).Files()) != len(p.WorkflowFiles) {
		return false
	}
	for _, item := range p.Items {
		if item.Name == "" || item.Kind == "" || !utf8.ValidString(item.Name+item.Kind+item.Detail+item.Means) {
			return false
		}
	}
	digest, err := p.digest()
	return err == nil && digest == p.RecordSHA256
}

func (p ReleasePathPlan) digest() (string, error) {
	p.RecordSHA256 = ""
	encoded, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// ReadReleasePathFile reads a sealed plan from an explicit path, for the
// command that renders the round's instruction. A record that is not ours
// is an error rather than an absence: the round is being rendered for what
// is in that file, and rendering without it would produce a plausible
// instruction that has lost half of what the round is for.
func ReadReleasePathFile(path string) (ReleasePathPlan, error) {
	var plan ReleasePathPlan
	if err := ReadJSONFile(path, MaxReleasePathJSONBytes, &plan); err != nil {
		return ReleasePathPlan{}, errors.New("the release path plan could not be read")
	}
	if !plan.Bound() {
		return ReleasePathPlan{}, errors.New("the release path plan is not one of ours")
	}
	return plan, nil
}

// BoundedReleasePathInstruction holds the instruction to what a prompt can
// carry, cutting on a rune boundary so what survives is still readable.
func BoundedReleasePathInstruction(text string) string {
	text = strings.ToValidUTF8(text, "")
	if len(text) <= MaxReleasePathInstructionBytes {
		return text
	}
	cut := MaxReleasePathInstructionBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

// WorkflowFileNames are the deploy workflow files a plan says the round
// will build, nil-safe for the ordinary caller that has no plan at all.
func (p *ReleasePathPlan) WorkflowFileNames() []string {
	if p == nil {
		return nil
	}
	return p.WorkflowFiles
}
