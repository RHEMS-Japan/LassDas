package worker

import (
	"bytes"
	"crypto/sha1" // #nosec G505 -- GitHub's Git object identity is SHA-1 for this repository.
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	ArtifactSchemaVersion    = 1
	allowedArtifactClockSkew = 5 * time.Minute
)

type SourceSnapshot struct {
	SchemaVersion int          `json:"schema_version"`
	DeliveryID    string       `json:"delivery_id"`
	InputSHA256   string       `json:"input_sha256"`
	ConfigSHA256  string       `json:"config_sha256"`
	ToolSHA       string       `json:"tool_sha"`
	Repository    string       `json:"repository"`
	RepositoryID  int64        `json:"repository_id"`
	BaseBranch    string       `json:"base_branch"`
	BaseSHA       string       `json:"base_sha"`
	Files         []SourceFile `json:"files"`
	SourceSHA256  string       `json:"source_sha256"`
}

type SourceFile struct {
	Path       string `json:"path"`
	GitBlobSHA string `json:"git_blob_sha"`
	SHA256     string `json:"sha256"`
	Content    string `json:"content"`
}

type ModelCandidateOutput struct {
	Files     []ModelCandidateFile `json:"files"`
	Rationale string               `json:"rationale"`
}

type ModelCandidateFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type Candidate struct {
	SchemaVersion   int             `json:"schema_version"`
	Stage           int             `json:"stage"`
	DeliveryID      string          `json:"delivery_id"`
	InputSHA256     string          `json:"input_sha256"`
	ConfigSHA256    string          `json:"config_sha256"`
	ToolSHA         string          `json:"tool_sha"`
	SourceSHA256    string          `json:"source_sha256"`
	BaseSHA         string          `json:"base_sha"`
	Implementer     ModelEndpoint   `json:"implementer"`
	Invocation      InvocationUsage `json:"invocation"`
	GeneratedAt     time.Time       `json:"generated_at"`
	Files           []CandidateFile `json:"files"`
	Rationale       string          `json:"rationale"`
	CandidateSHA256 string          `json:"candidate_sha256"`
}

type CandidateFile struct {
	Path         string `json:"path"`
	BeforeSHA256 string `json:"before_sha256"`
	Content      string `json:"content"`
}

type ModelReviewOutput struct {
	Verdict  string         `json:"verdict"`
	Findings []ModelFinding `json:"findings"`
}

type ModelFinding struct {
	Code    string `json:"code"`
	Path    string `json:"path"`
	Line    int    `json:"line,omitempty"`
	Message string `json:"message"`
}

type Review struct {
	SchemaVersion    int             `json:"schema_version"`
	Stage            int             `json:"stage"`
	DeliveryID       string          `json:"delivery_id"`
	ConfigSHA256     string          `json:"config_sha256"`
	ToolSHA          string          `json:"tool_sha"`
	CandidateSHA256  string          `json:"candidate_sha256"`
	ReviewerID       string          `json:"reviewer_id"`
	Vendor           string          `json:"vendor"`
	Model            string          `json:"model"`
	BaseURL          string          `json:"base_url"`
	Lens             string          `json:"lens"`
	Effort           string          `json:"effort,omitempty"`
	StructuredOutput bool            `json:"structured_output"`
	MaxOutputTokens  int32           `json:"max_output_tokens"`
	Verdict          string          `json:"verdict"`
	Findings         []ModelFinding  `json:"findings"`
	Invocation       InvocationUsage `json:"invocation"`
	ReviewedAt       time.Time       `json:"reviewed_at"`
	ReviewSHA256     string          `json:"review_sha256"`
}

// StageDecision records AI consensus only. A converged stage is never
// publication authority on its own; callers must additionally validate
// deterministic ValidationEvidence with ValidatePublishGate.
type StageDecision struct {
	SchemaVersion   int      `json:"schema_version"`
	Stage           int      `json:"stage"`
	DeliveryID      string   `json:"delivery_id"`
	ConfigSHA256    string   `json:"config_sha256"`
	ToolSHA         string   `json:"tool_sha"`
	CandidateSHA256 string   `json:"candidate_sha256"`
	Outcome         string   `json:"outcome"`
	ReviewSHA256s   []string `json:"review_sha256s"`
	DecisionSHA256  string   `json:"decision_sha256"`
}

func ReadSourceSnapshot(repoRoot, baseSHA string, request TicketRequest, config Config) (SourceSnapshot, error) {
	if err := request.Validate(config); err != nil || !commitPattern.MatchString(baseSHA) {
		return SourceSnapshot{}, errors.New("source request is invalid")
	}
	consumer, err := request.Consumer(config)
	if err != nil {
		return SourceSnapshot{}, errors.New("source request is invalid")
	}
	root, err := filepath.Abs(repoRoot)
	if err != nil || filepath.Clean(root) != root {
		return SourceSnapshot{}, errors.New("source root is invalid")
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return SourceSnapshot{}, errors.New("source root is invalid")
	}

	files := make([]SourceFile, 0, len(request.TargetFiles))
	for _, name := range request.TargetFiles {
		filename, err := regularFileWithin(root, name)
		if err != nil {
			return SourceSnapshot{}, fmt.Errorf("source file %q is invalid", name)
		}
		content, err := readTextFile(filename, consumer.Mode.MaxFileBytes)
		if err != nil {
			return SourceSnapshot{}, fmt.Errorf("source file %q is invalid", name)
		}
		files = append(files, SourceFile{Path: name, GitBlobSHA: gitBlobDigest(content), SHA256: digestBytes(content), Content: string(content)})
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
		return SourceSnapshot{}, errors.New("source snapshot is invalid")
	}
	return snapshot, nil
}

func (s SourceSnapshot) Validate(request TicketRequest, config Config) error {
	if err := request.Validate(config); err != nil {
		return errors.New("ticket request is invalid")
	}
	consumer, err := request.Consumer(config)
	if err != nil {
		return errors.New("ticket request is invalid")
	}
	if s.SchemaVersion != ArtifactSchemaVersion || s.DeliveryID != request.DeliveryID || s.InputSHA256 != request.InputSHA256 ||
		s.ConfigSHA256 != request.ConfigSHA256 || s.ToolSHA != request.ToolSHA ||
		s.Repository != consumer.Repository || s.RepositoryID != consumer.RepositoryID ||
		s.BaseBranch != consumer.IntegrationBranch || !commitPattern.MatchString(s.BaseSHA) || !sha256Pattern.MatchString(s.SourceSHA256) {
		return errors.New("source identity is invalid")
	}
	if len(s.Files) != len(request.TargetFiles) {
		return errors.New("source file set is invalid")
	}
	total := 0
	absentFound := false
	for index, file := range s.Files {
		if file.Path != request.TargetFiles[index] || !commitPattern.MatchString(file.GitBlobSHA) || !sha256Pattern.MatchString(file.SHA256) ||
			!utf8.ValidString(file.Content) || strings.ContainsRune(file.Content, '\x00') || len(file.Content) > consumer.Mode.MaxFileBytes ||
			digestBytes([]byte(file.Content)) != file.SHA256 || gitBlobDigest([]byte(file.Content)) != file.GitBlobSHA {
			return errors.New("source file is invalid")
		}
		if request.HasWordingPromise() {
			if strings.Contains(file.Content, request.ExpectedText) {
				return errors.New("source already contains expected acceptance text")
			}
			absentFound = absentFound || strings.Contains(file.Content, request.AbsentText)
		}
		total += len(file.Content)
	}
	if request.HasWordingPromise() && !absentFound {
		return errors.New("source does not contain absent acceptance text")
	}
	if total > consumer.Mode.MaxTotalBytes {
		return errors.New("source file total is invalid")
	}
	digest, err := sourceDigest(s)
	if err != nil || digest != s.SourceSHA256 {
		return errors.New("source digest is invalid")
	}
	return nil
}

func NewCandidate(stage int, output ModelCandidateOutput, source SourceSnapshot, request TicketRequest, config Config, invocation InvocationUsage, generatedAt time.Time) (Candidate, error) {
	if err := source.Validate(request, config); err != nil || stage < 1 || stage > config.MaxStages {
		return Candidate{}, errors.New("candidate input is invalid")
	}
	if invocation.Validate(config.Models.Implementer) != nil || generatedAt.IsZero() || generatedAt.Location() != time.UTC {
		return Candidate{}, errors.New("candidate invocation is invalid")
	}
	if err := validateModelCandidateOutput(output, request, config); err != nil {
		return Candidate{}, err
	}
	sourceByPath := make(map[string]SourceFile, len(source.Files))
	for _, file := range source.Files {
		sourceByPath[file.Path] = file
	}
	files := make([]CandidateFile, 0, len(output.Files))
	for _, file := range output.Files {
		files = append(files, CandidateFile{Path: file.Path, BeforeSHA256: sourceByPath[file.Path].SHA256, Content: file.Content})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	candidate := Candidate{
		SchemaVersion: ArtifactSchemaVersion, Stage: stage, DeliveryID: request.DeliveryID,
		InputSHA256: request.InputSHA256, ConfigSHA256: request.ConfigSHA256, ToolSHA: request.ToolSHA,
		SourceSHA256: source.SourceSHA256, BaseSHA: source.BaseSHA,
		Implementer: config.Models.Implementer, Invocation: invocation, GeneratedAt: generatedAt,
		Files: files, Rationale: output.Rationale,
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

func (c Candidate) Validate(source SourceSnapshot, request TicketRequest, config Config) error {
	if err := source.Validate(request, config); err != nil {
		return errors.New("source snapshot is invalid")
	}
	consumer, err := request.Consumer(config)
	if err != nil {
		return errors.New("ticket request is invalid")
	}
	if c.SchemaVersion != ArtifactSchemaVersion || c.Stage < 1 || c.Stage > config.MaxStages ||
		c.DeliveryID != request.DeliveryID || c.InputSHA256 != request.InputSHA256 ||
		c.ConfigSHA256 != request.ConfigSHA256 || c.ToolSHA != request.ToolSHA ||
		c.SourceSHA256 != source.SourceSHA256 || c.BaseSHA != source.BaseSHA || c.Implementer != config.Models.Implementer ||
		c.Invocation.Validate(c.Implementer) != nil || c.GeneratedAt.IsZero() || c.GeneratedAt.Location() != time.UTC ||
		!sha256Pattern.MatchString(c.CandidateSHA256) {
		return errors.New("candidate identity is invalid")
	}
	if validatePlainText(c.Rationale, 4096, true) != nil || len(c.Files) != len(source.Files) {
		return errors.New("candidate content is invalid")
	}
	changed := false
	total := 0
	changedLines := 0
	changedBytes := 0
	heaviestLines := 0
	heaviestPath := ""
	anyFallback := false
	expectedFound := false
	for index, file := range c.Files {
		base := source.Files[index]
		if file.Path != base.Path || file.BeforeSHA256 != base.SHA256 || !utf8.ValidString(file.Content) ||
			strings.ContainsRune(file.Content, '\x00') || len(file.Content) > consumer.Mode.MaxFileBytes {
			return errors.New("candidate file is invalid")
		}
		if file.Content != base.Content {
			changed = true
			lines, bytes, fallback := conservativeChangeBudget(base.Content, file.Content)
			changedLines += lines
			changedBytes += bytes
			anyFallback = anyFallback || fallback
			if lines > heaviestLines {
				heaviestLines, heaviestPath = lines, file.Path
			}
		}
		if request.HasWordingPromise() {
			if strings.Contains(file.Content, request.AbsentText) {
				return errors.New("candidate retains absent acceptance text")
			}
			expectedFound = expectedFound || strings.Contains(file.Content, request.ExpectedText)
		}
		folded := strings.ToLower(file.Content)
		for _, forbidden := range consumer.Mode.ForbiddenCandidateText {
			if strings.Contains(folded, strings.ToLower(forbidden)) {
				return errors.New("candidate contains forbidden text")
			}
		}
		total += len(file.Content)
	}
	if request.HasWordingPromise() && !expectedFound {
		return errors.New("candidate does not show the promised wording")
	}
	// A refusal names its numbers: a budget breach that reads the same as
	// every other one cost a twenty-five-minute agent run to diagnose
	// (measured 2026-08-07).
	if !changed {
		return errors.New("candidate changes nothing")
	}
	if total > consumer.Mode.MaxTotalBytes {
		return fmt.Errorf("candidate carries %d bytes of files, over the %d allowed", total, consumer.Mode.MaxTotalBytes)
	}
	// A file too large for the subsequence search is counted whole; naming
	// that here keeps a "10,002-line" refusal for a four-line edit from
	// reading as a real ten-thousand-line change.
	countingNote := ""
	if anyFallback {
		countingNote = "; a file too large to diff was counted whole"
	}
	if changedLines > consumer.Mode.MaxChangedLines {
		return fmt.Errorf("candidate changes %d lines, over the %d allowed (heaviest file: %s at %d lines%s)",
			changedLines, consumer.Mode.MaxChangedLines, heaviestPath, heaviestLines, countingNote)
	}
	if changedBytes > consumer.Mode.MaxChangedBytes {
		return fmt.Errorf("candidate changes %d bytes, over the %d allowed%s", changedBytes, consumer.Mode.MaxChangedBytes, countingNote)
	}
	digest, err := candidateDigest(c)
	if err != nil || digest != c.CandidateSHA256 {
		return errors.New("candidate digest is invalid")
	}
	return nil
}

func NewReview(stage int, endpoint ModelEndpoint, output ModelReviewOutput, candidate Candidate, source SourceSnapshot, request TicketRequest, config Config, invocation InvocationUsage, reviewedAt time.Time) (Review, error) {
	if err := candidate.Validate(source, request, config); err != nil || candidate.Stage != stage || endpoint.validate(true) != nil {
		return Review{}, errors.New("review input is invalid")
	}
	if invocation.Validate(endpoint) != nil || reviewedAt.IsZero() || reviewedAt.Location() != time.UTC {
		return Review{}, errors.New("review invocation is invalid")
	}
	if err := validateModelReviewOutput(output, request); err != nil {
		return Review{}, err
	}
	review := Review{
		SchemaVersion: ArtifactSchemaVersion, Stage: stage, DeliveryID: request.DeliveryID,
		ConfigSHA256: request.ConfigSHA256, ToolSHA: request.ToolSHA,
		CandidateSHA256: candidate.CandidateSHA256, ReviewerID: endpoint.ID, Vendor: endpoint.Vendor,
		Model: endpoint.Model, BaseURL: endpoint.BaseURL, Lens: endpoint.Lens, Effort: endpoint.Effort,
		StructuredOutput: endpoint.StructuredOutput, MaxOutputTokens: endpoint.MaxOutputTokens, Verdict: output.Verdict,
		Findings: append([]ModelFinding(nil), output.Findings...), Invocation: invocation, ReviewedAt: reviewedAt,
	}
	digest, err := reviewDigest(review)
	if err != nil {
		return Review{}, errors.New("review could not be sealed")
	}
	review.ReviewSHA256 = digest
	if err := review.Validate(endpoint, candidate, request); err != nil {
		return Review{}, err
	}
	return review, nil
}

func (r Review) Validate(endpoint ModelEndpoint, candidate Candidate, request TicketRequest) error {
	if r.SchemaVersion != ArtifactSchemaVersion || r.Stage != candidate.Stage || r.DeliveryID != request.DeliveryID ||
		r.ConfigSHA256 != request.ConfigSHA256 || r.ToolSHA != request.ToolSHA ||
		r.CandidateSHA256 != candidate.CandidateSHA256 || r.ReviewerID != endpoint.ID || r.Vendor != endpoint.Vendor ||
		r.Model != endpoint.Model || r.BaseURL != endpoint.BaseURL || r.Lens != endpoint.Lens || r.Effort != endpoint.Effort ||
		r.StructuredOutput != endpoint.StructuredOutput || r.MaxOutputTokens != endpoint.MaxOutputTokens || r.Invocation.Validate(endpoint) != nil ||
		r.ReviewedAt.IsZero() || r.ReviewedAt.Location() != time.UTC || r.ReviewedAt.Add(allowedArtifactClockSkew).Before(candidate.GeneratedAt) ||
		!sha256Pattern.MatchString(r.ReviewSHA256) {
		return errors.New("review identity is invalid")
	}
	if err := validateModelReviewOutput(ModelReviewOutput{Verdict: r.Verdict, Findings: r.Findings}, request); err != nil {
		return err
	}
	digest, err := reviewDigest(r)
	if err != nil || digest != r.ReviewSHA256 {
		return errors.New("review digest is invalid")
	}
	return nil
}

func DecideStage(candidate Candidate, reviews []Review, source SourceSnapshot, request TicketRequest, config Config) (StageDecision, error) {
	if err := candidate.Validate(source, request, config); err != nil || len(reviews) != len(config.Models.Reviewers) {
		return StageDecision{}, errors.New("stage decision input is invalid")
	}
	byID := make(map[string]Review, len(reviews))
	requestIDs := map[string]struct{}{candidate.Invocation.RequestID: {}}
	for _, review := range reviews {
		if _, duplicate := byID[review.ReviewerID]; duplicate {
			return StageDecision{}, errors.New("stage reviews contain duplicates")
		}
		byID[review.ReviewerID] = review
		if _, duplicate := requestIDs[review.Invocation.RequestID]; duplicate {
			return StageDecision{}, errors.New("stage model request ids contain duplicates")
		}
		requestIDs[review.Invocation.RequestID] = struct{}{}
	}
	outcome := "converged"
	digests := make([]string, 0, len(reviews))
	for _, endpoint := range config.Models.Reviewers {
		review, exists := byID[endpoint.ID]
		if !exists || review.Validate(endpoint, candidate, request) != nil {
			return StageDecision{}, errors.New("stage review set is invalid")
		}
		digests = append(digests, review.ReviewSHA256)
		if review.Verdict == "revise" {
			outcome = "revise"
		}
	}
	if outcome == "revise" && candidate.Stage == config.MaxStages {
		outcome = "nonconverged"
	}
	decision := StageDecision{
		SchemaVersion: ArtifactSchemaVersion, Stage: candidate.Stage, DeliveryID: request.DeliveryID,
		ConfigSHA256: request.ConfigSHA256, ToolSHA: request.ToolSHA,
		CandidateSHA256: candidate.CandidateSHA256, Outcome: outcome, ReviewSHA256s: digests,
	}
	digest, err := stageDecisionDigest(decision)
	if err != nil {
		return StageDecision{}, errors.New("stage decision could not be sealed")
	}
	decision.DecisionSHA256 = digest
	if err := decision.Validate(candidate, reviews, source, request, config); err != nil {
		return StageDecision{}, err
	}
	return decision, nil
}

func (d StageDecision) Validate(candidate Candidate, reviews []Review, source SourceSnapshot, request TicketRequest, config Config) error {
	if err := candidate.Validate(source, request, config); err != nil || d.SchemaVersion != ArtifactSchemaVersion ||
		d.Stage != candidate.Stage || d.DeliveryID != request.DeliveryID || d.CandidateSHA256 != candidate.CandidateSHA256 ||
		d.ConfigSHA256 != request.ConfigSHA256 || d.ToolSHA != request.ToolSHA ||
		len(reviews) != len(config.Models.Reviewers) || len(d.ReviewSHA256s) != len(config.Models.Reviewers) ||
		!sha256Pattern.MatchString(d.DecisionSHA256) {
		return errors.New("stage decision identity is invalid")
	}
	byID := make(map[string]Review, len(reviews))
	requestIDs := map[string]struct{}{candidate.Invocation.RequestID: {}}
	for _, review := range reviews {
		if _, duplicate := byID[review.ReviewerID]; duplicate {
			return errors.New("stage decision reviews contain duplicates")
		}
		byID[review.ReviewerID] = review
		if _, duplicate := requestIDs[review.Invocation.RequestID]; duplicate {
			return errors.New("stage decision model request ids contain duplicates")
		}
		requestIDs[review.Invocation.RequestID] = struct{}{}
	}
	expectedOutcome := "converged"
	for index, endpoint := range config.Models.Reviewers {
		review, exists := byID[endpoint.ID]
		if !exists || review.Validate(endpoint, candidate, request) != nil || d.ReviewSHA256s[index] != review.ReviewSHA256 {
			return errors.New("stage decision review set is invalid")
		}
		if review.Verdict == "revise" {
			expectedOutcome = "revise"
		}
	}
	if expectedOutcome == "revise" && candidate.Stage == config.MaxStages {
		expectedOutcome = "nonconverged"
	}
	if d.Outcome != expectedOutcome {
		return errors.New("stage decision outcome is invalid")
	}
	digest, err := stageDecisionDigest(d)
	if err != nil || digest != d.DecisionSHA256 {
		return errors.New("stage decision digest is invalid")
	}
	return nil
}

func DecodeModelCandidateOutput(encoded []byte) (ModelCandidateOutput, error) {
	var output ModelCandidateOutput
	if err := decodeStrictJSON(encoded, &output); err != nil {
		return ModelCandidateOutput{}, errors.New("model candidate response is invalid")
	}
	return output, nil
}

func DecodeModelReviewOutput(encoded []byte) (ModelReviewOutput, error) {
	var output ModelReviewOutput
	if err := decodeStrictJSON(encoded, &output); err != nil {
		return ModelReviewOutput{}, errors.New("model review response is invalid")
	}
	return output, nil
}

func validateModelCandidateOutput(output ModelCandidateOutput, request TicketRequest, config Config) error {
	if validatePlainText(output.Rationale, 4096, true) != nil || len(output.Files) != len(request.TargetFiles) {
		return errors.New("model candidate response is invalid")
	}
	consumer, err := request.Consumer(config)
	if err != nil {
		return errors.New("model candidate response is invalid")
	}
	seen := make(map[string]struct{}, len(output.Files))
	for _, file := range output.Files {
		if !allowedPath(file.Path, consumer.Mode.AllowedFilePrefixes) || !utf8.ValidString(file.Content) ||
			strings.ContainsRune(file.Content, '\x00') || len(file.Content) > consumer.Mode.MaxFileBytes {
			return errors.New("model candidate response is invalid")
		}
		if _, expected := findString(request.TargetFiles, file.Path); !expected {
			return errors.New("model candidate response contains an unexpected file")
		}
		if _, duplicate := seen[file.Path]; duplicate {
			return errors.New("model candidate response contains duplicate files")
		}
		seen[file.Path] = struct{}{}
	}
	return nil
}

func validateModelReviewOutput(output ModelReviewOutput, request TicketRequest) error {
	if output.Verdict != "pass" && output.Verdict != "revise" {
		return errors.New("model review verdict is invalid")
	}
	if len(output.Findings) > 16 || output.Verdict == "pass" && len(output.Findings) != 0 || output.Verdict == "revise" && len(output.Findings) == 0 {
		return errors.New("model review findings do not match verdict")
	}
	for _, finding := range output.Findings {
		if !identifierPattern.MatchString(finding.Code) || finding.Line < 0 || finding.Line > 1_000_000 ||
			validatePlainText(finding.Message, 4000, true) != nil {
			return errors.New("model review finding is invalid")
		}
		if _, expected := findString(request.TargetFiles, finding.Path); !expected {
			return errors.New("model review finding has an unexpected path")
		}
	}
	return nil
}

func regularFileWithin(root, relative string) (string, error) {
	if !validRelativePath(relative) {
		return "", errors.New("relative path is invalid")
	}
	current := root
	parts := strings.Split(filepath.FromSlash(relative), string(filepath.Separator))
	for _, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("path component is invalid")
		}
	}
	info, err := os.Stat(current)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("file is not regular")
	}
	rel, err := filepath.Rel(root, current)
	if err != nil || rel != filepath.FromSlash(relative) {
		return "", errors.New("file escaped root")
	}
	return current, nil
}

func readTextFile(filename string, limit int) ([]byte, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil || len(content) > limit || !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 {
		return nil, errors.New("file content is invalid")
	}
	return content, nil
}

func sourceDigest(source SourceSnapshot) (string, error) {
	source.SourceSHA256 = ""
	encoded, err := json.Marshal(source)
	if err != nil {
		return "", err
	}
	return digestBytes(encoded), nil
}

func candidateDigest(candidate Candidate) (string, error) {
	candidate.CandidateSHA256 = ""
	encoded, err := json.Marshal(candidate)
	if err != nil {
		return "", err
	}
	return digestBytes(encoded), nil
}

func reviewDigest(review Review) (string, error) {
	review.ReviewSHA256 = ""
	encoded, err := json.Marshal(review)
	if err != nil {
		return "", err
	}
	return digestBytes(encoded), nil
}

func stageDecisionDigest(decision StageDecision) (string, error) {
	decision.DecisionSHA256 = ""
	encoded, err := json.Marshal(decision)
	if err != nil {
		return "", err
	}
	return digestBytes(encoded), nil
}

func digestBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func gitBlobDigest(value []byte) string {
	digest := sha1.New() // #nosec G401 -- required to match the repository's Git object format.
	_, _ = fmt.Fprintf(digest, "blob %d%c", len(value), byte(0))
	_, _ = digest.Write(value)
	return hex.EncodeToString(digest.Sum(nil))
}

func decodeStrictJSON(encoded []byte, destination any) error {
	if err := rejectDuplicateJSONKeys(encoded); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("multiple JSON values")
	}
	return nil
}

func findString(values []string, target string) (int, bool) {
	index := sort.SearchStrings(values, target)
	return index, index < len(values) && values[index] == target
}

// maxChangeBudgetCells bounds the quadratic diff below - roughly 8,000 by
// 8,000 trimmed lines, adversarial worst around half a second. Trimmed
// middles beyond it fall back to counting the whole middle as changed - the
// old conservative reading - so a pathological file pair costs bounded
// milliseconds in two rows of memory.
const maxChangeBudgetCells = 64_000_000

// changeBudgetLineWeight packs one matched line into the subsequence weight:
// the line term must dominate any possible byte total so that the search is
// lexicographic - most matched lines first, most matched bytes among those
// readings. Config validation holds MaxFileBytes to 512 KiB, so every
// per-file byte total stays below this constant and the packed weight
// decomposes exactly.
const changeBudgetLineWeight = int64(1) << 20

// conservativeChangeBudget measures the candidate's change against the base
// as the lines and bytes outside a longest common subsequence of logical
// lines, both taken from the same alignment: the one with the fewest changed
// lines, breaking ties toward the fewest changed bytes. One alignment, not
// two - lines and bytes each optimized separately produced a pair no single
// consistent diff could realize, and a crafted move of one huge line could
// pass the byte gate at a fraction of what any real reading of the change
// carries (found in adversarial review, 2026-08-17).
//
// The subsequence search replaced a prefix/suffix-only reading that counted
// everything between the first and last edit: two small template fixes at
// opposite ends of a 1,300-line locale dictionary counted the whole
// dictionary, and a valid eight-file candidate died at 5,283 counted lines
// against a 3,000-line budget (measured 2026-08-17). Where the search would
// exceed maxChangeBudgetCells, the prefix/suffix reading remains as the
// fallback - conservative, never under-counting - and the third return
// reports that the numbers are the fallback's, so a refusal can say so.
func conservativeChangeBudget(before, after string) (lines int, bytes int, fallback bool) {
	if before == after {
		return 0, 0, false
	}
	beforeLines := splitLogicalLines(before)
	afterLines := splitLogicalLines(after)
	prefix := 0
	for prefix < len(beforeLines) && prefix < len(afterLines) && beforeLines[prefix] == afterLines[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(beforeLines)-prefix && suffix < len(afterLines)-prefix &&
		beforeLines[len(beforeLines)-1-suffix] == afterLines[len(afterLines)-1-suffix] {
		suffix++
	}
	middleBefore := beforeLines[prefix : len(beforeLines)-suffix]
	middleAfter := afterLines[prefix : len(afterLines)-suffix]
	middleBeforeBytes := 0
	for _, line := range middleBefore {
		middleBeforeBytes += len(line)
	}
	middleAfterBytes := 0
	for _, line := range middleAfter {
		middleAfterBytes += len(line)
	}
	if len(middleBefore) > 0 && len(middleAfter) > maxChangeBudgetCells/len(middleBefore) {
		return len(middleBefore) + len(middleAfter), middleBeforeBytes + middleAfterBytes, true
	}
	common := longestCommonSubsequenceWeight(middleBefore, middleAfter)
	commonLines := int(common / changeBudgetLineWeight)
	commonBytes := int(common % changeBudgetLineWeight)
	lines = len(middleBefore) + len(middleAfter) - 2*commonLines
	bytes = middleBeforeBytes + middleAfterBytes - 2*commonBytes
	return lines, bytes, false
}

// longestCommonSubsequenceWeight returns the maximum packed weight of a
// common subsequence of the two line slices, in two rolling rows of memory.
// Each matched line contributes changeBudgetLineWeight plus its byte length,
// so the maximum is the line-count-first, byte-count-second lexicographic
// best and decomposes into both totals.
func longestCommonSubsequenceWeight(before, after []string) int64 {
	if len(before) == 0 || len(after) == 0 {
		return 0
	}
	previous := make([]int64, len(after)+1)
	current := make([]int64, len(after)+1)
	for _, beforeLine := range before {
		for column, afterLine := range after {
			if beforeLine == afterLine {
				current[column+1] = previous[column] + changeBudgetLineWeight + int64(len(beforeLine))
			} else if previous[column+1] >= current[column] {
				current[column+1] = previous[column+1]
			} else {
				current[column+1] = current[column]
			}
		}
		previous, current = current, previous
	}
	return previous[len(after)]
}

func splitLogicalLines(value string) []string {
	if value == "" {
		return nil
	}
	lines := strings.SplitAfter(value, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
