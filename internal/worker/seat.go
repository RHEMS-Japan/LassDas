package worker

import (
	"errors"
	"fmt"
	"strings"
)

// Seats.
//
// A role used to be one endpoint. When that endpoint would not answer, the
// delivery was over: measured 2026-09-25, a report review went twenty-six
// minutes without a word and the night's work ended there with nothing
// delivered. Nothing about that was a decision about the request — one
// provider was not answering, and there were others.
//
// So a role is a seat now, and a seat has occupants. The configured
// endpoint is the first of them and the candidates follow it in the order
// the consumer wrote them; the ladder moves a seat to the next occupant
// when the one sitting in it will not answer. Everything the seat is for —
// its id, the lens it judges under — belongs to the seat and is the same
// for every occupant; what changes is who answers and through which key.
//
// The structural work is in what a sealed record is held to. A review used
// to be admitted only if every field of it matched the configured
// endpoint exactly, which is precisely the rule that would refuse the
// review the moved seat produced. It is now held to the seat: the reviewer
// id must be the seat's, and the rest must match one of the seat's
// occupants. A record naming a seat that is not configured, or an occupant
// no one ever configured for it, is refused exactly as before.

// PromptRebuildShorten is the one rebuild the ladder plays today: the
// instruction is stripped back to the request and the change itself — the
// earlier rounds' objections go, and the change travels as a map of where
// to look rather than as its own patches (§5.3 of the plan, "shorten").
//
// It lives here, with the endpoints, because both the card that asks for
// the rebuild and the verb that performs it have to mean the same thing by
// the word, and the verb is not allowed to depend on the card.
const PromptRebuildShorten = "shorten"

// MaxSeatCandidates bounds the candidates one seat may carry. Five
// occupants is more providers than any consumer has keys for; the bound is
// here so a configuration cannot make one failed card walk an unbounded
// list, and because every occupant of a review seat needs a launch
// definition of its own beside it.
const MaxSeatCandidates = 4

// Seat lists everyone who may sit in this seat, the configured endpoint
// first and the candidates after it in order.
//
// Every occupant comes back as the plain endpoint it is: the candidate
// list belongs to the seat rather than to whoever is sitting in it, and an
// occupant that carried the list would seal it into records that are about
// one answer. The id is the seat's for the same reason — a candidate is a
// different occupant of one seat, not a second seat.
func (m ModelEndpoint) Seat() []ModelEndpoint {
	occupants := make([]ModelEndpoint, 0, len(m.Candidates)+1)
	seated := m
	seated.Candidates = nil
	occupants = append(occupants, seated)
	for _, candidate := range m.Candidates {
		candidate.Candidates = nil
		candidate.ID = m.ID
		occupants = append(occupants, candidate)
	}
	return occupants
}

// SeatDepth is how many occupants the seat has. One means the seat has
// only its configured endpoint, which is every configuration written
// before seats existed.
func (m ModelEndpoint) SeatDepth() int { return len(m.Candidates) + 1 }

// SeatOccupant is the endpoint at one place in the seat's order: place 0
// is the configured endpoint, place 1 the first candidate. A place outside
// the seat is not an occupant, which is how a stage told to run a
// candidate the configuration does not have refuses instead of guessing.
func (m ModelEndpoint) SeatOccupant(place int) (ModelEndpoint, bool) {
	occupants := m.Seat()
	if place < 0 || place >= len(occupants) {
		return ModelEndpoint{}, false
	}
	return occupants[place], true
}

// sameOccupant reports whether two endpoints are the same occupant: every
// field that says who answers and how, with the candidate list left out.
//
// ModelEndpoint stopped being comparable with == when it gained a list, and
// this is deliberately the whole of the replacement rather than a digest:
// a field added to the endpoint and not added here would be a field a
// sealed record no longer has to match. seatFieldCount holds this honest.
func (m ModelEndpoint) sameOccupant(other ModelEndpoint) bool {
	return m.ID == other.ID && m.Vendor == other.Vendor && m.Model == other.Model &&
		m.BaseURL == other.BaseURL && m.APIKeyEnv == other.APIKeyEnv &&
		m.Lens == other.Lens && m.Effort == other.Effort &&
		m.StructuredOutput == other.StructuredOutput && m.MaxOutputTokens == other.MaxOutputTokens &&
		m.DesignLens == other.DesignLens
}

// seatFieldCount is how many fields ModelEndpoint has when sameOccupant was
// last written: the ten it compares plus the candidate list it does not. A
// test reads the struct and fails here when the two drift apart.
const seatFieldCount = 11

// SeatPlaceOf finds where an occupant sits: the seat among these endpoints
// that carries its id, and the place in that seat it occupies. It is how a
// caller holding an occupant recovers the launch that belongs with it —
// the two move together, and a review sealed against one seat's endpoint
// and another's launch would name a model that never saw the change.
func SeatPlaceOf(endpoint ModelEndpoint, seats []ModelEndpoint) (int, bool) {
	seat, found := SeatOf(seats, endpoint.ID)
	if !found {
		return 0, false
	}
	for place, occupant := range seat.Seat() {
		if occupant.sameOccupant(endpoint) {
			return place, true
		}
	}
	return 0, false
}

// SeatOf finds the seat one role id names among these endpoints.
func SeatOf(endpoints []ModelEndpoint, id string) (ModelEndpoint, bool) {
	for _, endpoint := range endpoints {
		if endpoint.ID == id {
			return endpoint, true
		}
	}
	return ModelEndpoint{}, false
}

// SameVendor compares two vendor names the way every vendor rule in this
// package does: the name as written, case ignored.
func SameVendor(left, right string) bool { return strings.EqualFold(left, right) }

// implementerOccupant is the endpoint a sealed candidate names as its
// author: the implementer seat's configured endpoint, without the seat's
// candidate list.
func implementerOccupant(config Config) ModelEndpoint {
	return config.Models.Implementer.Seat()[0]
}

// implementerSeated reports whether a sealed candidate names an occupant of
// the implementer seat. A seat with no candidates admits exactly what the
// equality it replaces admitted.
func implementerSeated(sealed ModelEndpoint, config Config) bool {
	for _, occupant := range config.Models.Implementer.Seat() {
		if sealed.sameOccupant(occupant) {
			return true
		}
	}
	return false
}

// seatsStayDiverse refuses a decision whose review seats answered from one
// provider.
//
// Two judges on one vendor are one vendor's blind spot counted twice, which
// is why the configuration has always refused it. Seats reopen the question
// after load: one seat moving onto a candidate could land on the vendor the
// other seat is already sitting on, and by then the configuration has
// nothing left to say. The ladder skips such a candidate when it chooses;
// this is the check that holds whether or not the ladder chose well, on the
// records themselves, where the fact is no longer a matter of intent.
//
// A single configured judge has nobody to differ from, and the rule has
// nothing to say about it — the same allowance the configuration makes.
func seatsStayDiverse(reviews []Review, config Config) error {
	if len(config.Models.Reviewers) < 2 {
		return nil
	}
	seen := make(map[string]string, len(reviews))
	for _, review := range reviews {
		vendor := strings.ToLower(review.Vendor)
		if other, taken := seen[vendor]; taken && other != review.ReviewerID {
			return errors.New("two review seats answered from one vendor")
		}
		seen[vendor] = review.ReviewerID
	}
	return nil
}

// seatLaunches validates this binding's launches — the configured one and
// every candidate's — and holds each of their agent ids to the set of ids
// already taken. Every launch is a separate identity at the provider, so
// each needs an id of its own: a candidate reusing another launch's id
// would make the sealed run record unable to say which of them judged.
func (r ReviewerAgent) seatLaunches(taken map[string]struct{}) error {
	if len(r.Candidates) > MaxSeatCandidates {
		return errors.New("candidate launches are too many")
	}
	for _, agent := range append([]AgentConfig{r.Agent}, r.Candidates...) {
		if err := agent.validate(); err != nil {
			return err
		}
		if _, exists := taken[agent.ID]; exists {
			return errors.New("agent ids must differ")
		}
		taken[agent.ID] = struct{}{}
	}
	return nil
}

// validateSeatLaunches holds a review seat that can move to a launch for
// every occupant it can move to.
//
// A review is run by an agent, not by this process calling an address: the
// endpoint declares which model the launch's profile talks to, and the
// launch is what carries the profile and the credential. So an endpoint
// candidate with no launch beside it would name a second provider in the
// sealed record while the same program went on talking to the first one
// through the same key. Refused at load, where a consumer can still fix
// it, rather than discovered on the night a seat first has to move.
func (c Config) validateSeatLaunches() error {
	for _, seat := range c.Models.Reviewers {
		if err := seatLaunchesCover(seat, c.Agents.ReviewerAgents, "reviewer"); err != nil {
			return err
		}
	}
	for _, seat := range c.Models.DesignReviewers {
		if err := seatLaunchesCover(seat, c.Agents.DesignReviewerAgents, "design reviewer"); err != nil {
			return err
		}
	}
	return nil
}

func seatLaunchesCover(seat ModelEndpoint, bindings []ReviewerAgent, role string) error {
	for _, binding := range bindings {
		if binding.ReviewerID != seat.ID {
			continue
		}
		// The two lists are the same length or the configuration is
		// wrong in one of two ways. Too few launches and an endpoint the
		// ladder may move onto has none. Too many — including launches on
		// a seat with no endpoint candidates at all — and a launch is
		// written down that nothing can ever reach, which reads as a seat
		// that can move and is not one.
		if len(binding.Candidates) != len(seat.Candidates) {
			return fmt.Errorf("%s %s: the candidate seats and their launches must match", role, seat.ID)
		}
		return nil
	}
	if len(seat.Candidates) > 0 {
		return fmt.Errorf("%s %s: a seat with candidates needs its own launch definitions", role, seat.ID)
	}
	return nil
}

// validateCandidates checks the occupants below the configured endpoint.
//
// A candidate changes who answers and through which key — vendor, model,
// address, structured output, the output allowance. It does not change
// what the seat is for, so it carries the seat's id and the seat's lenses;
// a candidate that judged under another lens would be a different seat
// wearing this one's name, and the sealed record could not tell which.
func (m ModelEndpoint) validateCandidates(reviewer, judge bool) error {
	if len(m.Candidates) > MaxSeatCandidates {
		return errors.New("model endpoint candidates are too many")
	}
	occupied := map[string]struct{}{strings.ToLower(m.BaseURL + "\x00" + m.Model): {}}
	for _, candidate := range m.Candidates {
		if len(candidate.Candidates) > 0 {
			// A seat is one list. Nesting would make "the next candidate"
			// a question about which list, and the ladder's record says a
			// place, not a path.
			return errors.New("model endpoint candidate carries candidates of its own")
		}
		if candidate.ID != "" && candidate.ID != m.ID {
			return errors.New("model endpoint candidate names another seat")
		}
		named := candidate
		named.ID = m.ID
		if err := named.validateAs(reviewer, judge); err != nil {
			return fmt.Errorf("candidate: %w", err)
		}
		if named.Lens != m.Lens || named.DesignLens != m.DesignLens {
			return errors.New("model endpoint candidate judges under another lens")
		}
		// Two occupants on one address and model are one hand played
		// twice: the ladder would move the seat and meet the same
		// provider, which is the loop the ladder exists to replace.
		key := strings.ToLower(named.BaseURL + "\x00" + named.Model)
		if _, taken := occupied[key]; taken {
			return errors.New("model endpoint candidates contain duplicates")
		}
		occupied[key] = struct{}{}
	}
	return nil
}
