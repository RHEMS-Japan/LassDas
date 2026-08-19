package worker

import (
	"errors"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// AgentRun is the sealed record of one implementing agent run: what it was
// asked, what it changed, and what it said it did. It is bound to the ticket
// and the exact base revision, so a replay can be checked against it.
type AgentRun struct {
	SchemaVersion int       `json:"schema_version"`
	Stage         int       `json:"stage"`
	DeliveryID    string    `json:"delivery_id"`
	InputSHA256   string    `json:"input_sha256"`
	ConfigSHA256  string    `json:"config_sha256"`
	ToolSHA       string    `json:"tool_sha"`
	BaseSHA       string    `json:"base_sha"`
	AgentID       string    `json:"agent_id"`
	Command       string    `json:"command"`
	PromptBytes   int       `json:"prompt_bytes"`
	ExitCode      int       `json:"exit_code"`
	DurationMs    int64     `json:"duration_ms"`
	ChangedFiles  []string  `json:"changed_files"`
	Transcript    string    `json:"transcript"`
	RanAt         time.Time `json:"ran_at"`
	RunSHA256     string    `json:"run_sha256"`
}

// SealAgentRun computes the digest that binds a run record to its contents.
func SealAgentRun(run AgentRun) (AgentRun, error) {
	run.RunSHA256 = ""
	digest, err := sealedDigest(run)
	if err != nil {
		return AgentRun{}, errors.New("agent run could not be sealed")
	}
	run.RunSHA256 = digest
	return run, nil
}

func (r AgentRun) Validate(config Config) error {
	configSHA, err := config.SHA256()
	if err != nil {
		return errors.New("worker configuration is invalid")
	}
	agent, err := config.Agents.byID(r.AgentID)
	if err != nil {
		return err
	}
	if r.SchemaVersion != ArtifactSchemaVersion || r.Stage < 1 || r.Stage > config.MaxStages ||
		!deliveryPattern.MatchString(r.DeliveryID) || !sha256Pattern.MatchString(r.InputSHA256) ||
		r.ConfigSHA256 != configSHA || !ValidToolSHA(r.ToolSHA) || !commitPattern.MatchString(r.BaseSHA) ||
		r.Command != agent.Command ||
		r.PromptBytes < 1 || r.PromptBytes > MaxAgentPromptBytes ||
		r.DurationMs < 0 || r.RanAt.IsZero() || r.RanAt.Location() != time.UTC ||
		!sha256Pattern.MatchString(r.RunSHA256) {
		return errors.New("agent run identity is invalid")
	}
	// Only the implementing run is required to have changed something. The
	// reviewing run reads and reports; it is expected to leave the tree alone.
	if r.AgentID == config.Agents.Implementer.ID && len(r.ChangedFiles) == 0 {
		return errors.New("agent run changed no files")
	}
	if !sort.StringsAreSorted(r.ChangedFiles) {
		return errors.New("agent run changed files are not ordered")
	}
	if len(r.Transcript) > MaxAgentTranscriptBytes || !utf8.ValidString(r.Transcript) {
		return errors.New("agent run transcript is invalid")
	}
	unsealed := r
	unsealed.RunSHA256 = ""
	digest, err := sealedDigest(unsealed)
	if err != nil || digest != r.RunSHA256 {
		return errors.New("agent run digest is invalid")
	}
	return nil
}

// ObservedChange is one file the agent changed, with the bytes on both sides.
// The before-bytes come from the sealed base revision and the after-bytes from
// the working copy, so neither is taken from the agent's word.
type ObservedChange struct {
	Path    string
	Before  []byte
	After   []byte
	Created bool
}

// ReadObservedChanges reads both sides of every file the agent changed. The
// before-bytes come from an untouched copy of the base, which the agent never
// had access to, so what a change started from is not taken from the agent.
// A file the agent created has no before-side: it is carried as Created with
// empty before-bytes, and every later verifier holds the path to be absent
// from the base. The first live migration ticket measurably needed this -
// adding a numbered SQL file is ordinary development, and rejecting creation
// outright made that ticket impossible.
func ReadObservedChanges(workspace, base string, changed []string, consumer ConsumerConfig) ([]ObservedChange, error) {
	if len(changed) == 0 {
		return nil, errors.New("the agent changed nothing")
	}
	if len(changed) > consumer.Mode.MaxFiles {
		return nil, errors.New("the agent changed more files than this destination allows")
	}
	observed := make([]ObservedChange, 0, len(changed))
	for _, path := range changed {
		if !allowedPath(path, consumer.Mode.AllowedFilePrefixes) {
			return nil, errors.New("the agent changed a file outside the writable scope")
		}
		created := false
		var before []byte
		if beforeName, err := regularFileWithin(base, path); err != nil {
			created = true
		} else if before, err = readTextFile(beforeName, consumer.Mode.MaxFileBytes); err != nil {
			return nil, errors.New("the file this change started from could not be read: " + path)
		}
		afterName, err := regularFileWithin(workspace, path)
		if err != nil {
			return nil, errors.New("a changed file is not addressable: " + path)
		}
		after, err := readTextFile(afterName, consumer.Mode.MaxFileBytes)
		if err != nil {
			return nil, errors.New("a changed file could not be read back: " + path)
		}
		observed = append(observed, ObservedChange{Path: path, Before: before, After: after, Created: created})
	}
	return observed, nil
}

// TicketWithObservedTargets completes the contract from what the agent
// actually changed. Which files a change touches is discovered by working in
// the repository, not declared in advance, so the target set is sealed here.
func TicketWithObservedTargets(draft TicketDraft, observed []ObservedChange, config Config) (TicketRequest, error) {
	paths := make([]string, 0, len(observed))
	for _, change := range observed {
		paths = append(paths, change.Path)
	}
	sort.Strings(paths)
	return draft.WithTargetFiles(paths, config)
}

// SourceFromObservedChanges seals the before-bytes as the source snapshot the
// rest of the chain is written against.
func SourceFromObservedChanges(baseSHA string, observed []ObservedChange, request TicketRequest, config Config) (SourceSnapshot, error) {
	consumer, err := request.Consumer(config)
	if err != nil {
		return SourceSnapshot{}, errors.New("source request is invalid")
	}
	if !commitPattern.MatchString(baseSHA) || len(observed) != len(request.TargetFiles) {
		return SourceSnapshot{}, errors.New("observed changes do not match the ticket")
	}
	files := make([]SourceFile, 0, len(observed))
	for index, change := range observed {
		if change.Path != request.TargetFiles[index] {
			return SourceSnapshot{}, errors.New("observed changes do not match the ticket")
		}
		files = append(files, SourceFile{
			Path: change.Path, GitBlobSHA: gitBlobDigest(change.Before),
			SHA256: digestBytes(change.Before), Content: string(change.Before),
			Created: change.Created,
		})
	}
	snapshot := SourceSnapshot{
		SchemaVersion: ArtifactSchemaVersion, DeliveryID: request.DeliveryID, InputSHA256: request.InputSHA256,
		ConfigSHA256: request.ConfigSHA256, ToolSHA: request.ToolSHA,
		Repository: consumer.Repository, RepositoryID: consumer.RepositoryID,
		BaseBranch: consumer.IntegrationBranch, BaseSHA: baseSHA, Files: files,
	}
	digest, err := sourceDigest(snapshot)
	if err != nil {
		return SourceSnapshot{}, errors.New("source snapshot could not be sealed")
	}
	snapshot.SourceSHA256 = digest
	if err := snapshot.Validate(request, config); err != nil {
		return SourceSnapshot{}, err
	}
	return snapshot, nil
}

// CandidateFromObservedChanges seals the after-bytes as the candidate. It goes
// through exactly the checks a model-authored candidate goes through, so the
// budget, the forbidden text and the acceptance wording are enforced the same
// way whoever produced the change.
func CandidateFromObservedChanges(
	stage int,
	observed []ObservedChange,
	source SourceSnapshot,
	request TicketRequest,
	config Config,
	run AgentRun,
	generatedAt time.Time,
) (Candidate, error) {
	if err := source.Validate(request, config); err != nil || stage < 1 || stage > config.MaxStages {
		return Candidate{}, errors.New("candidate input is invalid")
	}
	if run.Validate(config) != nil || run.Stage != stage {
		return Candidate{}, errors.New("candidate agent run is invalid")
	}
	if generatedAt.IsZero() || generatedAt.Location() != time.UTC {
		return Candidate{}, errors.New("candidate time is invalid")
	}
	consumer, err := request.Consumer(config)
	if err != nil {
		return Candidate{}, errors.New("candidate input is invalid")
	}
	files := make([]CandidateFile, 0, len(observed))
	for index, change := range observed {
		if change.Path != source.Files[index].Path {
			return Candidate{}, errors.New("candidate file set is invalid")
		}
		content := string(change.After)
		if !utf8.ValidString(content) || strings.ContainsRune(content, '\x00') ||
			len(content) > consumer.Mode.MaxFileBytes {
			return Candidate{}, errors.New("a changed file is not valid text")
		}
		files = append(files, CandidateFile{
			Path: change.Path, BeforeSHA256: source.Files[index].SHA256, Content: content,
		})
	}
	candidate := Candidate{
		SchemaVersion: ArtifactSchemaVersion, Stage: stage, DeliveryID: request.DeliveryID,
		InputSHA256: request.InputSHA256, ConfigSHA256: request.ConfigSHA256, ToolSHA: request.ToolSHA,
		SourceSHA256: source.SourceSHA256, BaseSHA: source.BaseSHA,
		Implementer: config.Models.Implementer,
		Invocation:  agentInvocation(config, run),
		GeneratedAt: generatedAt, Files: files, Rationale: agentRationale(run),
	}
	digest, err := candidateDigest(candidate)
	if err != nil {
		return Candidate{}, errors.New("candidate could not be sealed")
	}
	candidate.CandidateSHA256 = digest
	if err := candidate.Validate(source, request, config); err != nil {
		return Candidate{}, err
	}
	return candidate, nil
}

// agentInvocation records the run in the shape the rest of the chain already
// checks. An agent run is many model calls with no single token meter, so the
// counts here are measured bytes: the instruction in, the transcript out. The
// floor of one keeps a silent agent distinguishable from a missing record.
func agentInvocation(config Config, run AgentRun) InvocationUsage {
	output := int32(len(run.Transcript)) // #nosec G115 -- bounded by MaxAgentTranscriptBytes.
	if output < 1 {
		output = 1
	}
	prompt := int32(run.PromptBytes) // #nosec G115 -- bounded by MaxAgentPromptBytes.
	return InvocationUsage{
		RequestedModel: config.Models.Implementer.Model,
		RequestID:      run.RunSHA256,
		StopReason:     ChatFinishStop,
		InputTokens:    prompt, OutputTokens: output, TotalTokens: prompt + output,
		LatencyMillis: run.DurationMs,
	}
}

func agentRationale(run AgentRun) string {
	rationale := strings.TrimSpace(run.Transcript)
	if rationale == "" {
		rationale = "The agent reported nothing."
	}
	// The transcript is the agent's own words and may carry anything; the
	// rationale is validated plain text, so control characters are dropped
	// rather than allowed to fail the seal. Cleaning runs before the byte
	// budget: a tail cut through the middle of a multi-byte character used
	// to leave broken lead bytes that this cleaning swelled into three-byte
	// replacement runes, pushing the result past the budget it had just
	// been cut to - a 4,425-byte Japanese completion report failed its
	// whole candidate that way on a live run.
	cleaned := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f || !utf8.ValidRune(r) {
			return -1
		}
		return r
	}, rationale)
	cleaned = strings.TrimSpace(cleaned)
	if len(cleaned) > 4096 {
		cut := cleaned[len(cleaned)-4096:]
		for len(cut) > 0 && !utf8.RuneStart(cut[0]) {
			cut = cut[1:]
		}
		cleaned = strings.TrimSpace(cut)
	}
	if cleaned == "" {
		return "The agent reported nothing readable."
	}
	return cleaned
}
