package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seatedCandidate is the artifact fixture built on a configuration whose
// first review seat has somewhere to move to.
func seatedCandidate(t *testing.T) (Config, TicketRequest, SourceSnapshot, Candidate) {
	t.Helper()
	return seatedFixture(t, func(Config) Config { return seatedTestConfig(t) })
}

// seatedFixture is the same, over a configuration the caller may shape
// first. Everything a record is bound to is derived from the configuration
// it is built with, so a test that wants a different seating has to ask for
// it here rather than move the seats afterwards.
func seatedFixture(t *testing.T, shape func(Config) Config) (Config, TicketRequest, SourceSnapshot, Candidate) {
	t.Helper()
	config := shape(seatedTestConfig(t))
	if err := config.Validate(); err != nil {
		t.Fatalf("the shaped configuration was refused: %v", err)
	}
	request, err := ParseTicket(validTicketEnvelope(t, validTicketDescription()), config)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	filename := filepath.Join(root, "client", "src", "components", "Example.tsx")
	if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte("export const label = 'Old label';\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := ReadSourceSnapshot(root, strings.Repeat("a", 40), request, config)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := NewCandidate(1, ModelCandidateOutput{
		Files:     []ModelCandidateFile{{Path: request.TargetFiles[0], Content: "export const label = 'Updated label';\n"}},
		Rationale: "Update the requested visible label.",
	}, source, request, config, validTestInvocation(config.Models.Implementer), testInvocationTime)
	if err != nil {
		t.Fatal(err)
	}
	return config, request, source, candidate
}

// seatedReview seals one judgment by one occupant of one seat.
func seatedReview(t *testing.T, config Config, seat ModelEndpoint, place int, candidate Candidate, source SourceSnapshot, request TicketRequest) Review {
	t.Helper()
	occupant, seated := seat.SeatOccupant(place)
	if !seated {
		t.Fatalf("seat %s has no place %d", seat.ID, place)
	}
	review, err := NewReview(1, occupant, ModelReviewOutput{Verdict: "pass", Findings: []ModelFinding{}},
		candidate, source, request, config, validTestInvocation(occupant), testInvocationTime)
	if err != nil {
		t.Fatalf("a review by %s could not be sealed: %v", occupant.Model, err)
	}
	return review
}

// The whole point of the change, at the record layer: the judgment a
// candidate produced is admitted. Before seats it was refused by the same
// rule that refuses a forged review — every field had to equal the
// configured endpoint — and a delivery whose provider went quiet had
// nowhere to go.
func TestDecideAcceptsAReviewFromACandidateSeat(t *testing.T) {
	config, request, source, candidate := seatedCandidate(t)
	moved := seatedReview(t, config, config.Models.Reviewers[0], 1, candidate, source, request)
	other := seatedReview(t, config, config.Models.Reviewers[1], 0, candidate, source, request)
	if moved.Model != "model-c" || moved.ReviewerID != "review-a" {
		t.Fatalf("the sealed review names %s by %s, want the candidate under the seat's own id", moved.Model, moved.ReviewerID)
	}
	decision, err := DecideStage(candidate, []Review{moved, other}, source, request, config)
	if err != nil {
		t.Fatalf("decide refused a review by a candidate seat: %v", err)
	}
	if decision.Outcome != "converged" {
		t.Fatalf("outcome = %q, want the verdicts counted as they are", decision.Outcome)
	}
	if err := decision.Validate(candidate, []Review{moved, other}, source, request, config); err != nil {
		t.Fatalf("the decision did not re-derive: %v", err)
	}
}

// What the seat does not loosen. The reviewer id is the seat, and a review
// wearing another seat's name is somebody else's judgment however valid it
// reads; so is one naming a model no occupant of the seat ever was, and one
// that mixes a candidate's vendor with the configured model.
func TestDecideStillRefusesAReviewThatIsNotThisSeats(t *testing.T) {
	config, request, source, candidate := seatedCandidate(t)
	good := seatedReview(t, config, config.Models.Reviewers[0], 1, candidate, source, request)
	other := seatedReview(t, config, config.Models.Reviewers[1], 0, candidate, source, request)
	for name, spoil := range map[string]func(Review) Review{
		"another seat's id":        func(r Review) Review { r.ReviewerID = "review-b"; return r },
		"a model nobody was":       func(r Review) Review { r.Model = "model-z"; return r },
		"half of one occupant":     func(r Review) Review { r.Vendor = config.Models.Reviewers[0].Vendor; return r },
		"a wider output allowance": func(r Review) Review { r.MaxOutputTokens = 32768; return r },
	} {
		spoiled := spoil(good)
		if err := spoiled.Validate(config.Models.Reviewers[0], candidate, request); err == nil {
			t.Fatalf("a review naming %s was admitted to the seat", name)
		}
		if _, err := DecideStage(candidate, []Review{spoiled, other}, source, request, config); err == nil {
			t.Fatalf("decide accepted a review naming %s", name)
		}
	}
}

// Two judges exist so that one vendor's blind spot is not every judge's.
// The configuration has always refused two seats configured on one vendor;
// seats reopen the question at run time, because a seat that moves could
// land on the vendor the other one is already sitting on. The records are
// where it is settled, whatever the ladder intended.
func TestDecideRefusesTwoSeatsOnOneVendor(t *testing.T) {
	// A candidate sitting on the other seat's vendor: a configuration may
	// legally list it, because the two are only forbidden from being there
	// at the same time.
	config, request, source, candidate := seatedFixture(t, func(config Config) Config {
		config.Models.Reviewers[0].Candidates[0].Vendor = config.Models.Reviewers[1].Vendor
		return config
	})
	clashing := seatedReview(t, config, config.Models.Reviewers[0], 1, candidate, source, request)
	other := seatedReview(t, config, config.Models.Reviewers[1], 0, candidate, source, request)
	if err := clashing.Validate(config.Models.Reviewers[0], candidate, request); err != nil {
		t.Fatalf("the review itself is the seat's own: %v", err)
	}
	if _, err := DecideStage(candidate, []Review{clashing, other}, source, request, config); err == nil {
		t.Fatal("decide accepted two seats answering from one vendor")
	}
	decision, err := DecideStage(candidate, []Review{
		seatedReview(t, config, config.Models.Reviewers[0], 0, candidate, source, request), other,
	}, source, request, config)
	if err != nil {
		t.Fatalf("decide refused the seats where the configuration put them: %v", err)
	}
	// And a decision cannot be re-derived past the rule either: the check
	// belongs to the records, not to the one moment they were counted.
	if err := decision.Validate(candidate, []Review{clashing, other}, source, request, config); err == nil {
		t.Fatal("a sealed decision re-derived over two seats on one vendor")
	}
}

// The seat id is the seat, not a hint. Two seats may legitimately be able
// to hold the same model under the same lens — nothing in the
// configuration forbids it, they are only forbidden from holding it at the
// same time — and then the one thing telling their reviews apart is the
// name each carries. A judgment sealed by one seat is not the other's,
// however exactly it matches.
func TestAReviewSealedByTheOtherSeatIsNotThisSeats(t *testing.T) {
	config, request, source, candidate := seatedFixture(t, func(config Config) Config {
		// Everything about review-b, as a candidate of review-a, save the
		// lens each seat judges under — which a candidate inherits from its
		// own seat, so it is the seat's and not the occupant's.
		twin := config.Models.Reviewers[1]
		twin.ID, twin.Lens, twin.Candidates = "", config.Models.Reviewers[0].Lens, nil
		config.Models.Reviewers[0].Candidates = []ModelEndpoint{twin}
		config.Models.Reviewers[1].Lens = config.Models.Reviewers[0].Lens
		return config
	})
	theirs := seatedReview(t, config, config.Models.Reviewers[1], 0, candidate, source, request)
	mine, _ := config.Models.Reviewers[0].SeatOccupant(1)
	if theirs.Model != mine.Model || theirs.Vendor != mine.Vendor || theirs.Lens != mine.Lens {
		t.Fatalf("this test needs two seats that could hold one occupant; got %+v and %+v", theirs, mine)
	}
	if err := theirs.Validate(config.Models.Reviewers[0], candidate, request); err == nil {
		t.Fatal("one seat's sealed judgment was admitted to the other seat")
	}
	if err := theirs.Validate(config.Models.Reviewers[1], candidate, request); err != nil {
		t.Fatalf("the judgment was refused by the seat that produced it: %v", err)
	}
}

// The launch and the endpoint move together. A run launched under another
// occupant's profile judged through another provider's key than the record
// would name, so it is not this occupant's review.
func TestAReviewRunMustBeItsOwnOccupantsLaunch(t *testing.T) {
	config, request, source, candidate := seatedCandidate(t)
	seat := config.Models.Reviewers[0]
	occupant, _ := seat.SeatOccupant(1)
	launch, _ := config.Agents.ReviewerAgentSeat(seat.ID, 1)
	own := sealedReviewRun(t, launch, request, source, nil)
	if _, err := AgentReviewFromRun(occupant, own, candidate, source, request, config, testInvocationTime); err != nil {
		t.Fatalf("the candidate's own launch was refused: %v", err)
	}
	configured, _ := config.Agents.ReviewerAgentSeat(seat.ID, 0)
	elsewhere := sealedReviewRun(t, configured, request, source, nil)
	if _, err := AgentReviewFromRun(occupant, elsewhere, candidate, source, request, config, testInvocationTime); err == nil {
		t.Fatal("a run under the seat's configured launch sealed as the candidate's review")
	}
}
