package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The workflow reads the consumer configuration with jq at run time, so a
// change to the configuration's shape cannot be caught by the compiler. These
// tests read the workflow itself and fail when the two drift apart — the
// failure mode otherwise is a live run that dies in its first job.
const workflowFile = "../../.github/workflows/m1-worker.yml"

func readWorkflow(t *testing.T) string {
	t.Helper()
	encoded, err := os.ReadFile(workflowFile)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// TestWorkflowHasNoEmptyKeys catches a shape that parses as YAML but that
// GitHub refuses to run: a key such as "outputs:" left with nothing under it,
// which is what removing the last entry of a block leaves behind. The failure
// otherwise appears only when a run is dispatched.
func TestWorkflowHasNoEmptyKeys(t *testing.T) {
	lines := strings.Split(readWorkflow(t), "\n")
	blockKey := regexp.MustCompile(`^(\s*)([a-zA-Z_-]+):\s*$`)
	for index, line := range lines {
		match := blockKey.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		indent := len(match[1])
		// A block key must be followed by something indented under it.
		hasChild := false
		for _, next := range lines[index+1:] {
			if strings.TrimSpace(next) == "" || strings.HasPrefix(strings.TrimSpace(next), "#") {
				continue
			}
			hasChild = len(next)-len(strings.TrimLeft(next, " ")) > indent
			break
		}
		if !hasChild {
			t.Fatalf("line %d: %q has nothing under it; GitHub refuses to parse the workflow", index+1, strings.TrimSpace(line))
		}
	}
}

func TestWorkflowReadsTheConfiguredShape(t *testing.T) {
	workflow := readWorkflow(t)
	if strings.Contains(workflow, ".consumer.") {
		t.Fatal("the workflow still reads a single consumer; the configuration holds a list")
	}
	if !strings.Contains(workflow, ".consumers[]") {
		t.Fatal("the workflow no longer reads the consumer list")
	}
}

// TestWorkflowJQPathsResolveAgainstTheRealConfiguration walks every field the
// workflow selects out of a consumer and proves the shipped configuration
// actually carries it, for every configured destination.
func TestWorkflowJQPathsResolveAgainstTheRealConfiguration(t *testing.T) {
	workflow := readWorkflow(t)
	config, err := LoadConfig(filepath.Join("..", "..", "config", "m1-consumer.json"))
	if err != nil {
		t.Fatal(err)
	}
	selected := regexp.MustCompile(`\[\.consumers\[\] \| select\(\.repository == \$repo\)\]\[0\]\.([a-z_.]+)`)
	matches := selected.FindAllStringSubmatch(workflow, -1)
	if len(matches) == 0 {
		t.Fatal("no consumer field selections were found in the workflow")
	}
	for _, match := range matches {
		field := match[1]
		for _, consumer := range config.Consumers {
			present := false
			switch field {
			case "delivery":
				present = consumer.Delivery.valid()
			case "mode.toolchain":
				// An empty toolchain is a valid answer (a destination may pin
				// no binary); the path itself must exist on the type.
				present = consumer.Mode.ID != ""
			default:
				t.Fatalf("the workflow selects %q, which no test verifies", field)
			}
			if !present {
				t.Fatalf("consumer %s does not carry %q", consumer.Repository, field)
			}
		}
	}
}

// TestWorkflowReadsFieldsTheArtifactsCarry fixes the field names the workflow
// selects out of worker artifacts. These live only in shell, so a renamed JSON
// tag would otherwise surface as a run that dies partway through.
func TestWorkflowReadsFieldsTheArtifactsCarry(t *testing.T) {
	workflow := readWorkflow(t)
	for _, expected := range []struct {
		path     string
		artifact any
		field    string
	}{
		{"ticket-draft.json", TicketDraft{}, "repository"},
		{"ticket-draft.json", TicketDraft{}, "absent_text"},
		{"intake.json", ContractIntake{}, "gaps"},
		{"decision.json", StageDecision{}, "outcome"},
		{"readiness decision", ReadinessDecision{}, "outcome"},
		{"readiness check", ReadinessCheck{}, "verdict"},
	} {
		if !strings.Contains(workflow, "'."+expected.field) && !strings.Contains(workflow, "."+expected.field+" ") {
			t.Errorf("the workflow no longer reads %q; this test is stale", expected.field)
			continue
		}
		if !marshalsField(t, expected.artifact, expected.field) {
			t.Errorf("%s does not carry %q, which the workflow reads", expected.path, expected.field)
		}
	}
}

func marshalsField(t *testing.T, value any, field string) bool {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	_, present := fields[field]
	return present
}

// TestWorkflowDeliveryGatesCoverEveryStoppingPoint fixes the gate expressions:
// every job past the proposal must be skipped for a pull-request delivery, and
// the production jobs must run only for a production delivery.
func TestWorkflowDeliveryGatesCoverEveryStoppingPoint(t *testing.T) {
	workflow := readWorkflow(t)
	for _, gate := range []string{
		"needs.source.outputs.delivery != 'pull_request'",
		"needs.source.outputs.delivery == 'production'",
	} {
		if !strings.Contains(workflow, gate) {
			t.Fatalf("the workflow lost the delivery gate %q", gate)
		}
	}
	if strings.Contains(workflow, "needs.tools.outputs.delivery") {
		t.Fatal("the delivery is a property of the ticket's destination, so it cannot come from the tools job")
	}
}

// TestWorkflowJobOutputsAreDeclared proves every value one job reads from
// another is actually published by that job. An undeclared output resolves to
// an empty string at run time, which reaches a command as a missing argument
// rather than as a workflow error.
func TestWorkflowJobOutputsAreDeclared(t *testing.T) {
	workflow := readWorkflow(t)
	declared := declaredJobOutputs(t, workflow)
	referenced := regexp.MustCompile(`needs\.([a-z-]+)\.outputs\.([a-z_-]+)`)
	matches := referenced.FindAllStringSubmatch(workflow, -1)
	if len(matches) == 0 {
		t.Fatal("no job output references were found in the workflow")
	}
	for _, match := range matches {
		job, output := match[1], match[2]
		outputs, known := declared[job]
		if !known {
			t.Fatalf("the workflow reads outputs of job %q, which it does not define", job)
		}
		if _, published := outputs[output]; !published {
			t.Fatalf("job %q does not publish %q, so reading it yields nothing", job, output)
		}
	}
}

// declaredJobOutputs reads each job's outputs block out of the workflow text.
// The file is indented by a fixed amount per level, which is what the offsets
// below follow.
func declaredJobOutputs(t *testing.T, workflow string) map[string]map[string]struct{} {
	t.Helper()
	declared := make(map[string]map[string]struct{})
	jobHeader := regexp.MustCompile(`^  ([a-z][a-z-]*):$`)
	outputEntry := regexp.MustCompile(`^      ([a-z_-]+):`)
	job := ""
	inOutputs := false
	for _, line := range strings.Split(workflow, "\n") {
		if header := jobHeader.FindStringSubmatch(line); header != nil {
			job, inOutputs = header[1], false
			declared[job] = map[string]struct{}{}
			continue
		}
		if job == "" {
			continue
		}
		if line == "    outputs:" {
			inOutputs = true
			continue
		}
		if strings.HasPrefix(line, "    ") && !strings.HasPrefix(line, "     ") {
			inOutputs = false
			continue
		}
		if inOutputs {
			if entry := outputEntry.FindStringSubmatch(line); entry != nil {
				declared[job][entry[1]] = struct{}{}
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("no jobs were found in the workflow")
	}
	return declared
}

// TestWorkflowArtifactsAreWrittenBeforeTheyAreRead proves every file one step
// hands to a later step is produced somewhere. A renamed artifact otherwise
// surfaces as a command that cannot find its input, deep into a live run.
func TestWorkflowArtifactsAreWrittenBeforeTheyAreRead(t *testing.T) {
	workflow := readWorkflow(t)
	// Files read out of a directory another job filled. The archive and its
	// digest are produced by shell, and the configuration ships with the
	// verified tools, so neither comes from a command's output flag.
	produced := map[string]struct{}{
		"target.tar": {}, "target.tar.sha256": {}, "m1-consumer.json": {},
	}
	for _, match := range regexp.MustCompile(`--[a-z-]*out\s+"\$[A-Za-z_]+/([a-z0-9./$-]+\.json)"`).FindAllStringSubmatch(workflow, -1) {
		produced[filepath.Base(match[1])] = struct{}{}
	}
	for _, match := range regexp.MustCompile(`cp "[^"]+" "[^"]*/([a-z-]+\.json)"`).FindAllStringSubmatch(workflow, -1) {
		produced[match[1]] = struct{}{}
	}
	read := regexp.MustCompile(`"\$(?:SOURCE_INPUT|INPUT|DOWNLOADED_[A-Z]+|sandbox/[a-z]+)/([a-z0-9-]+\.(?:json|tar|sha256))"`)
	matches := read.FindAllStringSubmatch(workflow, -1)
	if len(matches) == 0 {
		t.Fatal("no handed-over artifacts were found in the workflow")
	}
	for _, match := range matches {
		if _, written := produced[match[1]]; !written {
			t.Fatalf("the workflow reads %q, which no step writes", match[1])
		}
	}
}

// TestWorkflowGivesTheModelJobTimeForItsAgents holds the model job's ceiling
// to the agent budgets the configuration grants. Whichever side drifts, this
// fails before a live run is killed mid-stage.
func TestWorkflowGivesTheModelJobTimeForItsAgents(t *testing.T) {
	workflow := readWorkflow(t)
	config, err := LoadConfig(filepath.Join("..", "..", "config", "m1-consumer.json"))
	if err != nil {
		t.Fatal(err)
	}
	jobStart := strings.Index(workflow, "\n  model:\n")
	if jobStart < 0 {
		t.Fatal("the model job was not found")
	}
	match := regexp.MustCompile(`timeout-minutes: (\d+)`).FindStringSubmatch(workflow[jobStart:])
	if match == nil {
		t.Fatal("the model job has no timeout")
	}
	granted, err := strconv.Atoi(match[1])
	if err != nil {
		t.Fatal(err)
	}
	// The reviewer's worst case is every fast retry burned plus one full
	// slow attempt - the retry gate refuses to roll again after a slow
	// failure, which is what keeps this sum finite.
	reviewerWorst := (ReviewAttemptLimit-1)*int(ReviewRetryEligible.Seconds()) + config.Agents.Reviewer.TimeoutSeconds
	perStage := config.Agents.Implementer.TimeoutSeconds + reviewerWorst
	impasseMinutes := int(ModelInvocationTimeout.Minutes())
	// Readiness is a model cost like any other: up to MaxReadinessAttempts
	// assessor+checker invocations, each bounded by ModelInvocationTimeout.
	// A fixed constant here measurably went stale when the attempt limit
	// grew, which is exactly the drift this test exists to catch.
	readinessMinutes := MaxReadinessAttempts * 2 * int(ModelInvocationTimeout.Minutes())
	needed := (config.MaxStages*perStage)/60 + 10 + readinessMinutes + impasseMinutes // stages plus setup, readiness and the impasse question
	if granted < needed {
		t.Fatalf("the model job may be killed mid-stage: %d minutes granted, %d needed", granted, needed)
	}
}

// TestWorkflowReportsIntakeQuestionsAsAnHonestStop pins the mapping that kept
// the first live ticket's dead end unreported: an intake that still has open
// questions must reach the requester as a clarification stop, never fall
// through to an internal failure.
func TestWorkflowReportsIntakeQuestionsAsAnHonestStop(t *testing.T) {
	workflow := readWorkflow(t)
	guard := `if [[ "$INTAKE_RESULT" == success && "$INTAKE_OUTCOME" == questions ]]; then`
	if !strings.Contains(workflow, guard) {
		t.Fatal("the report step no longer routes open intake questions")
	}
	block := workflow[strings.Index(workflow, guard):]
	if len(block) > 700 {
		block = block[:700]
	}
	if !strings.Contains(block, "code=clarification_required") {
		t.Fatal("open intake questions no longer stop as clarification_required")
	}
}
