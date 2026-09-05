package investigate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// The design reviewers judge a sealed investigation report or a sealed
// design before any code is written (docs/INVESTIGATING_DESIGNER.md §5).
// The candidate review record cannot carry that judgment - it is bound to a
// candidate's fingerprint and its findings to paths among the ticket's
// target files - so the records here bind to the subject record's
// fingerprint instead and name the section of it each objection concerns.

// Subjects a design review may judge.
const (
	SubjectInvestigation = "investigation"
	SubjectDesign        = "design"
)

// Verdicts a design review may carry.
const (
	VerdictPass   = "pass"
	VerdictRevise = "revise"
)

// Outcomes a design decision may reach.
const (
	OutcomeApproved     = "approved"
	OutcomeRevise       = "revise"
	OutcomeNonconverged = "nonconverged"
)

// Sections of a design a finding may name.
const (
	SectionCause        = "cause"
	SectionApproach     = "approach"
	SectionFiles        = "files"
	SectionVerification = "verification"
	SectionBlastRadius  = "blast_radius"
	SectionNotDoing     = "not_doing"
)

// Sections of an investigation report a finding may name.
const (
	SectionFindings = "findings"
	SectionUnknowns = "unknowns"
	SectionNext     = "next"
)

// DesignReviewsPerDesign and ReviewsPerInvestigation are how many judgments
// a decision needs: two lenses on a design, the evidence lens alone on a
// report that no design follows.
const (
	DesignReviewsPerDesign  = 2
	ReviewsPerInvestigation = 1
)

const (
	maxDesignFindings = 16
	maxFindingMessage = 4000
	maxLensText       = 1024
	maxEndpointText   = 512
)

var (
	codePattern      = regexp.MustCompile(`^[a-z][a-z0-9-]{1,63}$`)
	requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`)
)

// Sections lists the sections a finding may name for a subject, nil for an
// unknown subject.
func Sections(subject string) []string {
	switch subject {
	case SubjectDesign:
		return []string{SectionCause, SectionApproach, SectionFiles, SectionVerification, SectionBlastRadius, SectionNotDoing}
	case SubjectInvestigation:
		return []string{SectionFindings, SectionUnknowns, SectionNext}
	default:
		return nil
	}
}

// ReviewsRequired is how many reviews a decision on the subject needs.
func ReviewsRequired(subject string) int {
	if subject == SubjectInvestigation {
		return ReviewsPerInvestigation
	}
	return DesignReviewsPerDesign
}

// ReviewSubject names the sealed record a review judged: which kind of
// record, which round, which fingerprint. Callers build it from a record
// they have already validated, so the fingerprint is the record's own and
// the review can only ever be about that exact content.
type ReviewSubject struct {
	Kind   string
	Round  int
	SHA256 string
}

// InvestigationSubject names a sealed investigation report as the subject.
func InvestigationSubject(record Investigation) ReviewSubject {
	return ReviewSubject{Kind: SubjectInvestigation, Round: record.Round, SHA256: record.InvestigationSHA256}
}

// DesignSubject names a sealed design as the subject.
func DesignSubject(record Design) ReviewSubject {
	return ReviewSubject{Kind: SubjectDesign, Round: record.Round, SHA256: record.DesignSHA256}
}

func (s ReviewSubject) validate() error {
	if Sections(s.Kind) == nil || s.Round < 1 || !sha256Pattern.MatchString(s.SHA256) {
		return errors.New("review subject is invalid")
	}
	return nil
}

// Reviewer is the judge as configured - the endpoint's identity - and the
// lens it judged under. The lens is recorded because the same endpoint
// judges evidence in one seat and approach in another.
type Reviewer struct {
	ID      string
	Vendor  string
	Model   string
	BaseURL string
	Lens    string
}

func (r Reviewer) validate() error {
	if !codePattern.MatchString(r.ID) || !singleLine(r.Vendor, maxEndpointText) || !singleLine(r.Model, maxEndpointText) ||
		!singleLine(r.BaseURL, maxEndpointText) || !strings.HasPrefix(r.BaseURL, "https://") || strings.Contains(r.BaseURL, " ") ||
		!singleLine(r.Lens, maxLensText) {
		return errors.New("design reviewer is invalid")
	}
	return nil
}

func singleLine(value string, limit int) bool {
	return validText(value, limit) && !strings.ContainsAny(value, "\n\t")
}

// Usage is the invocation evidence a review carries, field for field the
// worker's InvocationUsage. The worker imports this package, so the shape
// is declared here rather than imported; the worker converts.
type Usage struct {
	RequestedModel string  `json:"requested_model"`
	RequestID      string  `json:"request_id"`
	StopReason     string  `json:"stop_reason"`
	InputTokens    int32   `json:"input_tokens"`
	OutputTokens   int32   `json:"output_tokens"`
	TotalTokens    int32   `json:"total_tokens"`
	LatencyMillis  int64   `json:"latency_millis"`
	CostUSD        float64 `json:"cost_usd,omitempty"`
}

func (u Usage) validate(model string) error {
	if u.RequestedModel != model || !requestIDPattern.MatchString(u.RequestID) || u.StopReason != "stop" ||
		u.InputTokens <= 0 || u.OutputTokens <= 0 || u.TotalTokens <= 0 || u.InputTokens+u.OutputTokens != u.TotalTokens ||
		u.LatencyMillis < 0 || u.CostUSD < 0 {
		return errors.New("design review invocation evidence is invalid")
	}
	return nil
}

// DesignFinding is one objection: a short code, the section of the subject
// it concerns, and what is wrong there.
type DesignFinding struct {
	Code    string `json:"code"`
	Section string `json:"section"`
	Message string `json:"message"`
}

// ModelDesignReviewOutput is the one JSON object a design reviewer answers
// with. Decoded strictly: a finding carrying a path, a line or any other
// field of the candidate review's shape is refused, not ignored.
type ModelDesignReviewOutput struct {
	Verdict  string          `json:"verdict"`
	Findings []DesignFinding `json:"findings"`
}

// DecodeModelDesignReviewOutput reads the reviewer's answer strictly.
func DecodeModelDesignReviewOutput(encoded []byte) (ModelDesignReviewOutput, error) {
	var output ModelDesignReviewOutput
	return output, decodeStrict(encoded, &output)
}

// DesignReview is one reviewer's sealed judgment of one subject record.
type DesignReview struct {
	SchemaVersion int `json:"schema_version"`
	Identity
	Round         int             `json:"round"`
	Subject       string          `json:"subject"`
	SubjectSHA256 string          `json:"subject_sha256"`
	ReviewerID    string          `json:"reviewer_id"`
	Vendor        string          `json:"vendor"`
	Model         string          `json:"model"`
	BaseURL       string          `json:"base_url"`
	Lens          string          `json:"lens"`
	Verdict       string          `json:"verdict"`
	Findings      []DesignFinding `json:"findings"`
	Invocation    Usage           `json:"invocation"`
	ReviewedAt    time.Time       `json:"reviewed_at"`
	ReviewSHA256  string          `json:"review_sha256"`
}

// NewDesignReview validates the reviewer's answer against the subject and
// seals it. A pass carries no findings; a revise carries at least one, each
// naming a section the subject actually has.
func NewDesignReview(identity Identity, subject ReviewSubject, reviewer Reviewer, output ModelDesignReviewOutput, usage Usage, reviewedAt time.Time) (DesignReview, error) {
	record := DesignReview{
		SchemaVersion: SchemaVersion, Identity: identity, Round: subject.Round,
		Subject: subject.Kind, SubjectSHA256: subject.SHA256,
		ReviewerID: reviewer.ID, Vendor: reviewer.Vendor, Model: reviewer.Model, BaseURL: reviewer.BaseURL, Lens: reviewer.Lens,
		Verdict: output.Verdict, Findings: append([]DesignFinding{}, output.Findings...),
		Invocation: usage, ReviewedAt: reviewedAt,
	}
	digest, err := designReviewDigest(record)
	if err != nil {
		return DesignReview{}, errors.New("design review could not be sealed")
	}
	record.ReviewSHA256 = digest
	if err := record.Validate(identity, subject); err != nil {
		return DesignReview{}, err
	}
	return record, nil
}

// Validate re-derives the binding to the subject and the identity, the
// reviewer, the verdict rules and the fingerprint.
func (r DesignReview) Validate(identity Identity, subject ReviewSubject) error {
	if err := subject.validate(); err != nil {
		return err
	}
	if r.SchemaVersion != SchemaVersion || r.Identity != identity || r.Round != subject.Round ||
		r.Subject != subject.Kind || r.SubjectSHA256 != subject.SHA256 {
		return errors.New("design review is not bound to this subject")
	}
	if err := identity.validate(); err != nil {
		return err
	}
	reviewer := Reviewer{ID: r.ReviewerID, Vendor: r.Vendor, Model: r.Model, BaseURL: r.BaseURL, Lens: r.Lens}
	if err := reviewer.validate(); err != nil {
		return err
	}
	if err := r.Invocation.validate(r.Model); err != nil {
		return err
	}
	if r.ReviewedAt.IsZero() || r.ReviewedAt.Location() != time.UTC {
		return errors.New("design review time is invalid")
	}
	if err := validateDesignReviewOutput(ModelDesignReviewOutput{Verdict: r.Verdict, Findings: r.Findings}, r.Subject); err != nil {
		return err
	}
	digest, err := designReviewDigest(r)
	if err != nil || digest != r.ReviewSHA256 {
		return errors.New("design review digest is invalid")
	}
	return nil
}

func validateDesignReviewOutput(output ModelDesignReviewOutput, subject string) error {
	switch output.Verdict {
	case VerdictPass:
		if len(output.Findings) != 0 {
			return errors.New("a pass verdict must carry no findings")
		}
	case VerdictRevise:
		if len(output.Findings) == 0 {
			return errors.New("a revise verdict must carry at least one finding")
		}
	default:
		return errors.New("design review verdict must be pass or revise")
	}
	if len(output.Findings) > maxDesignFindings {
		return errors.New("design review carries too many findings")
	}
	sections := Sections(subject)
	for _, finding := range output.Findings {
		if !codePattern.MatchString(finding.Code) {
			return errors.New("design review finding code is invalid")
		}
		if !containsString(sections, finding.Section) {
			return fmt.Errorf("design review finding names %q, which is not a section of the %s", finding.Section, subject)
		}
		if !validText(finding.Message, maxFindingMessage) {
			return errors.New("design review finding message is invalid")
		}
	}
	return nil
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// DesignDecision is the sealed outcome of one round's reviews of one
// subject. Like the stage decision it records AI consensus only; the gates
// that consume an approved design re-check the design itself.
type DesignDecision struct {
	SchemaVersion int `json:"schema_version"`
	Identity
	Round          int      `json:"round"`
	Subject        string   `json:"subject"`
	SubjectSHA256  string   `json:"subject_sha256"`
	ReviewSHA256s  []string `json:"review_sha256s"`
	Outcome        string   `json:"outcome"`
	DecisionSHA256 string   `json:"decision_sha256"`
}

// DecideDesign seals the round's outcome: approved when every review
// passes; revise when any review objects and rounds remain; nonconverged
// when any review objects at the last round. round is the caller's own
// count and must be the subject's; maxRounds comes from the configuration
// the identity is bound to.
func DecideDesign(identity Identity, subject ReviewSubject, reviews []DesignReview, round, maxRounds int) (DesignDecision, error) {
	digests, outcome, err := deriveDesignOutcome(identity, subject, reviews, round, maxRounds)
	if err != nil {
		return DesignDecision{}, err
	}
	decision := DesignDecision{
		SchemaVersion: SchemaVersion, Identity: identity, Round: round,
		Subject: subject.Kind, SubjectSHA256: subject.SHA256, ReviewSHA256s: digests, Outcome: outcome,
	}
	digest, err := designDecisionDigest(decision)
	if err != nil {
		return DesignDecision{}, errors.New("design decision could not be sealed")
	}
	decision.DecisionSHA256 = digest
	if err := decision.Validate(identity, subject, reviews, maxRounds); err != nil {
		return DesignDecision{}, err
	}
	return decision, nil
}

// Validate re-derives the outcome from the reviews and the round limit and
// checks the binding and the fingerprint.
func (d DesignDecision) Validate(identity Identity, subject ReviewSubject, reviews []DesignReview, maxRounds int) error {
	if d.SchemaVersion != SchemaVersion || d.Identity != identity || d.Round != subject.Round ||
		d.Subject != subject.Kind || d.SubjectSHA256 != subject.SHA256 {
		return errors.New("design decision is not bound to this subject")
	}
	digests, outcome, err := deriveDesignOutcome(identity, subject, reviews, d.Round, maxRounds)
	if err != nil {
		return err
	}
	if len(d.ReviewSHA256s) != len(digests) {
		return errors.New("design decision review set is invalid")
	}
	for index, digest := range digests {
		if d.ReviewSHA256s[index] != digest {
			return errors.New("design decision review set is invalid")
		}
	}
	if d.Outcome != outcome {
		return errors.New("design decision outcome is invalid")
	}
	digest, err := designDecisionDigest(d)
	if err != nil || digest != d.DecisionSHA256 {
		return errors.New("design decision digest is invalid")
	}
	return nil
}

// deriveDesignOutcome validates the review set and derives the digests, in
// reviewer-id order, and the outcome.
func deriveDesignOutcome(identity Identity, subject ReviewSubject, reviews []DesignReview, round, maxRounds int) ([]string, string, error) {
	if err := subject.validate(); err != nil {
		return nil, "", err
	}
	if maxRounds < 1 || round != subject.Round || round > maxRounds {
		return nil, "", errors.New("design decision round is invalid")
	}
	if expected := ReviewsRequired(subject.Kind); len(reviews) != expected {
		return nil, "", fmt.Errorf("a %s decision needs %d reviews and was given %d", subject.Kind, expected, len(reviews))
	}
	ordered := append([]DesignReview(nil), reviews...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ReviewerID < ordered[j].ReviewerID })
	reviewers := make(map[string]struct{}, len(ordered))
	requestIDs := make(map[string]struct{}, len(ordered))
	digests := make([]string, 0, len(ordered))
	outcome := OutcomeApproved
	for _, review := range ordered {
		if err := review.Validate(identity, subject); err != nil {
			return nil, "", err
		}
		if _, duplicate := reviewers[review.ReviewerID]; duplicate {
			return nil, "", errors.New("design reviews contain duplicates")
		}
		reviewers[review.ReviewerID] = struct{}{}
		if _, duplicate := requestIDs[review.Invocation.RequestID]; duplicate {
			return nil, "", errors.New("design review request ids contain duplicates")
		}
		requestIDs[review.Invocation.RequestID] = struct{}{}
		digests = append(digests, review.ReviewSHA256)
		if review.Verdict == VerdictRevise {
			outcome = OutcomeRevise
		}
	}
	if outcome == OutcomeRevise && round == maxRounds {
		outcome = OutcomeNonconverged
	}
	return digests, outcome, nil
}

func designReviewDigest(record DesignReview) (string, error) {
	record.ReviewSHA256 = ""
	encoded, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	return digestBytes(encoded), nil
}

func designDecisionDigest(record DesignDecision) (string, error) {
	record.DecisionSHA256 = ""
	encoded, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	return digestBytes(encoded), nil
}

// ReadDesignReview and ReadDesignDecision load sealed records from disk
// without validating them; callers validate against their own inputs.
func ReadDesignReview(path string) (DesignReview, error) {
	var record DesignReview
	encoded, err := os.ReadFile(path)
	if err != nil {
		return DesignReview{}, err
	}
	return record, decodeStrict(encoded, &record)
}

func ReadDesignDecision(path string) (DesignDecision, error) {
	var record DesignDecision
	encoded, err := os.ReadFile(path)
	if err != nil {
		return DesignDecision{}, err
	}
	return record, decodeStrict(encoded, &record)
}
