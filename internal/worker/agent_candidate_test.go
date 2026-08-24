package worker

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// validExternalRun is a sealed record of a change some other launcher's agent
// made: launch facts at their sentinels, the observation filled in.
func validExternalRun(t *testing.T, config Config) AgentRun {
	t.Helper()
	configSHA, err := config.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	run, err := SealAgentRun(AgentRun{
		SchemaVersion: ArtifactSchemaVersion, Stage: 1,
		DeliveryID: "delivery_" + strings.Repeat("ab", 16), InputSHA256: strings.Repeat("ab", 32),
		ConfigSHA256: configSHA, ToolSHA: strings.Repeat("ab", 20), BaseSHA: strings.Repeat("cd", 20),
		Kind: AgentRunKindExternal, AgentID: AgentRunKindExternal,
		ChangedFiles: []string{"client/src/label.ts"}, Transcript: "I edited the label.",
		RanAt: time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func resealRun(t *testing.T, run AgentRun) AgentRun {
	t.Helper()
	sealed, err := SealAgentRun(run)
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

func TestExternalAgentRunValidates(t *testing.T) {
	config := validTestConfig()
	if err := validExternalRun(t, config).Validate(config); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

// A launch fact carried in an external record is a claim nobody checked, so
// every one of them must hold its sentinel.
func TestExternalAgentRunRejectsClaimedLaunchFacts(t *testing.T) {
	config := validTestConfig()
	cases := map[string]func(*AgentRun){
		"command":      func(r *AgentRun) { r.Command = "stand-in-author" },
		"prompt bytes": func(r *AgentRun) { r.PromptBytes = 1 },
		"exit code":    func(r *AgentRun) { r.ExitCode = 1 },
		"duration":     func(r *AgentRun) { r.DurationMs = 1200 },
		"agent id":     func(r *AgentRun) { r.AgentID = "author-agent" },
	}
	for name, mutate := range cases {
		run := validExternalRun(t, config)
		mutate(&run)
		if err := resealRun(t, run).Validate(config); err == nil {
			t.Errorf("Validate() accepted an external run with a claimed %s", name)
		}
	}
}

func TestExternalAgentRunRequiresChangedFiles(t *testing.T) {
	config := validTestConfig()
	run := validExternalRun(t, config)
	run.ChangedFiles = nil
	if err := resealRun(t, run).Validate(config); err == nil {
		t.Fatal("Validate() accepted an external run that changed nothing")
	}
}

func TestAgentRunRejectsUnknownKind(t *testing.T) {
	config := validTestConfig()
	run := validExternalRun(t, config)
	run.Kind = "other"
	if err := resealRun(t, run).Validate(config); err == nil {
		t.Fatal("Validate() accepted an unknown run kind")
	}
}

// The external sentinel id must not double as a configured-agent id: a
// kernel-launched record still resolves its agent against the configuration.
func TestKernelAgentRunRejectsTheExternalSentinelID(t *testing.T) {
	config := validTestConfig()
	run := validExternalRun(t, config)
	run.Kind = ""
	if err := resealRun(t, run).Validate(config); err == nil {
		t.Fatal("Validate() accepted a kernel run naming the external sentinel")
	}
}

// A tail cut through a multi-byte character used to leave broken lead bytes
// that the cleaning pass swelled into replacement runes past the byte
// budget - a 4,425-byte Japanese completion report failed its whole
// candidate as "content is invalid" on a live run. The rationale must stay
// within budget and valid for any transcript.
func TestAgentRationaleStaysWithinBudgetForMultibyteTails(t *testing.T) {
	for _, size := range []int{4425, 4097, 4098, 8000} {
		transcript := strings.Repeat("あ", (size/3)+2)
		transcript = transcript[:size]
		rationale := agentRationale(AgentRun{Transcript: transcript})
		if len(rationale) > 4096 || !utf8.ValidString(rationale) {
			t.Fatalf("size %d: rationale bytes=%d valid=%v", size, len(rationale), utf8.ValidString(rationale))
		}
		if err := validatePlainText(rationale, 4096, true); err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
	}
}
