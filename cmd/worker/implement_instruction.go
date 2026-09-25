package main

import (
	"errors"
	"os"
	"path/filepath"

	"automation.internal/ticket-ingress/internal/worker"
)

// runImplementInstruction renders the implementer's instruction into a file,
// because the implement card is a native kanban worker and launches the
// implementing agent itself: the prompt stays kernel-authored even when the
// launch is not, and the earlier rounds' objections ride in with it. The
// file is plainly overwritten — each round's instruction replaces the one
// before it on the shared run directory.
func runImplementInstruction(args []string) error {
	flags := commandFlags("implement-instruction")
	configPath := flags.String("config", "", "")
	toolSHA := flags.String("tool-sha", "", "")
	draftPath := flags.String("draft", "", "")
	clarificationPath := flags.String("clarification", "", "")
	var findingsPaths stringList
	flags.Var(&findingsPaths, "previous-findings", "")
	validationFailurePath := flags.String("validation-failure", "", "")
	rulingPath := flags.String("ruling", "", "")
	returnedPath := flags.String("returned", "", "")
	rebuild := flags.String("rebuild-prompt", "", "")
	outputPath := flags.String("out", "", "")
	repoRoot := flags.String("repo-root", "", "")
	if !parseFlags(flags, args) ||
		!allPresent(*configPath, *toolSHA, *draftPath, *outputPath, *repoRoot) || !filepath.IsAbs(*repoRoot) ||
		!worker.ValidToolSHA(*toolSHA) || !validRebuild(*rebuild) {
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
	validationFailure, err := readValidationFailure(*validationFailurePath)
	if err != nil {
		return err
	}
	ruling, err := readRuling(*rulingPath)
	if err != nil {
		return err
	}
	returned, err := readReturnedRound(*returnedPath)
	if err != nil {
		return err
	}
	if *rebuild != "" {
		// The ladder has been here before and the implementer answered
		// nothing. The request and the boundaries stay; what the earlier
		// rounds objected to goes, because an instruction a model would not
		// answer is asked again shorter rather than asked again. The ruling
		// stays: it is the reason this round exists, not commentary on it.
		findings = nil
	}
	prompt, err := implementPrompt(draft, consumer, config.Agents.Implementer, clarification, findings, validationFailure, ruling, returned, *repoRoot)
	if err != nil {
		return errors.New("implement instruction could not be built")
	}
	if err := os.WriteFile(*outputPath, []byte(prompt), 0o600); err != nil {
		return errors.New("implement instruction could not be written")
	}
	return nil
}

// readValidationFailure loads what the previous round's deterministic
// validation refused, or nil when the flag was not given — which is the
// ordinary case, because most rounds are repeated over an objection and never
// reached the validation.
//
// A path that was given and cannot be read is a failure rather than an
// absence. The round is being run for what is in that file, and rendering
// without it would produce a plausible instruction that has lost the point of
// the round.
func readValidationFailure(path string) (*worker.ValidationFailure, error) {
	if path == "" {
		return nil, nil
	}
	record, err := worker.ReadValidationFailureFile(path)
	if err != nil {
		return nil, errors.New("the previous round's validation failure could not be read")
	}
	return &record, nil
}

// readReturnedRound loads what the engine decided when this round's agent
// handed the work back, or nil when the flag was not given — the ordinary
// case, because almost no round is handed back at all.
//
// A path that was given and cannot be read is a failure rather than an
// absence. The round is being rendered again for what is in that file, and
// rendering without it would hand the agent back the instruction it has
// already answered, which is the loop this exists to end.
func readReturnedRound(path string) (*worker.ReturnedWork, error) {
	if path == "" {
		return nil, nil
	}
	record, err := worker.ReadReturnedRoundFile(path)
	if err != nil {
		return nil, errors.New("this round's returned-work record could not be read")
	}
	latest := record.Latest()
	if latest == nil {
		return nil, errors.New("this round's returned-work record holds no answer")
	}
	return latest, nil
}
