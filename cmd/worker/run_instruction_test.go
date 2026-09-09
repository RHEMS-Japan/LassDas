package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/worker"
)

// The cards orchestration's implement and apply cards run their agent
// through the worker on the rendered instruction — the same launch path
// as every reviewing agent — and record the run; a role without a launch
// fails closed instead of running anything as the engine.
func TestRunInstructionRunsTheAgentOnTheRenderedInstruction(t *testing.T) {
	fixture := newAgentFixture(t, `printf 'instruction: %s\n' "$1"; `+editTheLabel, "true")
	instruction := filepath.Join(t.TempDir(), "INSTRUCTION.md")
	if err := os.WriteFile(instruction, []byte("Change the label exactly as the ticket says.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), "implementer-run.json")
	common := []string{"--config", fixture.configPath, "--tool-sha", cliToolSHA, "--draft", fixture.draftPath,
		"--instruction", instruction, "--repo-root", fixture.repoRoot, "--base-sha", fixture.baseSHA, "--stage", "1"}
	err := run(context.Background(), append([]string{"run-instruction", "--role", "implementer", "--out", record}, common...))
	if err != nil {
		t.Fatalf("run-instruction implementer: %v", err)
	}
	var sealed worker.AgentRun
	if err := worker.ReadJSONFile(record, worker.MaxArtifactJSONBytes, &sealed); err != nil {
		t.Fatalf("run record: %v", err)
	}
	if sealed.AgentID != fixture.config.Agents.Implementer.ID || sealed.PromptBytes != len("Change the label exactly as the ticket says.\n") ||
		sealed.Stage != 1 || sealed.BaseSHA != fixture.baseSHA || len(sealed.ChangedFiles) == 0 ||
		!strings.Contains(sealed.Transcript, "instruction: Change the label") {
		t.Fatalf("run record = %+v", sealed)
	}

	applierRecord := filepath.Join(t.TempDir(), "applier-run.json")
	err = run(context.Background(), append([]string{"run-instruction", "--role", "applier", "--out", applierRecord}, common...))
	if err == nil || !strings.Contains(err.Error(), "applier agent is not configured") {
		t.Fatalf("applier without a launch: %v", err)
	}
	if _, statErr := os.Stat(applierRecord); statErr == nil {
		t.Fatal("a record was written for a role that did not run")
	}
	err = run(context.Background(), append([]string{"run-instruction", "--role", "reviewer", "--out", applierRecord}, common...))
	if err == nil || !strings.Contains(err.Error(), "role is invalid") {
		t.Fatalf("unknown role: %v", err)
	}
}

// The applier of an approved design may stop instead of editing: it writes
// revise-design.json at the root of its working copy — the only place it can
// write — and the apply card seals that into the design round's objection
// record, removes the file from the tree and fails so the attendant reopens
// the design (issue #103). Before this, the honest objection ended as "a
// file outside the writable scope" and the run as model_failed.
func TestRunInstructionSealsTheAppliersObjectionWrittenInTheWorkingDirectory(t *testing.T) {
	newApplierFixture := func(t *testing.T, body string) agentFixture {
		return newTunedAgentFixture(t, "true", "true", func(binaries string, config *worker.Config) {
			writeStandInAgent(t, binaries, "stand-in-applier", body)
			applier := config.Agents.Implementer
			applier.ID = "applier-stand-in"
			applier.Command = "stand-in-applier"
			config.Agents.Applier = &applier
		})
	}
	instruction := filepath.Join(t.TempDir(), "INSTRUCTION.md")
	if err := os.WriteFile(instruction, []byte("Apply the design.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	argsFor := func(fixture agentFixture, design string, extra ...string) []string {
		args := []string{"run-instruction", "--role", "applier", "--config", fixture.configPath, "--tool-sha", cliToolSHA, "--draft", fixture.draftPath,
			"--instruction", instruction, "--repo-root", fixture.repoRoot, "--base-sha", fixture.baseSHA, "--stage", "1",
			"--out", filepath.Join(t.TempDir(), "applier-run.json")}
		if design != "" {
			args = append(args, "--design", design, "--objection-out", fixture.path("history/design-1/objection.json"))
		}
		return append(args, extra...)
	}

	// The honest objection: written where the instruction says, sealed into the round.
	fixture := newApplierFixture(t, `printf '{"reason":"the label is rendered from a translation key, not the file the design names","section":"files"}' > revise-design.json`)
	design := sealedDesignFor(t, fixture, "client/src/label.ts")
	err := run(context.Background(), argsFor(fixture, design))
	if err == nil || !strings.Contains(err.Error(), "objected to the design") {
		t.Fatalf("objection did not end the card as an objection: %v", err)
	}
	var record DesignObjection
	readAgentArtifact(t, fixture.path("history/design-1/objection.json"), worker.MaxArtifactJSONBytes, &record)
	if record.Section != "files" || !strings.Contains(record.Reason, "translation key") || record.Stage != 1 || record.DesignSHA256 == "" || record.ObjectionSHA256 == "" {
		t.Fatalf("objection record = %+v", record)
	}
	if _, statErr := os.Stat(filepath.Join(fixture.repoRoot, "revise-design.json")); statErr == nil {
		t.Fatal("the objection stayed in the working copy; the next round's scan would refuse it")
	}
	if _, statErr := os.Stat(fixture.path("history/design-1/revise-design.json")); statErr != nil {
		t.Fatal("the applier's file was not kept with the round")
	}
	if changed, err := worker.ChangedFilesUnder(fixture.repoRoot, nil, nil); err != nil || len(changed) != 0 {
		t.Fatalf("the tree after the objection: %v %v", changed, err)
	}

	// An objection next to an edit is refused, the file removed, the edit named.
	fixture = newApplierFixture(t, editTheLabel+`; printf '{"reason":"and yet I edited","section":"files"}' > revise-design.json`)
	design = sealedDesignFor(t, fixture, "client/src/label.ts")
	err = run(context.Background(), argsFor(fixture, design))
	if err == nil || !strings.Contains(err.Error(), "also changed 1 file") {
		t.Fatalf("objection beside an edit: %v", err)
	}
	if _, statErr := os.Stat(fixture.path("history/design-1/objection.json")); statErr == nil {
		t.Fatal("an objection beside an edit was sealed")
	}
	if _, statErr := os.Stat(filepath.Join(fixture.repoRoot, "revise-design.json")); statErr == nil {
		t.Fatal("the refused objection stayed in the working copy")
	}

	// An empty reason is refused by name, and the file does not linger.
	fixture = newApplierFixture(t, `printf '{"reason":"   "}' > revise-design.json`)
	design = sealedDesignFor(t, fixture, "client/src/label.ts")
	err = run(context.Background(), argsFor(fixture, design))
	if err == nil || !strings.Contains(err.Error(), "1 to 600 bytes of text (got 0)") {
		t.Fatalf("empty reason: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(fixture.repoRoot, "revise-design.json")); statErr == nil {
		t.Fatal("the refused objection stayed in the working copy")
	}

	// Without a design the file is what it always was: a change outside the scope.
	fixture = newApplierFixture(t, `printf '{"reason":"no design here"}' > revise-design.json`)
	err = run(context.Background(), argsFor(fixture, ""))
	if err == nil || !strings.Contains(err.Error(), "outside the writable scope") {
		t.Fatalf("halt file without a design: %v", err)
	}

	// The design and the record's destination travel together or not at all,
	// and only the applier's card carries a design.
	fixture = newApplierFixture(t, "true")
	design = sealedDesignFor(t, fixture, "client/src/label.ts")
	args := argsFor(fixture, "")
	if err := run(context.Background(), append(args, "--design", design)); err == nil || !strings.Contains(err.Error(), "arguments are invalid") {
		t.Fatalf("design without objection-out: %v", err)
	}
	implementer := argsFor(fixture, design)
	implementer[2] = "implementer"
	if err := run(context.Background(), implementer); err == nil || !strings.Contains(err.Error(), "only the applier") {
		t.Fatalf("implementer with a design: %v", err)
	}
}

// A run that did not finish leaves nothing for the card's second attempt to
// read: the kanban re-dispatches a blocked card once, in the same working
// copy, and an objection left behind by the dead attempt would be read as
// that attempt's — either as an objection beside the edits the second
// attempt made (losing a round that applied the design), or, if it changed
// nothing, as this round's objection carrying the previous reason.
func TestAFailedApplierLeavesNoObjectionForTheNextAttempt(t *testing.T) {
	fixture := newTunedAgentFixture(t, "true", "true", func(binaries string, config *worker.Config) {
		writeStandInAgent(t, binaries, "stand-in-applier",
			`printf '{"reason":"a stale objection from the previous round","section":"files"}' > revise-design.json; exit 3`)
		applier := config.Agents.Implementer
		applier.ID = "applier-stand-in"
		applier.Command = "stand-in-applier"
		config.Agents.Applier = &applier
	})
	instruction := filepath.Join(t.TempDir(), "INSTRUCTION.md")
	if err := os.WriteFile(instruction, []byte("Apply the design.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	design := sealedDesignFor(t, fixture, "client/src/label.ts")
	args := []string{"run-instruction", "--role", "applier", "--config", fixture.configPath, "--tool-sha", cliToolSHA,
		"--draft", fixture.draftPath, "--instruction", instruction, "--repo-root", fixture.repoRoot,
		"--base-sha", fixture.baseSHA, "--stage", "1", "--out", filepath.Join(t.TempDir(), "applier-run.json"),
		"--design", design, "--objection-out", fixture.path("history/design-1/objection.json")}
	err := run(context.Background(), args)
	if err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("a dead applier: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(fixture.repoRoot, "revise-design.json")); statErr == nil {
		t.Fatal("the dead attempt left its objection in the tree for the next attempt to read")
	}
	if _, statErr := os.Stat(fixture.path("history/design-1/objection.json")); statErr == nil {
		t.Fatal("a run that did not finish sealed an objection")
	}
}

// The halt file is honoured only as a regular file. A named pipe in its
// place would otherwise pass every check: git does not list it, so the
// scope scan sees an unchanged tree and the card succeeds having done
// nothing.
func TestAnApplierHaltThatIsNotARegularFileIsRefused(t *testing.T) {
	fixture := newTunedAgentFixture(t, "true", "true", func(binaries string, config *worker.Config) {
		writeStandInAgent(t, binaries, "stand-in-applier", `mkfifo revise-design.json`)
		applier := config.Agents.Implementer
		applier.ID = "applier-stand-in"
		applier.Command = "stand-in-applier"
		config.Agents.Applier = &applier
	})
	instruction := filepath.Join(t.TempDir(), "INSTRUCTION.md")
	if err := os.WriteFile(instruction, []byte("Apply the design.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	design := sealedDesignFor(t, fixture, "client/src/label.ts")
	err := run(context.Background(), []string{"run-instruction", "--role", "applier", "--config", fixture.configPath,
		"--tool-sha", cliToolSHA, "--draft", fixture.draftPath, "--instruction", instruction, "--repo-root", fixture.repoRoot,
		"--base-sha", fixture.baseSHA, "--stage", "1", "--out", filepath.Join(t.TempDir(), "applier-run.json"),
		"--design", design, "--objection-out", fixture.path("history/design-1/objection.json")})
	if err == nil || !strings.Contains(err.Error(), "other than a regular file") {
		t.Fatalf("a named pipe in the halt file's place: %v", err)
	}
}

// The applier's card clears a leftover objection before it starts. A run
// killed outright leaves one behind (nothing of its own cleanup runs), and
// the card is re-dispatched into the same working copy: the second attempt
// would read the dead attempt's file as its own.
func TestTheApplierCardClearsALeftoverObjectionBeforeItRuns(t *testing.T) {
	fixture := newTunedAgentFixture(t, "true", "true", func(binaries string, config *worker.Config) {
		writeStandInAgent(t, binaries, "stand-in-applier", editTheLabel)
		applier := config.Agents.Implementer
		applier.ID = "applier-stand-in"
		applier.Command = "stand-in-applier"
		config.Agents.Applier = &applier
	})
	instruction := filepath.Join(t.TempDir(), "INSTRUCTION.md")
	if err := os.WriteFile(instruction, []byte("Apply the design.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// What the killed attempt left behind.
	if err := os.WriteFile(filepath.Join(fixture.repoRoot, "revise-design.json"),
		[]byte(`{"reason":"a stale objection from the attempt that was killed","section":"files"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	design := sealedDesignFor(t, fixture, "client/src/label.ts")
	err := run(context.Background(), []string{"run-instruction", "--role", "applier", "--config", fixture.configPath,
		"--tool-sha", cliToolSHA, "--draft", fixture.draftPath, "--instruction", instruction, "--repo-root", fixture.repoRoot,
		"--base-sha", fixture.baseSHA, "--stage", "1", "--out", filepath.Join(t.TempDir(), "applier-run.json"),
		"--design", design, "--objection-out", fixture.path("history/design-1/objection.json")})
	if err != nil {
		t.Fatalf("the second attempt applied the design but did not finish: %v", err)
	}
	if _, statErr := os.Stat(fixture.path("history/design-1/objection.json")); statErr == nil {
		t.Fatal("the dead attempt's objection was sealed as this attempt's")
	}
	applied, readErr := os.ReadFile(filepath.Join(fixture.repoRoot, "client", "src", "label.ts"))
	if readErr != nil || !strings.Contains(string(applied), "Updated label") {
		t.Fatalf("the round that applied the design was thrown away: %q (%v)", string(applied), readErr)
	}
}
