package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/worker"
)

// A failed card leaves an exit code and nothing else. The exit code says the
// stage stopped; it cannot say whether the model would not answer, the volume
// filled up, or a binary was missing — and those three want three different
// remedies. So every card writes down which of them it was, beside the
// round's other records, before it returns.
//
// Nothing acts on the class yet. The attendant's classification, and every
// ending a run can have, are exactly what they were; the record is only
// evidence, and the one line the attendant logs about a failed card now says
// what kind of failure it was looking at.
//
// Measured 2026-09-25: a review card went twenty-six minutes without an
// answer and the run ended. From the outside that is indistinguishable from
// a missing binary or a full disk, because nothing wrote down which it was.

// FailureClass names the kind of thing that went wrong, in the vocabulary the
// remedies are organised by: a full volume is reclaimed, a missing tool is
// fetched, a model that will not answer is asked somewhere else.
type FailureClass string

const (
	// FailureClassModel covers a model that gave up, answered unusably, was
	// refused by its provider, or spent its allowance mid-turn.
	FailureClassModel FailureClass = "model"
	// FailureClassNetwork covers something that could not be reached: a
	// connection, a name, a certificate, a registry, a server answering 5xx.
	FailureClassNetwork FailureClass = "network"
	// FailureClassTool covers a binary the stage needs that is not there.
	FailureClassTool FailureClass = "tool"
	// FailureClassValidation covers the one failure the validate card owns:
	// the deterministic verification ran and refused the round. The change is
	// what is wrong, not the machinery that carried it.
	FailureClassValidation FailureClass = "validation"
	// FailureClassDisk covers a volume with no room left.
	FailureClassDisk FailureClass = "disk"
	// FailureClassUnknown is the honest answer when none of the above
	// recognised it. It is a class like the others, not an error: a record
	// that says "unknown" still says which stage and round stopped, and when.
	FailureClassUnknown FailureClass = "unknown"
)

// StageFailureSchemaVersion is this record's shape, alongside the round's
// other records.
const StageFailureSchemaVersion = 1

// maxStageFailureErrorBytes bounds the sentence a record keeps. The errors
// this seals are the pipeline's own one-liners; anything longer is not one.
const maxStageFailureErrorBytes = 1024

// StageFailure is one card's account of why it returned non-zero, sealed in
// the round it belongs to.
//
// Only the pipeline's own error text is kept. A failed step's output is read
// to classify the failure and is never copied in here: upstream text belongs
// in no record, for the same reason the model failure detail composes its
// phrase from this repository's own constants rather than quoting the wire.
type StageFailure struct {
	SchemaVersion int    `json:"schema_version"`
	DeliveryID    string `json:"delivery_id"`
	InputSHA256   string `json:"input_sha256"`
	ConfigSHA256  string `json:"config_sha256"`
	ToolSHA       string `json:"tool_sha"`
	// Stage and Round are where the failure happened, restated inside the
	// record: a record found at a path proves nothing about the path, and the
	// reader refuses one that does not name where it was found.
	Stage string       `json:"stage"`
	Round int          `json:"round"`
	Class FailureClass `json:"class"`
	// TerminalCode is the ending the delivery itself named, kept only by the
	// publish stage. Without it the difference between "the destination
	// refused the change" and "the machinery broke" survives nowhere: the
	// stage returns one exit code for both.
	TerminalCode string `json:"terminal_code,omitempty"`
	Error        string `json:"error"`
	// CardRunID is the dispatch this card ran as, when the dispatcher said.
	// Each re-dispatch gets a fresh one, so two failures of the same round can
	// be told apart.
	CardRunID     string    `json:"card_run_id,omitempty"`
	FailedAt      time.Time `json:"failed_at"`
	FailureSHA256 string    `json:"failure_sha256"`
}

// ErrValidationRejected is the validate card's own refusal: the deterministic
// verification ran to completion and would not pass the round. It is a
// sentinel rather than a sentence because the class turns on which branch the
// card took, not on words — the consumer's own test output goes past here,
// and a repository whose tests print "connection refused" must not turn its
// own red build into a network failure.
var ErrValidationRejected = errors.New("the deterministic validation rejected the round")

// verbFailure is one worker verb that ran and did not succeed. It keeps what
// the call sites used to drop: which verb it was, how it ended, and the tail
// of what it said. Without the cause, a binary that was never there and a
// model that would not answer arrive as the same sentence.
type verbFailure struct {
	verb   string
	code   int
	err    error
	stderr string
}

func (f *verbFailure) Error() string {
	if f.err != nil {
		return fmt.Sprintf("the %s step could not run: %v", f.verb, f.err)
	}
	return fmt.Sprintf("the %s step exited %d", f.verb, f.code)
}

func (f *verbFailure) Unwrap() error { return f.err }

// couldNotStart reports whether the step never ran at all. A step that ran
// and exited non-zero comes back as an exit code with no error, so an error
// here means the command could not be started — which is what a binary that
// is not on the image looks like from the inside.
func (f *verbFailure) couldNotStart() bool { return f != nil && f.err != nil }

// deliveryRefusal carries the delivery's own terminal code out of the publish
// card. The card can only exit non-zero, so the code has to ride the error to
// reach the record.
type deliveryRefusal struct {
	code hook.TerminalCode
	err  error
}

func (d *deliveryRefusal) Error() string {
	if d.err != nil {
		return fmt.Sprintf("delivery ended %s: %v", d.code, d.err)
	}
	return "delivery ended " + string(d.code)
}

func (d *deliveryRefusal) Unwrap() error { return d.err }

// runVerb runs one worker verb and returns nil, or a failure that keeps the
// cause. Every stage call site goes through it so that "did not finish" has
// something underneath it to read.
func (p *Pipeline) runVerb(ctx context.Context, name string, arguments []string, extraEnv ...string) error {
	code, err := p.worker(ctx, name, arguments, extraEnv...)
	if err == nil && code == 0 {
		return nil
	}
	// p.lastStepStderr belongs to the step that just returned, so the tail is
	// bound to this verb rather than to whatever ran most recently.
	return &verbFailure{verb: name, code: code, err: err, stderr: p.lastStepStderr}
}

// modelSpendingVerbs are the worker verbs that spend a model turn, whether
// through the gateway or through an agent launcher. A non-zero exit from one
// of them is a model failure even when it left nothing readable behind: the
// verb's whole job was the turn.
//
// The deterministic verbs are deliberately absent. A decide or a seal that
// exits non-zero is the machinery's own breakdown, and calling that "model"
// would send the remedy to the wrong place.
var modelSpendingVerbs = []string{
	"assess-readiness", "check-readiness", "decide-readiness",
	"investigate", "agent-design-review", "agent-review",
	"run-instruction", "impasse-question", "design-impasse-question",
	"compose-trail",
}

// diskMarkers, toolMarkers and networkMarkers are read against the failure's
// own text and the failed verb's output. They are lowercase and matched
// case-insensitively.
var (
	diskMarkers = []string{
		"no space left on device", "enospc", "disk quota exceeded",
	}
	toolMarkers = []string{
		"executable file not found", "command not found", "no such command",
	}
	networkMarkers = []string{
		"connection refused", "connection reset", "connection timed out",
		"no such host", "could not resolve host", "name resolution",
		"network is unreachable", "no route to host", "i/o timeout",
		"tls handshake", "x509:", "certificate verify", "certificate has expired",
		"failed to connect", "unable to access", "registry",
		"500 internal server error", "502 bad gateway", "503 service unavailable",
		"504 gateway", "bad gateway", "service unavailable",
	}
)

// classifyStageFailure decides which kind of thing went wrong.
//
// The order is the order of certainty, not the order of the ladder. A branch
// the card itself took beats a word found in text; an operating-system error
// carried in the chain beats a word too; the worker's own machine-readable
// account of a model turn beats the text around it. Only after all of those
// does a keyword decide anything, and a verb that spends a model turn is the
// last resort before "unknown".
func classifyStageFailure(err error) FailureClass {
	if err == nil {
		return FailureClassUnknown
	}
	if errors.Is(err, ErrValidationRejected) {
		return FailureClassValidation
	}
	var failed *verbFailure
	stderr := ""
	if errors.As(err, &failed) {
		stderr = failed.stderr
	}
	text := strings.ToLower(err.Error() + "\n" + stderr)
	switch {
	case errors.Is(err, syscall.ENOSPC) || containsAny(text, diskMarkers):
		return FailureClassDisk
	case errors.Is(err, exec.ErrNotFound) || containsAny(text, toolMarkers) ||
		(failed.couldNotStart() && errors.Is(err, fs.ErrNotExist)):
		// A bare name missing from the path reports ErrNotFound; a binary
		// named by its full path reports that the path is not there. Both are
		// the same absence, and the second is only read where the step never
		// started — an artifact that is missing says so somewhere else.
		return FailureClassTool
	case modelTurnGaveUp(stderr):
		// The worker says so itself, on its own stderr line: a provider that
		// refused, an answer that would not decode, an allowance spent. That
		// is a stronger statement than any word found in the surrounding text,
		// including a gateway's 5xx — the remedy for a provider that will not
		// answer is a different provider, not a different route.
		return FailureClassModel
	case containsAny(text, networkMarkers):
		return FailureClassNetwork
	case failed != nil && slices.Contains(modelSpendingVerbs, failed.verb):
		return FailureClassModel
	default:
		return FailureClassUnknown
	}
}

// modelTurnGaveUp reads the worker's machine-readable account of a model turn
// that ended badly. The line exists precisely so a reader outside the process
// can tell a model failure from anything else.
func modelTurnGaveUp(stderr string) bool {
	detail, ok := worker.ParseFailureDetailLine(stderr)
	if !ok {
		return false
	}
	return detail.ProviderErrors > 0 || detail.Malformed > 0 || detail.AllowanceSpent > 0 ||
		detail.Objection != "" || detail.Phrase != ""
}

func containsAny(text string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// StageFailureFile is where a card leaves its account: beside the round's
// other records, named for the stage that wrote it. Design stages keep design
// rounds, everything else keeps implementation rounds, which is the same
// split every other record in the run directory already observes.
func StageFailureFile(runDir, stage string, round int) string {
	directory := "stage"
	if runtime.IsDesignStage(stage) {
		directory = "design"
	}
	return filepath.Join(runDir, "history", fmt.Sprintf("%s-%d", directory, round), stage+"-failure.json")
}

// SealStageFailure writes why this card is about to return non-zero.
//
// Best-effort by design, like the trail: a record that cannot be written must
// never change what the card returns, because the exit code is what the
// kanban and the attendant already act on. A stage name that is not one of
// the chain's own is refused outright — it would otherwise choose the path.
func (p *Pipeline) SealStageFailure(stage string, failure error) {
	if failure == nil || !slices.Contains(runtime.AllStages(), stage) {
		return
	}
	round := p.failureRound(stage)
	if round < 1 {
		return
	}
	record := StageFailure{
		SchemaVersion: StageFailureSchemaVersion,
		ToolSHA:       p.Config.Identity.EngineSHA,
		Stage:         stage,
		Round:         round,
		Class:         classifyStageFailure(failure),
		Error:         boundedFailureText(failure.Error()),
		CardRunID:     cardRunID(),
		FailedAt:      time.Now().UTC(),
	}
	var refused *deliveryRefusal
	if errors.As(failure, &refused) {
		record.TerminalCode = string(refused.code)
	}
	// The same binding the round's other records carry, read from the draft
	// this run was prepared with. A record that cannot name its delivery is
	// still worth keeping: the class is the point, and the reader checks the
	// stage and the round it was asked for either way.
	record.DeliveryID, _ = p.readJSONField("ticket-draft.json", "delivery_id")
	record.InputSHA256, _ = p.readJSONField("ticket-draft.json", "input_sha256")
	record.ConfigSHA256, _ = p.readJSONField("ticket-draft.json", "config_sha256")
	digest, err := stageFailureDigest(record)
	if err != nil {
		return
	}
	record.FailureSHA256 = digest
	encoded, err := json.Marshal(record)
	if err != nil {
		return
	}
	path := StageFailureFile(p.Workspace, stage, round)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	// Removed before the write for the reason every other record here is: a
	// link left at the path must not carry the write somewhere else. A second
	// attempt at the same round overwrites, so the newest account wins.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return
	}
	_ = writeRecordAtomically(path, encoded)
}

// failureRound is the round a stage's failure belongs to — the same round the
// stage's own records go to, so the account sits beside them. Zero means
// there is no round to write into.
func (p *Pipeline) failureRound(stage string) int {
	switch stage {
	case runtime.StageValidate, runtime.StagePublish:
		// These two belong to the round that sealed the candidate, which does
		// not move when the decision lands; currentRound would have walked
		// past it.
		if round := p.latestCandidateRound(); round > 0 {
			return round
		}
		return p.currentRound()
	case runtime.StageDesignDecide:
		// The decide card seals the decision and then fails on what it says —
		// a revision, a disagreement that would not close. By then "the first
		// design round without a decision" is the next one, and the account
		// would land in a round nothing else has written to, where the tick
		// looking at the failed card would never find it.
		if round := p.LatestDesignRound(); round > 0 {
			return round
		}
		return p.currentDesignRound()
	case runtime.StageInvestigate, runtime.StageDesignReviewA, runtime.StageDesignReviewB:
		// These fail before any decision of their round exists, and the
		// investigation they belong to may itself be what failed to seal.
		return p.currentDesignRound()
	default:
		return p.currentRound()
	}
}

// ReadStageFailure reads back what a card said about its own failure. It
// reports false for a record that is missing, unreadable, or does not bind to
// the stage and round asked for: a run directory outlives its cards, and a
// stale account would explain the wrong failure.
func ReadStageFailure(runDir, stage string, round int) (StageFailure, bool) {
	if round < 1 || !slices.Contains(runtime.AllStages(), stage) {
		return StageFailure{}, false
	}
	encoded, err := readWorkspaceFile(StageFailureFile(runDir, stage, round), maxStageFailureRecordBytes)
	if err != nil {
		return StageFailure{}, false
	}
	var record StageFailure
	if json.Unmarshal(encoded, &record) != nil {
		return StageFailure{}, false
	}
	if record.SchemaVersion != StageFailureSchemaVersion || record.Stage != stage || record.Round != round {
		return StageFailure{}, false
	}
	digest, err := stageFailureDigest(record)
	if err != nil || digest != record.FailureSHA256 {
		return StageFailure{}, false
	}
	return record, true
}

// maxStageFailureRecordBytes bounds the read. The record is a handful of
// short fields; anything larger is not one of ours.
const maxStageFailureRecordBytes = 8 * 1024

func stageFailureDigest(record StageFailure) (string, error) {
	record.FailureSHA256 = ""
	encoded, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// boundedFailureText keeps the sentence short and valid. The write is not
// atomic against a pod that stops mid-record, so what this produces has to be
// something a reader can refuse cleanly rather than choke on.
func boundedFailureText(text string) string {
	text = strings.ToValidUTF8(strings.Map(func(r rune) rune {
		if r == '\x00' {
			return -1
		}
		return r
	}, text), "")
	if len(text) <= maxStageFailureErrorBytes {
		return text
	}
	cut := text[:maxStageFailureErrorBytes]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

// cardRunIDEnvironment is the dispatcher's per-attempt identity for the card
// this process is running as.
const cardRunIDEnvironment = "HERMES_KANBAN_RUN_ID"

// cardRunID reads that identity, or nothing when it is absent or not the
// number the dispatcher documents. A record never carries an unchecked
// environment value.
func cardRunID() string {
	value := os.Getenv(cardRunIDEnvironment)
	if value == "" || len(value) > 32 {
		return ""
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return value
}
