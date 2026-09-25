package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/worker"
)

// allowConsumerInfrastructure points the pipeline at a destination that
// allows the kinds given, and gives the run the draft the collector reads
// the destination's name from.
func allowConsumerInfrastructure(t *testing.T, pipeline *Pipeline, kinds ...string) {
	t.Helper()
	consumer := map[string]any{
		"repository": "example/app",
		"delivery":   "pull_request",
		"mode":       map[string]any{"toolchain": []any{}},
	}
	if kinds != nil {
		consumer["infrastructure"] = map[string]any{"provider": "aws", "resources": kinds}
	}
	encoded, err := json.Marshal(map[string]any{
		"max_stages": 3,
		"models":     map[string]any{"reviewers": []any{map[string]any{"id": "review-a"}, map[string]any{"id": "review-b"}}},
		"consumers":  []any{consumer},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "consumer.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	pipeline.Config.ConsumerConfigPath = path
	if err := os.WriteFile(pipeline.path("ticket-draft.json"), []byte(`{"repository":"example/app"}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readRecordedResources(t *testing.T, pipeline *Pipeline) []worker.CreatedResource {
	t.Helper()
	raw, err := os.ReadFile(pipeline.path(ResourcesFile))
	if err != nil {
		t.Fatalf("the record was not written: %v", err)
	}
	var created []worker.CreatedResource
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var record worker.CreatedResource
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("a recorded line is not readable: %q", line)
		}
		created = append(created, record)
	}
	return created
}

// A resource the run brought into existence is written down with what it
// is, where to find it, which card made it and when. Without the record it
// is invisible: it is not in the diff, it outlives the delivery, and it
// costs money until somebody knows it is there.
func TestACreatedResourceIsRecordedWithTheCardAndTheTime(t *testing.T) {
	pipeline := chainStagePipeline(t)
	allowConsumerInfrastructure(t, pipeline, "sqs", "s3", "rds")
	workingCopy := pipeline.path("target-repo")
	if err := os.MkdirAll(workingCopy, 0o755); err != nil {
		t.Fatal(err)
	}
	claims := `{"kind":"sqs","identifier":"lassdas-orders-intake","provider":"aws"}
{"kind":"s3","identifier":"lassdas-orders-archive"}
`
	if err := os.WriteFile(filepath.Join(workingCopy, AgentResourcesFile), []byte(claims), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.RecordCreatedResources(runtime.StageImplement, workingCopy); err != nil {
		t.Fatalf("RecordCreatedResources: %v", err)
	}
	created := readRecordedResources(t, pipeline)
	if len(created) != 2 {
		t.Fatalf("recorded %d resources", len(created))
	}
	if created[0].Kind != "sqs" || created[0].Identifier != "lassdas-orders-intake" || created[0].Provider != "aws" {
		t.Fatalf("the first record is %+v", created[0])
	}
	if created[0].Stage != runtime.StageImplement || created[0].CreatedAt.IsZero() {
		t.Fatalf("the card and the time were not stamped: %+v", created[0])
	}
	// The claim file must not reach the sealed candidate: the next card
	// snapshots this working copy as the change being proposed.
	if _, err := os.Stat(filepath.Join(workingCopy, AgentResourcesFile)); !os.IsNotExist(err) {
		t.Fatalf("the agent's claim file stayed in the working copy: %v", err)
	}
}

// The card stamps the two fields it is in a position to assert. An agent
// that writes them is not refused, but what it wrote is a claim about this
// process's own timeline, and a report must not say a resource was made in
// a stage that never ran.
func TestTheCardStampsTheStageAndTheTimeItself(t *testing.T) {
	pipeline := chainStagePipeline(t)
	allowConsumerInfrastructure(t, pipeline, "sqs", "s3", "rds")
	workingCopy := pipeline.path("target-repo")
	if err := os.MkdirAll(workingCopy, 0o755); err != nil {
		t.Fatal(err)
	}
	claims := `{"kind":"rds","identifier":"orders-db","stage":"publish","created_at":"1999-01-01T00:00:00Z"}` + "\n"
	if err := os.WriteFile(filepath.Join(workingCopy, AgentResourcesFile), []byte(claims), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.RecordCreatedResources(runtime.StageApply, workingCopy); err != nil {
		t.Fatalf("RecordCreatedResources: %v", err)
	}
	created := readRecordedResources(t, pipeline)
	if created[0].Stage != runtime.StageApply {
		t.Fatalf("the agent's own stage was believed: %+v", created[0])
	}
	if created[0].CreatedAt.Year() < 2020 {
		t.Fatalf("the agent's own time was believed: %+v", created[0])
	}
}

// Rounds add to the record rather than replacing it, and a line that names
// neither what was made nor where to find it records nothing.
func TestTheRecordAccumulatesAndSkipsEmptyClaims(t *testing.T) {
	pipeline := chainStagePipeline(t)
	allowConsumerInfrastructure(t, pipeline, "sqs", "s3", "rds")
	workingCopy := pipeline.path("target-repo")
	if err := os.MkdirAll(workingCopy, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(workingCopy, AgentResourcesFile), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"kind":"sqs","identifier":"one"}` + "\n")
	if err := pipeline.RecordCreatedResources(runtime.StageImplement, workingCopy); err != nil {
		t.Fatal(err)
	}
	write("{\"kind\":\"sqs\"}\nnot json at all\n{\"identifier\":\"two\"}\n{\"kind\":\"s3\",\"identifier\":\"two\"}\n")
	if err := pipeline.RecordCreatedResources(runtime.StageImplement, workingCopy); err != nil {
		t.Fatal(err)
	}
	created := readRecordedResources(t, pipeline)
	if len(created) != 2 || created[1].Identifier != "two" {
		t.Fatalf("recorded %+v", created)
	}
}

// A run that made nothing writes no record, which is every run that
// delivers only a change to a repository.
func TestARunThatCreatedNothingWritesNoRecord(t *testing.T) {
	pipeline := chainStagePipeline(t)
	allowConsumerInfrastructure(t, pipeline, "sqs", "s3", "rds")
	workingCopy := pipeline.path("target-repo")
	if err := os.MkdirAll(workingCopy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.RecordCreatedResources(runtime.StageImplement, workingCopy); err != nil {
		t.Fatalf("RecordCreatedResources: %v", err)
	}
	if _, err := os.Stat(pipeline.path(ResourcesFile)); !os.IsNotExist(err) {
		t.Fatalf("a record was written for a run that created nothing: %v", err)
	}
}

// The record has to reach the composer, or it is a file nobody reads. The
// run's trail is what the pull request body and the ticket's closing
// comment carry, so this is the whole path from a card's claim to the
// requester.
func TestTheRunsRecordReachesTheTrailComposer(t *testing.T) {
	log := filepath.Join(t.TempDir(), "worker.log")
	fake := filepath.Join(t.TempDir(), "worker")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+log+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	pipeline := chainStagePipeline(t)
	pipeline.Config.WorkerBin = fake
	allowConsumerInfrastructure(t, pipeline, "sqs")
	workingCopy := pipeline.path("target-repo")
	if err := os.MkdirAll(workingCopy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workingCopy, AgentResourcesFile), []byte(`{"kind":"sqs","identifier":"lassdas-orders-intake"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.RecordCreatedResources(runtime.StageImplement, workingCopy); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.EnsureTrail(context.Background()); err != nil {
		t.Fatalf("EnsureTrail: %v", err)
	}
	argv, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("the composer did not run: %v", err)
	}
	if !strings.Contains(string(argv), "--resources "+pipeline.path(ResourcesFile)) {
		t.Fatalf("the record did not reach the composer: %s", argv)
	}
}

// The card collects what its agent made, and does so whether or not the
// agent's card then finished: something that was provisioned exists from
// then on, and a resource left out of the record is one nobody knows to
// remove. The claim file goes either way, because the next card seals this
// working copy as the change being proposed.
func TestTheImplementCardCollectsWhatItsAgentMade(t *testing.T) {
	for _, exit := range []struct {
		name string
		code string
	}{{"a card that finished", "exit 0"}, {"a card that failed", "exit 4"}} {
		t.Run(exit.name, func(t *testing.T) {
			pipeline := chainStagePipeline(t)
			allowConsumerInfrastructure(t, pipeline, "sqs")
			workingCopy := pipeline.path("target-repo")
			if err := os.MkdirAll(workingCopy, 0o755); err != nil {
				t.Fatal(err)
			}
			// RunChainStage resolves the consumer and reads the baseline
			// before it dispatches anything, exactly as a live card does.
			if err := os.WriteFile(pipeline.path("baseline.json"), []byte(`{"baseline":{"Integration":{"SHA":"`+strings.Repeat("ab", 20)+`"}}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			claim := filepath.Join(workingCopy, AgentResourcesFile)
			agent := filepath.Join(t.TempDir(), "worker")
			script := "#!/bin/sh\nprintf '%s\\n' '{\"kind\":\"sqs\",\"identifier\":\"lassdas-orders-intake\"}' > " + claim + "\n" + exit.code + "\n"
			if err := os.WriteFile(agent, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			pipeline.Config.WorkerBin = agent
			if err := os.WriteFile(pipeline.path("INSTRUCTION.md"), []byte("do the thing"), 0o600); err != nil {
				t.Fatal(err)
			}
			err := pipeline.RunChainStage(context.Background(), runtime.StageImplement)
			if (err != nil) != (exit.code != "exit 0") {
				t.Fatalf("RunChainStage: %v", err)
			}
			created := readRecordedResources(t, pipeline)
			if len(created) != 1 || created[0].Identifier != "lassdas-orders-intake" {
				t.Fatalf("the card recorded %+v", created)
			}
			if created[0].Stage != runtime.StageImplement {
				t.Fatalf("the card was not stamped: %+v", created[0])
			}
			if _, statErr := os.Stat(claim); !os.IsNotExist(statErr) {
				t.Fatalf("the claim file stayed where the next card seals: %v", statErr)
			}
		})
	}
}

// A kind the destination never allowed is not something it agreed to. The
// declaration is kept — an agent that said it made something is the only
// evidence anyone has that it may exist — but it is recorded as refused and
// never reported as created.
func TestAKindTheDestinationDoesNotAllowIsRecordedAsRefused(t *testing.T) {
	pipeline := chainStagePipeline(t)
	allowConsumerInfrastructure(t, pipeline, "sqs")
	workingCopy := pipeline.path("target-repo")
	if err := os.MkdirAll(workingCopy, 0o755); err != nil {
		t.Fatal(err)
	}
	claims := `{"kind":"sqs","identifier":"lassdas-orders-intake"}` + "\n" +
		`{"kind":"rds","identifier":"orders-db"}` + "\n"
	if err := os.WriteFile(filepath.Join(workingCopy, AgentResourcesFile), []byte(claims), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.RecordCreatedResources(runtime.StageImplement, workingCopy); err != nil {
		t.Fatal(err)
	}
	created := readRecordedResources(t, pipeline)
	if len(created) != 2 {
		t.Fatalf("recorded %+v", created)
	}
	if created[0].Refused {
		t.Fatalf("an allowed kind was refused: %+v", created[0])
	}
	if !created[1].Refused {
		t.Fatalf("a kind the destination never allowed was accepted: %+v", created[1])
	}
	// And the report keeps the two apart.
	trail := worker.ComposeUnsealedTrailWithResources(worker.UnsealedRound{Round: 1, Report: "済み"}, "", created)
	outcome := strings.Index(trail, "この依頼で作った資源")
	refused := strings.Index(trail, "許可されていない種類として退けた宣言")
	if outcome < 0 || refused < 0 || refused < outcome {
		t.Fatalf("the report does not keep created and refused apart:\n%s", trail)
	}
	if strings.Index(trail, "orders-db") < refused {
		t.Fatalf("a refused declaration is reported as created:\n%s", trail)
	}
}

// A destination that declared no infrastructure allows nothing, and so does
// a configuration that cannot be read: the permission has to be
// established, never assumed from silence.
func TestNothingIsAllowedWithoutAStandingPermission(t *testing.T) {
	for name, configure := range map[string]func(*testing.T, *Pipeline){
		"no infrastructure block": func(t *testing.T, pipeline *Pipeline) {
			allowConsumerInfrastructure(t, pipeline)
		},
		"an unreadable configuration": func(t *testing.T, pipeline *Pipeline) {
			allowConsumerInfrastructure(t, pipeline, "sqs")
			pipeline.Config.ConsumerConfigPath = filepath.Join(t.TempDir(), "absent.json")
		},
	} {
		t.Run(name, func(t *testing.T) {
			pipeline := chainStagePipeline(t)
			configure(t, pipeline)
			workingCopy := pipeline.path("target-repo")
			if err := os.MkdirAll(workingCopy, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(workingCopy, AgentResourcesFile), []byte(`{"kind":"sqs","identifier":"one"}`+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := pipeline.RecordCreatedResources(runtime.StageImplement, workingCopy); err != nil {
				t.Fatal(err)
			}
			created := readRecordedResources(t, pipeline)
			if len(created) != 1 || !created[0].Refused {
				t.Fatalf("recorded %+v", created)
			}
		})
	}
}

// Not only the two cards that launch a writing agent. A review agent, the
// destination's own verification commands and a delivery step all run with
// the credentials their card was named in, so any of them can bring
// something into existence, and a resource left out of the record is one
// nobody knows to remove.
func TestEveryCardCollectsWhatItMade(t *testing.T) {
	for _, stage := range []string{runtime.StageReviewA, runtime.StageValidate, runtime.StagePublish, runtime.StageApply} {
		t.Run(stage, func(t *testing.T) {
			pipeline := chainStagePipeline(t)
			allowConsumerInfrastructure(t, pipeline, "sqs")
			pipeline.Config.WorkerBin = "false"
			workingCopy := pipeline.path("target-repo")
			if err := os.MkdirAll(workingCopy, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(pipeline.path("baseline.json"), []byte(`{"baseline":{"Integration":{"SHA":"`+strings.Repeat("ab", 20)+`"}}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(workingCopy, AgentResourcesFile), []byte(`{"kind":"sqs","identifier":"made-by-`+stage+`"}`+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			// The card itself fails — there is nothing sealed for it to work
			// on — and what it made is collected all the same.
			_ = pipeline.RunChainStage(context.Background(), stage)
			created := readRecordedResources(t, pipeline)
			if len(created) != 1 || created[0].Stage != stage {
				t.Fatalf("%s recorded %+v", stage, created)
			}
			if _, err := os.Stat(filepath.Join(workingCopy, AgentResourcesFile)); !os.IsNotExist(err) {
				t.Fatalf("%s left the declaration in the working copy: %v", stage, err)
			}
		})
	}
}

// The declarations are written down before the file goes. The other order
// lost every one of them whenever the removal failed, and turned a card
// that had finished its work into a failed one — over a file the engine
// itself was tidying up.
func TestADeclarationSurvivesAFileThatCannotBeRemoved(t *testing.T) {
	pipeline := chainStagePipeline(t)
	allowConsumerInfrastructure(t, pipeline, "sqs")
	// A directory in the file's place: the read fails the same way an
	// unremovable file would, and the removal of a non-empty one fails.
	workingCopy := pipeline.path("target-repo")
	if err := os.MkdirAll(workingCopy, 0o755); err != nil {
		t.Fatal(err)
	}
	claim := filepath.Join(workingCopy, AgentResourcesFile)
	if err := os.WriteFile(claim, []byte(`{"kind":"sqs","identifier":"one"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The directory holding it is made read-only, so the entry cannot be
	// unlinked while the file itself still reads.
	if err := os.Chmod(workingCopy, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(workingCopy, 0o755) })

	if err := pipeline.RecordCreatedResources(runtime.StageImplement, workingCopy); err != nil {
		t.Fatalf("a card was failed over a file the engine was tidying up: %v", err)
	}
	created := readRecordedResources(t, pipeline)
	if len(created) != 1 || created[0].Identifier != "one" {
		t.Fatalf("the declaration was lost: %+v", created)
	}
	// And what it held is gone, so an unremovable file cannot carry a
	// declaration — or a credential an agent wrote into it — into the
	// change being proposed.
	left, err := os.ReadFile(claim)
	if err == nil && len(left) > 0 {
		t.Fatalf("the file still holds %q", left)
	}
}
