package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"automation.internal/ticket-ingress/internal/decisions"
)

// The address is stated twice, here and in the client, so that a
// configuration can be checked without the client being built. Two
// spellings of one address is exactly how they drift apart, so they are
// held equal in one line, and the client's own test writes the literal out.
func TestTheTwoSpellingsOfTheAddressAgree(t *testing.T) {
	if DecisionsBaseURL != decisions.DefaultBaseURL {
		t.Errorf("the configuration reaches %q and the client reaches %q", DecisionsBaseURL, decisions.DefaultBaseURL)
	}
}

// The reception judge is optional, and a destination that says nothing about
// it must be the destination it was before the role existed - byte for byte,
// because the digest is folded from the encoded configuration and every
// sealed record of a delivery in flight is bound to it. A field that encoded
// even as a null would invalidate every flying delivery at once.
func TestTheReceptionJudgeRoleIsOptional(t *testing.T) {
	const shipped = "../../config/m1-consumer.json"
	config, err := LoadConfig(shipped)
	if err != nil {
		t.Fatal(err)
	}
	if role, present := config.Models.ReceptionJudgeRole(); present {
		t.Fatalf("the shipped destination names a reception judge: %+v", role)
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	var canonical map[string]any
	if err := json.Unmarshal(encoded, &canonical); err != nil {
		t.Fatal(err)
	}
	models, _ := canonical["models"].(map[string]any)
	if _, present := models["reception_judge"]; present {
		t.Error("a destination that names no reception judge still encodes the key")
	}
}

// Naming the role has to be possible at all: the destination file is read
// with unknown keys refused, so a key the engine does not know is a
// destination that will not load.
func TestADestinationMayNameAReceptionJudge(t *testing.T) {
	config := withReceptionJudge(t, func(judge map[string]any) {})
	role, present := config.Models.ReceptionJudgeRole()
	if !present {
		t.Fatal("the named role did not reach the configuration")
	}
	if role.Provider != "TypeSafe" || role.Model != "typesafe/jev-1.13" || role.APIKeyEnv != "MODEL_API_KEY_DECISIONS" {
		t.Errorf("the role did not load as written: %+v", role)
	}
	// Naming no address is the service's own, so an operator writes three
	// fields rather than four and cannot mistype the one that matters most.
	if role.BaseURL != "" || role.Address() != DecisionsBaseURL {
		t.Errorf("address = %q (base_url %q), want the default", role.Address(), role.BaseURL)
	}
}

// Adding the role changes the digest. This is the other half of the rule
// above: absence must be free, and presence must be visible, or the pin on
// the shipped digests would be measuring nothing.
func TestNamingTheReceptionJudgeMovesTheDigest(t *testing.T) {
	shipped, err := LoadConfig("../../config/m1-consumer.json")
	if err != nil {
		t.Fatal(err)
	}
	before, err := shipped.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	after, err := withReceptionJudge(t, func(judge map[string]any) {}).SHA256()
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Error("naming a reception judge left the destination's digest unchanged")
	}
}

// The role is checked where it is written. A model nobody named, an address
// that is not one, or a key variable that is not a variable name is a
// configuration error, not a model that appears to be down on every call.
func TestAReceptionJudgeThatCannotBeCalledIsRefused(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(judge map[string]any)
	}{
		{"no provider", func(judge map[string]any) { delete(judge, "provider") }},
		{"no model", func(judge map[string]any) { delete(judge, "model") }},
		{"a padded model name", func(judge map[string]any) { judge["model"] = " typesafe/jev-1.13" }},
		{"no key variable", func(judge map[string]any) { delete(judge, "api_key_env") }},
		{"a key variable that is not one", func(judge map[string]any) { judge["api_key_env"] = "lower case" }},
		{"an address that is not encrypted", func(judge map[string]any) { judge["base_url"] = "http://example.com/api/alpha" }},
		{"an address carrying credentials", func(judge map[string]any) { judge["base_url"] = "https://user:secret@example.com/api/alpha" }},
		{"an address with a trailing slash", func(judge map[string]any) { judge["base_url"] = "https://example.com/api/alpha/" }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := loadWithReceptionJudge(t, testCase.mutate); err == nil {
				t.Fatal("a reception judge that cannot be called was accepted")
			}
		})
	}
}

// A reachable address an operator pins themselves is kept as written.
func TestAPinnedReceptionJudgeAddressIsKept(t *testing.T) {
	config := withReceptionJudge(t, func(judge map[string]any) { judge["base_url"] = "https://decisions.example.com/api/alpha" })
	role, _ := config.Models.ReceptionJudgeRole()
	if role.Address() != "https://decisions.example.com/api/alpha" {
		t.Errorf("address = %q", role.Address())
	}
}

func withReceptionJudge(t *testing.T, mutate func(judge map[string]any)) Config {
	t.Helper()
	config, err := loadWithReceptionJudge(t, mutate)
	if err != nil {
		t.Fatalf("loading a destination that names a reception judge: %v", err)
	}
	return config
}

func loadWithReceptionJudge(t *testing.T, mutate func(judge map[string]any)) (Config, error) {
	t.Helper()
	raw, err := os.ReadFile("../../config/m1-consumer.json")
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	judge := map[string]any{
		"provider":    "TypeSafe",
		"model":       "typesafe/jev-1.13",
		"api_key_env": "MODEL_API_KEY_DECISIONS",
	}
	mutate(judge)
	parsed["models"].(map[string]any)["reception_judge"] = judge
	encoded, err := json.Marshal(parsed)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "m1-consumer.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return LoadConfig(path)
}
