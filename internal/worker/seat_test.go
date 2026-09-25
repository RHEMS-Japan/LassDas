package worker

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// seatedTestConfig gives the first review seat a candidate on a third
// vendor, with the launch that goes with it: the shape a consumer writes
// when it wants a judge that can be moved.
func seatedTestConfig(t *testing.T) Config {
	t.Helper()
	config := validTestConfig()
	config.Models.Reviewers[0].Candidates = []ModelEndpoint{{
		Vendor: "Vendor C", Model: "model-c", BaseURL: "https://gateway.example.com/api/v1",
		APIKeyEnv: "TEST_MODEL_KEY_C", Lens: "correctness", MaxOutputTokens: 2048,
	}}
	config.Agents.ReviewerAgents = []ReviewerAgent{
		{ReviewerID: "review-a", Agent: reviewLaunch("review-a-agent", "judge-a", "TEST_MODEL_KEY_A"),
			Candidates: []AgentConfig{reviewLaunch("review-a-candidate-agent", "judge-a-next", "TEST_MODEL_KEY_C")}},
		{ReviewerID: "review-b", Agent: reviewLaunch("review-b-agent", "judge-b", "TEST_MODEL_KEY_B")},
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("the seated configuration was refused: %v", err)
	}
	return config
}

// reviewLaunch is one judge's launch: the same program under its own
// profile, drawing on its own credential source, which is what the
// separation rules ask of launches that share a binary.
func reviewLaunch(id, profile, secret string) AgentConfig {
	return AgentConfig{
		ID: id, Command: "judge", Args: []string{"--profile", profile}, Profile: profile,
		SecretEnv: map[string]string{"REVIEW_TOKEN": secret}, TimeoutSeconds: 900,
	}
}

// The promise the whole change rests on for anyone already running this
// engine: a configuration written before seats existed encodes exactly as
// it did, so its digest is the same number and every record sealed under
// it still reads.
func TestAConfigurationWithoutCandidatesKeepsItsDigest(t *testing.T) {
	config := validTestConfig()
	digest, err := config.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "candidates") {
		t.Fatalf("an empty seat wrote itself into the configuration: %s", encoded)
	}
	// The digest the engine carried before seats existed, over this same
	// fixture. Pinned rather than recomputed: a recomputed one would move
	// with any change and prove nothing about the promise.
	const before = "e5c13b3b2705862c505a24fde4ffd72296b650b44200d366cea8961b7d31a076"
	if digest != before {
		t.Fatalf("digest = %q, want the digest this configuration had before seats: %q", digest, before)
	}
	seated := seatedTestConfig(t)
	moved, err := seated.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	if moved == digest {
		t.Fatal("a configuration that gained candidates kept its digest; the records it seals must be told apart")
	}
}

// The seat is its occupants in order, and every one of them comes out as
// the plain endpoint it is — carrying the seat's id and none of the seat's
// list, because a record is about one answer.
func TestASeatIsItsOccupantsInOrder(t *testing.T) {
	seat := seatedTestConfig(t).Models.Reviewers[0]
	if seat.SeatDepth() != 2 {
		t.Fatalf("SeatDepth() = %d, want the configured endpoint and its one candidate", seat.SeatDepth())
	}
	occupants := seat.Seat()
	if occupants[0].Vendor != "Vendor A" || occupants[1].Vendor != "Vendor C" {
		t.Fatalf("occupants = %v, want the configured endpoint first", occupants)
	}
	for place, occupant := range occupants {
		if occupant.ID != "review-a" {
			t.Fatalf("occupant %d names %q, want the seat's own id", place, occupant.ID)
		}
		if len(occupant.Candidates) != 0 {
			t.Fatalf("occupant %d carries the seat's list", place)
		}
	}
	if _, seated := seat.SeatOccupant(2); seated {
		t.Fatal("a place the configuration does not have was answered")
	}
	if place, found := SeatPlaceOf(occupants[1], []ModelEndpoint{seat}); !found || place != 1 {
		t.Fatalf("SeatPlaceOf() = %d, %v, want the candidate's own place", place, found)
	}
}

// sameOccupant is compared field by field, so a field added to the endpoint
// and forgotten here would be a field a sealed record no longer has to
// match. The count is the guard.
func TestEveryEndpointFieldIsPartOfTheOccupantComparison(t *testing.T) {
	if fields := reflect.TypeOf(ModelEndpoint{}).NumField(); fields != seatFieldCount {
		t.Fatalf("ModelEndpoint has %d fields and sameOccupant was written for %d; a new field must be compared or deliberately excluded", fields, seatFieldCount)
	}
}

// What the configuration will not admit: a candidate that is a different
// seat, one that judges under another lens, one that repeats an occupant
// the seat already has, and one nested inside another.
func TestTheConfigurationRefusesACandidateThatIsNotThisSeat(t *testing.T) {
	for name, spoil := range map[string]func(*Config){
		"another seat's id": func(c *Config) { c.Models.Reviewers[0].Candidates[0].ID = "review-b" },
		"another lens":      func(c *Config) { c.Models.Reviewers[0].Candidates[0].Lens = "adversarial" },
		"the occupant it already has": func(c *Config) {
			c.Models.Reviewers[0].Candidates[0].Model = c.Models.Reviewers[0].Model
			c.Models.Reviewers[0].Candidates[0].BaseURL = c.Models.Reviewers[0].BaseURL
		},
		"a candidate of its own": func(c *Config) {
			c.Models.Reviewers[0].Candidates[0].Candidates = []ModelEndpoint{c.Models.Reviewers[0].Candidates[0]}
		},
	} {
		config := seatedTestConfig(t)
		spoil(&config)
		if err := config.Validate(); err == nil {
			t.Fatalf("Validate() accepted a candidate that is %s", name)
		}
	}
}

// A seat that can move needs a launch for everywhere it can move to. The
// endpoint declares which model the launch talks to; without a launch of
// its own the same program would go on talking to the same provider
// through the same key while the record claimed another vendor answered.
func TestASeatWithCandidatesNeedsALaunchForEachOfThem(t *testing.T) {
	config := seatedTestConfig(t)
	config.Agents.ReviewerAgents[0].Candidates = nil
	if err := config.Validate(); err == nil {
		t.Fatal("Validate() accepted a candidate endpoint with no launch")
	}
	bare := validTestConfig()
	bare.Models.Reviewers[0].Candidates = []ModelEndpoint{{
		Vendor: "Vendor C", Model: "model-c", BaseURL: "https://gateway.example.com/api/v1",
		APIKeyEnv: "TEST_MODEL_KEY_C", Lens: "correctness", MaxOutputTokens: 2048,
	}}
	if err := bare.Validate(); err == nil {
		t.Fatal("Validate() accepted candidates on a seat that shares the reviewer launch")
	}
	if _, launched := bare.Agents.ReviewerAgentSeat("review-a", 1); launched {
		t.Fatal("the shared reviewer launch answered for a candidate seat")
	}
}

// The vendor host table pins a vendor name to the hosts its endpoints may
// be reached through. A candidate is an endpoint, so a seat that could be
// moved onto an unregistered host would be a way round the table that
// opens the moment the first model fails.
func TestTheVendorHostTableCoversEveryOccupant(t *testing.T) {
	config := seatedTestConfig(t)
	config.Models.VendorHosts = map[string][]string{
		"vendor a": {"gateway.example.com"},
		"vendor b": {"gateway.example.com"},
		"vendor c": {"gateway.example.com"},
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("a registered candidate host was refused: %v", err)
	}
	config.Models.Reviewers[0].Candidates[0].BaseURL = "https://elsewhere.example.com/api/v1"
	if err := config.Validate(); err == nil {
		t.Fatal("Validate() accepted a candidate on a host its vendor does not have")
	}
}

// A candidate's launch is a separate identity at the provider, so it is
// held to the separation every other launch is: it cannot be another
// launch under a different name.
func TestACandidateLaunchIsSeparatedLikeEveryOther(t *testing.T) {
	config := seatedTestConfig(t)
	config.Agents.ReviewerAgents[0].Candidates[0].ID = config.Agents.ReviewerAgents[1].Agent.ID
	if err := config.Validate(); err == nil {
		t.Fatal("Validate() accepted a candidate launch wearing another launch's id")
	}
	shared := seatedTestConfig(t)
	shared.Agents.ReviewerAgents[0].Candidates[0].SecretEnv = shared.Agents.ReviewerAgents[1].Agent.SecretEnv
	if err := shared.Validate(); err == nil {
		t.Fatal("Validate() accepted two launches drawing on one credential source")
	}
}
