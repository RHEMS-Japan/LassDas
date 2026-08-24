package main

import (
	"errors"
	"time"
	"unicode/utf8"

	"automation.internal/ticket-ingress/internal/worker"
)

// runSealCandidate seals a change an externally-launched implementer left in
// the working copy. The M2 orchestration starts the implementing agent itself
// (a Hermes-native worker); what this command owns is the observation: which
// files changed is read from git, checked against the writable scope, and
// sealed into the same artifacts a kernel-launched implement run produces, so
// everything downstream — reviews, validation, the publish gate — is
// unchanged. Launch facts nobody here observed (command, prompt, exit) are
// pinned to sentinels in the run record instead of being taken on faith; the
// optional report file is the implementer's own account and lands in the
// transcript, where it is treated exactly like an agent's stdout.
func runSealCandidate(args []string) error {
	flags := commandFlags("seal-candidate")
	configPath := flags.String("config", "", "")
	toolSHA := flags.String("tool-sha", "", "")
	draftPath := flags.String("draft", "", "")
	repoRoot := flags.String("repo-root", "", "")
	baseRoot := flags.String("base-root", "", "")
	baseSHA := flags.String("base-sha", "", "")
	stage := flags.Int("stage", 0, "")
	reportPath := flags.String("report", "", "")
	runOutPath := flags.String("run-out", "", "")
	ticketOutPath := flags.String("ticket-out", "", "")
	sourceOutPath := flags.String("source-out", "", "")
	outputPath := flags.String("out", "", "")
	if !parseFlags(flags, args) ||
		!allPresent(*configPath, *toolSHA, *draftPath, *repoRoot, *baseRoot, *baseSHA, *runOutPath, *ticketOutPath, *sourceOutPath, *outputPath) ||
		!worker.ValidToolSHA(*toolSHA) || *stage < 1 {
		return errors.New("seal-candidate arguments are invalid")
	}
	config, err := readConfig(*configPath)
	if err != nil {
		return err
	}
	if *stage > config.MaxStages {
		return errors.New("seal-candidate stage is invalid")
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
	report, err := readImplementerReport(*reportPath)
	if err != nil {
		return err
	}

	changed, err := worker.ChangedFilesUnder(*repoRoot, consumer.Mode.AllowedFilePrefixes, consumer.Mode.IgnoredByproducts)
	if err != nil {
		return err
	}
	run, err := worker.SealAgentRun(worker.AgentRun{
		SchemaVersion: worker.ArtifactSchemaVersion, Stage: *stage,
		DeliveryID: draft.DeliveryID, InputSHA256: draft.InputSHA256,
		ConfigSHA256: draft.ConfigSHA256, ToolSHA: draft.ToolSHA, BaseSHA: *baseSHA,
		Kind: worker.AgentRunKindExternal, AgentID: worker.AgentRunKindExternal,
		ChangedFiles: changed, Transcript: report, RanAt: time.Now().UTC(),
	})
	if err != nil {
		return errors.New("agent run could not be sealed")
	}
	if err := worker.WriteJSONFileExclusive(*runOutPath, run, worker.MaxArtifactJSONBytes); err != nil {
		return errors.New("run artifact could not be written")
	}
	return sealObservedChain(changed, draft, run, *repoRoot, *baseRoot, config, *ticketOutPath, *sourceOutPath, *outputPath)
}

// readImplementerReport reads what the external implementer said it did, if it
// left a report at all. The report becomes the run transcript, so it obeys the
// transcript's own bounds — and it is read through the defended reader: the
// implementer creates this inode, so a symlink or a FIFO here is an attack on
// the seal, not an operator mistake.
func readImplementerReport(filename string) (string, error) {
	if filename == "" {
		return "", nil
	}
	content, err := worker.ReadBoundedRegularFile(filename, int64(worker.MaxAgentTranscriptBytes))
	if err != nil {
		return "", errors.New("the implementer report could not be read")
	}
	if !utf8.Valid(content) {
		return "", errors.New("the implementer report is not valid text")
	}
	return string(content), nil
}

// sealObservedChain is the shared back half of implement and seal-candidate:
// read both sides of every changed file, complete the ticket contract from
// what actually changed, and write the ticket, source and candidate
// artifacts. The two verbs differ only in who started the agent.
func sealObservedChain(
	changed []string,
	draft worker.TicketDraft,
	run worker.AgentRun,
	repoRoot, baseRoot string,
	config worker.Config,
	ticketOutPath, sourceOutPath, outputPath string,
) error {
	consumer, err := config.ConsumerFor(draft.Repository)
	if err != nil {
		return errors.New("ticket draft repository is not a configured consumer")
	}
	observed, err := worker.ReadObservedChanges(repoRoot, baseRoot, changed, consumer)
	if err != nil {
		return err
	}
	request, err := worker.TicketWithObservedTargets(draft, observed, config)
	if err != nil {
		return errors.New("the files the agent changed do not form a valid contract")
	}
	source, err := worker.SourceFromObservedChanges(run.BaseSHA, observed, request, config)
	if err != nil {
		return err
	}
	candidate, err := worker.CandidateFromObservedChanges(run.Stage, observed, source, request, config, run, time.Now().UTC())
	if err != nil {
		return err
	}
	if err := worker.WriteJSONFileExclusive(ticketOutPath, request, worker.MaxTicketJSONBytes); err != nil {
		return errors.New("ticket artifact could not be written")
	}
	if err := worker.WriteJSONFileExclusive(sourceOutPath, source, worker.MaxArtifactJSONBytes); err != nil {
		return errors.New("source artifact could not be written")
	}
	if err := worker.WriteJSONFileExclusive(outputPath, candidate, worker.MaxArtifactJSONBytes); err != nil {
		return errors.New("candidate artifact could not be written")
	}
	return nil
}
