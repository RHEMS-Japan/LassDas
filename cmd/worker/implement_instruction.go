package main

import (
	"errors"
	"os"

	"automation.internal/ticket-ingress/internal/worker"
)

// runImplementInstruction renders the implementer's instruction into a file,
// for orchestrations that launch the implementing agent themselves (the M2
// cards mode, where the implement card is a native kanban worker): the
// prompt stays kernel-authored even when the launch is not, and the earlier
// rounds' objections ride in exactly as the in-process implementer receives
// them. The file is plainly overwritten — each round's instruction replaces
// the one before it on the shared run directory.
func runImplementInstruction(args []string) error {
	flags := commandFlags("implement-instruction")
	configPath := flags.String("config", "", "")
	toolSHA := flags.String("tool-sha", "", "")
	draftPath := flags.String("draft", "", "")
	clarificationPath := flags.String("clarification", "", "")
	var findingsPaths stringList
	flags.Var(&findingsPaths, "previous-findings", "")
	outputPath := flags.String("out", "", "")
	if !parseFlags(flags, args) ||
		!allPresent(*configPath, *toolSHA, *draftPath, *outputPath) ||
		!worker.ValidToolSHA(*toolSHA) {
		return errors.New("implement-instruction arguments are invalid")
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
	clarification, err := readClarificationContext(*clarificationPath)
	if err != nil {
		return err
	}
	findings, err := readPreviousFindings(findingsPaths)
	if err != nil {
		return err
	}
	prompt, err := implementPrompt(draft, consumer, config.Agents.Implementer, clarification, findings)
	if err != nil {
		return errors.New("implement instruction could not be built")
	}
	if err := os.WriteFile(*outputPath, []byte(prompt), 0o600); err != nil {
		return errors.New("implement instruction could not be written")
	}
	return nil
}
