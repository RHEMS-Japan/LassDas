package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/runtime"
)

func credentialConfig(t *testing.T, credentials ...runtime.Credential) runtime.Config {
	t.Helper()
	directory := t.TempDir()
	for index, credential := range credentials {
		path := filepath.Join(directory, credential.Name)
		if err := os.WriteFile(path, []byte("value-of-"+credential.Name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		credentials[index].Path = path
	}
	var config runtime.Config
	config.Chain.Credentials = credentials
	return config
}

// The files are read by the card that was named, and by no other. A card
// left off a credential's list never opens the file, so what runs inside it
// has no way to reach the service whether or not it tries.
func TestOnlyTheNamedCardReadsACredentialFile(t *testing.T) {
	config := credentialConfig(t,
		runtime.Credential{Name: "warehouse", Env: runtime.EnvNames{"DATABASE_URL"}, Stages: []string{runtime.StageValidate}},
		runtime.Credential{Name: "cloud", Env: runtime.EnvNames{"AWS_SHARED_CREDENTIALS_FILE", "AWS_CONFIG_FILE"}, Stages: []string{runtime.StageImplement, runtime.StageValidate}},
	)
	for stage, want := range map[string][]string{
		runtime.StageImplement: {"AWS_SHARED_CREDENTIALS_FILE=value-of-cloud", "AWS_CONFIG_FILE=value-of-cloud"},
		runtime.StageValidate:  {"DATABASE_URL=value-of-warehouse", "AWS_SHARED_CREDENTIALS_FILE=value-of-cloud", "AWS_CONFIG_FILE=value-of-cloud"},
		runtime.StageReviewA:   nil,
		runtime.StagePublish:   nil,
		runtime.DeliverStagePromote:   nil,
	} {
		assignments, err := stageCredentials(config, stage)
		if err != nil {
			t.Fatalf("%s: %v", stage, err)
		}
		if len(assignments) != len(want) {
			t.Fatalf("%s received %v, want %v", stage, assignments, want)
		}
		for _, assignment := range want {
			if !contains(assignments, assignment) {
				t.Fatalf("%s received %v, want %s among them", stage, assignments, assignment)
			}
		}
	}
}

// A delivery card asks for a milestone and is dispatched as a card; the
// credential names the card, so the two vocabularies have to meet.
func TestEachDeliveryMilestoneIsOneNamedCard(t *testing.T) {
	for milestone, stage := range map[string]string{
		"checks":              runtime.DeliverStageChecks,
		"staging-observed":    runtime.DeliverStageIntegrate,
		"production-observed": runtime.DeliverStagePromote,
		"nonsense":            "",
	} {
		if got := deliverStage(milestone); got != stage {
			t.Fatalf("%s is card %q, want %q", milestone, got, stage)
		}
	}
}

// A declared file that was never provisioned fails the card rather than
// running it without. A delivery told it may reach a service, and silently
// unable to, spends a whole round finding out in the worst way there is.
func TestAnUnprovisionedCredentialStopsItsCard(t *testing.T) {
	config := credentialConfig(t,
		runtime.Credential{Name: "warehouse", Env: runtime.EnvNames{"DATABASE_URL"}, Stages: []string{runtime.StageValidate}},
	)
	provisioned := config.Chain.Credentials[0].Path
	for name, break_ := range map[string]func(){
		"missing": func() { _ = os.Remove(provisioned) },
		"empty":   func() { _ = os.WriteFile(provisioned, []byte("\n"), 0o600) },
		"a directory": func() {
			_ = os.Remove(provisioned)
			_ = os.Mkdir(provisioned, 0o700)
		},
	} {
		break_()
		_, err := stageCredentials(config, runtime.StageValidate)
		if err == nil {
			t.Fatalf("%s: the card ran without its credential", name)
		}
		if !strings.Contains(err.Error(), "warehouse") {
			t.Fatalf("%s: the refusal does not name the credential: %v", name, err)
		}
		_ = os.RemoveAll(provisioned)
		if err := os.WriteFile(provisioned, []byte("value-of-warehouse\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// Only the trailing newline an editor leaves goes; a credentials file's own
// interior lines are its content.
func TestACredentialKeepsItsInteriorLines(t *testing.T) {
	config := credentialConfig(t,
		runtime.Credential{Name: "cloud", Env: runtime.EnvNames{"AWS_SHARED_CREDENTIALS_FILE"}, Stages: []string{runtime.StageImplement}},
	)
	if err := os.WriteFile(config.Chain.Credentials[0].Path, []byte("[default]\nregion = elsewhere\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	assignments, err := stageCredentials(config, runtime.StageImplement)
	if err != nil || len(assignments) != 1 {
		t.Fatalf("stageCredentials() = %v, %v", assignments, err)
	}
	if assignments[0] != "AWS_SHARED_CREDENTIALS_FILE=[default]\nregion = elsewhere" {
		t.Fatalf("the file's content changed: %q", assignments[0])
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
