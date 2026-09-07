package main

import (
	"context"
	"errors"
	"fmt"
	"os"
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
	if !parseFlags(flags, args) ||
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
	outcome, runErr := worker.RunAgent(ctx, agent, *repoRoot, string(instruction), consumer.Mode.AllowedFilePrefixes, consumer.Mode.IgnoredByproducts)
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
	return nil
}
