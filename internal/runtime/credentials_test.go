package runtime

import (
	"strings"
	"testing"
)

func chainWithCredentials(entries ...any) map[string]any {
	chain := cardsChainMap()
	chain["credentials"] = entries
	return chain
}

// Two credentials exporting one variable name are not both handed to a
// card: the second assignment replaces the first, and which one that is
// depends on the order of a JSON array nobody reads as significant. The
// file is refused, and the refusal names both entries and the variable.
func TestLoadRefusesTwoCredentialsExportingOneVariable(t *testing.T) {
	raw := validRuntimeConfigMap()
	raw["chain"] = chainWithCredentials(
		map[string]any{"name": "warehouse", "path": "/secrets/warehouse", "env": "DATABASE_URL", "stages": []any{"validate"}},
		map[string]any{"name": "reporting", "path": "/secrets/reporting", "env": []any{"REPORTING_TOKEN", "DATABASE_URL"}, "stages": []any{"implement"}},
	)
	_, err := Load(writeRuntimeConfig(t, raw))
	if err == nil {
		t.Fatal("a duplicated env name loaded")
	}
	for _, want := range []string{"warehouse", "reporting", "DATABASE_URL"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not name %s: %v", want, err)
		}
	}
}

// A credential that takes a variable the engine already sets would not sit
// beside the engine's value but after it, and the card would lose the
// destination token or a model key with nothing saying so.
func TestLoadRefusesACredentialTakingAVariableTheEngineSets(t *testing.T) {
	raw := validRuntimeConfigMap()
	raw["chain"] = chainWithCredentials(
		map[string]any{"name": "mirror", "path": "/secrets/mirror", "env": "TARGET_GITHUB_TOKEN", "stages": []any{"publish"}},
	)
	_, err := Load(writeRuntimeConfig(t, raw))
	if err == nil || !strings.Contains(err.Error(), "TARGET_GITHUB_TOKEN") {
		t.Fatalf("Load() error = %v", err)
	}
}

// A stage name no card answers to would hand the secret to nothing, and a
// delivery would find out by failing at the card that needed it.
func TestLoadRefusesACredentialNamingACardThatDoesNotExist(t *testing.T) {
	raw := validRuntimeConfigMap()
	raw["chain"] = chainWithCredentials(
		map[string]any{"name": "warehouse", "path": "/secrets/warehouse", "env": "DATABASE_URL", "stages": []any{"implement", "deploy"}},
	)
	_, err := Load(writeRuntimeConfig(t, raw))
	if err == nil || !strings.Contains(err.Error(), "deploy") {
		t.Fatalf("Load() error = %v", err)
	}
	// And the same list with only real cards loads, the delivery
	// continuation's three included.
	raw["chain"] = chainWithCredentials(
		map[string]any{"name": "warehouse", "path": "/secrets/warehouse", "env": "DATABASE_URL", "stages": []any{"implement", "validate", "checks", "integrate", "promote"}},
	)
	if _, err := Load(writeRuntimeConfig(t, raw)); err != nil {
		t.Fatalf("a credential naming only real cards was refused: %v", err)
	}
}

// Other shapes that cannot mean what they say. Each is one line of a file
// somebody typed, so each is refused rather than repaired.
func TestLoadRefusesMalformedCredentials(t *testing.T) {
	for name, entry := range map[string]map[string]any{
		"no name":         {"path": "/secrets/a", "env": "A_TOKEN", "stages": []any{"implement"}},
		"relative path":   {"name": "a", "path": "secrets/a", "env": "A_TOKEN", "stages": []any{"implement"}},
		"no env":          {"name": "a", "path": "/secrets/a", "stages": []any{"implement"}},
		"lowercase env":   {"name": "a", "path": "/secrets/a", "env": "a_token", "stages": []any{"implement"}},
		"no stages":       {"name": "a", "path": "/secrets/a", "env": "A_TOKEN"},
		"repeated stage":  {"name": "a", "path": "/secrets/a", "env": "A_TOKEN", "stages": []any{"implement", "implement"}},
		"uppercase name":  {"name": "A", "path": "/secrets/a", "env": "A_TOKEN", "stages": []any{"implement"}},
		"env not a name":  {"name": "a", "path": "/secrets/a", "env": []any{"A_TOKEN", "9LIVES"}, "stages": []any{"implement"}},
		"stages not list": {"name": "a", "path": "/secrets/a", "env": "A_TOKEN", "stages": "implement"},
	} {
		raw := validRuntimeConfigMap()
		raw["chain"] = chainWithCredentials(entry)
		if _, err := Load(writeRuntimeConfig(t, raw)); err == nil {
			t.Fatalf("%s: loaded", name)
		}
	}
}

// One variable name is written as a string and several as an array. Both
// decode, so the file's shape does not depend on how many there are.
func TestACredentialTakesOneVariableNameOrSeveral(t *testing.T) {
	raw := validRuntimeConfigMap()
	raw["chain"] = chainWithCredentials(
		map[string]any{"name": "cloud", "path": "/secrets/cloud", "env": "AWS_SHARED_CREDENTIALS_FILE", "stages": []any{"implement", "validate"}},
		map[string]any{"name": "warehouse", "path": "/secrets/warehouse", "env": []any{"DATABASE_URL", "PGURL"}, "stages": []any{"validate"}},
	)
	config, err := Load(writeRuntimeConfig(t, raw))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := config.Chain.Credentials[0].Env; len(got) != 1 || got[0] != "AWS_SHARED_CREDENTIALS_FILE" {
		t.Fatalf("one name decoded as %v", got)
	}
	if got := config.Chain.Credentials[1].Env; len(got) != 2 {
		t.Fatalf("two names decoded as %v", got)
	}
}

// CredentialsFor is what one card receives, and no card receives what it
// was not named in.
func TestOnlyTheNamedCardsReceiveACredential(t *testing.T) {
	raw := validRuntimeConfigMap()
	raw["chain"] = chainWithCredentials(
		map[string]any{"name": "cloud", "path": "/secrets/cloud", "env": "AWS_SHARED_CREDENTIALS_FILE", "stages": []any{"implement", "validate"}},
		map[string]any{"name": "warehouse", "path": "/secrets/warehouse", "env": "DATABASE_URL", "stages": []any{"validate"}},
	)
	config, err := Load(writeRuntimeConfig(t, raw))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	for stage, want := range map[string][]string{
		StageImplement:      {"cloud"},
		StageValidate:       {"cloud", "warehouse"},
		StageReviewA:        nil,
		StageReviewB:        nil,
		StagePublish:        nil,
		StageApply:          nil,
		DeliverStagePromote: nil,
		StageDesignDecide:   nil,
		StageInvestigate:    nil,
		StageDesignReviewA:  nil,
	} {
		handed := config.Chain.CredentialsFor(stage)
		if len(handed) != len(want) {
			t.Fatalf("%s receives %d credentials, want %d", stage, len(handed), len(want))
		}
		for index, name := range want {
			if handed[index].Name != name {
				t.Fatalf("%s receives %q at %d, want %q", stage, handed[index].Name, index, name)
			}
		}
	}
	if _, known := config.Chain.CredentialNamed("warehouse"); !known {
		t.Fatal("a declared credential cannot be found by name")
	}
	if _, known := config.Chain.CredentialNamed("nothing"); known {
		t.Fatal("an undeclared credential was found by name")
	}
}

// A configuration that declares none keeps the shape it always had.
func TestAConfigurationWithoutCredentialsHandsOverNothing(t *testing.T) {
	config, err := Load(writeRuntimeConfig(t, validRuntimeConfigMap()))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	for _, stage := range DispatchedStages() {
		if handed := config.Chain.CredentialsFor(stage); len(handed) != 0 {
			t.Fatalf("%s received %v", stage, handed)
		}
	}
}
