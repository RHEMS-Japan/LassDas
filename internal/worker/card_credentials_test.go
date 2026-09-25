package worker

import (
	"os"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/cardsecret"
)

// The agent is the one thing in a card that has to reach a service to write
// a change against it. Its environment is built here from nothing, so a
// credential that stopped at the process which read the file would leave the
// agent unable to do the work the card was handed the credential for.
func TestTheAgentsEnvironmentCarriesTheCardsCredentials(t *testing.T) {
	cardsecret.Forget()
	t.Setenv("DATABASE_URL", "postgres://warehouse.invalid/orders")
	t.Setenv(cardsecret.NamesEnv, "DATABASE_URL")
	cardsecret.FromEnvironment()
	t.Cleanup(cardsecret.Forget)

	environment, err := agentEnvironment(AgentConfig{ID: "implementer", Command: "agent"}, t.TempDir())
	if err != nil {
		t.Fatalf("agentEnvironment: %v", err)
	}
	if !hasAssignment(environment, "DATABASE_URL=postgres://warehouse.invalid/orders") {
		t.Fatalf("the agent cannot reach the service its card was handed: %v", environment)
	}
}

// A card that was handed nothing hands the agent nothing, which is every
// card of every configuration that declares no credentials.
func TestAnAgentOfACardWithNoCredentialsGetsNone(t *testing.T) {
	cardsecret.Forget()
	t.Cleanup(cardsecret.Forget)
	environment, err := agentEnvironment(AgentConfig{ID: "implementer", Command: "agent"}, t.TempDir())
	if err != nil {
		t.Fatalf("agentEnvironment: %v", err)
	}
	for _, entry := range environment {
		if strings.HasPrefix(entry, "DATABASE_URL=") {
			t.Fatalf("an unnamed card's agent received a credential: %v", environment)
		}
	}
}

// The destination's own verification commands run in a sandbox built from
// nothing, which is what makes the verification mean something. A
// destination whose tests need a database would have them fail there for a
// reason the output cannot explain.
func TestTheValidationSandboxCarriesTheCardsCredentials(t *testing.T) {
	cardsecret.Forget()
	t.Setenv("DATABASE_URL", "postgres://warehouse.invalid/orders")
	t.Setenv(cardsecret.NamesEnv, "DATABASE_URL")
	cardsecret.FromEnvironment()
	t.Cleanup(cardsecret.Forget)

	environment, cleanup, err := createValidationEnvironment(os.Environ())
	if err != nil {
		t.Fatalf("createValidationEnvironment: %v", err)
	}
	defer cleanup()
	if !hasAssignment(environment, "DATABASE_URL=postgres://warehouse.invalid/orders") {
		t.Fatalf("the verification commands cannot reach the service: %v", environment)
	}
	// And the sandbox stays a sandbox: nothing else of the host's came in.
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "PATH", "HOME", "TMPDIR", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "LC_ALL", "CI", "NO_COLOR", "DATABASE_URL":
		default:
			t.Fatalf("the sandbox inherited %s", name)
		}
	}
}

// And a card handed nothing verifies in the sandbox it always had.
func TestTheValidationSandboxOfACardWithNoCredentialsIsUnchanged(t *testing.T) {
	cardsecret.Forget()
	t.Cleanup(cardsecret.Forget)
	environment, cleanup, err := createValidationEnvironment(os.Environ())
	if err != nil {
		t.Fatalf("createValidationEnvironment: %v", err)
	}
	defer cleanup()
	if len(environment) != 8 {
		t.Fatalf("the sandbox holds %d variables: %v", len(environment), environment)
	}
}

// A credential an agent printed must not survive in the transcript: for a
// run that stopped, the transcript is the whole of what the ticket gets.
func TestTheAgentsTranscriptKeepsNoCredential(t *testing.T) {
	cardsecret.Forget()
	t.Setenv("DATABASE_URL", "postgres://warehouse.invalid/orders?password=hunter2hunter2")
	t.Setenv(cardsecret.NamesEnv, "DATABASE_URL")
	cardsecret.FromEnvironment()
	t.Cleanup(cardsecret.Forget)

	said := "接続できませんでした: postgres://warehouse.invalid/orders?password=hunter2hunter2 を確認してください。"
	kept := boundedTranscript(said)
	if strings.Contains(kept, "hunter2hunter2") {
		t.Fatalf("the transcript kept the credential: %q", kept)
	}
	if !strings.Contains(kept, "接続できませんでした") {
		t.Fatalf("the agent's account of what happened was lost with it: %q", kept)
	}
	// And the trail that carries a stopped run's report to the ticket.
	trail := ComposeUnsealedTrailWithResources(UnsealedRound{Round: 1, Report: said}, "実装", nil)
	if strings.Contains(trail, "hunter2hunter2") {
		t.Fatalf("the ticket comment would carry the credential:\n%s", trail)
	}
}

func hasAssignment(environment []string, want string) bool {
	for _, entry := range environment {
		if entry == want {
			return true
		}
	}
	return false
}
