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
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"automation.internal/ticket-ingress/internal/cardsecret"
	"automation.internal/ticket-ingress/internal/modeljson"
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
	// Created marks a file the change brings into existence: its before-side
	// is the empty content, and every verifier expects the path to be absent
	// from the base instead of matching a blob. The flag rides the sealed
	// digests (omitempty keeps pre-existing snapshots' digests unchanged).
	Created bool   `json:"created,omitempty"`
	SHA256  string `json:"sha256"`
	Content string `json:"content"`
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
	SchemaVersion int             `json:"schema_version"`
	Stage         int             `json:"stage"`
	DeliveryID    string          `json:"delivery_id"`
	InputSHA256   string          `json:"input_sha256"`
	ConfigSHA256  string          `json:"config_sha256"`
	ToolSHA       string          `json:"tool_sha"`
	SourceSHA256  string          `json:"source_sha256"`
	BaseSHA       string          `json:"base_sha"`
	Implementer   ModelEndpoint   `json:"implementer"`
	Invocation    InvocationUsage `json:"invocation"`
	GeneratedAt   time.Time       `json:"generated_at"`
	Files         []CandidateFile `json:"files"`
	Rationale     string          `json:"rationale"`
	// DesignSHA256 binds a candidate that applies an approved design to that
	// design (docs/INVESTIGATING_DESIGNER.md §7); empty for the original
	// chain, which keeps every existing digest unchanged.
	DesignSHA256    string `json:"design_sha256,omitempty"`
	CandidateSHA256 string `json:"candidate_sha256"`
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
	// Ruling is what the engine decided about a round that had stopped
	// moving, absent from every round nobody had to rule on.
	//
	// It is carried whole rather than named by digest, which is not how the
	// other records refer to each other. The reason is that a decision is
	// read by gates that are handed artifacts from four different places —
	// a run directory, a command line, a staging bundle, a release proof —
	// and every one of them re-derives the outcome. Named by digest, each
	// would have to be taught to find and carry the ruling too, and any one
	// that was not would refuse a delivery the engine had legitimately
	// decided. Carried here, a decision proves itself wherever it goes.
	//
	// Omitted when absent, so a round decided the ordinary way seals
	// exactly the bytes it always did and every digest bound to those bytes
	// still holds.
	Ruling         *Ruling `json:"ruling,omitempty"`
	DecisionSHA256 string  `json:"decision_sha256"`
}

// Overruled is what the round's ruling set aside, in seat order: the
// objections a reader counting the reviews again would otherwise find
// standing. Empty for a round nobody ruled on.
func (d StageDecision) Overruled() []OverruledFinding {
	if d.Ruling == nil {
		return nil
	}
	return d.Ruling.Overruled
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

	allowance := request.WorkflowAllowance()
	files := make([]SourceFile, 0, len(request.TargetFiles))
	for _, name := range request.TargetFiles {
		filename, err := regularFileWithinAllowing(root, name, allowance)
		if err != nil {
			// A target the change is to create has no before-side: the file
			// is absent from the base, and the snapshot records that rather
			// than refusing the run. Only a name the request already holds
			// reaches here, and that name passed the writable-prefix rules
			// when the target files were derived. Anything else that makes
			// the path unreadable — a component that is a symlink, a
			// directory in the way — is still a refusal.
			if created, createdErr := newFileTarget(root, name, allowance); createdErr != nil {
				return SourceSnapshot{}, fmt.Errorf("source file %q is invalid", name)
			} else if created {
				files = append(files, SourceFile{Path: name, GitBlobSHA: gitBlobDigest(nil), SHA256: digestBytes(nil), Created: true})
				continue
			}
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
		if file.Created && file.Content != "" {
			return errors.New("created source file must start from empty content")
		}
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
	if err := source.Validate(request, config); err != nil || stage < 1 || stage > config.StageCeiling() {
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
		// The occupant, not the seat: a sealed record is about one answer,
		// and carrying the seat's other candidates into it would put a
		// list of models that did not run inside the candidate's digest.
		Implementer: implementerOccupant(config), Invocation: invocation, GeneratedAt: generatedAt,
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
	if c.SchemaVersion != ArtifactSchemaVersion || c.Stage < 1 || c.Stage > config.StageCeiling() ||
		c.DeliveryID != request.DeliveryID || c.InputSHA256 != request.InputSHA256 ||
		c.ConfigSHA256 != request.ConfigSHA256 || c.ToolSHA != request.ToolSHA ||
		c.SourceSHA256 != source.SourceSHA256 || c.BaseSHA != source.BaseSHA || !implementerSeated(c.Implementer, config) ||
		c.Invocation.Validate(c.Implementer) != nil || c.GeneratedAt.IsZero() || c.GeneratedAt.Location() != time.UTC ||
		!sha256Pattern.MatchString(c.CandidateSHA256) {
		return errors.New("candidate identity is invalid")
	}
	if validatePlainText(c.Rationale, 4096, true) != nil || len(c.Files) != len(source.Files) {
		return errors.New("candidate content is invalid")
	}
	// A workflow file in the candidate is held to the destination's content
	// policy here, which is before this candidate is sealed and therefore
	// before any branch carrying it is pushed. It is checked again by every
	// later reader of the candidate for the same reason every other binding
	// is: the seal proves what the bytes are, not that they were allowed.
	if err := CheckDeployWorkflows(c.Files, consumer); err != nil {
		return err
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
		// The logs a handed credential could leave through are masked, and
		// the round's own diff is the way out that is left: a change
		// carrying the value goes into the pull request and stays there.
		//
		// The refusal names the variable and never the value — a refusal
		// travels into the round's record and onto the ticket, so saying
		// the value would publish it in the act of complaining about it.
		//
		// Every credential the deployment provisions, not only the ones
		// this card was handed: the sealing worker reads them all for the
		// comparison (cmd/worker/credential_scan.go). Which cards receive a
		// value is a different question, answered by the list of stages,
		// and neither answer moves the other.
		if variable := cardsecret.VariableIn(file.Content); variable != "" {
			return errors.New("candidate contains the value of " + variable)
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

// Validate holds a sealed review to the seat that produced it.
//
// The seat id is exact: a review naming another reviewer is another seat's
// judgment however valid it reads, and that refusal is unchanged. What the
// seat admits is who answered — the record has to match one of the seat's
// occupants, which is the configured endpoint for a seat that never moved
// and the candidate that took over for one that did. Before seats a
// candidate's review was refused by the same rule that refuses a forged
// one, and a delivery whose provider went quiet had nowhere to go.
func (r Review) Validate(seat ModelEndpoint, candidate Candidate, request TicketRequest) error {
	endpoint, seated := r.seatedIn(seat)
	if !seated {
		return errors.New("review identity is invalid")
	}
	if r.SchemaVersion != ArtifactSchemaVersion || r.Stage != candidate.Stage || r.DeliveryID != request.DeliveryID ||
		r.ConfigSHA256 != request.ConfigSHA256 || r.ToolSHA != request.ToolSHA ||
		r.CandidateSHA256 != candidate.CandidateSHA256 || r.Invocation.Validate(endpoint) != nil ||
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

// seatedIn finds the occupant a sealed review says answered. The reviewer
// id must be the seat's own; the rest of the identity must be one of the
// occupants, whole — a record mixing one candidate's vendor with another's
// model is nobody the configuration ever had.
func (r Review) seatedIn(seat ModelEndpoint) (ModelEndpoint, bool) {
	if r.ReviewerID != seat.ID {
		return ModelEndpoint{}, false
	}
	for _, occupant := range seat.Seat() {
		if r.Vendor == occupant.Vendor && r.Model == occupant.Model && r.BaseURL == occupant.BaseURL &&
			r.Lens == occupant.Lens && r.Effort == occupant.Effort &&
			r.StructuredOutput == occupant.StructuredOutput && r.MaxOutputTokens == occupant.MaxOutputTokens {
			return occupant, true
		}
	}
	return ModelEndpoint{}, false
}

// DecideStage tallies one round: every configured seat's sealed verdict,
// and the ruling — when the engine had to make one — that says which of
// their objections the ticket does not require.
//
// The round limit is deliberately not here. It used to be: a revise at the
// last configured round was rewritten as nonconverged, which is how a
// delivery ended on a count. Rounds end when the seats agree or when the
// arbiter rules, so what this verb answers is only what the seats said.
func DecideStage(candidate Candidate, reviews []Review, source SourceSnapshot, request TicketRequest, config Config, ruling *Ruling) (StageDecision, error) {
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
	// The ruling is held to this round's artifacts by the decision itself,
	// which is re-derived below before anything is returned. Checking it
	// here as well would be the same refusal, one step earlier, and a second
	// place to keep the rule in step with the reader's.
	outcome, digests, err := tallyRound(candidate, byID, reviews, request, config, ruling)
	if err != nil {
		return StageDecision{}, err
	}
	decision := StageDecision{
		SchemaVersion: ArtifactSchemaVersion, Stage: candidate.Stage, DeliveryID: request.DeliveryID,
		ConfigSHA256: request.ConfigSHA256, ToolSHA: request.ToolSHA,
		CandidateSHA256: candidate.CandidateSHA256, Outcome: outcome, ReviewSHA256s: digests,
		Ruling: ruling,
	}
	digest, digestErr := stageDecisionDigest(decision)
	if digestErr != nil {
		return StageDecision{}, errors.New("stage decision could not be sealed")
	}
	decision.DecisionSHA256 = digest
	if err := decision.Validate(candidate, reviews, source, request, config); err != nil {
		return StageDecision{}, err
	}
	return decision, nil
}

// tallyRound counts one round's verdicts under the ruling, if any, that the
// engine made about it.
//
// A seat's revise stands unless every objection it raised is set aside: a
// seat that still has one standing is still asking for a change, and a seat
// that objected without naming anything cannot be answered by a ruling at
// all.
func tallyRound(
	candidate Candidate,
	byID map[string]Review,
	reviews []Review,
	request TicketRequest,
	config Config,
	ruling *Ruling,
) (string, []string, error) {
	outcome := "converged"
	digests := make([]string, 0, len(reviews))
	for _, seat := range config.Models.Reviewers {
		review, exists := byID[seat.ID]
		if !exists || review.Validate(seat, candidate, request) != nil {
			return "", nil, errors.New("stage review set is invalid")
		}
		digests = append(digests, review.ReviewSHA256)
		if review.Verdict != "revise" {
			continue
		}
		standing := 0
		for _, finding := range review.Findings {
			if ruling != nil && ruling.Overrules(review.ReviewerID, finding.Code, finding.Path) {
				continue
			}
			standing++
		}
		if standing > 0 || len(review.Findings) == 0 {
			outcome = "revise"
		}
	}
	if err := seatsStayDiverse(reviews, config); err != nil {
		return "", nil, err
	}
	return outcome, digests, nil
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
	// A ruling the decision carries is held to this round's own artifacts
	// before it is allowed to change the count. A forged one — naming
	// objections nobody raised, or another round's change — fails here, so
	// a decision cannot talk itself into converging.
	if d.Ruling != nil {
		if err := d.Ruling.Validate(candidate, reviews, request, config); err != nil {
			return err
		}
	}
	expectedOutcome, digests, err := tallyRound(candidate, byID, reviews, request, config, d.Ruling)
	if err != nil {
		return err
	}
	for index := range digests {
		if d.ReviewSHA256s[index] != digests[index] {
			return errors.New("stage decision review set is invalid")
		}
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
	if err := decodeModelJSON(encoded, &output); err != nil {
		return ModelCandidateOutput{}, errors.New("model candidate response is invalid")
	}
	return output, nil
}

func DecodeModelReviewOutput(encoded []byte) (ModelReviewOutput, error) {
	var output ModelReviewOutput
	if err := decodeModelJSON(encoded, &output); err != nil {
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
	// Gate five of five. The allowance is the contract's own, so a file the
	// sealed plan named is sealable and a dotted sibling of it is not —
	// and neither is the same file in a delivery whose contract names none.
	allowance := request.WorkflowAllowance()
	seen := make(map[string]struct{}, len(output.Files))
	for _, file := range output.Files {
		if !allowedPathWithin(file.Path, consumer.Mode.AllowedFilePrefixes, allowance) || !utf8.ValidString(file.Content) ||
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
		return fmt.Errorf("model review verdict is invalid: %q is not pass or revise", output.Verdict)
	}
	// A reviewer that passes a change and still writes a note has passed it,
	// and one that asks for a revision without listing a finding has still
	// asked. Throwing the answer away for that made the turn ask again, and
	// asking again is what rounds are spent on.
	for _, finding := range output.Findings {
		if !identifierPattern.MatchString(finding.Code) || finding.Line < 0 || finding.Line > 1_000_000 ||
			validatePlainText(finding.Message, 4000, true) != nil {
			return errors.New("model review finding is invalid")
		}
		// A finding travels into the next round's review instruction ahead
		// of the answer-rules boundary; a message carrying the boundary line
		// could shift where the verdict decoder starts reading.
		if strings.Contains(finding.Message, ReviewAnswerRulesTail) {
			return errors.New("model review finding is invalid")
		}
		if _, expected := findString(request.TargetFiles, finding.Path); !expected {
			return errors.New("model review finding has an unexpected path")
		}
	}
	return nil
}

// newFileTarget reports whether a target the request names is simply absent
// from the base — the shape of a request to create a file. Every component
// that does exist must be an ordinary directory: a symlink or a file in the
// path is a refusal, not a file to create.
func newFileTarget(root, relative string, allowance WorkflowAllowance) (bool, error) {
	if !allowance.Admits(relative) && (!validRelativePath(relative) || hasHiddenComponent(relative)) {
		return false, errors.New("relative path is invalid")
	}
	current := root
	parts := strings.Split(filepath.FromSlash(relative), string(filepath.Separator))
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return true, nil
			}
			return false, errors.New("path component is invalid")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return false, errors.New("path component is invalid")
		}
		if index < len(parts)-1 && !info.IsDir() {
			return false, errors.New("path component is invalid")
		}
		if index == len(parts)-1 {
			// The name exists after all: whatever it is, it is not a file
			// this run may create, and the ordinary reader refused it.
			return false, errors.New("path component is invalid")
		}
	}
	return false, errors.New("path component is invalid")
}

func regularFileWithin(root, relative string) (string, error) {
	return regularFileWithinAllowing(root, relative, WorkflowAllowance{})
}

// regularFileWithinAllowing is regularFileWithin for a caller that carries
// a run's workflow allowance. The walk itself is unchanged — every
// component is still lstat'd and a symlink anywhere still refuses — and
// what the allowance moves is only the shape check at the top, which is the
// path vocabulary rather than a safety property.
func regularFileWithinAllowing(root, relative string, allowance WorkflowAllowance) (string, error) {
	if !validRelativePathWithin(relative, allowance) {
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

// createdFileWithin is regularFileWithin's mirror for files that do not
// exist yet: every ancestor component that exists must be a real directory
// inside root - never a symlink - and the final element must be absent. The
// created path measurably escaped the root through a committed symlink
// directory before this walk existed, while the edit path was protected.
func createdFileWithin(root, relative string, allowance WorkflowAllowance) (string, error) {
	if !validRelativePathWithin(relative, allowance) {
		return "", errors.New("relative path is invalid")
	}
	current := root
	parts := strings.Split(filepath.FromSlash(relative), string(filepath.Separator))
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			// Everything below a missing component is new by construction;
			// the final element in particular must land here.
			continue
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("path component is invalid")
		}
		if index == len(parts)-1 {
			return "", errors.New("created file already exists in the workspace")
		}
		if !info.IsDir() {
			return "", errors.New("path component is invalid")
		}
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

// decodeModelJSON reads a model's answer for what this engine needs: keys
// the struct does not carry are ignored, prose or a code fence around the
// JSON is peeled off, and a needed field of the wrong type is still an
// error. Why an answer is read differently from a record is in
// internal/modeljson.
func decodeModelJSON(encoded []byte, destination any) error {
	return modeljson.Decode(encoded, destination)
}

// decodeStrictJSON reads a record this engine wrote, or configuration a
// person wrote: one JSON value, every key known to the struct, no
// duplicates. An unknown key there means the bytes were written by a
// different engine or a different version of this one, and reading them as
// if they were understood is how a record silently gains authority.
//
// It is not for a model's answer. Those go through decodeModelJSON, and the
// two names are spelled out at every call site so the intent of each read is
// readable where the read is.
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
