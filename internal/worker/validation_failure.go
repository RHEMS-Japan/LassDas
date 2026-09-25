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

// A round the judges passed can still be refused by the deterministic
// validation: the change reads correctly and does not survive the
// destination's own build and test commands. That used to be where the
// delivery ended — the run said validation had failed and stopped, and the
// only copy of what the commands printed was the pod's log, for a person to
// find and carry back by hand.
//
// It no longer ends there. What the validation printed is the one thing the
// next round needs, so the card that saw it seals it in the round it refused,
// and the next round's instruction carries it the way it already carries the
// judges' objections.
//
// The text is untrusted. It is whatever the destination's own commands
// printed while running code an agent wrote, so it travels to the agent that
// has to fix it and nowhere else — never into a comment on the tracker, which
// is the line this output has observed since it was first captured.

// ValidationFailureSchemaVersion is this record's shape, alongside the
// round's other records.
const ValidationFailureSchemaVersion = 1

// MaxValidationOutputBytes bounds the output one record carries.
//
// Four kibibytes is not a choice about how much is useful; it is the whole of
// what exists. The command's own output is kept as a 4 KiB tail, and the step
// that ran it keeps a 4 KiB tail of that, so nothing longer ever reaches
// here. Stated as a bound anyway, because the record is read back in another
// process and a reader that trusts an upstream promise is a reader that
// breaks when the promise moves.
//
// It also has to fit in an instruction. The whole prompt is bounded at
// MaxAgentPromptBytes and the previous round's objections already claim a
// quarter of it, so this much more leaves the change itself the room it needs.
const MaxValidationOutputBytes = 4 * 1024

// MaxValidationFailureJSONBytes bounds the read of a sealed record. The
// record is the bounded output and a handful of short fields; the slack above
// the output covers JSON escaping of text that is mostly quotes and newlines.
const MaxValidationFailureJSONBytes int64 = 64 * 1024

// ValidationFailure is one round's account of the deterministic validation
// refusing it.
type ValidationFailure struct {
	SchemaVersion int    `json:"schema_version"`
	DeliveryID    string `json:"delivery_id,omitempty"`
	InputSHA256   string `json:"input_sha256,omitempty"`
	ConfigSHA256  string `json:"config_sha256,omitempty"`
	ToolSHA       string `json:"tool_sha,omitempty"`
	// Round is the round that was refused, restated inside the record: a
	// record found at a path proves nothing about the path, and a run
	// directory outlives its cards, so a stale account would explain the
	// wrong round's failure to the round that follows it.
	Round int `json:"round"`
	// Step names which of the deterministic steps refused — applying the
	// change to a fresh copy, running the destination's own commands,
	// checking the applied tree, or the publish gate. Without it the output
	// arrives with no idea what produced it, and the four want different
	// fixes.
	Step string `json:"step"`
	// Output is what that step printed: the end of it, bounded, valid UTF-8.
	Output string `json:"output"`
	// OutputSHA256 is the digest of the normalised output, which is the whole
	// reason the digest is a field rather than something recomputed later.
	// Two rounds in a row that print the same thing are a run repeating
	// itself, and noticing that is what keeps "do not stop" from becoming "do
	// the same thing forever"; comparing the texts instead would mean keeping
	// every round's output alive to compare against.
	OutputSHA256 string    `json:"output_sha256"`
	FailedAt     time.Time `json:"failed_at"`
	RecordSHA256 string    `json:"record_sha256"`
}

// NewValidationFailure builds the record for one refused round. The caller
// adds the bindings the round's other records carry and then seals it.
func NewValidationFailure(round int, step, output string) ValidationFailure {
	bounded := boundedValidationOutput(output)
	return ValidationFailure{
		SchemaVersion: ValidationFailureSchemaVersion,
		Round:         round,
		Step:          step,
		Output:        bounded,
		OutputSHA256:  validationOutputDigest(bounded),
		FailedAt:      time.Now().UTC(),
	}
}

// Seal fills in the digest over everything else the record says, so a reader
// in another process can tell a record from a half-written one.
func (v *ValidationFailure) Seal() error {
	digest, err := v.digest()
	if err != nil {
		return err
	}
	v.RecordSHA256 = digest
	return nil
}

// Bound reports whether a record read back is one of ours, intact, and about
// the round asked for. Anything else is reported as not ours rather than
// repaired: the round after this one is about to be told what is in here.
func (v ValidationFailure) Bound(round int) bool {
	if v.SchemaVersion != ValidationFailureSchemaVersion || v.Round != round || v.Round < 1 {
		return false
	}
	if v.Step == "" || v.OutputSHA256 != validationOutputDigest(v.Output) {
		return false
	}
	digest, err := v.digest()
	return err == nil && digest == v.RecordSHA256
}

func (v ValidationFailure) digest() (string, error) {
	v.RecordSHA256 = ""
	encoded, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// ReadValidationFailureFile reads a sealed record from an explicit path, for
// the command that renders the next round's instruction. It checks the shape
// and the digest; the round it belongs to is checked by whoever knows which
// round they asked for.
func ReadValidationFailureFile(path string) (ValidationFailure, error) {
	var record ValidationFailure
	if err := ReadJSONFile(path, MaxValidationFailureJSONBytes, &record); err != nil {
		return ValidationFailure{}, errors.New("the validation failure record could not be read")
	}
	if !record.Bound(record.Round) {
		return ValidationFailure{}, errors.New("the validation failure record is not one of ours")
	}
	return record, nil
}

// validationOutputDigest is taken over the normalised text, never the raw
// bytes, so the comparison it exists for survives a build runner that pads a
// line differently between two invocations of the same failure.
func validationOutputDigest(output string) string {
	sum := sha256.Sum256([]byte(normalizeValidationOutput(output)))
	return hex.EncodeToString(sum[:])
}

// normalizeValidationOutput is what the digest is taken over.
//
// Deliberately shallow: line endings, the trailing blanks on each line, and
// the blank lines at either end. It does not try to erase elapsed times,
// temporary paths or process ids, and that direction is chosen on purpose.
// Erasing them by guesswork would make two different failures read as the
// same one, and a run told it is repeating itself when it is not would be
// sent to be arbitrated over a problem it was actually still working
// through. Missing a repeat costs another round of a run that is already
// running; inventing one costs the round that would have fixed it.
func normalizeValidationOutput(output string) string {
	output = strings.ReplaceAll(strings.ToValidUTF8(output, ""), "\r\n", "\n")
	output = strings.ReplaceAll(output, "\r", "\n")
	lines := strings.Split(output, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	return strings.Trim(strings.Join(lines, "\n"), "\n")
}

// boundedValidationOutput keeps the end of what the step printed, within the
// budget and on whole characters.
//
// The end, because a build or test runner puts its verdict there — the same
// reason both tails upstream of here are tails. Whole characters, because
// what arrives has already been cut at a byte boundary twice, so its first
// character is very often half of one; a broken sequence would travel into a
// prompt and into every digest taken of the text around it. The bytes that
// carry no text at all go too: a record is read back by a decoder, and one
// that has to be told how to handle a NUL is one more thing to get wrong.
func boundedValidationOutput(output string) string {
	output = strings.ToValidUTF8(strings.Map(func(r rune) rune {
		if r == '\x00' {
			return -1
		}
		return r
	}, output), "")
	if len(output) <= MaxValidationOutputBytes {
		return output
	}
	cut := output[len(output)-MaxValidationOutputBytes:]
	for len(cut) > 0 && !utf8.RuneStart(cut[0]) {
		cut = cut[1:]
	}
	return cut
}
