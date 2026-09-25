package worker

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// unnamedFilesDraft is the reception contract of an ordinary request: it
// names no file, and it promises no visible wording, which is the shape of
// every ticket that is not a text change. What the run changes is decided by
// making the change.
func unnamedFilesDraft(t *testing.T) (Config, TicketDraft) {
	t.Helper()
	config := validTestConfig()
	configSHA, err := config.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	consumer := config.Consumers[0]
	return config, TicketDraft{
		SchemaVersion: 1, DeliveryID: "delivery_" + strings.Repeat("a", 32),
		InputSHA256: strings.Repeat("b", 64), ConfigSHA256: configSHA, ToolSHA: strings.Repeat("c", 40),
		IssueKey: "TICKET-501", RunID: "run_20260925_" + strings.Repeat("d", 24),
		Repository: consumer.Repository, Mode: consumer.Mode.ID,
		Summary: "設定画面に新しい補助モジュールを足す",
		Request: "client/src/ の下に新しいモジュールを作って、設定画面から使えるようにしてください。",
	}
}

// sealObservedForTest walks the production seal: what the agent left is read
// from the working copy, the contract is completed from that observation,
// and the candidate goes through the same gate a model-authored one does.
func sealObservedForTest(t *testing.T, config Config, draft TicketDraft, root, base, baseSHA string, changed []string) (Candidate, error) {
	t.Helper()
	consumer, err := config.ConsumerFor(draft.Repository)
	if err != nil {
		t.Fatal(err)
	}
	observed, err := ReadObservedChanges(root, base, changed, consumer, WorkflowAllowance{})
	if err != nil {
		return Candidate{}, err
	}
	request, err := TicketWithObservedTargets(draft, observed, nil, config)
	if err != nil {
		return Candidate{}, err
	}
	source, err := SourceFromObservedChanges(baseSHA, observed, request, config)
	if err != nil {
		return Candidate{}, err
	}
	run, err := SealAgentRun(AgentRun{
		SchemaVersion: ArtifactSchemaVersion, Stage: 1,
		DeliveryID: request.DeliveryID, InputSHA256: request.InputSHA256,
		ConfigSHA256: request.ConfigSHA256, ToolSHA: request.ToolSHA, BaseSHA: baseSHA,
		AgentID: config.Agents.Implementer.ID, Command: config.Agents.Implementer.Command, PromptBytes: 42,
		ChangedFiles: changed, Transcript: "書きました。",
		RanAt: time.Date(2026, 9, 25, 1, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := CandidateFromObservedChanges(1, observed, source, request, config, run, time.Date(2026, 9, 25, 1, 1, 0, 0, time.UTC))
	if err != nil {
		return Candidate{}, err
	}
	if err := candidate.Validate(source, request, config); err != nil {
		return Candidate{}, err
	}
	return candidate, nil
}

// A request that needs a file nobody named can now be delivered: the file is
// created inside the writable scope, and the whole seal accepts it. Before
// this, the reception picked the files from the ticket text and the
// implementer was forbidden to touch anything else, so a request to add a
// module ended having changed nothing at all (live 2026-09-25).
func TestASealAcceptsAFileTheRequestNeverNamed(t *testing.T) {
	config, draft := unnamedFilesDraft(t)
	root, head := buildAgentRepository(t)
	base := copyAgentBase(t, root)
	writeAgentFile(t, root, "client/src/module/helper.ts", "export const helper = () => true;\n")
	writeAgentFile(t, root, "client/src/label.ts", "export const submitLabel = 'Send';\nimport './module/helper';\n")

	changed := []string{"client/src/label.ts", "client/src/module/helper.ts"}
	candidate, err := sealObservedForTest(t, config, draft, root, base, head, changed)
	if err != nil {
		t.Fatalf("a change that created a file inside the scope was refused: %v", err)
	}
	if len(candidate.Files) != 2 || candidate.Files[1].Path != "client/src/module/helper.ts" {
		t.Fatalf("candidate files = %+v", candidate.Files)
	}
	// The contract's file set is the observation, so nothing had to be
	// declared in advance for the created file to be deliverable.
	request, err := TicketWithObservedTargets(draft, []ObservedChange{{Path: "client/src/module/helper.ts", Created: true}}, nil, config)
	if err != nil {
		t.Fatalf("the contract refused a file the ticket never named: %v", err)
	}
	if len(request.TargetFiles) != 1 || request.TargetFiles[0] != "client/src/module/helper.ts" {
		t.Fatalf("target files = %v", request.TargetFiles)
	}
}

// The writable scope is the boundary that remains. It is the destination's
// own configuration, not a per-request guess, and a change outside it is
// still discarded whether the file was edited or created.
func TestASealStillRefusesAnythingOutsideTheWritableScope(t *testing.T) {
	config, draft := unnamedFilesDraft(t)
	root, head := buildAgentRepository(t)
	base := copyAgentBase(t, root)
	writeAgentFile(t, root, "server/handler.go", "package server\n")
	if _, err := sealObservedForTest(t, config, draft, root, base, head, []string{"server/handler.go"}); err == nil {
		t.Fatal("a file created outside the writable scope was sealed")
	}
	writeAgentFile(t, root, "README.md", "changed\n")
	if _, err := sealObservedForTest(t, config, draft, root, base, head, []string{"README.md"}); err == nil {
		t.Fatal("a file edited outside the writable scope was sealed")
	}
}

// The budget of one run is the other boundary that remains: how many files,
// how many lines, how many bytes, and the text the destination forbids.
func TestASealStillEnforcesTheDestinationsLimits(t *testing.T) {
	config, draft := unnamedFilesDraft(t)
	consumer := config.Consumers[0]

	t.Run("more files than the destination allows", func(t *testing.T) {
		root, head := buildAgentRepository(t)
		base := copyAgentBase(t, root)
		changed := make([]string, 0, consumer.Mode.MaxFiles+1)
		for index := 0; index <= consumer.Mode.MaxFiles; index++ {
			name := filepath.Join("client", "src", "generated", "part-"+string(rune('a'+index))+".ts")
			writeAgentFile(t, root, filepath.ToSlash(name), "export const part = true;\n")
			changed = append(changed, filepath.ToSlash(name))
		}
		if _, err := sealObservedForTest(t, config, draft, root, base, head, changed); err == nil {
			t.Fatalf("a change of %d files was sealed under a limit of %d", len(changed), consumer.Mode.MaxFiles)
		}
	})

	t.Run("more lines than the destination allows", func(t *testing.T) {
		root, head := buildAgentRepository(t)
		base := copyAgentBase(t, root)
		writeAgentFile(t, root, "client/src/generated/big.ts", strings.Repeat("export const line = true;\n", consumer.Mode.MaxChangedLines+1))
		if _, err := sealObservedForTest(t, config, draft, root, base, head, []string{"client/src/generated/big.ts"}); err == nil {
			t.Fatalf("a change of %d lines was sealed under a limit of %d", consumer.Mode.MaxChangedLines+1, consumer.Mode.MaxChangedLines)
		}
	})

	t.Run("a file larger than the destination allows", func(t *testing.T) {
		root, head := buildAgentRepository(t)
		base := copyAgentBase(t, root)
		writeAgentFile(t, root, "client/src/generated/huge.ts", strings.Repeat("x", consumer.Mode.MaxFileBytes+1))
		if _, err := sealObservedForTest(t, config, draft, root, base, head, []string{"client/src/generated/huge.ts"}); err == nil {
			t.Fatal("a file over the per-file byte limit was sealed")
		}
	})

	t.Run("the text the destination forbids", func(t *testing.T) {
		root, head := buildAgentRepository(t)
		base := copyAgentBase(t, root)
		writeAgentFile(t, root, "client/src/module/helper.ts", "// "+consumer.Mode.ForbiddenCandidateText[0]+"\n")
		if _, err := sealObservedForTest(t, config, draft, root, base, head, []string{"client/src/module/helper.ts"}); err == nil {
			t.Fatal("a created file carrying the forbidden text was sealed")
		}
	})

	// A hidden path is never a deliverable, even inside the writable scope.
	// The rule lives where the change is observed: a run that wrote one is
	// refused whole, so it never reaches the seal.
	t.Run("a hidden path", func(t *testing.T) {
		root, _ := buildAgentRepository(t)
		writeAgentFile(t, root, "client/src/.env", "SECRET=1\n")
		if _, err := ChangedFilesUnder(root, consumer.Mode.AllowedFilePrefixes, nil, WorkflowAllowance{}); err == nil {
			t.Fatal("a hidden path inside the writable scope was observed as a change")
		}
	})
}
