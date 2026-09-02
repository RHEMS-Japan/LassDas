package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// AgentReviewFromRun turns a reviewing agent's run into the sealed Review the
// rest of the chain already checks. The reviewer works in the repository rather
// than on the diff alone, so it can read whatever it needs to judge the change
// in context; what it reports is still held to the same shape as any review.
func AgentReviewFromRun(
	endpoint ModelEndpoint,
	run AgentRun,
	candidate Candidate,
	source SourceSnapshot,
	request TicketRequest,
	config Config,
	reviewedAt time.Time,
) (Review, error) {
	// The record must be this reviewer's own launch, sealed consistently: a
	// run naming any other configured agent — the implementer's above all —
	// is not this reviewer's judgment however valid its verdict reads. This
	// is where the profile binding is enforced, not merely checkable.
	if run.Validate(config) != nil || run.AgentID != config.Agents.ReviewerAgentFor(endpoint.ID).ID {
		return Review{}, errors.New("review run is not the reviewer's own launch")
	}
	output, err := DecodeAgentReviewOutput(run.Transcript)
	if err != nil {
		return Review{}, err
	}
	transcriptBytes := int32(len(run.Transcript)) // #nosec G115 -- bounded by MaxAgentTranscriptBytes.
	if transcriptBytes < 1 {
		transcriptBytes = 1
	}
	promptBytes := int32(run.PromptBytes) // #nosec G115 -- bounded by MaxAgentPromptBytes.
	// Measured bytes, not tokens: an agent run has no single token meter.
	invocation := InvocationUsage{
		RequestedModel: endpoint.Model, RequestID: run.RunSHA256, StopReason: ChatFinishStop,
		InputTokens: promptBytes, OutputTokens: transcriptBytes, TotalTokens: promptBytes + transcriptBytes,
		LatencyMillis: run.DurationMs,
	}
	return NewReview(candidate.Stage, endpoint, output, candidate, source, request, config, invocation, reviewedAt)
}

// ConfirmTreeMatchesCandidate checks that the working copy still holds exactly
// the change that was submitted for review. The reviewer is told to read only;
// this is what makes that instruction binding, so a reviewer cannot quietly
// fix what it was asked to judge.
//
// Only tracked files count as the tree here. A reviewer that runs the
// repository's own tests leaves untracked and ignored byproducts behind (a
// build cache, a config timestamp file), and those never reach the published
// change — it is built from the sealed candidate, not from this tree. The
// strict scan that refuses ignored files inside the writable scope protects
// the implementing run's deliverables and stays there; applied after a review
// it killed a run whose review had passed (RFDEV-674).
func ConfirmTreeMatchesCandidate(root string, candidate Candidate, consumer ConsumerConfig) error {
	inCandidate := make(map[string]bool, len(candidate.Files))
	for _, file := range candidate.Files {
		inCandidate[file.Path] = true
	}
	changed, err := trackedChangesUnder(root)
	if err != nil {
		return fmt.Errorf("the tree could not be read after review: %w", err)
	}
	for _, path := range changed {
		if !inCandidate[path] {
			return errors.New("the reviewing agent changed the tree: " + path)
		}
	}
	// Every submitted file must still hold its submitted content. This also
	// covers what the scan above cannot see: a tracked file quietly reverted
	// to its base content no longer shows as changed, and a submitted new
	// file is untracked.
	for _, file := range candidate.Files {
		filename, err := regularFileWithin(root, file.Path)
		if err != nil {
			return errors.New("the tree could not be read after review: " + file.Path)
		}
		actual, err := readTextFile(filename, consumer.Mode.MaxFileBytes)
		if err != nil {
			return errors.New("the tree could not be read after review: " + file.Path)
		}
		if string(actual) != file.Content {
			return errors.New("the reviewing agent changed the tree: " + file.Path)
		}
	}
	return nil
}

// RepositoryHead reads the working copy's HEAD commit. The review command
// records it before the reviewing agent runs and refuses a moved HEAD after:
// a reviewer that commits would make its tracked edits invisible to the
// status scan and shift what the next round's scan calls "changed".
func RepositoryHead(root string) (string, error) {
	output, err := gitOutput(root, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("repository head could not be read: %w", err)
	}
	head := strings.TrimSpace(output)
	if !commitPattern.MatchString(head) {
		return "", errors.New("repository head could not be read")
	}
	return head, nil
}

// CleanReviewByproducts removes what a reviewing agent left behind: every
// untracked or ignored path that is not part of the submitted candidate. The
// next round treats the tree as the implementer's work — the strict implement
// scan would either seal a reviewer's leftover into the next candidate or die
// on it as an ignored file inside the writable scope — so the leftovers must
// be gone before the round ends, not merely tolerated by the review check.
func CleanReviewByproducts(root string, candidate Candidate) error {
	inCandidate := make(map[string]bool, len(candidate.Files))
	for _, file := range candidate.Files {
		inCandidate[file.Path] = true
	}
	output, err := gitOutput(root, "status", "--porcelain=v1", "--untracked-files=all", "--ignored=matching", "-z")
	if err != nil {
		return fmt.Errorf("review byproducts could not be listed: %w", err)
	}
	entries := strings.Split(output, "\x00")
	for index := 0; index < len(entries); index++ {
		entry := entries[index]
		if len(entry) < 4 {
			continue
		}
		// A rename or copy carries its original path as a second bare record;
		// consume it so a pathological filename can never read as a status.
		if entry[0] == 'R' || entry[0] == 'C' || entry[1] == 'R' || entry[1] == 'C' {
			index++
			continue
		}
		if entry[:2] != "??" && entry[:2] != "!!" {
			continue
		}
		path := strings.TrimSuffix(entry[3:], "/")
		if inCandidate[path] {
			continue
		}
		// A reviewer that writes an ignore rule can make git collapse the
		// directory holding a submitted new file into one ignored entry;
		// deleting it would take the candidate's file with it.
		for _, file := range candidate.Files {
			if strings.HasPrefix(file.Path, path+"/") {
				return errors.New("review byproducts could not be removed: " + path + " holds " + file.Path)
			}
		}
		full, ok := byproductWithin(root, path)
		if !ok {
			return errors.New("review byproducts could not be removed: " + path)
		}
		if err := os.RemoveAll(full); err != nil {
			return errors.New("review byproducts could not be removed: " + path)
		}
		removeEmptyParents(root, full)
	}
	return nil
}

// removeEmptyParents deletes the directories a removed byproduct leaves
// empty, up to but never including root. A directory that still holds
// anything — a tracked file, the candidate's new file — refuses the
// removal, which is the stop condition.
func removeEmptyParents(root, full string) {
	base := filepath.Clean(root)
	for parent := filepath.Dir(full); parent != base && strings.HasPrefix(parent, base+string(filepath.Separator)); parent = filepath.Dir(parent) {
		if os.Remove(parent) != nil {
			return
		}
	}
}

// byproductWithin resolves a status path for deletion. Unlike the candidate
// path rules it must accept hidden names (an .eslintcache is exactly the
// leftover that would kill the next round), so it checks only what deletion
// needs: the path stays inside root, and no directory on the way is a
// symlink — RemoveAll on a path through a link would delete outside root.
// The final element may itself be a link; RemoveAll then removes the link.
func byproductWithin(root, path string) (string, bool) {
	if path == "" || strings.HasPrefix(path, "/") || strings.ContainsAny(path, "\\\r\n\x00") {
		return "", false
	}
	elements := strings.Split(path, "/")
	current := filepath.Clean(root)
	for index, element := range elements {
		if element == "" || element == "." || element == ".." {
			return "", false
		}
		current = filepath.Join(current, element)
		info, err := os.Lstat(current)
		if err != nil {
			return "", false
		}
		if index < len(elements)-1 && info.Mode()&os.ModeSymlink != 0 {
			return "", false
		}
	}
	return current, true
}

// trackedChangesUnder lists the tracked files the working copy has modified,
// as repository-relative paths. Untracked and ignored files are deliberately
// absent: after a review they are the reviewer's tooling byproducts, not part
// of the judged change.
func trackedChangesUnder(root string) ([]string, error) {
	output, err := gitOutput(root, "status", "--porcelain=v1", "--untracked-files=no", "-z")
	if err != nil {
		return nil, fmt.Errorf("changed files could not be read: %w", err)
	}
	changed := make([]string, 0, 8)
	entries := strings.Split(output, "\x00")
	for index := 0; index < len(entries); index++ {
		entry := entries[index]
		if len(entry) < 4 {
			continue
		}
		changed = append(changed, entry[3:])
		// A rename or copy is two records: the new path, then the original
		// path as its own bare entry (see ChangedFilesUnder).
		if entry[0] == 'R' || entry[0] == 'C' || entry[1] == 'R' || entry[1] == 'C' {
			index++
			if index >= len(entries) || entries[index] == "" {
				return nil, errors.New("changed files could not be read")
			}
			changed = append(changed, entries[index])
		}
	}
	sort.Strings(changed)
	return changed, nil
}

// ReviewAnswerRulesTail is the last line of the answer-format rules in the
// reviewing agent's instruction. The transcript usually echoes the whole
// instruction — including the verdict-shaped format examples — so the decoder
// uses this line as the boundary and only reads what the agent printed after
// it. Prompt builder and decoder must share the exact string for the boundary
// to hold.
const ReviewAnswerRulesTail = "- findings は 16 件までです。"

// DecodeAgentReviewOutput reads the verdict out of what the reviewing agent
// printed. A CLI agent writes prose and then its answer, so the last balanced
// JSON object in the output is taken as the verdict; anything else is a failed
// review rather than a guess. Text up to the echoed instruction's final rule
// line is ignored so a format example inside the instruction can never be
// mistaken for the verdict.
func DecodeAgentReviewOutput(transcript string) (ModelReviewOutput, error) {
	if len(transcript) > MaxAgentTranscriptBytes {
		return ModelReviewOutput{}, errors.New("transcript is too large")
	}
	if boundary := strings.LastIndex(transcript, ReviewAnswerRulesTail); boundary >= 0 {
		transcript = transcript[boundary+len(ReviewAnswerRulesTail):]
	}
	block, err := lastJSONObject(transcript)
	if err != nil {
		return ModelReviewOutput{}, errors.New("the reviewing agent did not report a verdict")
	}
	return DecodeModelReviewOutput([]byte(block))
}

// lastJSONObject returns the final balanced {...} run in the text, ignoring
// braces inside strings so prose containing braces cannot split an object.
func lastJSONObject(text string) (string, error) {
	if len(text) > MaxAgentTranscriptBytes {
		return "", errors.New("transcript is too large")
	}
	for end := strings.LastIndexByte(text, '}'); end >= 0; end = strings.LastIndexByte(text[:end], '}') {
		start, ok := matchingObjectStart(text[:end+1])
		if !ok {
			continue
		}
		block := text[start : end+1]
		var probe map[string]json.RawMessage
		if json.Unmarshal([]byte(block), &probe) != nil {
			continue
		}
		if _, carriesVerdict := probe["verdict"]; carriesVerdict {
			return block, nil
		}
	}
	return "", errors.New("no verdict object was printed")
}

// matchingObjectStart scans backwards from a closing brace at the end of text
// to the brace that opens it.
func matchingObjectStart(text string) (int, bool) {
	depth := 0
	inString := false
	for index := len(text) - 1; index >= 0; index-- {
		character := text[index]
		if inString {
			if character == '"' && !escapedAt(text, index) {
				inString = false
			}
			continue
		}
		switch character {
		case '"':
			inString = true
		case '}':
			depth++
		case '{':
			depth--
			if depth == 0 {
				return index, true
			}
		}
	}
	return 0, false
}

func escapedAt(text string, index int) bool {
	backslashes := 0
	for cursor := index - 1; cursor >= 0 && text[cursor] == '\\'; cursor-- {
		backslashes++
	}
	return backslashes%2 == 1
}
