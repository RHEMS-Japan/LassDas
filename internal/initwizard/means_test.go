package initwizard

import (
	"encoding/json"
	"strings"
	"testing"
)

func answersFrom(t *testing.T, values map[string]any) Answers {
	t.Helper()
	answers := Answers{Answers: map[string]json.RawMessage{}}
	for id, value := range values {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		answers.Answers[id] = encoded
	}
	return answers
}

// A project hands its engine what the request will need. The answers are
// read, never asked: a setup that hands over nothing must not be shown a
// question about a cloud account it does not have.
func TestTheMeansAreReadFromTheAnswersFile(t *testing.T) {
	means, err := MeansFromAnswers(answersFrom(t, map[string]any{
		"repository":                  "example/app",
		"credential-cloud-path":       "/data/secrets/cloud",
		"credential-cloud-env":        "AWS_SHARED_CREDENTIALS_FILE",
		"credential-cloud-stages":     []string{"implement", "validate", "promote"},
		"credential-warehouse-path":   "/data/secrets/warehouse",
		"credential-warehouse-env":    []string{"DATABASE_URL", "PGURL"},
		"credential-warehouse-stages": []string{"validate"},
		"infrastructure-provider":     "aws",
		"infrastructure-region":       "ap-northeast-1",
		"infrastructure-credential":   "cloud",
		"infrastructure-resources":    []string{"sqs", "s3"},
	}))
	if err != nil {
		t.Fatalf("MeansFromAnswers: %v", err)
	}
	if len(means.Credentials) != 2 {
		t.Fatalf("read %d credentials", len(means.Credentials))
	}
	// Sorted, because the answers are a map and the configuration they
	// generate is digested: an order that changed between two runs of
	// apply would read as a changed configuration.
	if means.Credentials[0].Name != "cloud" || means.Credentials[1].Name != "warehouse" {
		t.Fatalf("the credentials are in a map's order: %+v", means.Credentials)
	}
	if got := means.Credentials[1].Env; len(got) != 2 || got[0] != "DATABASE_URL" {
		t.Fatalf("the variable names read as %v", got)
	}
	if means.Infrastructure == nil || means.Infrastructure.Provider != "aws" || means.Infrastructure.Region != "ap-northeast-1" {
		t.Fatalf("the infrastructure read as %+v", means.Infrastructure)
	}
	if len(means.Infrastructure.Resources) != 2 {
		t.Fatalf("the resource kinds read as %v", means.Infrastructure.Resources)
	}
}

// A file that names none gets none, which keeps every project that
// delivers only a pull request exactly as it was.
func TestAnAnswersFileWithoutMeansHandsOverNothing(t *testing.T) {
	means, err := MeansFromAnswers(answersFrom(t, map[string]any{"repository": "example/app"}))
	if err != nil {
		t.Fatalf("MeansFromAnswers: %v", err)
	}
	if len(means.Credentials) != 0 || means.Infrastructure != nil {
		t.Fatalf("means read as %+v", means)
	}
	s, secrets := wizardFixture(t)
	consumer, runtime, _, err := Generate(s, secrets)
	if err != nil {
		t.Fatal(err)
	}
	if consumer.Consumers[0].Infrastructure != nil || len(runtime.Chain.Credentials) != 0 {
		t.Fatalf("a project with no means generated some")
	}
}

// What was read reaches both generated configurations: the secrets are the
// body's to hand out, the account is the destination's to allow.
func TestTheMeansReachBothGeneratedConfigurations(t *testing.T) {
	s, secrets := wizardFixture(t)
	means, err := MeansFromAnswers(answersFrom(t, map[string]any{
		"credential-cloud-path":     "/data/secrets/cloud",
		"credential-cloud-env":      "AWS_SHARED_CREDENTIALS_FILE",
		"credential-cloud-stages":   []string{"implement"},
		"infrastructure-provider":   "aws",
		"infrastructure-credential": "cloud",
		"infrastructure-resources":  []string{"sqs"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	s.Means = &means
	consumer, runtime, _, err := Generate(s, secrets)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(runtime.Chain.Credentials) != 1 || runtime.Chain.Credentials[0].Name != "cloud" {
		t.Fatalf("the runtime configuration carries %+v", runtime.Chain.Credentials)
	}
	if consumer.Consumers[0].Infrastructure == nil || consumer.Consumers[0].Infrastructure.Provider != "aws" {
		t.Fatalf("the destination configuration carries %+v", consumer.Consumers[0].Infrastructure)
	}
	if !consumer.Consumers[0].MayCreate("sqs") || consumer.Consumers[0].MayCreate("rds") {
		t.Fatal("the destination's standing permission does not say what it allows")
	}
}

// A mistyped answer is named where it was written, while the file can
// still be edited, rather than by a body that will not start.
func TestAMistypedMeansAnswerIsRefusedAtTheFile(t *testing.T) {
	for name, values := range map[string]map[string]any{
		"no path": {
			"credential-cloud-env": "AWS_SHARED_CREDENTIALS_FILE", "credential-cloud-stages": []string{"implement"},
		},
		"no env": {
			"credential-cloud-path": "/data/secrets/cloud", "credential-cloud-stages": []string{"implement"},
		},
		"no stages": {
			"credential-cloud-path": "/data/secrets/cloud", "credential-cloud-env": "AWS_SHARED_CREDENTIALS_FILE",
		},
		"a card that does not exist": {
			"credential-cloud-path": "/data/secrets/cloud", "credential-cloud-env": "AWS_SHARED_CREDENTIALS_FILE",
			"credential-cloud-stages": []string{"deploy"},
		},
		"two credentials on one variable": {
			"credential-cloud-path": "/data/secrets/cloud", "credential-cloud-env": "SHARED",
			"credential-cloud-stages": []string{"implement"},
			"credential-other-path":   "/data/secrets/other", "credential-other-env": "SHARED",
			"credential-other-stages": []string{"validate"},
		},
		"an account with no provider": {
			"infrastructure-region": "ap-northeast-1",
		},
		"an account pointing at no credential": {
			"infrastructure-provider": "aws", "infrastructure-credential": "cloud",
		},
	} {
		if _, err := MeansFromAnswers(answersFrom(t, values)); err == nil {
			t.Fatalf("%s: read without complaint", name)
		}
	}
}

// The name of a credential is part of a variable the engine exports; a
// refusal has to say which one, so the message carries it.
func TestARefusalNamesTheCredentialItIsAbout(t *testing.T) {
	_, err := MeansFromAnswers(answersFrom(t, map[string]any{
		"credential-warehouse-env":    "DATABASE_URL",
		"credential-warehouse-stages": []string{"validate"},
	}))
	if err == nil || !strings.Contains(err.Error(), "credential-warehouse-path") {
		t.Fatalf("the refusal does not name the answer to fix: %v", err)
	}
}

// A credential file the agent user can open makes the list of cards
// decorative: an agent on a card the credential does not name could read
// the file on disk and have the secret anyway. So every provisioned file
// joins the list the boot refuses to start without.
func TestEveryCredentialFileIsGuardedAtBoot(t *testing.T) {
	s, secrets := wizardFixture(t)
	means, err := MeansFromAnswers(answersFrom(t, map[string]any{
		"credential-cloud-path":       "/data/secrets/cloud",
		"credential-cloud-env":        "AWS_SHARED_CREDENTIALS_FILE",
		"credential-cloud-stages":     []string{"implement"},
		"credential-warehouse-path":   "/data/secrets/warehouse",
		"credential-warehouse-env":    "DATABASE_URL",
		"credential-warehouse-stages": []string{"validate"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	s.Means = &means
	_, _, env, err := Generate(s, secrets)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	guarded := env["LASSDAS_GUARDED_FILES"]
	for _, want := range []string{"/data/secrets/target-token", "/data/secrets/cloud", "/data/secrets/warehouse"} {
		if !strings.Contains(guarded, want) {
			t.Fatalf("the boot does not guard %s: %q", want, guarded)
		}
	}
}
