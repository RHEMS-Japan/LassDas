package main

import (
	"automation.internal/ticket-ingress/internal/worker/investigate"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"automation.internal/ticket-ingress/internal/worker"
)

// run-instruction runs the implementer or the applier on the instruction the
// attendant rendered (INSTRUCTION.md) in the cards orchestration. The card
// is a direct command (`runner chain-stage --stage implement|apply`), so the
// agent starts here — through the launcher, under the agent user — like
// every reviewing agent, instead of as the kanban's native worker under the
// engine's user (issue #23). The seal of what it left stays with the first
// review card, as before.
func runRunInstruction(ctx context.Context, args []string) error {
	flags := commandFlags("run-instruction")
	configPath := flags.String("config", "", "")
	toolSHA := flags.String("tool-sha", "", "")
	draftPath := flags.String("draft", "", "")
	role := flags.String("role", "", "")
	instructionPath := flags.String("instruction", "", "")
	repoRoot := flags.String("repo-root", "", "")
	baseSHA := flags.String("base-sha", "", "")
	stage := flags.Int("stage", 0, "")
	knowledgeRoot := flags.String("knowledge-root", "", "")
	runOutPath := flags.String("out", "", "")
	designPath := flags.String("design", "", "")
	objectionOutPath := flags.String("objection-out", "", "")
	if !parseFlags(flags, args) || (*designPath == "") != (*objectionOutPath == "") ||
		!allPresent(*configPath, *toolSHA, *draftPath, *role, *instructionPath, *repoRoot, *baseSHA, *runOutPath) ||
		!worker.ValidToolSHA(*toolSHA) || *stage < 1 {
		return errors.New("run-instruction arguments are invalid")
	}
	config, err := readConfig(*configPath)
	if err != nil {
		return err
	}
	var draft worker.TicketDraft
	if err := worker.ReadJSONFile(*draftPath, worker.MaxTicketJSONBytes, &draft); err != nil {
		return errors.New("ticket draft could not be read")
	}
	configSHA, err := config.SHA256()
	if err != nil || draft.ConfigSHA256 != configSHA || draft.ToolSHA != *toolSHA {
		return errors.New("ticket draft is not bound to this run")
	}
	consumer, err := config.ConsumerFor(draft.Repository)
	if err != nil {
		return errors.New("ticket draft repository is not a configured consumer")
	}
	var agent worker.AgentConfig
	switch *role {
	case "implementer":
		agent = config.Agents.Implementer
	case "applier":
		if config.Agents.Applier != nil {
			agent = *config.Agents.Applier
		}
	default:
		return errors.New("run-instruction role is invalid")
	}
	if agent.Command == "" {
		return fmt.Errorf("the %s agent is not configured (agents.%s)", *role, *role)
	}
	instruction, err := os.ReadFile(*instructionPath)
	if err != nil || len(instruction) == 0 || len(instruction) > worker.MaxAgentPromptBytes {
		return errors.New("the instruction could not be read")
	}
	if err := placeAgentKnowledge(agent, *knowledgeRoot, *repoRoot); err != nil {
		return err
	}
	// A design-backed apply card: the applier may stop instead of editing by
	// leaving revise-design.json at the root of its working copy — the only
	// place it can write — and this command turns that into the design
	// round's objection record (docs/INVESTIGATING_DESIGNER.md §7, issue
	// #103). The design is read the way the seal reads it, so the record is
	// bound to the design the objection is against.
	var design *investigate.Design
	haltFile := ""
	if *designPath != "" {
		if *role != "applier" {
			return errors.New("only the applier's card carries a design")
		}
		loaded, err := investigate.ReadDesign(*designPath)
		if err != nil || !loaded.DigestMatches() {
			return errors.New("the approved design could not be read or is not intact")
		}
		if loaded.DeliveryID != draft.DeliveryID || loaded.InputSHA256 != draft.InputSHA256 || loaded.ConfigSHA256 != draft.ConfigSHA256 ||
			loaded.ToolSHA != draft.ToolSHA || loaded.BaseSHA != *baseSHA {
			return errors.New("the design belongs to another run")
		}
		design = &loaded
		haltFile = objectionFileName
	}
	outcome, halted, runErr := worker.RunAgentUnlessHalted(ctx, agent, *repoRoot, string(instruction), consumer.Mode.AllowedFilePrefixes, consumer.Mode.IgnoredByproducts, haltFile)
	run, sealErr := worker.SealAgentRun(worker.AgentRun{
		SchemaVersion: worker.ArtifactSchemaVersion, Stage: *stage,
		DeliveryID: draft.DeliveryID, InputSHA256: draft.InputSHA256,
		ConfigSHA256: draft.ConfigSHA256, ToolSHA: draft.ToolSHA, BaseSHA: *baseSHA,
		AgentID: outcome.AgentID, Command: outcome.Command, PromptBytes: len(instruction), ExitCode: outcome.ExitCode,
		DurationMs: outcome.Duration.Milliseconds(), ChangedFiles: outcome.ChangedFiles,
		Transcript: outcome.Transcript, RanAt: time.Now().UTC(),
	})
	if sealErr == nil {
		// The run record is evidence of what happened, written even when
		// the run failed.
		_ = worker.WriteJSONFileExclusive(*runOutPath, run, worker.MaxArtifactJSONBytes)
	}
	if runErr != nil {
		return errors.New("the " + *role + " did not finish: " + runErr.Error())
	}
	if halted {
		return sealAppliersHalt(*repoRoot, *objectionOutPath, consumer, draft, *baseSHA, *stage, design)
	}
	return nil
}

// sealAppliersHalt reads the objection the applier left at the root of its
// working copy and seals it as the design round's record, then fails the
// card so the attendant reads the record and reopens the design. The file
// is gone from the tree either way: a refused objection travels in the
// error, and a stale halt would otherwise stop the next round before it
// began. An objection next to other edits is refused — the contract is to
// stop without editing anything else, and a tree that was edited anyway is
// not the design's author's to answer.
func sealAppliersHalt(repoRoot, objectionOut string, consumer worker.ConsumerConfig, draft worker.TicketDraft, baseSHA string, stage int, design *investigate.Design) error {
	halt := filepath.Join(repoRoot, objectionFileName)
	changed, scanErr := worker.ChangedFilesUnderExcept(repoRoot, consumer.Mode.AllowedFilePrefixes, consumer.Mode.IgnoredByproducts, objectionFileName)
	if scanErr != nil || len(changed) > 0 {
		_ = os.Remove(halt)
		if scanErr != nil {
			return errors.New("the applier objected to the design but also changed the tree; an objection leaves it untouched (" + scanErr.Error() + ")")
		}
		return fmt.Errorf("the applier objected to the design but also changed %d file(s); an objection leaves the tree untouched", len(changed))
	}
	objected, err := sealDesignObjection(halt, objectionOut, draft, baseSHA, stage, design)
	if err != nil {
		_ = os.Remove(halt)
		return errors.New("the applier's objection was refused: " + err.Error())
	}
	if !objected {
		return errors.New("the applier's objection vanished before it was sealed")
	}
	return errors.New("the applier objected to the design; no candidate was sealed and the design goes back to its author")
}
