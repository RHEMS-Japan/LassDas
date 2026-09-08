package worker

import (
	"strings"
	"testing"
	"time"
)

func TestApplierRunRecognizesOnlyConfiguredLaunch(t *testing.T) {
	config := validTestConfig()
	config.Agents.Applier = &AgentConfig{ID: "applier", Command: "applier-fixture", TimeoutSeconds: 60}
	configSHA, err := config.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	run, err := SealAgentRun(AgentRun{SchemaVersion: ArtifactSchemaVersion, Stage: 1,
		DeliveryID: "delivery_" + strings.Repeat("a", 32), InputSHA256: strings.Repeat("b", 64), ConfigSHA256: configSHA,
		ToolSHA: strings.Repeat("c", 40), BaseSHA: strings.Repeat("d", 40), AgentID: "applier", Command: "applier-fixture",
		PromptBytes: 100, ChangedFiles: []string{"client/src/label.ts"}, RanAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Validate(config); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*AgentRun){
		func(r *AgentRun) { r.AgentID = "unconfigured" },
		func(r *AgentRun) { r.Command = "different-launch" },
		func(r *AgentRun) { r.PromptBytes = 0 },
	} {
		altered := run
		change(&altered)
		altered, err = SealAgentRun(altered)
		if err != nil {
			t.Fatal(err)
		}
		if altered.Validate(config) == nil {
			t.Fatal("unobserved or unconfigured launch accepted")
		}
	}
	without := config
	without.Agents.Applier = nil
	if run.Validate(without) == nil {
		t.Fatal("applier absent from config accepted")
	}
}
