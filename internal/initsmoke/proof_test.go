package initsmoke

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/githubapi"
	"automation.internal/ticket-ingress/internal/initwizard"
	"automation.internal/ticket-ingress/internal/probe"
	"automation.internal/ticket-ingress/internal/worker"
	"automation.internal/ticket-ingress/internal/worker/investigate"
)

// The fixture uses the real artifact constructors and runs deterministic
// validation locally. No model, tracker or GitHub request is made.
func proofFixture(t *testing.T) (map[string]json.RawMessage, worker.Config, Record) {
	t.Helper()
	config, err := worker.LoadConfig("../../config/m1-consumer.json")
	if err != nil {
		t.Fatal(err)
	}
	mode := config.Consumers[0].Mode
	mode.AllowedFilePrefixes = []string{"README.md"}
	mode.VerifyWorkingDirectory = "."
	mode.Toolchain = nil
	mode.InstallCommand = []string{"true"}
	mode.VerifyCommands = [][]string{{"true"}}
	config.Consumers = []worker.ConsumerConfig{{Kind: "cli", Repository: "example/cli", RepositoryID: 1, Delivery: worker.DeliverPullRequest, IntegrationBranch: "develop", GitHub: worker.ConsumerGitHubContract{DefaultBranch: "main"}, Mode: mode, Design: &worker.DesignConfig{Default: "on"}}}
	config.Agents.Applier = &worker.AgentConfig{ID: "applier", Command: "applier-fixture", TimeoutSeconds: 60}
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	configSHA, _ := config.SHA256()
	_, record, _ := smokeState()
	record.IssueID = 42
	record.IssueKey = "EXAMPLE-42"
	record.DeliveryID = "delivery_" + strings.Repeat("a", 32)
	ticket := worker.TicketRequest{SchemaVersion: 1, DeliveryID: record.DeliveryID, InputSHA256: strings.Repeat("b", 64), ConfigSHA256: configSHA, ToolSHA: strings.Repeat("c", 40), IssueKey: record.IssueKey, RunID: "run_20260908_" + strings.Repeat("a", 24), Repository: record.Repository, Mode: mode.ID, Summary: record.Summary, TargetFiles: []string{record.Path}, Request: record.Description}
	if err := ticket.Validate(config); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, record.Path), []byte("before\n"), 0600); err != nil {
		t.Fatal(err)
	}
	changes := []worker.ObservedChange{{Path: record.Path, Before: []byte("before\n"), After: []byte(record.After)}}
	source, err := worker.SourceFromObservedChanges(record.BaseSHA, changes, ticket, config)
	if err != nil {
		t.Fatal(err)
	}
	id := investigate.Identity{DeliveryID: record.DeliveryID, InputSHA256: ticket.InputSHA256, ConfigSHA256: configSHA, ToolSHA: ticket.ToolSHA, BaseSHA: record.BaseSHA}
	catalog, err := probe.NewCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	measurements := filepath.Join(t.TempDir(), "measurements.jsonl")
	recorder, err := probe.OpenRecorder(measurements)
	if err != nil {
		t.Fatal(err)
	}
	session := probe.Session{Catalog: catalog, Recorder: recorder, RepoRoot: root}
	if _, err := session.Run(context.Background(), probe.Request{Probe: "repo.read", Args: map[string]string{"path": record.Path}}); err != nil {
		t.Fatal(err)
	}
	investigation, err := investigate.NewInvestigation(id, 1, investigate.ModelInvestigationOutput{Questions: []string{"Where is the text?"}, Findings: []investigate.Finding{{Claim: "The file contains the initial text", Evidence: []string{"m-0001"}, Confidence: investigate.ConfidenceMeasured}}, Next: "Append the check line"}, measurements, 1, investigate.Budget{ProbesUsed: 1})
	if err != nil {
		t.Fatal(err)
	}
	design, err := investigate.NewDesign(id, 1, investigate.ModelDesignOutput{Cause: "The initial check line is missing", CauseEvidence: []string{"m-0001"}, Approach: "Append the requested line", Alternatives: []string{"Create another file"}, Files: []investigate.FileChange{{Path: record.Path, Changes: []string{"Append the check line"}}}, Verification: investigate.Verification{Form: investigate.VerificationWording, Path: "/readme", ExpectedText: "check", AbsentText: "before"}, BlastRadius: []string{"Only the readme"}, NotDoing: []string{"Changing behavior"}}, investigation, investigate.Bounds{AllowedFilePrefixes: []string{record.Path}, MaxFiles: 1, Catalog: catalog, RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	applier, err := worker.SealAgentRun(worker.AgentRun{SchemaVersion: 1, Stage: 1, DeliveryID: record.DeliveryID, InputSHA256: ticket.InputSHA256, ConfigSHA256: configSHA, ToolSHA: ticket.ToolSHA, BaseSHA: record.BaseSHA, AgentID: config.Agents.Applier.ID, Command: config.Agents.Applier.Command, PromptBytes: 100, ChangedFiles: []string{record.Path}, Transcript: "Applied the requested addition", RanAt: now})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := worker.CandidateFromObservedChangesForDesign(1, changes, source, ticket, config, applier, now, design.DesignSHA256)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.ApplyCandidate(root, candidate, source, ticket, config); err != nil {
		t.Fatal(err)
	}
	validation, err := worker.RunValidationEvidence(context.Background(), root, candidate, source, ticket, config, record.BaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]json.RawMessage{}
	put := func(name string, value any) {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		files[name] = raw
	}
	var reviews []worker.Review
	var designReviews []investigate.DesignReview
	for _, endpoint := range config.Models.Reviewers {
		usage := worker.InvocationUsage{RequestedModel: endpoint.Model, RequestID: "fixture-" + endpoint.ID, StopReason: "stop", InputTokens: 1, OutputTokens: 1, TotalTokens: 2}
		review, err := worker.NewReview(1, endpoint, worker.ModelReviewOutput{Verdict: "pass", Findings: []worker.ModelFinding{}}, candidate, source, ticket, config, usage, now)
		if err != nil {
			t.Fatal(err)
		}
		reviews = append(reviews, review)
		put("history/stage-1/"+endpoint.ID+".json", review)
		judge, _ := config.Models.DesignReviewerFor(endpoint.ID)
		designReview, err := investigate.NewDesignReview(id, investigate.DesignSubject(design), investigate.Reviewer{ID: judge.ID, Vendor: judge.Vendor, Model: judge.Model, BaseURL: judge.BaseURL, Lens: "evidence"}, investigate.ModelDesignReviewOutput{Verdict: "pass", Findings: []investigate.DesignFinding{}}, investigate.Usage{RequestedModel: judge.Model, RequestID: "fixture-design-" + judge.ID, StopReason: "stop", InputTokens: 1, OutputTokens: 1, TotalTokens: 2}, now)
		if err != nil {
			t.Fatal(err)
		}
		designReviews = append(designReviews, designReview)
		put("history/design-1/"+endpoint.ID+"-design-review.json", designReview)
	}
	decision, err := worker.DecideStage(candidate, reviews, source, ticket, config)
	if err != nil {
		t.Fatal(err)
	}
	designDecision, err := investigate.DecideDesign(id, investigate.DesignSubject(design), designReviews, 1, config.DesignRounds())
	if err != nil {
		t.Fatal(err)
	}
	proof := featureProof{SchemaVersion: 1, Kind: "m1-feature-pull-request", Binding: deliveryBinding{DeliveryID: ticket.DeliveryID, InputSHA256: ticket.InputSHA256, ConfigSHA256: configSHA, ToolSHA: ticket.ToolSHA, IssueKey: ticket.IssueKey, Repository: ticket.Repository, SourceSHA256: source.SourceSHA256, CandidateSHA256: candidate.CandidateSHA256, DecisionSHA256: decision.DecisionSHA256, ValidationSHA256: validation.ValidationSHA256, ProductPaths: []string{record.Path}}}
	proof.Payload.Feature = githubapi.PublishedFeature{Base: githubapi.Snapshot{SHA: record.BaseSHA}, Branch: "ticket-automation/example", HeadSHA: strings.Repeat("d", 40), TreeSHA: strings.Repeat("e", 40), Paths: []string{record.Path}}
	proof.Payload.PullRequest = githubapi.PullRequest{Number: 1, HTMLURL: "https://github.com/example/cli/pull/1", HeadRef: proof.Payload.Feature.Branch, HeadSHA: proof.Payload.Feature.HeadSHA, BaseRef: record.Branch, BaseSHA: record.BaseSHA, HeadFullName: record.Repository}
	raw, _ := json.Marshal(proof)
	proof.ArtifactSHA256 = digest(string(raw))
	put("feature-pr.json", proof)
	put("validation.json", validation)
	for name, value := range map[string]any{"ticket.json": ticket, "source.json": source, "candidate.json": candidate, "decision.json": decision, "applier-run.json": applier} {
		put("history/stage-1/"+name, value)
	}
	put("history/design-1/design.json", design)
	put("history/design-1/investigation.json", investigation)
	put("history/design-1/decision.json", designDecision)
	return files, config, record
}

func TestProofRequiresActualDesignReviewsApplierValidationAndExpectedBytes(t *testing.T) {
	files, config, record := proofFixture(t)
	if _, err := validateProof(files, config, record); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"feature-pr.json", "validation.json", "history/stage-1/applier-run.json", "history/design-1/investigation.json", "history/design-1/decision.json", "history/design-1/" + config.Models.Reviewers[0].ID + "-design-review.json"} {
		original := files[name]
		delete(files, name)
		if _, err := validateProof(files, config, record); err == nil {
			t.Fatalf("missing %s passed", name)
		}
		files[name] = original
	}
	changed := record
	changed.After += "unrequested\n"
	if _, err := validateProof(files, config, changed); err == nil {
		t.Fatal("unexpected candidate bytes passed")
	}
	changed = record
	changed.DeliveryID = "delivery_" + strings.Repeat("f", 32)
	if _, err := validateProof(files, config, changed); err == nil {
		t.Fatal("another delivery passed")
	}
	var applier worker.AgentRun
	_ = json.Unmarshal(files["history/stage-1/applier-run.json"], &applier)
	applier.ExitCode = 1
	applier, _ = worker.SealAgentRun(applier)
	files["history/stage-1/applier-run.json"], _ = json.Marshal(applier)
	if _, err := validateProof(files, config, record); err == nil {
		t.Fatal("failed applier accepted")
	}
}

func TestRemotePRRequiresExactRepositoryParentAndFileBytes(t *testing.T) {
	files, config, record := proofFixture(t)
	proof, err := validateProof(files, config, record)
	if err != nil {
		t.Fatal(err)
	}
	feature := proof.Payload.Feature
	wrong := ""
	api := initwizard.API{HTTP: &http.Client{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") != "Bearer artificial-target" {
			t.Fatal("wrong GitHub credential")
		}
		switch req.URL.Path {
		case "/repos/example/cli/pulls/1":
			head := feature.HeadSHA
			if wrong == "head" {
				head = strings.Repeat("f", 40)
			}
			repo := map[string]any{"id": 1, "full_name": "example/cli"}
			if wrong == "repo" {
				repo["id"] = 2
			}
			return reply(map[string]any{"number": 1, "html_url": proof.Payload.PullRequest.HTMLURL, "changed_files": 1, "head": map[string]any{"ref": feature.Branch, "sha": head, "repo": repo}, "base": map[string]any{"ref": record.Branch, "sha": feature.Base.SHA, "repo": repo}}), nil
		case "/repos/example/cli/pulls/1/files":
			path := record.Path
			if wrong == "file" {
				path = "other.md"
			}
			return reply([]any{map[string]any{"filename": path, "status": "modified"}}), nil
		case "/repos/example/cli/contents/README.md":
			content := "before\n"
			if req.URL.Query().Get("ref") == feature.HeadSHA {
				content = record.After
				if wrong == "bytes" {
					content += "unexpected\n"
				}
			}
			return reply(map[string]any{"encoding": "base64", "size": len(content), "content": base64.StdEncoding.EncodeToString([]byte(content))}), nil
		case "/repos/example/cli/git/commits/" + feature.HeadSHA:
			parent := feature.Base.SHA
			if wrong == "parent" {
				parent = strings.Repeat("f", 40)
			}
			return reply(map[string]any{"sha": feature.HeadSHA, "tree": map[string]any{"sha": feature.TreeSHA}, "parents": []any{map[string]any{"sha": parent}}}), nil
		}
		t.Fatalf("unexpected GitHub request %s", req.URL.Path)
		return nil, nil
	})}}
	observer := RuntimeObserver{API: api}
	s, _, secrets := smokeState()
	if err := observer.verifyRemote(context.Background(), s, record, proof, secrets); err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"head", "repo", "file", "bytes", "parent"} {
		wrong = change
		if err := observer.verifyRemote(context.Background(), s, record, proof, secrets); err == nil {
			t.Fatalf("wrong %s accepted", change)
		}
	}
}

func TestDockerReadsExistingSealedProofWithoutStartingResidents(t *testing.T) {
	image := os.Getenv("LASSDAS_LOCALRUN_TEST_IMAGE")
	if image == "" {
		t.Skip("set LASSDAS_LOCALRUN_TEST_IMAGE to a cached distribution digest")
	}
	if !strings.Contains(image, "@sha256:") {
		t.Fatal("a distribution digest is required")
	}
	t.Setenv("TMPDIR", "/tmp")
	files, config, record := proofFixture(t)
	dir := t.TempDir()
	for name, raw := range files {
		filename := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(filename), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, raw, 0644); err != nil {
			t.Fatal(err)
		}
	}
	volume := "ticket-init-proof-" + filepath.Base(dir)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	call := func(args ...string) []byte {
		t.Helper()
		cmd := exec.CommandContext(ctx, "docker", append([]string{"--context", "desktop-linux"}, args...)...)
		raw, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("disposable proof fixture: %v\n%s", err, raw)
		}
		return raw
	}
	call("volume", "create", volume)
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		if err := exec.CommandContext(cleanup, "docker", "--context", "desktop-linux", "volume", "rm", volume).Run(); err != nil {
			t.Error("proof fixture cleanup failed")
		}
	})
	call("run", "--rm", "--pull=never", "--network=none", "--user", "0:0", "--mount", "type=volume,src="+volume+",dst=/data", "--mount", "type=bind,src="+dir+",dst=/input,readonly", "--entrypoint", "/bin/sh", image, "-ec", `mkdir -p "/data/runs/$1"; cp -R /input/. "/data/runs/$1/"; chown -R 1000:1000 /data/runs`, "fixture", record.DeliveryID)
	raw := call("run", "--rm", "--pull=never", "--network=none", "--user", "1000:1000", "--mount", "type=volume,src="+volume+",dst=/data,readonly", "--entrypoint", "python3", image, "-c", readArtifactsScript, record.DeliveryID)
	var observed map[string]json.RawMessage
	if json.Unmarshal(raw, &observed) != nil {
		t.Fatal("proof reader returned invalid JSON")
	}
	if _, err := validateProof(observed, config, record); err != nil {
		t.Fatal(err)
	}
}
