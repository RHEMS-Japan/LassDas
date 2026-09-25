package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/worker"
)

// gateWorkflowPath is the one file this destination lets the engine write.
const gateWorkflowPath = ".github/workflows/deploy-staging.yml"

// refusedWorkflow asks for more than the destination's policy allows: a
// push on every branch, and a token that may write the repository.
const refusedWorkflow = `name: Deploy staging
on:
  push:
permissions:
  contents: write
jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - run: ./ops/deploy.sh
`

// allowedWorkflow is the same file inside the policy.
const allowedWorkflow = `name: Deploy staging
on:
  workflow_dispatch:
permissions:
  contents: read
jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - run: ./ops/deploy.sh
`

// gateFixture is a destination that handed the means, with the sealed plan
// naming the one workflow file this round may build.
func gateFixture(t *testing.T) (agentFixture, string) {
	t.Helper()
	fixture := newTunedAgentFixture(t, "true", "true", func(_ string, config *worker.Config) {
		config.Consumers[0].Infrastructure = &worker.InfrastructureConfig{
			Provider: "aws",
			DeployWorkflows: &worker.DeployWorkflowPolicy{
				Paths:       []string{"deploy-staging.yml"},
				Triggers:    []string{worker.WorkflowTriggerDispatch},
				Permissions: map[string]string{"contents": "read"},
				Runners:     []string{"ubuntu-latest"},
			},
		}
	})
	plan := worker.ReleasePathPlan{
		SchemaVersion: worker.ReleasePathSchemaVersion,
		Repository:    fixture.config.Consumers[0].Repository,
		Configured:    "production", WorkflowFiles: []string{gateWorkflowPath},
		DecidedAt: time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC),
		Items: []worker.ReleasePathItem{{
			Name: gateWorkflowPath, Kind: worker.ReleasePathWorkflow, Detail: "build it",
		}},
	}
	if err := plan.Seal(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	planPath := fixture.path("release-path.json")
	if err := os.WriteFile(planPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return fixture, planPath
}

// writeWorkflow puts a workflow file into the fixture's working copy,
// beside the change the ticket itself asked for — which is how a real round
// leaves it: the release path is built in the same pull request as the
// request, never on its own.
func writeWorkflow(t *testing.T, fixture agentFixture, content string) {
	t.Helper()
	label := filepath.Join(fixture.repoRoot, "client", "src", "label.ts")
	if err := os.WriteFile(label, []byte("export const label = 'Updated label';\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fixture.repoRoot, filepath.FromSlash(gateWorkflowPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The content gate runs where the change is sealed, which is before any
// branch carrying it is pushed. Nothing downstream re-reads a workflow
// file's meaning, so a seal that skipped this would put a file on a branch
// that runs with the destination's own secrets the moment it lands.
func TestTheSealRefusesAWorkflowOutsideTheDestinationsPolicy(t *testing.T) {
	fixture, planPath := gateFixture(t)
	writeWorkflow(t, fixture, refusedWorkflow)
	refusalPath := fixture.path("validation-failure.json")

	err := fixture.sealCandidate(t, "--release-path", planPath, "--refusal-out", refusalPath)
	if err == nil {
		t.Fatal("a workflow outside the destination's policy was sealed")
	}
	for _, part := range []string{gateWorkflowPath, "on: の起動条件"} {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("the refusal does not name %q: %v", part, err)
		}
	}
	// Nothing was sealed: the round produced no contract, no source and no
	// candidate for anything downstream to read.
	for _, artifact := range []string{"ticket.json", "source.json", "candidate.json"} {
		if _, statErr := os.Stat(fixture.path(artifact)); statErr == nil {
			t.Fatalf("%s was written for a refused round", artifact)
		}
	}
	// The objection is left where the next round's instruction reads it.
	record, readErr := worker.ReadValidationFailureFile(refusalPath)
	if readErr != nil {
		t.Fatalf("the next round was told nothing: %v", readErr)
	}
	if record.Round != 1 || !strings.Contains(record.Output, "on: の起動条件") {
		t.Fatalf("the objection does not carry the rule: %+v", record)
	}
}

// The same round, with the same file inside the policy, seals exactly as it
// always did — so the refusal above is the gate rather than the path.
func TestTheSealAcceptsAWorkflowInsideTheDestinationsPolicy(t *testing.T) {
	fixture, planPath := gateFixture(t)
	writeWorkflow(t, fixture, allowedWorkflow)

	if err := fixture.sealCandidate(t, "--release-path", planPath); err != nil {
		t.Fatalf("a workflow inside the policy was refused: %v", err)
	}
	var request worker.TicketRequest
	readAgentArtifact(t, fixture.path("ticket.json"), worker.MaxTicketJSONBytes, &request)
	if len(request.ReleaseWorkflows) != 1 || request.ReleaseWorkflows[0] != gateWorkflowPath {
		t.Fatalf("the contract does not carry what the round was allowed to build: %+v", request.ReleaseWorkflows)
	}
}

// The plan is a record found at a path, and a run directory outlives its
// cards. A plan another destination left would tell this seal to admit that
// destination's workflow file into this delivery, so the restatement inside
// the record is checked rather than trusted.
//
// Measured on a plan the seal would otherwise use: the same run, the same
// file, the same policy, and only the destination inside the record
// changed.
func TestTheSealRefusesAPlanDecidedForAnotherDestination(t *testing.T) {
	fixture, planPath := gateFixture(t)
	writeWorkflow(t, fixture, allowedWorkflow)
	if err := fixture.sealCandidate(t, "--release-path", planPath); err != nil {
		t.Fatalf("the reference round did not seal: %v", err)
	}

	elsewhere, elsewherePlan := gateFixture(t)
	writeWorkflow(t, elsewhere, allowedWorkflow)
	var plan worker.ReleasePathPlan
	readAgentArtifact(t, elsewherePlan, worker.MaxReleasePathJSONBytes, &plan)
	plan.Repository = "someone/else"
	if err := plan.Seal(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(elsewherePlan, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := elsewhere.sealCandidate(t, "--release-path", elsewherePlan); err == nil {
		t.Fatal("a plan decided for another destination admitted a workflow file into this one")
	} else if !strings.Contains(err.Error(), "not bound to this run") {
		t.Fatalf("the refusal came from somewhere else: %v", err)
	}
}
