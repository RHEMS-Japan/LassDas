package worker

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/worker/investigate"
)

// The design reviewers judge the investigating designer's sealed records
// before any code is written (docs/INVESTIGATING_DESIGNER.md §5). They are
// the configured reviewing agents, launched exactly as a code review is;
// what differs is the subject (a sealed investigation report or design, not
// a candidate), the lens, and the record the verdict is sealed into.

// The built-in design lenses. A reviewer's own design_lens replaces them;
// without one the first configured reviewer judges the evidence and the
// second the approach.
const (
	// DesignLensEvidence asks whether the cause stands on measurements:
	// is anything standing on inferred findings alone, and was something
	// left unmeasured that should have been measured.
	DesignLensEvidence = "根拠: 原因 (cause) は measured の実測に支えられているか。inferred の所見だけで立っている結論はないか。計るべきなのに計っていないものはないか。"
	// DesignLensApproach asks whether the plan is the right and smallest
	// fix: are the files necessary and sufficient, what side effects were
	// missed, can the verification judge this change.
	DesignLensApproach = "方針: 触るファイル (files) は必要十分か。副作用 (blast_radius) の見落としはないか。確認方法 (verification) はこの変更の成否を判定できるか。より小さい直し方はないか。"
)

// Lens selectors the command line accepts: the two built-in seats.
const (
	DesignLensSelectorEvidence = "A"
	DesignLensSelectorApproach = "B"
)

// ResolveDesignLens picks the lens a reviewer judges a subject under. An
// explicit selector names a built-in seat; without one the reviewer's own
// design_lens applies, else its position among the configured reviewers.
// An investigation report has no approach to judge, so the approach lens
// is refused for it.
func ResolveDesignLens(config Config, reviewerID, selector, subject string) (string, error) {
	position := -1
	for index, candidate := range config.Models.Reviewers {
		if candidate.ID == reviewerID {
			position = index
			break
		}
	}
	if position < 0 {
		return "", errors.New("design reviewer is not configured")
	}
	// The lens comes from the judge that runs (its own design_lens when the
	// design judges are configured), the position from the reviewer list.
	endpoint, _ := config.Models.DesignReviewerFor(reviewerID)
	var lens string
	switch selector {
	case DesignLensSelectorEvidence:
		lens = DesignLensEvidence
	case DesignLensSelectorApproach:
		lens = DesignLensApproach
	case "":
		switch {
		case endpoint.DesignLens != "":
			lens = endpoint.DesignLens
		case position == 0:
			lens = DesignLensEvidence
		case position == 1:
			lens = DesignLensApproach
		default:
			return "", errors.New("design reviewer has no design lens")
		}
	default:
		return "", errors.New("design lens selector must be A or B")
	}
	if subject == investigate.SubjectInvestigation && lens == DesignLensApproach {
		return "", errors.New("an investigation report is judged under the evidence lens only")
	}
	return lens, nil
}

// DecodeAgentDesignReviewOutput reads the verdict out of what the design
// reviewing agent printed, by the rule DecodeAgentReviewOutput applies: the
// text up to the echoed instruction's final rule line is ignored and the
// last balanced verdict object is the answer. The object is then decoded
// strictly into the design review shape, so a finding carrying a path or a
// line - the candidate review's fields - is refused rather than dropped.
func DecodeAgentDesignReviewOutput(transcript string) (investigate.ModelDesignReviewOutput, error) {
	if len(transcript) > MaxAgentTranscriptBytes {
		return investigate.ModelDesignReviewOutput{}, errors.New("transcript is too large")
	}
	if boundary := strings.LastIndex(transcript, ReviewAnswerRulesTail); boundary >= 0 {
		transcript = transcript[boundary+len(ReviewAnswerRulesTail):]
	}
	block, err := lastJSONObject(transcript)
	if err != nil {
		return investigate.ModelDesignReviewOutput{}, errors.New("the design reviewing agent did not report a verdict")
	}
	var output investigate.ModelDesignReviewOutput
	if err := decodeStrictJSON([]byte(block), &output); err != nil {
		return investigate.ModelDesignReviewOutput{}, errors.New("model design review response is invalid")
	}
	for i, finding := range output.Findings {
		output.Findings[i].Code = investigate.NormalizeFindingCode(finding.Code)
	}
	return output, nil
}

// AgentDesignReviewFromRun turns a design reviewing agent's run into the
// sealed DesignReview. The run must be the reviewer's own launch, bound to
// the same delivery, configuration, engine revision and baseline as the
// subject and to the subject's round; the invocation evidence is measured
// bytes, as for every agent run.
func AgentDesignReviewFromRun(
	endpoint ModelEndpoint,
	lens string,
	run AgentRun,
	identity investigate.Identity,
	subject investigate.ReviewSubject,
	config Config,
	reviewedAt time.Time,
) (investigate.DesignReview, error) {
	if !configuredReviewer(endpoint, config.Models.DesignJudges()) {
		return investigate.DesignReview{}, errors.New("design reviewer is not configured")
	}
	// The launch that belongs with this occupant of the judge's seat, for
	// the reason the candidate reviews hold to: the endpoint and the launch
	// move together or the record names a model that never judged.
	place, seated := SeatPlaceOf(endpoint, config.Models.DesignJudges())
	launch, launched := config.Agents.DesignReviewerAgentSeat(endpoint.ID, place)
	if !seated || !launched || run.Validate(config) != nil || run.AgentID != launch.ID {
		return investigate.DesignReview{}, errors.New("design review run is not the reviewer's own launch")
	}
	if run.DeliveryID != identity.DeliveryID || run.InputSHA256 != identity.InputSHA256 || run.ConfigSHA256 != identity.ConfigSHA256 ||
		run.ToolSHA != identity.ToolSHA || run.BaseSHA != identity.BaseSHA || run.Stage != subject.Round {
		return investigate.DesignReview{}, errors.New("design review run is not bound to this round")
	}
	output, err := DecodeAgentDesignReviewOutput(run.Transcript)
	if err != nil {
		return investigate.DesignReview{}, err
	}
	// A finding travels into the next round's instruction ahead of the
	// answer-rules boundary, like a candidate review's finding does.
	for _, finding := range output.Findings {
		if strings.Contains(finding.Message, ReviewAnswerRulesTail) {
			return investigate.DesignReview{}, errors.New("model design review finding is invalid")
		}
	}
	transcriptBytes := int32(len(run.Transcript)) // #nosec G115 -- bounded by MaxAgentTranscriptBytes.
	if transcriptBytes < 1 {
		transcriptBytes = 1
	}
	promptBytes := int32(run.PromptBytes) // #nosec G115 -- bounded by MaxAgentPromptBytes.
	usage := investigate.Usage{
		RequestedModel: endpoint.Model, RequestID: run.RunSHA256, StopReason: ChatFinishStop,
		InputTokens: promptBytes, OutputTokens: transcriptBytes, TotalTokens: promptBytes + transcriptBytes,
		LatencyMillis: run.DurationMs,
	}
	reviewer := investigate.Reviewer{ID: endpoint.ID, Vendor: endpoint.Vendor, Model: endpoint.Model, BaseURL: endpoint.BaseURL, Lens: lens}
	return investigate.NewDesignReview(identity, subject, reviewer, output, usage, reviewedAt)
}

// ConfirmTreeUnchanged checks that the baseline working copy a design
// reviewer read has no tracked change. Before code is written there is no
// candidate to match against: the baseline is the whole tree, and any
// tracked change is the reviewer's doing. Untracked byproducts are handled
// by CleanReviewByproducts, as after a code review.
func ConfirmTreeUnchanged(root string) error {
	changed, err := trackedChangesUnder(root)
	if err != nil {
		return fmt.Errorf("the tree could not be read after review: %w", err)
	}
	if len(changed) > 0 {
		return errors.New("the reviewing agent changed the tree: " + changed[0])
	}
	return nil
}

// ValidateDesignReviewSet checks that a decision's reviews come from this
// configuration's reviewers, one review each: one review per configured
// judge for a design - two of different vendors where two are configured,
// the same veto structure as a code review - and the evidence reviewer
// alone for an investigation report.
func ValidateDesignReviewSet(config Config, subject investigate.ReviewSubject, reviews []investigate.DesignReview) error {
	if expected := investigate.ReviewsRequired(subject.Kind, len(config.Models.DesignJudges())); len(reviews) != expected {
		return fmt.Errorf("a %s decision needs %d reviews and was given %d", subject.Kind, expected, len(reviews))
	}
	seen := make(map[string]struct{}, len(reviews))
	vendors := make(map[string]struct{}, len(reviews))
	for _, review := range reviews {
		// Held to the seat, not to one endpoint: the judge that answered is
		// the configured one for a seat that never moved, and the candidate
		// that took over for one the ladder moved. A record naming a seat
		// that is not configured, or an occupant nobody ever configured for
		// it, is refused exactly as before. Without this the ladder could
		// move a design seat and the decision would then refuse the review
		// the move produced, which is a delivery moving and getting nowhere.
		seat, ok := reviewerByID(config, review.ReviewerID)
		if !ok || !designReviewSeated(seat, review) {
			return errors.New("design review names a reviewer that is not configured")
		}
		if _, duplicate := seen[review.ReviewerID]; duplicate {
			return errors.New("design reviews contain duplicates")
		}
		seen[review.ReviewerID] = struct{}{}
		vendors[strings.ToLower(review.Vendor)] = struct{}{}
	}
	if subject.Kind == investigate.SubjectDesign && len(vendors) < 2 {
		return errors.New("design reviews must come from two vendors")
	}
	return nil
}

func reviewerByID(config Config, id string) (ModelEndpoint, bool) {
	return config.Models.DesignReviewerFor(id)
}

// designReviewSeated reports whether a sealed design review names one of
// the occupants of its seat. The same rule the candidate reviews are held
// to (Review.Validate), stated here because a design review is a record of
// its own shape.
func designReviewSeated(seat ModelEndpoint, review investigate.DesignReview) bool {
	for _, occupant := range seat.Seat() {
		if occupant.Vendor == review.Vendor && occupant.Model == review.Model && occupant.BaseURL == review.BaseURL {
			return true
		}
	}
	return false
}
