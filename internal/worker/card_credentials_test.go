package worker

import (
	"os"
	"path/filepath"
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

	environment, err := agentEnvironment(AgentConfig{ID: "implementer", Command: "agent"}, t.TempDir(), "")
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
	environment, err := agentEnvironment(AgentConfig{ID: "implementer", Command: "agent"}, t.TempDir(), "")
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

// A credential handed over as a file name names a file the AI user is
// guaranteed not to be able to open: the configured file is in the set the
// boot refuses to start over, precisely so that a card the credential does
// not name cannot read it. So the launch places a copy the AI can open, and
// hands it that.
func TestAPathCredentialIsLentToTheAgentAsACopyItCanOpen(t *testing.T) {
	cardsecret.Forget()
	t.Cleanup(cardsecret.Forget)
	configured := filepath.Join(t.TempDir(), "cloud")
	contents := "[dev]\naws_secret_access_key = wJalrXUtnFEMIexampleKEY\n"
	if err := os.WriteFile(configured, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", configured)
	t.Setenv(cardsecret.NamesEnv, "AWS_SHARED_CREDENTIALS_FILE")
	t.Setenv(cardsecret.PathNamesEnv, "AWS_SHARED_CREDENTIALS_FILE")
	cardsecret.FromEnvironment()

	workingCopy := t.TempDir()
	lentHome := t.TempDir()
	environment, err := agentEnvironment(AgentConfig{ID: "implementer", Command: "agent"}, lentHome, lentHome)
	if err != nil {
		t.Fatalf("agentEnvironment: %v", err)
	}
	handed := valueOf(environment, "AWS_SHARED_CREDENTIALS_FILE")
	if handed == "" || handed == configured {
		t.Fatalf("the agent was handed the configured path it cannot open: %q", handed)
	}
	if strings.HasPrefix(handed, workingCopy) {
		t.Fatalf("the copy is inside the working copy and would be committed: %q", handed)
	}
	if !strings.HasPrefix(handed, lentHome) {
		t.Fatalf("the copy is outside the home the launch cleans up: %q", handed)
	}
	info, err := os.Stat(handed)
	if err != nil {
		t.Fatalf("the copy is not there: %v", err)
	}
	if info.Mode().Perm()&0o444 != 0o444 {
		t.Fatalf("the copy is not readable: %v", info.Mode())
	}
	if info.Mode().Perm()&0o222 != 0 {
		t.Fatalf("the copy is writable: %v", info.Mode())
	}
	read, err := os.ReadFile(handed)
	if err != nil || string(read) != contents {
		t.Fatalf("the copy does not hold the credential: %q %v", read, err)
	}
	// The configured file is still the one the boot guards; nothing moved.
	if _, err := os.Stat(configured); err != nil {
		t.Fatalf("the configured file was disturbed: %v", err)
	}
}

// The copy belongs to the launch, so the launch's own cleanup takes it —
// the same one that runs for a card that failed or was interrupted.
func TestTheLentCopyGoesWithTheLaunchHome(t *testing.T) {
	cardsecret.Forget()
	t.Cleanup(cardsecret.Forget)
	configured := filepath.Join(t.TempDir(), "cloud")
	if err := os.WriteFile(configured, []byte("a-credential-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", configured)
	t.Setenv(cardsecret.NamesEnv, "KUBECONFIG")
	t.Setenv(cardsecret.PathNamesEnv, "KUBECONFIG")
	cardsecret.FromEnvironment()

	lentHome := t.TempDir()
	environment, err := agentEnvironment(AgentConfig{ID: "implementer", Command: "agent"}, lentHome, lentHome)
	if err != nil {
		t.Fatal(err)
	}
	handed := valueOf(environment, "KUBECONFIG")
	if _, err := os.Stat(handed); err != nil {
		t.Fatalf("the copy was not placed: %v", err)
	}
	// removeAgentHome is what the launch defers, for every way a card ends.
	if err := removeAgentHome(lentHome); err != nil {
		t.Fatalf("removeAgentHome: %v", err)
	}
	if _, err := os.Stat(handed); !os.IsNotExist(err) {
		t.Fatalf("the copy outlived the card: %v", err)
	}
}

// A card that launches nothing under another user keeps the configured
// path: the process is the engine's own, which can open it.
func TestACardWithoutALentHomeKeepsTheConfiguredPath(t *testing.T) {
	cardsecret.Forget()
	t.Cleanup(cardsecret.Forget)
	configured := filepath.Join(t.TempDir(), "cloud")
	if err := os.WriteFile(configured, []byte("a-credential-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", configured)
	t.Setenv(cardsecret.NamesEnv, "AWS_CONFIG_FILE")
	t.Setenv(cardsecret.PathNamesEnv, "AWS_CONFIG_FILE")
	cardsecret.FromEnvironment()

	environment, err := agentEnvironment(AgentConfig{ID: "implementer", Command: "agent"}, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := valueOf(environment, "AWS_CONFIG_FILE"); got != configured {
		t.Fatalf("the configured path was replaced where nothing needed it: %q", got)
	}
}

// A credential handed over as contents is unchanged: the value goes into
// the variable and no file is placed anywhere.
func TestAContentsCredentialIsNotCopiedAnywhere(t *testing.T) {
	cardsecret.Forget()
	t.Cleanup(cardsecret.Forget)
	t.Setenv("DATABASE_URL", "postgres://warehouse.invalid/orders")
	t.Setenv(cardsecret.NamesEnv, "DATABASE_URL")
	cardsecret.FromEnvironment()

	lentHome := t.TempDir()
	environment, err := agentEnvironment(AgentConfig{ID: "implementer", Command: "agent"}, lentHome, lentHome)
	if err != nil {
		t.Fatal(err)
	}
	if got := valueOf(environment, "DATABASE_URL"); got != "postgres://warehouse.invalid/orders" {
		t.Fatalf("a contents credential changed: %q", got)
	}
	if _, err := os.Stat(filepath.Join(lentHome, credentialLendDir)); !os.IsNotExist(err) {
		t.Fatalf("a file was placed for a credential that needed none: %v", err)
	}
}

func valueOf(environment []string, name string) string {
	for _, entry := range environment {
		if variable, value, found := strings.Cut(entry, "="); found && variable == name {
			return value
		}
	}
	return ""
}

// The logs a handed credential could leave through are masked; the round's
// own diff is the way out that is left. A change carrying the value would
// go into the pull request and stay there, so the last gate before a change
// leaves refuses it — naming the variable, because a refusal travels into
// the record and onto the ticket and the value must not travel with it.
func TestACandidateCarryingAHandedValueIsRefused(t *testing.T) {
	cardsecret.Forget()
	t.Cleanup(cardsecret.Forget)
	secret := "postgres://warehouse.invalid/orders?password=hunter2hunter2"

	config, request, source, candidate := validCandidate(t)
	clean := candidate
	if err := clean.Validate(source, request, config); err != nil {
		t.Fatalf("the fixture is refused before anything is added: %v", err)
	}

	cardsecret.Register([]cardsecret.Entry{{Name: "DATABASE_URL", Secret: secret}})
	carried := candidate
	carried.Files = append([]CandidateFile(nil), candidate.Files...)
	carried.Files[0].Content += "\nconst dsn = \"" + secret + "\"\n"
	err := carried.Validate(source, request, config)
	if err == nil {
		t.Fatal("a change carrying the handed value was accepted")
	}
	if !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("the refusal does not name the variable: %v", err)
	}
	if strings.Contains(err.Error(), "hunter2hunter2") {
		t.Fatalf("the refusal published the value: %v", err)
	}
	// One line of a file handed over by path is the same leak.
	cardsecret.Forget()
	cardsecret.Register([]cardsecret.Entry{{Name: "AWS_SHARED_CREDENTIALS_FILE", Secret: "[dev]\naws_secret_access_key = wJalrXUtnFEMIexampleKEY\n", Path: true}})
	line := candidate
	line.Files = append([]CandidateFile(nil), candidate.Files...)
	line.Files[0].Content += "\n// aws_secret_access_key = wJalrXUtnFEMIexampleKEY\n"
	if err := line.Validate(source, request, config); err == nil {
		t.Fatal("a change carrying one line of a handed file was accepted")
	}
}

// The head of a model's answer travels into the run's failure record and
// the job log, both of which outlive the turn. A model asked to work
// against a service can repeat the credential it was given back at the
// engine, and the general masker knows no shape for it.
func TestTheAnswerHeadKeepsNoHandedCredential(t *testing.T) {
	cardsecret.Forget()
	t.Cleanup(cardsecret.Forget)
	secret := "postgres://warehouse.invalid/orders?password=hunter2hunter2"
	cardsecret.Register([]cardsecret.Entry{{Name: "DATABASE_URL", Secret: secret}})

	head := answerHead("I could not connect to " + secret + " and stopped there")
	if strings.Contains(head, "hunter2hunter2") {
		t.Fatalf("the failure record would carry the credential: %q", head)
	}
	if head == "" {
		t.Fatal("the head says nothing at all about what the model answered")
	}
}
