package hook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	TerminalReportProtocolVersion         = "terminal-report-v1"
	TerminalReportPath                    = "/terminal-report/v1"
	TerminalReportSignatureHeader         = "x-terminal-report-signature"
	TerminalReportResponseSignatureHeader = "x-terminal-report-response-signature"
	MaxTerminalReportRequestBytes         = 16 * 1024
	MaxTerminalReportClockSkew            = 10 * time.Minute
	MaxTerminalReportLease                = 10 * time.Minute
)

var (
	repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)
	pullURLPattern    = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9_.-]{1,100})/([A-Za-z0-9_.-]{1,100})/pull/([1-9][0-9]{0,18})$`)
	runURLPattern     = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9_.-]{1,100})/([A-Za-z0-9_.-]{1,100})/actions/runs/([1-9][0-9]{0,18})/attempts/([1-9][0-9]{0,9})$`)
	// localRunURLPattern is the run reference of a pod-resident engine run
	// (docs: HERMES_AS_LASSDAS_RUNTIME): no workflow page exists to link,
	// but the reference still seals the same three identities — repository,
	// run id, attempt — so a stored record binds to exactly one engine run.
	localRunURLPattern = regexp.MustCompile(`^local-run://([A-Za-z0-9_.-]{1,100})/([A-Za-z0-9_.-]{1,100})/([1-9][0-9]{0,18})/attempts/([1-9][0-9]{0,9})$`)
)

type TerminalCode string

const (
	TerminalSuccess                        TerminalCode = "success"
	TerminalInputRejected                  TerminalCode = "input_rejected"
	TerminalReadinessRejected              TerminalCode = "readiness_rejected"
	TerminalClarificationRequired          TerminalCode = "clarification_required"
	TerminalReadinessUnresolved            TerminalCode = "readiness_unresolved"
	TerminalClarificationExpired           TerminalCode = "clarification_expired"
	TerminalCancelled                      TerminalCode = "cancelled"
	TerminalModelFailed                    TerminalCode = "model_failed"
	TerminalNonconverged                   TerminalCode = "nonconverged"
	TerminalValidationFailed               TerminalCode = "validation_failed"
	TerminalReleaseFailed                  TerminalCode = "release_failed"
	TerminalProductionDeploymentUnverified TerminalCode = "production_deployment_unverified"
	TerminalProductionVerificationFailed   TerminalCode = "production_verification_failed"
	TerminalInternalFailed                 TerminalCode = "internal_failed"
	// The investigating designer's endings (docs/INVESTIGATING_DESIGNER.md
	// §4.4, §5): a sealed investigation report delivered without a pull
	// request; a round whose probe budget or wall ran out before a record
	// was sealed; a report the evidence review never passed; a design the
	// design reviews never agreed on.
	TerminalInvestigated              TerminalCode = "investigated"
	TerminalInvestigationIncomplete   TerminalCode = "investigation_incomplete"
	TerminalInvestigationNonconverged TerminalCode = "investigation_nonconverged"
	TerminalDesignNonconverged        TerminalCode = "design_nonconverged"
	// TerminalDesignRoundsSpent is the other way a delivery can run out of
	// design: the design's own judges agreed, the change was written, and
	// then a reviewer of that change - or the applier - said the plan
	// itself was wrong, with no design round left to change it. Reporting
	// that as design_nonconverged sent a requester whose design reviews all
	// passed looking for a disagreement that never happened (live
	// 2026-09-17).
	TerminalDesignRoundsSpent TerminalCode = "design_rounds_spent"
	// TerminalImplementationReturned is the implementing agent handing the
	// request back: it is told to change nothing and say why when it
	// cannot carry the request out, and what it wrote is an answer, not a
	// breakdown. Reporting it as model_failed told a requester the AI had
	// failed on a run where the AI had explained itself and the
	// explanation was the only thing missing from the ticket (live
	// 2026-09-25).
	TerminalImplementationReturned TerminalCode = "implementation_returned"
)

// Valid reports whether c is one of the terminal codes the automation ends
// with. The ledger and the reporter defer to it so a code added here is
// accepted everywhere a report travels.
func (c TerminalCode) Valid() bool { return c.valid() }

// AllTerminalCodes is every ending the automation can reach, in one place so
// the checks that must cover all of them enumerate this rather than a list
// each keeps by hand. Two such lists had already fallen four codes behind
// (live 2026-09-17).
func AllTerminalCodes() []TerminalCode {
	return []TerminalCode{
		TerminalSuccess, TerminalInputRejected, TerminalReadinessRejected, TerminalClarificationRequired,
		TerminalReadinessUnresolved, TerminalClarificationExpired, TerminalCancelled,
		TerminalModelFailed, TerminalNonconverged,
		TerminalValidationFailed, TerminalReleaseFailed, TerminalProductionDeploymentUnverified,
		TerminalProductionVerificationFailed, TerminalInternalFailed,
		TerminalInvestigated, TerminalInvestigationIncomplete, TerminalInvestigationNonconverged,
		TerminalDesignNonconverged, TerminalDesignRoundsSpent, TerminalImplementationReturned,
	}
}

func (c TerminalCode) valid() bool {
	for _, known := range AllTerminalCodes() {
		if c == known {
			return true
		}
	}
	return false
}

// Delivery names how far a run travels without a person. The hook holds it per
// destination so it can require exactly the evidence that stopping point
// produces: a run that only proposes a change cannot claim a deployment, and a
// run that was configured to reach production cannot claim success without one.
const (
	DeliverPullRequest = "pull_request"
	DeliverIntegration = "integration"
	DeliverProduction  = "production"
)

// validDelivery reports whether a value names one of the three depths.
func validDelivery(delivery string) bool {
	switch delivery {
	case DeliverPullRequest, DeliverIntegration, DeliverProduction:
		return true
	default:
		return false
	}
}

// deliveryDepthRank orders the three depths so a report can be checked
// against the one its destination was configured for. A run may stop short
// of what was configured and say so; it may never claim more.
func deliveryDepthRank(delivery string) int {
	switch delivery {
	case DeliverIntegration:
		return 1
	case DeliverProduction:
		return 2
	default:
		return 0
	}
}

// ReportDestination is one place the automation may deliver to, as the hook
// knows it. Nothing about a destination is inferred: a report naming a
// repository absent from this list is refused.
type ReportDestination struct {
	Kind             string `json:"kind,omitempty"`
	Repository       string `json:"repository"`
	Delivery         string `json:"delivery"`
	StagingOrigin    string `json:"staging_origin"`
	ProductionOrigin string `json:"production_origin"`
}

// EffectiveKind treats existing report destinations as web destinations.
func (d ReportDestination) EffectiveKind() string {
	if d.Kind == "" {
		return "web"
	}
	return d.Kind
}

// Validate checks the destination before the runtime opens its ledger.
func (d ReportDestination) Validate() error { return d.validate() }

func (d ReportDestination) validate() error {
	if !repositoryPattern.MatchString(d.Repository) {
		return errors.New("report destination repository is invalid")
	}
	switch d.EffectiveKind() {
	case "cli":
		if d.Delivery != DeliverPullRequest || d.StagingOrigin != "" || d.ProductionOrigin != "" {
			return errors.New("cli report destination must stop at pull_request without origins")
		}
		return nil
	case "web":
	default:
		return errors.New("report destination kind is invalid")
	}
	switch d.Delivery {
	case DeliverPullRequest, DeliverIntegration, DeliverProduction:
	default:
		return errors.New("report destination delivery is invalid")
	}
	if !validEvidenceOrigin(d.StagingOrigin) || !validEvidenceOrigin(d.ProductionOrigin) ||
		d.StagingOrigin == d.ProductionOrigin {
		return errors.New("report destination origins are invalid")
	}
	return nil
}

type ReportRouteConfig struct {
	HMACKey             []byte
	RepositoryID        int64
	RepositorySHA256    string
	WorkflowRefSHA256   string
	ExpectedRunID       string
	Destinations        []ReportDestination
	ClockSkew           time.Duration
	LeaseDuration       time.Duration
	SpaceKey            string
	ProjectID           int64
	ProjectKey          string
	AllowedCreatorID    int64
	AllowedActivityType int
	Target              DeliveryTarget
	// RunReferenceScheme names the run-reference form this deployment seals:
	// "" or "github" for workflow runs (the only form before the pod
	// constitution), "local" for pod-resident runs. The other form is
	// refused, so a workflow deployment cannot seal an unclickable local
	// reference and a pod cannot seal a fabricated workflow link.
	RunReferenceScheme string
}

func runReferenceSchemeAllowed(raw, scheme string) bool {
	if scheme == "local" {
		return localRunURLPattern.MatchString(raw)
	}
	return runURLPattern.MatchString(raw)
}

// DestinationFor resolves the destination a report names. The match is exact;
// nothing is guessed from a prefix or an owner.
func (c ReportRouteConfig) DestinationFor(repository string) (ReportDestination, error) {
	for _, destination := range c.Destinations {
		if destination.Repository == repository {
			return destination, nil
		}
	}
	return ReportDestination{}, errors.New("report repository is not a configured destination")
}

func (c ReportRouteConfig) Validate() error {
	if err := ValidatePullKey(c.HMACKey); err != nil {
		return errors.New("report authentication is invalid")
	}
	if c.RepositoryID <= 0 || !validIdentityDigest(c.RepositorySHA256) || !validIdentityDigest(c.WorkflowRefSHA256) {
		return errors.New("report repository identity is invalid")
	}
	if len(c.Destinations) == 0 || len(c.Destinations) > 8 {
		return errors.New("report destinations are invalid")
	}
	seen := make(map[string]struct{}, len(c.Destinations))
	for _, destination := range c.Destinations {
		if err := destination.validate(); err != nil {
			return err
		}
		if _, exists := seen[destination.Repository]; exists {
			return errors.New("report destinations contain duplicates")
		}
		seen[destination.Repository] = struct{}{}
	}
	if c.ClockSkew <= 0 || c.ClockSkew > MaxTerminalReportClockSkew ||
		c.LeaseDuration <= 0 || c.LeaseDuration > MaxTerminalReportLease {
		return errors.New("report timing is invalid")
	}
	if !runIDPattern.MatchString(c.ExpectedRunID) || !componentPattern.MatchString(c.SpaceKey) ||
		c.ProjectID <= 0 || !componentPattern.MatchString(c.ProjectKey) || c.AllowedCreatorID <= 0 || c.AllowedActivityType <= 0 {
		return errors.New("report allowlist is invalid")
	}
	if c.Target.RepositoryID != c.RepositoryID || c.Target.WorkflowRefSHA256 != c.WorkflowRefSHA256 || c.Target.Validate() != nil {
		return errors.New("report target binding is invalid")
	}
	return nil
}

func (c ReportRouteConfig) isZero() bool {
	return len(c.HMACKey) == 0 && c.RepositoryID == 0 && c.RepositorySHA256 == "" && c.WorkflowRefSHA256 == "" &&
		c.ExpectedRunID == "" && len(c.Destinations) == 0 && c.ClockSkew == 0 &&
		c.LeaseDuration == 0 && c.SpaceKey == "" && c.ProjectID == 0 && c.ProjectKey == "" &&
		c.AllowedCreatorID == 0 && c.AllowedActivityType == 0 && c.Target == (DeliveryTarget{})
}

type TerminalReportRequest struct {
	Protocol              string       `json:"protocol"`
	DeliveryID            string       `json:"delivery_id"`
	InputSHA256           string       `json:"input_sha256"`
	RepositoryID          int64        `json:"repository_id"`
	RepositorySHA256      string       `json:"repository_sha256"`
	WorkflowRefSHA256     string       `json:"workflow_ref_sha256"`
	WorkflowSHA           string       `json:"workflow_sha"`
	WorkflowRunID         int64        `json:"workflow_run_id"`
	RunAttempt            int          `json:"run_attempt"`
	AutomationRunID       string       `json:"automation_run_id"`
	Code                  TerminalCode `json:"code"`
	Repository            string       `json:"repository"`
	RunURL                string       `json:"run_url"`
	PullRequestURL        string       `json:"pull_request_url"`
	CommitSHA             string       `json:"commit_sha"`
	CommitURL             string       `json:"commit_url"`
	StagingEvidenceURL    string       `json:"staging_evidence_url"`
	ProductionEvidenceURL string       `json:"production_evidence_url"`
	TrailText             string       `json:"trail_text,omitempty"`
	// SpendText is what this run was billed, rendered for the requester.
	// Empty when no reading was available — the report then simply omits the
	// cost line rather than printing a zero that reads as "this was free".
	SpendText string `json:"spend_text,omitempty"`
	// IncompleteReason and IncompleteObjection carry, for an
	// investigation_incomplete end, why the round sealed nothing and the
	// last objection the contract raised against the role's answer, so the
	// requester-facing text can say whether the budget ran out (narrowing
	// the request helps) or the answers kept being refused (it does not).
	IncompleteReason    string `json:"incomplete_reason,omitempty"`
	IncompleteObjection string `json:"incomplete_objection,omitempty"`
	// FailedStep names, in the requester's words, the step of the work that
	// could not be completed. A model_failed end is the most common way a
	// run stops, and without this the requester and the operator are both
	// told only that "generation or review" failed — which step it was lived
	// in the container log alone, and a release erases that (a live run, 2026-09-09:
	// the cause was gone before it was read). No count is
	// written here on purpose: three successive attempts to state one in a
	// comment were wrong, and the list lives in runtime.AllStages.
	// Empty means an older engine's report, which keeps the older sentence.
	FailedStep string `json:"failed_step,omitempty"`
	// ModelFailureReason is a fixed explanation, like FailedStep, rather than
	// a new terminal outcome. Empty preserves reports from older engines.
	ModelFailureReason string `json:"model_failure_reason,omitempty"`
	// ReachedDelivery is how far this run actually carried the change, which
	// is not always how far its destination was configured to go: a
	// destination that asks for production and has no release path
	// configured is delivered to its pull request, and saying so is the
	// report's job.
	//
	// Empty is what every report written before the delivery depth moved
	// inside the run carries, and it keeps the older rule exactly — the
	// evidence is judged against the destination's own setting. A value
	// never reaches past that setting; it only ever says the run stopped
	// short of it, and the evidence is then judged against where it stopped.
	ReachedDelivery string `json:"reached_delivery,omitempty"`
	// DeliveryShortfall says, in one requester-facing line, what a deeper
	// delivery would have needed. Set only alongside a ReachedDelivery
	// shallower than the destination asked for.
	DeliveryShortfall string    `json:"delivery_shortfall,omitempty"`
	IssuedAt          time.Time `json:"issued_at"`
}

// MaxDeliveryShortfallBytes bounds the shortfall line, on the same footing
// as every other requester-facing string the report carries: one bounded
// plain line, never a path or a log.
const MaxDeliveryShortfallBytes = 400

// MaxTrailRecordBytes bounds the whole composed run record: the version the
// composer writes, the pull request body carries and the run directory keeps.
// The carrier that takes it whole is the pull request body -- GitHub allows
// 65,536 characters there and the API client refuses a body over 64 KiB of
// bytes -- so this leaves the digest header above the record room of its own.
// It lives in this package because every reader of the record file already
// depends on it and none of them should be guessing a different number.
const MaxTrailRecordBytes = 60 * 1024

// MaxTerminalTrailBytes bounds the run record one terminal report may carry
// to the ticket. It is not the composer's bound any more: the whole record
// goes to the pull request body, which holds far more, and this is only what
// the report envelope (MaxTerminalReportRequestBytes) can carry beside the
// report's other fields. A record longer than this is shortened for the
// comment by ShortenTrailForComment, which says so in the comment, never cut
// in silence.
const MaxTerminalTrailBytes = 8 * 1024

// TrailShortenedNote is what a shortened run record ends with. It replaces a
// bare ellipsis, which reads as "that was all there was": a live run
// (2026-09-25) answered a requester's acceptance criterion in the
// implementer's report, the cut fell before the answer, and nothing on the
// ticket said there was more to read.
const TrailShortenedNote = "\n…（この記録はコメントに収まらないため、ここまでを掲示しています。" +
	"全文は上の実行履歴に残してあり、Pull Request がある依頼ではその説明にも全文があります）\n"

// ShortenTrailForComment cuts a run record down to a byte budget and says,
// in the requester's words, that it was cut and where the whole record is. A
// record that already fits comes back untouched, and the cut lands on a line
// boundary so the record stays readable and valid UTF-8.
func ShortenTrailForComment(trail string, limit int) string {
	if len(trail) <= limit {
		return trail
	}
	budget := limit - len(TrailShortenedNote)
	if budget <= 0 {
		return ""
	}
	clipped := trail[:budget]
	// The cut lands on a character, not on the end of the last whole line.
	// Ending on a line was tidier but cost an unbounded amount of text: a
	// section whose body is one long paragraph has no break of its own, so
	// the last line boundary in reach is the heading above it, and the
	// whole body went. The comment then showed a heading, the note saying
	// there was more, and none of what it was introducing, while the
	// comment's own fixed fields told the requester to read it.
	for len(clipped) > 0 {
		if r, size := utf8.DecodeLastRuneInString(clipped); r != utf8.RuneError || size > 1 {
			break
		}
		clipped = clipped[:len(clipped)-1]
	}
	return clipped + TrailShortenedNote
}

// MaxFailedStepBytes bounds the step name a model_failed report carries. The
// names are a fixed short vocabulary; the bound is what stops anything else
// from arriving in their place. It is exported because the side that builds
// the name has to hold itself to it: a name past the bound fails the shape
// check, and a report that fails the shape check never reaches the
// requester at all — the run retries for ever instead of ending.
const MaxFailedStepBytes = 120

const ModelFailureBudgetExhausted = "budget_exhausted"

// BudgetFailureAction describes an operator action; it does not authorise a
// budget increase or change the terminal run into a resumable hold.
const BudgetFailureAction = "運用担当者が、終了した役に設定されているモデル利用枠と残高を確認し、必要な承認を得て利用枠を確保してください。依頼者の再起票は不要です。"

// ValidateTrailText holds the trail to the same plain-text discipline as
// every other requester-facing string: bounded, valid UTF-8, newlines only.
func ValidateTrailText(value string) error {
	return ValidateTrailTextWithin(value, MaxTerminalTrailBytes)
}

// ValidateTrailTextWithin is ValidateTrailText against the caller's own
// bound. The two carriers of the record have very different room: a pull
// request body takes the whole thing, a ticket comment takes what one comment
// holds. Both still owe the same plain-text discipline.
func ValidateTrailTextWithin(value string, limit int) error {
	if len(value) > limit || !utf8.ValidString(value) ||
		strings.ContainsAny(value, "\x00\r") {
		return errors.New("trail text is invalid")
	}
	return nil
}

func (r TerminalReportRequest) ValidateShape() error {
	if r.Protocol != TerminalReportProtocolVersion || !validDeliveryID(r.DeliveryID) || !validDigest(r.InputSHA256) ||
		r.RepositoryID <= 0 || !validIdentityDigest(r.RepositorySHA256) || !validIdentityDigest(r.WorkflowRefSHA256) ||
		!commitPattern.MatchString(r.WorkflowSHA) || r.WorkflowRunID <= 0 || r.RunAttempt <= 0 ||
		!runIDPattern.MatchString(r.AutomationRunID) || !r.Code.valid() {
		return errors.New("terminal report identity is invalid")
	}
	// A report names a destination exactly when it claims something happened
	// there. A stop before any repository work names none, and a report that
	// shows a pull request or a deployment must say where.
	claimsEvidence := r.PullRequestURL != "" || r.CommitSHA != "" || r.StagingEvidenceURL != "" || r.ProductionEvidenceURL != ""
	if r.Repository != "" && !repositoryPattern.MatchString(r.Repository) {
		return errors.New("terminal report repository is invalid")
	}
	if claimsEvidence && r.Repository == "" {
		return errors.New("terminal report evidence names no destination")
	}
	if r.IssuedAt.IsZero() || !r.IssuedAt.Equal(r.IssuedAt.UTC()) {
		return errors.New("terminal report timestamp is invalid")
	}
	if r.RunURL == "" || len(r.RunURL) > 512 || len(r.PullRequestURL) > 512 || len(r.CommitURL) > 512 ||
		len(r.StagingEvidenceURL) > 2048 || len(r.ProductionEvidenceURL) > 2048 {
		return errors.New("terminal report evidence shape is invalid")
	}
	if err := ValidateTrailText(r.TrailText); err != nil {
		return err
	}
	// The step name is requester-facing text on the same footing as every
	// other such string: one bounded plain line, never a path or a log.
	if len(r.FailedStep) > MaxFailedStepBytes || !utf8.ValidString(r.FailedStep) ||
		strings.ContainsAny(r.FailedStep, "\x00\r\n") {
		return errors.New("terminal report failed step is invalid")
	}
	if r.ModelFailureReason != "" && (r.ModelFailureReason != ModelFailureBudgetExhausted || r.Code != TerminalModelFailed) {
		return errors.New("terminal report model failure reason is invalid")
	}
	// A depth belongs to an ending that got somewhere: a delivery that
	// finished, or one the requester stopped after part of it had already
	// landed. Every other ending either reached nowhere or reached
	// somewhere it is already forbidden to claim.
	if r.ReachedDelivery != "" && (!validDelivery(r.ReachedDelivery) ||
		(r.Code != TerminalSuccess && r.Code != TerminalCancelled)) {
		return errors.New("terminal report reached delivery is invalid")
	}
	if len(r.DeliveryShortfall) > MaxDeliveryShortfallBytes || !utf8.ValidString(r.DeliveryShortfall) ||
		strings.ContainsAny(r.DeliveryShortfall, "\x00\r\n") ||
		(r.DeliveryShortfall != "" && r.ReachedDelivery == "") {
		return errors.New("terminal report delivery shortfall is invalid")
	}
	if (r.CommitSHA == "") != (r.CommitURL == "") || (r.CommitSHA != "" && !commitPattern.MatchString(r.CommitSHA)) {
		return errors.New("terminal report commit binding is invalid")
	}
	return nil
}

func (r TerminalReportRequest) ValidateRoute(config ReportRouteConfig) error {
	if err := config.Validate(); err != nil {
		return errors.New("terminal report route configuration is invalid")
	}
	if err := r.ValidateShape(); err != nil {
		return err
	}
	if r.RepositoryID != config.RepositoryID || r.RepositorySHA256 != config.RepositorySHA256 ||
		r.WorkflowRefSHA256 != config.WorkflowRefSHA256 || r.AutomationRunID != config.ExpectedRunID {
		return errors.New("terminal report route is not allowed")
	}
	if !validRunURL(r.RunURL, config.RepositorySHA256, r.WorkflowRunID, r.RunAttempt) ||
		!runReferenceSchemeAllowed(r.RunURL, config.RunReferenceScheme) {
		return errors.New("terminal report run url is invalid")
	}
	if r.Repository == "" {
		// Nothing was delivered anywhere, so there is no destination to check
		// against; the code-specific rules below still forbid any evidence.
		return validateTerminalEvidenceShape(r, ReportDestination{})
	}
	destination, err := config.DestinationFor(r.Repository)
	if err != nil {
		return errors.New("terminal report repository is not allowed")
	}
	if r.PullRequestURL != "" && !validPullRequestURL(r.PullRequestURL, destination.Repository) {
		return errors.New("terminal report pull request url is invalid")
	}
	if r.CommitSHA != "" {
		expectedCommitURL := "https://github.com/" + destination.Repository + "/commit/" + r.CommitSHA
		if r.CommitURL != expectedCommitURL {
			return errors.New("terminal report commit url is invalid")
		}
	}
	if r.StagingEvidenceURL != "" && !validEvidenceURL(r.StagingEvidenceURL, destination.StagingOrigin) {
		return errors.New("terminal report staging evidence url is invalid")
	}
	if r.ProductionEvidenceURL != "" && !validEvidenceURL(r.ProductionEvidenceURL, destination.ProductionOrigin) {
		return errors.New("terminal report production evidence url is invalid")
	}
	return validateTerminalEvidenceShape(r, destination)
}

// validateTerminalEvidenceShape checks that the evidence a report carries
// matches both its outcome and how far its destination was configured to go.
func validateTerminalEvidenceShape(r TerminalReportRequest, destination ReportDestination) error {
	switch r.Code {
	case TerminalSuccess:
		return evidenceMatchesDepth(r, destination)
	case TerminalCancelled:
		// A stop before anything left the pod claims nothing, exactly as
		// every pre-generation stop does.
		//
		// A stop after part of the delivery had landed says where it got
		// to and carries that depth's evidence. The release branch moved
		// and the deployment ran; a report stripped of every link, over a
		// footer saying production is untouched, would be false about the
		// environment the requester now has to look at.
		if r.ReachedDelivery == "" {
			if r.PullRequestURL != "" || r.CommitSHA != "" || r.StagingEvidenceURL != "" || r.ProductionEvidenceURL != "" {
				return errors.New("a stop that reached nowhere cannot claim repository evidence")
			}
			return nil
		}
		return evidenceMatchesDepth(r, destination)
	case TerminalProductionDeploymentUnverified, TerminalProductionVerificationFailed:
		if r.PullRequestURL == "" || r.CommitSHA == "" || r.StagingEvidenceURL == "" || r.ProductionEvidenceURL != "" {
			return errors.New("post-promotion failure evidence is invalid")
		}
	case TerminalReadinessRejected, TerminalClarificationRequired, TerminalReadinessUnresolved,
		TerminalClarificationExpired:
		if r.PullRequestURL != "" || r.CommitSHA != "" || r.StagingEvidenceURL != "" || r.ProductionEvidenceURL != "" {
			return errors.New("pre-generation stop cannot claim repository evidence")
		}
	default:
		if r.ProductionEvidenceURL != "" {
			return errors.New("failed terminal report cannot claim production evidence")
		}
	}
	return nil
}

// evidenceMatchesDepth checks a report against the depth it got to: the
// evidence it must carry is exactly what that stopping point produces — no
// less, and nothing it never reached.
//
// Which stopping point is the destination's setting, unless the report
// names a shallower one it actually reached. That is the one direction this
// opens: a destination configured for production whose release path is not
// configured is delivered to its pull request and says so, and is then
// judged against the pull request. Claiming more than the destination
// allows stays refused, and so does naming a depth that is not one of the
// three.
func evidenceMatchesDepth(r TerminalReportRequest, destination ReportDestination) error {
	reached := destination.Delivery
	if r.ReachedDelivery != "" {
		if !validDelivery(r.ReachedDelivery) ||
			deliveryDepthRank(r.ReachedDelivery) > deliveryDepthRank(destination.Delivery) {
			return errors.New("a report cannot claim a depth its destination was not configured for")
		}
		reached = r.ReachedDelivery
	}
	switch reached {
	case DeliverPullRequest:
		if r.PullRequestURL == "" {
			return errors.New("terminal report is missing the evidence of the depth it names")
		}
		if r.CommitSHA != "" || r.StagingEvidenceURL != "" || r.ProductionEvidenceURL != "" {
			return errors.New("a proposal cannot claim a deployment")
		}
	case DeliverIntegration:
		if r.PullRequestURL == "" || r.CommitSHA == "" || r.StagingEvidenceURL == "" {
			return errors.New("terminal report is missing the evidence of the depth it names")
		}
		if r.ProductionEvidenceURL != "" {
			return errors.New("a delivery that reached staging cannot claim production evidence")
		}
	default:
		if r.PullRequestURL == "" || r.CommitSHA == "" || r.StagingEvidenceURL == "" || r.ProductionEvidenceURL == "" {
			return errors.New("terminal report is missing the evidence of the depth it names")
		}
	}
	return nil
}

func MarshalTerminalReportRequest(request TerminalReportRequest) ([]byte, error) {
	request.IssuedAt = request.IssuedAt.UTC()
	if err := request.ValidateShape(); err != nil {
		return nil, err
	}
	return json.Marshal(request)
}

func DecodeTerminalReportRequest(encoded []byte) (TerminalReportRequest, error) {
	if len(encoded) == 0 || len(encoded) > MaxTerminalReportRequestBytes {
		return TerminalReportRequest{}, errors.New("terminal report request size is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var request TerminalReportRequest
	if err := decoder.Decode(&request); err != nil {
		return TerminalReportRequest{}, errors.New("terminal report request is invalid")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return TerminalReportRequest{}, errors.New("terminal report request is invalid")
	}
	canonical, err := MarshalTerminalReportRequest(request)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return TerminalReportRequest{}, errors.New("terminal report request is not canonical")
	}
	return request, nil
}

type terminalReportRecord struct {
	Protocol              string       `json:"protocol"`
	DeliveryID            string       `json:"delivery_id"`
	InputSHA256           string       `json:"input_sha256"`
	RepositoryID          int64        `json:"repository_id"`
	RepositorySHA256      string       `json:"repository_sha256"`
	WorkflowRefSHA256     string       `json:"workflow_ref_sha256"`
	WorkflowSHA           string       `json:"workflow_sha"`
	WorkflowRunID         int64        `json:"workflow_run_id"`
	RunAttempt            int          `json:"run_attempt"`
	AutomationRunID       string       `json:"automation_run_id"`
	Code                  TerminalCode `json:"code"`
	Repository            string       `json:"repository"`
	RunURL                string       `json:"run_url"`
	PullRequestURL        string       `json:"pull_request_url"`
	CommitSHA             string       `json:"commit_sha"`
	CommitURL             string       `json:"commit_url"`
	StagingEvidenceURL    string       `json:"staging_evidence_url"`
	ProductionEvidenceURL string       `json:"production_evidence_url"`
}

// MarshalTerminalReportRecord returns the immutable terminal outcome. IssuedAt
// authenticates one HTTP attempt and is deliberately excluded so a retry can
// use a fresh timestamp without becoming a different terminal result.
func MarshalTerminalReportRecord(request TerminalReportRequest) ([]byte, error) {
	if err := request.ValidateShape(); err != nil {
		return nil, err
	}
	return json.Marshal(terminalReportRecord{
		Protocol: request.Protocol, DeliveryID: request.DeliveryID, InputSHA256: request.InputSHA256,
		RepositoryID: request.RepositoryID, RepositorySHA256: request.RepositorySHA256,
		WorkflowRefSHA256: request.WorkflowRefSHA256, WorkflowSHA: request.WorkflowSHA,
		WorkflowRunID: request.WorkflowRunID, RunAttempt: request.RunAttempt, AutomationRunID: request.AutomationRunID,
		Code: request.Code, RunURL: request.RunURL, PullRequestURL: request.PullRequestURL,
		CommitSHA: request.CommitSHA, CommitURL: request.CommitURL,
		StagingEvidenceURL: request.StagingEvidenceURL, ProductionEvidenceURL: request.ProductionEvidenceURL,
	})
}

func TerminalReportDigest(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

func SignTerminalReportRequest(key, body []byte) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(TerminalReportProtocolVersion + "\nrequest\nPOST\n" + TerminalReportPath + "\n"))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func VerifyTerminalReportRequestSignature(key, body []byte, signature string) bool {
	expected := SignTerminalReportRequest(key, body)
	return len(signature) == len(expected) && hmac.Equal([]byte(signature), []byte(expected))
}

func SignTerminalReportResponse(key []byte, status int, requestBody, responseBody []byte) string {
	requestDigest := sha256.Sum256(requestBody)
	responseDigest := sha256.Sum256(responseBody)
	message := strings.Join([]string{
		TerminalReportProtocolVersion,
		"response",
		strconv.Itoa(status),
		hex.EncodeToString(requestDigest[:]),
		hex.EncodeToString(responseDigest[:]),
	}, "\n")
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(message))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func VerifyTerminalReportResponseSignature(key []byte, status int, requestBody, responseBody []byte, signature string) bool {
	expected := SignTerminalReportResponse(key, status, requestBody, responseBody)
	return len(signature) == len(expected) && hmac.Equal([]byte(signature), []byte(expected))
}

func validEvidenceOrigin(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.Port() == "" && parsed.User == nil &&
		parsed.Path == "" && parsed.RawPath == "" && parsed.RawQuery == "" && parsed.Fragment == "" && parsed.String() == raw
}

func validEvidenceURL(raw, origin string) bool {
	if len(raw) == 0 || len(raw) > 2048 || strings.ContainsAny(raw, "\r\n") || !validEvidenceOrigin(origin) {
		return false
	}
	parsed, err := url.Parse(raw)
	base, baseErr := url.Parse(origin)
	if err != nil || baseErr != nil || parsed.Scheme != base.Scheme || parsed.Host != base.Host || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" || parsed.Path == "" || parsed.Path[0] != '/' ||
		path.Clean(parsed.Path) != parsed.Path || parsed.String() != raw {
		return false
	}
	return !strings.Contains(parsed.EscapedPath(), "\\")
}

func validPullRequestURL(raw, repository string) bool {
	matches := pullURLPattern.FindStringSubmatch(raw)
	return len(matches) == 4 && matches[1]+"/"+matches[2] == repository
}

func validRunURL(raw, repositoryDigest string, workflowRunID int64, runAttempt int) bool {
	matches := runURLPattern.FindStringSubmatch(raw)
	if len(matches) != 5 {
		matches = localRunURLPattern.FindStringSubmatch(raw)
	}
	if len(matches) != 5 || HashIdentity(matches[1]+"/"+matches[2]) != repositoryDigest {
		return false
	}
	runID, runErr := strconv.ParseInt(matches[3], 10, 64)
	attempt, attemptErr := strconv.Atoi(matches[4])
	return runErr == nil && attemptErr == nil && runID == workflowRunID && attempt == runAttempt
}
