package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"time"
)

const (
	ValidationEvidenceSchemaVersion = 1
	ToolVersionCommandTimeout       = 30 * time.Second
)

type ValidationFileEvidence struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type ValidationEvidence struct {
	SchemaVersion    int                      `json:"schema_version"`
	DeliveryID       string                   `json:"delivery_id"`
	InputSHA256      string                   `json:"input_sha256"`
	ConfigSHA256     string                   `json:"config_sha256"`
	ToolSHA          string                   `json:"tool_sha"`
	SourceSHA256     string                   `json:"source_sha256"`
	CandidateSHA256  string                   `json:"candidate_sha256"`
	BaseSHA          string                   `json:"base_sha"`
	Stage            int                      `json:"stage"`
	Tools            []ObservedTool           `json:"tools"`
	Commands         [][]string               `json:"commands"`
	Files            []ValidationFileEvidence `json:"files"`
	StartedAt        time.Time                `json:"started_at"`
	CompletedAt      time.Time                `json:"completed_at"`
	ValidationSHA256 string                   `json:"validation_sha256"`
}

// RunValidationEvidence executes the configured deterministic commands in a
// fresh allowlisted environment and seals the exact candidate bytes that still
// exist after every command completes.
func RunValidationEvidence(
	ctx context.Context,
	repoRoot string,
	candidate Candidate,
	source SourceSnapshot,
	request TicketRequest,
	config Config,
) (ValidationEvidence, error) {
	if ctx == nil || candidate.Validate(source, request, config) != nil {
		return ValidationEvidence{}, errors.New("validation evidence input is invalid")
	}
	consumer, err := request.Consumer(config)
	if err != nil {
		return ValidationEvidence{}, errors.New("validation evidence input is invalid")
	}
	workingDirectory, err := validationWorkingDirectory(repoRoot, consumer.Mode.VerifyWorkingDirectory)
	if err != nil {
		return ValidationEvidence{}, errors.New("validation working directory is invalid")
	}
	if err := VerifyApplied(repoRoot, candidate, source, request, config); err != nil {
		return ValidationEvidence{}, errors.New("candidate is not applied before validation")
	}
	environment, cleanup, err := createValidationEnvironment(os.Environ())
	if err != nil {
		return ValidationEvidence{}, errors.New("validation environment is invalid")
	}
	defer cleanup()

	startedAt := time.Now().UTC()
	tools := make([]ObservedTool, 0, len(consumer.Mode.Toolchain))
	for _, tool := range consumer.Mode.Toolchain {
		observed, err := observedToolVersion(ctx, workingDirectory, environment, tool.Binary, tool.Version, tool.StripVPrefix)
		if err != nil {
			return ValidationEvidence{}, errors.New("tool version validation failed")
		}
		tools = append(tools, ObservedTool{Binary: tool.Binary, Version: observed})
	}
	if err := runValidationCommands(ctx, workingDirectory, consumer, ValidationCommandTimeout, environment); err != nil {
		return ValidationEvidence{}, err
	}
	if err := VerifyApplied(repoRoot, candidate, source, request, config); err != nil {
		return ValidationEvidence{}, errors.New("validation changed candidate bytes")
	}
	root, err := validatedApplyRoot(repoRoot, candidate, source, request, config)
	if err != nil {
		return ValidationEvidence{}, errors.New("validation root changed")
	}
	files := make([]ValidationFileEvidence, 0, len(candidate.Files))
	for _, file := range candidate.Files {
		_, _, content, err := currentBoundFile(root, file.Path, consumer.Mode.MaxFileBytes)
		if err != nil || string(content) != file.Content {
			return ValidationEvidence{}, errors.New("validated candidate bytes changed")
		}
		files = append(files, ValidationFileEvidence{Path: file.Path, SHA256: digestBytes(content)})
	}
	evidence := ValidationEvidence{
		SchemaVersion: ValidationEvidenceSchemaVersion,
		DeliveryID:    request.DeliveryID, InputSHA256: request.InputSHA256,
		ConfigSHA256: request.ConfigSHA256, ToolSHA: request.ToolSHA,
		SourceSHA256: source.SourceSHA256, CandidateSHA256: candidate.CandidateSHA256,
		BaseSHA: source.BaseSHA, Stage: candidate.Stage,
		Tools:    tools,
		Commands: expectedValidationCommands(consumer), Files: files,
		StartedAt: startedAt, CompletedAt: time.Now().UTC(),
	}
	digest, err := validationEvidenceDigest(evidence)
	if err != nil {
		return ValidationEvidence{}, errors.New("validation evidence could not be sealed")
	}
	evidence.ValidationSHA256 = digest
	if err := evidence.Validate(candidate, source, request, config); err != nil {
		return ValidationEvidence{}, err
	}
	return evidence, nil
}

func (e ValidationEvidence) Validate(candidate Candidate, source SourceSnapshot, request TicketRequest, config Config) error {
	consumer, err := request.Consumer(config)
	if err != nil {
		return errors.New("validation evidence identity is invalid")
	}
	if candidate.Validate(source, request, config) != nil || e.SchemaVersion != ValidationEvidenceSchemaVersion ||
		e.DeliveryID != request.DeliveryID || e.InputSHA256 != request.InputSHA256 ||
		e.ConfigSHA256 != request.ConfigSHA256 || e.ToolSHA != request.ToolSHA ||
		e.SourceSHA256 != source.SourceSHA256 || e.CandidateSHA256 != candidate.CandidateSHA256 ||
		e.BaseSHA != source.BaseSHA || e.Stage != candidate.Stage || !sha256Pattern.MatchString(e.ValidationSHA256) {
		return errors.New("validation evidence identity is invalid")
	}
	if e.StartedAt.IsZero() || e.CompletedAt.IsZero() || e.StartedAt.Location() != time.UTC || e.CompletedAt.Location() != time.UTC ||
		e.StartedAt.Add(allowedArtifactClockSkew).Before(candidate.GeneratedAt) || e.CompletedAt.Before(e.StartedAt) ||
		!equalCommands(e.Commands, expectedValidationCommands(consumer)) {
		return errors.New("validation evidence execution contract is invalid")
	}
	if len(e.Tools) != len(consumer.Mode.Toolchain) {
		return errors.New("validation evidence execution contract is invalid")
	}
	for index, tool := range consumer.Mode.Toolchain {
		if e.Tools[index].Binary != tool.Binary || !versionMatches(e.Tools[index].Version, tool.Version) {
			return errors.New("validation evidence execution contract is invalid")
		}
	}
	if len(e.Files) != len(candidate.Files) {
		return errors.New("validation evidence file set is invalid")
	}
	for index, file := range e.Files {
		candidateFile := candidate.Files[index]
		if file.Path != candidateFile.Path || file.SHA256 != digestBytes([]byte(candidateFile.Content)) {
			return errors.New("validation evidence file is invalid")
		}
	}
	digest, err := validationEvidenceDigest(e)
	if err != nil || digest != e.ValidationSHA256 {
		return errors.New("validation evidence digest is invalid")
	}
	return nil
}

// ValidatePublishGate is the deterministic publication gate. AI convergence
// alone is insufficient: the exact same candidate must also carry valid command
// evidence produced under the same config and tool revisions.
func ValidatePublishGate(
	decision StageDecision,
	validation ValidationEvidence,
	candidate Candidate,
	reviews []Review,
	source SourceSnapshot,
	request TicketRequest,
	config Config,
) error {
	if decision.Validate(candidate, reviews, source, request, config) != nil || decision.Outcome != "converged" {
		return errors.New("publish gate AI decision is invalid")
	}
	if validation.Validate(candidate, source, request, config) != nil {
		return errors.New("publish gate validation evidence is invalid")
	}
	return nil
}

// ObservedTool is one sealed binary version observation.
type ObservedTool struct {
	Binary  string `json:"binary"`
	Version string `json:"version"`
}

func observedToolVersion(ctx context.Context, directory string, environment []string, binary, expected string, allowV bool) (string, error) {
	output, err := runCredentialFreeCommand(ctx, directory, environment, ToolVersionCommandTimeout, 256, []string{binary, "--version"})
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(output))
	if allowV {
		value = strings.TrimPrefix(value, "v")
	}
	if strings.ContainsAny(value, "\r\n\x00") || !versionPattern.MatchString(value) || !versionMatches(value, expected) {
		return "", errors.New("tool version is invalid")
	}
	return value, nil
}

func versionMatches(actual, expected string) bool {
	if !versionPattern.MatchString(actual) || !versionPattern.MatchString(expected) {
		return false
	}
	actualParts := strings.Split(actual, ".")
	expectedParts := strings.Split(expected, ".")
	return len(actualParts) >= len(expectedParts) && slices.Equal(actualParts[:len(expectedParts)], expectedParts)
}

func expectedValidationCommands(consumer ConsumerConfig) [][]string {
	commands := make([][]string, 0, 1+len(consumer.Mode.VerifyCommands))
	commands = append(commands, append([]string(nil), consumer.Mode.InstallCommand...))
	for _, command := range consumer.Mode.VerifyCommands {
		commands = append(commands, append([]string(nil), command...))
	}
	return commands
}

func equalCommands(left, right [][]string) bool {
	return slices.EqualFunc(left, right, func(a, b []string) bool { return slices.Equal(a, b) })
}

func validationEvidenceDigest(evidence ValidationEvidence) (string, error) {
	evidence.ValidationSHA256 = ""
	encoded, err := json.Marshal(evidence)
	if err != nil {
		return "", err
	}
	return digestBytes(encoded), nil
}
