package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/worker"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "worker:", err)
		os.Exit(commandExitCode(err))
	}
}

type ticketInputRejection struct{}

func (ticketInputRejection) Error() string { return "ticket envelope was rejected" }

func commandExitCode(err error) int {
	var rejected ticketInputRejection
	if errors.As(err, &rejected) {
		return 2
	}
	return 1
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("a worker command is required")
	}
	switch args[0] {
	case "parse-ticket":
		return runParseTicket(args[1:])
	case "read-ticket":
		return runReadTicket(args[1:])
	case "read-contract":
		return runReadContract(ctx, args[1:])
	case "build-draft":
		return runBuildDraft(args[1:])
	case "snapshot":
		return runSnapshot(ctx, args[1:])
	case "assess-readiness":
		return runAssessReadiness(ctx, args[1:])
	case "check-ticket":
		return runCheckTicket(args[1:])
	case "locate-target":
		return runLocateTarget(args[1:])
	case "list-candidates":
		return runListCandidates(args[1:])
	case "derive-contract":
		return runDeriveContract(ctx, args[1:])
	case "check-readiness":
		return runCheckReadiness(ctx, args[1:])
	case "decide-readiness":
		return runDecideReadiness(args[1:])
	case "generate":
		return runGenerate(ctx, args[1:])
	case "implement":
		return runImplement(ctx, args[1:])
	case "review":
		return runReview(ctx, args[1:])
	case "agent-review":
		return runAgentReview(ctx, args[1:])
	case "decide":
		return runDecide(args[1:])
	case "impasse-question":
		return runImpasseQuestion(ctx, args[1:])
	case "compose-trail":
		return runComposeTrail(args[1:])
	case "preserve-answers":
		return runPreserveAnswers(args[1:])
	case "apply":
		return runApply(args[1:], false)
	case "verify-applied":
		return runApply(args[1:], true)
	case "run-validation":
		return runValidation(ctx, args[1:])
	case "verify-validation":
		return runVerifyValidation(args[1:])
	case "verify-publish-gate":
		return runVerifyPublishGate(args[1:])
	case "preflight":
		return runPreflight(ctx, args[1:])
	default:
		return errors.New("worker command is invalid")
	}
}

func runValidation(ctx context.Context, args []string) error {
	flags := commandFlags("run-validation")
	configPath := flags.String("config", "", "")
	toolSHA := flags.String("tool-sha", "", "")
	ticketPath := flags.String("ticket", "", "")
	sourcePath := flags.String("source", "", "")
	candidatePath := flags.String("candidate", "", "")
	repoRoot := flags.String("repo-root", "", "")
	outputPath := flags.String("out", "", "")
	if !parseFlags(flags, args) || !allPresent(*configPath, *toolSHA, *ticketPath, *sourcePath, *candidatePath, *repoRoot, *outputPath) || !worker.ValidToolSHA(*toolSHA) {
		return errors.New("run-validation arguments are invalid")
	}
	config, request, source, err := readBoundInputs(*configPath, *toolSHA, *ticketPath, *sourcePath)
	if err != nil {
		return err
	}
	var candidate worker.Candidate
	if err := worker.ReadJSONFile(*candidatePath, worker.MaxArtifactJSONBytes, &candidate); err != nil {
		return errors.New("candidate artifact could not be read")
	}
	evidence, err := worker.RunValidationEvidence(ctx, *repoRoot, candidate, source, request, config)
	if err != nil {
		// The command line comes from the fixed consumer configuration; the
		// tail is untrusted build output and stays in the job log only.
		var commandFailure *worker.ValidationCommandError
		if errors.As(err, &commandFailure) {
			fmt.Fprintf(os.Stderr, "worker: validation command failed: %s\n", strings.Join(commandFailure.Arguments, " "))
			if len(commandFailure.Tail) > 0 {
				fmt.Fprintf(os.Stderr, "worker: validation output tail (%d bytes):\n%s\n", len(commandFailure.Tail), commandFailure.Tail)
			}
		}
		fmt.Fprintf(os.Stderr, "worker: %s: %v\n", "candidate validation failed", err)
		return errors.New("candidate validation failed")
	}
	if err := worker.WriteJSONFileExclusive(*outputPath, evidence, worker.MaxValidationJSONBytes); err != nil {
		return errors.New("validation evidence could not be written")
	}
	return nil
}

func runVerifyValidation(args []string) error {
	flags := commandFlags("verify-validation")
	configPath := flags.String("config", "", "")
	toolSHA := flags.String("tool-sha", "", "")
	ticketPath := flags.String("ticket", "", "")
	sourcePath := flags.String("source", "", "")
	candidatePath := flags.String("candidate", "", "")
	validationPath := flags.String("validation", "", "")
	if !parseFlags(flags, args) || !allPresent(*configPath, *toolSHA, *ticketPath, *sourcePath, *candidatePath, *validationPath) || !worker.ValidToolSHA(*toolSHA) {
		return errors.New("verify-validation arguments are invalid")
	}
	config, request, source, err := readBoundInputs(*configPath, *toolSHA, *ticketPath, *sourcePath)
	if err != nil {
		return err
	}
	var candidate worker.Candidate
	if err := worker.ReadJSONFile(*candidatePath, worker.MaxArtifactJSONBytes, &candidate); err != nil {
		return errors.New("candidate artifact could not be read")
	}
	var evidence worker.ValidationEvidence
	if err := worker.ReadJSONFile(*validationPath, worker.MaxValidationJSONBytes, &evidence); err != nil || evidence.Validate(candidate, source, request, config) != nil {
		return errors.New("validation evidence was rejected")
	}
	return nil
}

func runVerifyPublishGate(args []string) error {
	flags := commandFlags("verify-publish-gate")
	configPath := flags.String("config", "", "")
	toolSHA := flags.String("tool-sha", "", "")
	ticketPath := flags.String("ticket", "", "")
	sourcePath := flags.String("source", "", "")
	candidatePath := flags.String("candidate", "", "")
	decisionPath := flags.String("decision", "", "")
	validationPath := flags.String("validation", "", "")
	var reviewPaths stringList
	flags.Var(&reviewPaths, "review", "")
	if !parseFlags(flags, args) || !allPresent(*configPath, *toolSHA, *ticketPath, *sourcePath, *candidatePath, *decisionPath, *validationPath) ||
		!worker.ValidToolSHA(*toolSHA) || len(reviewPaths) == 0 {
		return errors.New("verify-publish-gate arguments are invalid")
	}
	config, request, source, err := readBoundInputs(*configPath, *toolSHA, *ticketPath, *sourcePath)
	if err != nil {
		return err
	}
	var candidate worker.Candidate
	if err := worker.ReadJSONFile(*candidatePath, worker.MaxArtifactJSONBytes, &candidate); err != nil {
		return errors.New("candidate artifact could not be read")
	}
	var decision worker.StageDecision
	if err := worker.ReadJSONFile(*decisionPath, worker.MaxDecisionJSONBytes, &decision); err != nil {
		return errors.New("decision artifact could not be read")
	}
	var validation worker.ValidationEvidence
	if err := worker.ReadJSONFile(*validationPath, worker.MaxValidationJSONBytes, &validation); err != nil {
		return errors.New("validation evidence could not be read")
	}
	reviews, err := readReviews(reviewPaths)
	if err != nil {
		return err
	}
	if err := worker.ValidatePublishGate(decision, validation, candidate, reviews, source, request, config); err != nil {
		return errors.New("publish gate was rejected")
	}
	return nil
}

// runReadTicket seals the ticket exactly as the tracker holds it. It requires
// no format of the requester and reads no repository, so it can run before any
// credential is minted. A ticket only fails here if it cannot be processed at
// all; being written in prose is not a failure.
func runReadTicket(args []string) error {
	flags := commandFlags("read-ticket")
	configPath := flags.String("config", "", "")
	toolSHA := flags.String("tool-sha", "", "")
	envelopePath := flags.String("envelope", "", "")
	clarificationOutPath := flags.String("clarification-out", "", "")
	outputPath := flags.String("out", "", "")
	if !parseFlags(flags, args) || !allPresent(*configPath, *toolSHA, *envelopePath, *outputPath) || !worker.ValidToolSHA(*toolSHA) {
		return errors.New("read-ticket arguments are invalid")
	}
	config, err := readConfig(*configPath)
	if err != nil {
		return err
	}
	var envelope hook.DispatchEnvelope
	if err := worker.ReadJSONFile(*envelopePath, worker.MaxEnvelopeJSONBytes, &envelope); err != nil {
		return errors.New("ticket envelope could not be read")
	}
	raw, err := worker.ReadRawTicket(envelope, config, *toolSHA)
	if err != nil {
		return ticketInputRejection{}
	}
	if envelope.ClarificationJSON != "" {
		if err := hook.ValidateEnvelope(envelope); err != nil {
			return ticketInputRejection{}
		}
		if *clarificationOutPath == "" {
			return errors.New("clarification artifact destination is missing")
		}
		if err := writeRawFileExclusive(*clarificationOutPath, []byte(envelope.ClarificationJSON), worker.MaxClarificationJSONBytes); err != nil {
			return errors.New("clarification artifact could not be written")
		}
	}
	if err := worker.WriteJSONFileExclusive(*outputPath, raw, worker.MaxTicketJSONBytes); err != nil {
		return errors.New("raw ticket artifact could not be written")
	}
	return nil
}

// runReadContract reads the contract out of the prose. It needs a model but no
// repository, so it runs in a job that holds model credentials only. Whatever
// the ticket does not state is returned as a question rather than an error, so
// this step does not fail on an incomplete request.
func runReadContract(ctx context.Context, args []string) error {
	flags := commandFlags("read-contract")
	configPath := flags.String("config", "", "")
	toolSHA := flags.String("tool-sha", "", "")
	rawPath := flags.String("raw", "", "")
	outputPath := flags.String("out", "", "")
	if !parseFlags(flags, args) || !allPresent(*configPath, *toolSHA, *rawPath, *outputPath) || !worker.ValidToolSHA(*toolSHA) {
		return errors.New("read-contract arguments are invalid")
	}
	config, err := readConfig(*configPath)
	if err != nil {
		return err
	}
	var raw worker.RawTicket
	if err := worker.ReadJSONFile(*rawPath, worker.MaxTicketJSONBytes, &raw); err != nil || raw.Validate(config) != nil || raw.ToolSHA != *toolSHA {
		return errors.New("raw ticket artifact was rejected")
	}
	invoker, err := newModelInvoker(ctx, config.Models.Readiness.Assessor)
	if err != nil {
		return err
	}
	intake, _, err := invoker.ReadContract(ctx, raw, config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "worker: %s: %v\n", "contract intake failed", err)
		return errors.New("contract intake failed")
	}
	if err := worker.WriteJSONFileExclusive(*outputPath, intake, worker.MaxArtifactJSONBytes); err != nil {
		return errors.New("contract intake artifact could not be written")
	}
	return nil
}

// runBuildDraft completes the contract from what intake read. It exits 2 when
// intake left questions open, which the caller routes to the clarification
// path: an unanswered question stops the run without pretending the ticket was
// malformed.
func runBuildDraft(args []string) error {
	flags := commandFlags("build-draft")
	configPath := flags.String("config", "", "")
	toolSHA := flags.String("tool-sha", "", "")
	rawPath := flags.String("raw", "", "")
	intakePath := flags.String("intake", "", "")
	outputPath := flags.String("out", "", "")
	if !parseFlags(flags, args) || !allPresent(*configPath, *toolSHA, *rawPath, *intakePath, *outputPath) || !worker.ValidToolSHA(*toolSHA) {
		return errors.New("build-draft arguments are invalid")
	}
	config, err := readConfig(*configPath)
	if err != nil {
		return err
	}
	var raw worker.RawTicket
	if err := worker.ReadJSONFile(*rawPath, worker.MaxTicketJSONBytes, &raw); err != nil || raw.Validate(config) != nil || raw.ToolSHA != *toolSHA {
		return errors.New("raw ticket artifact was rejected")
	}
	var intake worker.ContractIntake
	if err := worker.ReadJSONFile(*intakePath, worker.MaxArtifactJSONBytes, &intake); err != nil || intake.Validate(raw, config) != nil {
		return errors.New("contract intake artifact was rejected")
	}
	if !intake.Complete() {
		return ticketInputRejection{}
	}
	draft, err := intake.ToDraft(raw, config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "worker: %s: %v\n", "contract intake could not be completed", err)
		return errors.New("contract intake could not be completed")
	}
	if err := worker.WriteJSONFileExclusive(*outputPath, draft, worker.MaxTicketJSONBytes); err != nil {
		return errors.New("ticket draft artifact could not be written")
	}
	return nil
}

func runParseTicket(args []string) error {
	flags := commandFlags("parse-ticket")
	configPath := flags.String("config", "", "")
	toolSHA := flags.String("tool-sha", "", "")
	envelopePath := flags.String("envelope", "", "")
	clarificationOutPath := flags.String("clarification-out", "", "")
	draftOutPath := flags.String("draft-out", "", "")
	outputPath := flags.String("out", "", "")
	if !parseFlags(flags, args) || !allPresent(*configPath, *toolSHA, *envelopePath, *outputPath) || !worker.ValidToolSHA(*toolSHA) {
		return errors.New("parse-ticket arguments are invalid")
	}
	config, err := readConfig(*configPath)
	if err != nil {
		return err
	}
	var envelope hook.DispatchEnvelope
	if err := worker.ReadJSONFile(*envelopePath, worker.MaxEnvelopeJSONBytes, &envelope); err != nil {
		return errors.New("ticket envelope could not be read")
	}
	draft, targetFiles, err := worker.ParseTicketDraft(envelope, config, *toolSHA)
	if err != nil {
		return ticketInputRejection{}
	}
	if envelope.ClarificationJSON != "" {
		if err := hook.ValidateEnvelope(envelope); err != nil {
			return ticketInputRejection{}
		}
		if *clarificationOutPath == "" {
			return errors.New("clarification artifact destination is missing")
		}
		if err := writeRawFileExclusive(*clarificationOutPath, []byte(envelope.ClarificationJSON), worker.MaxClarificationJSONBytes); err != nil {
			return errors.New("clarification artifact could not be written")
		}
	}
	if len(targetFiles) == 0 {
		// The requester did not name the files, which is the normal case for
		// anyone who has not already read the repository. The contract is
		// completed by derive-contract before the pipeline continues.
		if *draftOutPath == "" {
			return errors.New("ticket draft destination is missing")
		}
		if err := worker.WriteJSONFileExclusive(*draftOutPath, draft, worker.MaxTicketJSONBytes); err != nil {
			return errors.New("ticket draft artifact could not be written")
		}
		return nil
	}
	request, err := draft.WithTargetFiles(targetFiles, config)
	if err != nil {
		return ticketInputRejection{}
	}
	if err := worker.WriteJSONFileExclusive(*outputPath, request, worker.MaxTicketJSONBytes); err != nil {
		return errors.New("ticket artifact could not be written")
	}
	return nil
}

// runCheckTicket reports whether a ticket description would be accepted, and
// what the automation understood from it. It exists so a requester can find
// out before filing the ticket rather than from a rejection comment
// afterwards: the rejection can only say the input was not in the permitted
// format, which is not enough to fix it.
func runCheckTicket(args []string) error {
	flags := commandFlags("check-ticket")
	configPath := flags.String("config", "", "")
	runID := flags.String("run-id", "", "")
	descriptionPath := flags.String("description", "", "")
	if !parseFlags(flags, args) || !allPresent(*configPath, *runID, *descriptionPath) {
		return errors.New("check-ticket arguments are invalid")
	}
	config, err := readConfig(*configPath)
	if err != nil {
		return err
	}
	description, err := os.ReadFile(*descriptionPath)
	if err != nil {
		return errors.New("ticket description could not be read")
	}
	envelope, err := hook.SealSnapshot(hook.TicketSnapshot{
		SchemaVersion: hook.SnapshotSchemaVersion,
		SpaceKey:      "example", ActivityID: 1, ActivityType: 1, ProjectID: 1, ProjectKey: "EXAMPLE",
		IssueID: 1, IssueKey: "EXAMPLE-1", IssueKeyID: 1, CreatorID: 1,
		RunID: *runID, CreatedAt: time.Now().UTC(),
		Target:    hook.DeliveryTarget{RepositoryID: 1, WorkflowRefSHA256: hook.HashIdentity("example/example/.github/workflows/m1-worker.yml@refs/heads/main")},
		Untrusted: hook.UntrustedTicketData{Summary: "check", Description: string(description)},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "worker: %s: %v\n", "ticket run id is invalid", err)
		return errors.New("ticket run id is invalid")
	}
	draft, files, err := worker.ParseTicketDraft(envelope, config, unboundCheckToolSHA)
	if err != nil {
		fmt.Println("この本文は受け付けられません。")
		fmt.Println("  理由:", err)
		fmt.Println("  ヘッダの順序は Automation-Run-ID / Automation-Mode / (Target-File) / Verification-Path / Expected-Text / Absent-Text、続けて --- の行、そのあとに本文です。")
		return ticketInputRejection{}
	}
	fmt.Println("この本文は受け付けられます。読み取った内容:")
	if len(files) == 0 {
		fmt.Println("  対象ファイル: 指定なし（変更前の文言を含むファイルを自動で探します）")
	} else {
		fmt.Println("  対象ファイル:", strings.Join(files, ", "))
	}
	fmt.Println("  確認する画面:", draft.VerificationPath)
	fmt.Println("  変更前の文言:", draft.AbsentText)
	fmt.Println("  変更後の文言:", draft.ExpectedText)
	fmt.Println("  依頼内容:", firstLine(draft.Request))
	return nil
}

func firstLine(value string) string {
	if index := strings.IndexByte(value, '\n'); index >= 0 {
		return value[:index] + " ..."
	}
	return value
}

// unboundCheckToolSHA marks a format check, which is never bound to a run.
const unboundCheckToolSHA = "0000000000000000000000000000000000000000"

// runLocateTarget completes a draft contract by finding the wording the
// ticket says must disappear. No model is involved: for a client-visible text
// change the target file is wherever the current wording lives.
func runLocateTarget(args []string) error {
	flags := commandFlags("locate-target")
	configPath := flags.String("config", "", "")
	toolSHA := flags.String("tool-sha", "", "")
	draftPath := flags.String("draft", "", "")
	repoRoot := flags.String("repo-root", "", "")
	outputPath := flags.String("out", "", "")
	if !parseFlags(flags, args) || !allPresent(*configPath, *toolSHA, *draftPath, *repoRoot, *outputPath) || !worker.ValidToolSHA(*toolSHA) {
		return errors.New("locate-target arguments are invalid")
	}
	config, err := readConfig(*configPath)
	if err != nil {
		return err
	}
	var draft worker.TicketDraft
	if err := worker.ReadJSONFile(*draftPath, worker.MaxTicketJSONBytes, &draft); err != nil {
		return errors.New("ticket draft could not be read")
	}
	location, err := worker.LocateTargetFiles(*repoRoot, draft, config)
	if err != nil {
		return ticketInputRejection{}
	}
	request, err := location.Resolve(draft, config)
	if err != nil {
		return ticketInputRejection{}
	}
	if err := worker.WriteJSONFileExclusive(*outputPath, request, worker.MaxTicketJSONBytes); err != nil {
		return errors.New("ticket artifact could not be written")
	}
	return nil
}

func runListCandidates(args []string) error {
	flags := commandFlags("list-candidates")
	configPath := flags.String("config", "", "")
	toolSHA := flags.String("tool-sha", "", "")
	repoRoot := flags.String("repo-root", "", "")
	baseSHA := flags.String("base-sha", "", "")
	draftPath := flags.String("draft", "", "")
	outputPath := flags.String("out", "", "")
	if !parseFlags(flags, args) || !allPresent(*configPath, *toolSHA, *repoRoot, *baseSHA, *draftPath, *outputPath) || !worker.ValidToolSHA(*toolSHA) {
		return errors.New("list-candidates arguments are invalid")
	}
	config, err := readConfig(*configPath)
	if err != nil {
		return err
	}
	var draft worker.TicketDraft
	if err := worker.ReadJSONFile(*draftPath, worker.MaxTicketJSONBytes, &draft); err != nil {
		return errors.New("ticket draft could not be read")
	}
	consumer, err := config.ConsumerFor(draft.Repository)
	if err != nil {
		return errors.New("ticket draft repository is not a configured consumer")
	}
	listing, err := worker.ReadCandidateListing(*repoRoot, *baseSHA, consumer, config)
	if err != nil {
		return errors.New("candidate listing could not be created")
	}
	if err := worker.WriteJSONFileExclusive(*outputPath, listing, worker.MaxArtifactJSONBytes); err != nil {
		return errors.New("candidate listing artifact could not be written")
	}
	return nil
}

func runDeriveContract(ctx context.Context, args []string) error {
	flags := commandFlags("derive-contract")
	configPath := flags.String("config", "", "")
	toolSHA := flags.String("tool-sha", "", "")
	draftPath := flags.String("draft", "", "")
	listingPath := flags.String("listing", "", "")
	derivationOutPath := flags.String("derivation-out", "", "")
	outputPath := flags.String("out", "", "")
	if !parseFlags(flags, args) || !allPresent(*configPath, *toolSHA, *draftPath, *listingPath, *derivationOutPath, *outputPath) ||
		!worker.ValidToolSHA(*toolSHA) {
		return errors.New("derive-contract arguments are invalid")
	}
	config, err := readConfig(*configPath)
	if err != nil {
		return err
	}
	var draft worker.TicketDraft
	if err := worker.ReadJSONFile(*draftPath, worker.MaxTicketJSONBytes, &draft); err != nil {
		return errors.New("ticket draft could not be read")
	}
	var listing worker.CandidateListing
	if err := worker.ReadJSONFile(*listingPath, worker.MaxArtifactJSONBytes, &listing); err != nil {
		return errors.New("candidate listing could not be read")
	}
	invoker, err := newModelInvoker(ctx, config.Models.Readiness.Assessor)
	if err != nil {
		return err
	}
	// One retry: a reasoning model occasionally returns one malformed
	// response, and a whole run must not die on a single roll when a second
	// costs seconds (a live run did, 2026-08-14). The first failure is
	// printed so a systematic cause still leaves both reasons in the log.
	derivation, _, err := invoker.DeriveTargetFiles(ctx, draft, listing, config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "worker: %s: %v\n", "contract derivation attempt 1 failed", err)
		derivation, _, err = invoker.DeriveTargetFiles(ctx, draft, listing, config)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "worker: %s: %v\n", "contract derivation failed", err)
		return errors.New("contract derivation failed")
	}
	request, err := draft.WithTargetFiles(derivation.TargetFiles, config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "worker: %s: %v\n", "derived contract is invalid", err)
		return errors.New("derived contract is invalid")
	}
	if err := worker.WriteJSONFileExclusive(*derivationOutPath, derivation, worker.MaxArtifactJSONBytes); err != nil {
		return errors.New("contract derivation artifact could not be written")
	}
	if err := worker.WriteJSONFileExclusive(*outputPath, request, worker.MaxTicketJSONBytes); err != nil {
		return errors.New("ticket artifact could not be written")
	}
	return nil
}

func runSnapshot(ctx context.Context, args []string) error {
	flags := commandFlags("snapshot")
	configPath := flags.String("config", "", "")
	toolSHA := flags.String("tool-sha", "", "")
	ticketPath := flags.String("ticket", "", "")
	repoRoot := flags.String("repo-root", "", "")
	baseSHA := flags.String("base-sha", "", "")
	outputPath := flags.String("out", "", "")
	if !parseFlags(flags, args) || !allPresent(*configPath, *toolSHA, *ticketPath, *repoRoot, *baseSHA, *outputPath) || !worker.ValidToolSHA(*toolSHA) {
		return errors.New("snapshot arguments are invalid")
	}
	config, request, err := readConfigAndTicket(*configPath, *toolSHA, *ticketPath)
	if err != nil {
		return err
	}
	source, err := worker.ReadVerifiedSourceSnapshot(ctx, *repoRoot, *baseSHA, request, config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "worker: %s: %v\n", "source snapshot could not be created", err)
		return errors.New("source snapshot could not be created")
	}
	if err := worker.WriteJSONFileExclusive(*outputPath, source, worker.MaxArtifactJSONBytes); err != nil {
		return errors.New("source artifact could not be written")
	}
	return nil
}

func runGenerate(ctx context.Context, args []string) error {
	flags := commandFlags("generate")
	configPath := flags.String("config", "", "")
	toolSHA := flags.String("tool-sha", "", "")
	ticketPath := flags.String("ticket", "", "")
	sourcePath := flags.String("source", "", "")
	readinessPath := flags.String("readiness", "", "")
	clarificationPath := flags.String("clarification", "", "")
	stage := flags.Int("stage", 0, "")
	previousPath := flags.String("previous-candidate", "", "")
	outputPath := flags.String("out", "", "")
	var reviewPaths stringList
	var assessmentPaths stringList
	var checkPaths stringList
	flags.Var(&reviewPaths, "previous-review", "")
	flags.Var(&assessmentPaths, "assessment", "")
	flags.Var(&checkPaths, "check", "")
	if !parseFlags(flags, args) || !allPresent(*configPath, *toolSHA, *ticketPath, *sourcePath, *readinessPath, *outputPath) || !worker.ValidToolSHA(*toolSHA) || *stage < 1 ||
		len(assessmentPaths) == 0 || len(assessmentPaths) != len(checkPaths) {
		return errors.New("generate arguments are invalid")
	}
	config, request, source, err := readBoundInputs(*configPath, *toolSHA, *ticketPath, *sourcePath)
	if err != nil {
		return err
	}
	if *stage > config.MaxStages {
		return errors.New("generate stage is invalid")
	}
	var readiness worker.ReadinessDecision
	if err := worker.ReadJSONFile(*readinessPath, worker.MaxReadinessJSONBytes, &readiness); err != nil {
		return errors.New("readiness decision artifact could not be read")
	}
	assessments := make([]worker.ReadinessAssessment, 0, len(assessmentPaths))
	for _, filename := range assessmentPaths {
		var assessment worker.ReadinessAssessment
		if err := worker.ReadJSONFile(filename, worker.MaxReadinessJSONBytes, &assessment); err != nil {
			return errors.New("readiness assessment artifact could not be read")
		}
		assessments = append(assessments, assessment)
	}
	checks := make([]worker.ReadinessCheck, 0, len(checkPaths))
	for _, filename := range checkPaths {
		var check worker.ReadinessCheck
		if err := worker.ReadJSONFile(filename, worker.MaxReadinessJSONBytes, &check); err != nil {
			return errors.New("readiness check artifact could not be read")
		}
		checks = append(checks, check)
	}
	if err := readiness.Validate(assessments, checks, source, request, config); err != nil {
		return errors.New("readiness decision chain was rejected")
	}
	var previous *worker.Candidate
	if *previousPath != "" {
		var value worker.Candidate
		if err := worker.ReadJSONFile(*previousPath, worker.MaxArtifactJSONBytes, &value); err != nil {
			return errors.New("previous candidate artifact could not be read")
		}
		previous = &value
	}
	reviews, err := readReviews(reviewPaths)
	if err != nil {
		return err
	}
	invoker, err := newModelInvoker(ctx, config.Models.Implementer)
	if err != nil {
		return err
	}
	clarification, err := readClarificationContext(*clarificationPath)
	if err != nil {
		return err
	}
	candidate, _, err := invoker.GenerateCandidate(ctx, *stage, readiness, clarification, source, request, previous, reviews, config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "worker: %s: %v\n", "candidate generation failed", err)
		return errors.New("candidate generation failed")
	}
	if err := worker.WriteJSONFileExclusive(*outputPath, candidate, worker.MaxArtifactJSONBytes); err != nil {
		return errors.New("candidate artifact could not be written")
	}
	return nil
}

func runAssessReadiness(ctx context.Context, args []string) error {
	flags := commandFlags("assess-readiness")
	configPath := flags.String("config", "", "")
	toolSHA := flags.String("tool-sha", "", "")
	ticketPath := flags.String("ticket", "", "")
	sourcePath := flags.String("source", "", "")
	attempt := flags.Int("attempt", 0, "")
	previousPath := flags.String("previous-assessment", "", "")
	previousCheckPath := flags.String("previous-check", "", "")
	clarificationPath := flags.String("clarification", "", "")
	outputPath := flags.String("out", "", "")
	if !parseFlags(flags, args) || !allPresent(*configPath, *toolSHA, *ticketPath, *sourcePath, *outputPath) ||
		!worker.ValidToolSHA(*toolSHA) || *attempt < 1 || *attempt > worker.MaxReadinessAttempts ||
		(*attempt == 1) != (*previousPath == "" && *previousCheckPath == "") {
		return errors.New("assess-readiness arguments are invalid")
	}
	config, request, source, err := readBoundInputs(*configPath, *toolSHA, *ticketPath, *sourcePath)
	if err != nil {
		return err
	}
	var previous *worker.ReadinessAssessment
	var previousCheck *worker.ReadinessCheck
	if *previousPath != "" {
		var assessment worker.ReadinessAssessment
		if err := worker.ReadJSONFile(*previousPath, worker.MaxReadinessJSONBytes, &assessment); err != nil {
			return errors.New("previous readiness assessment could not be read")
		}
		previous = &assessment
		var check worker.ReadinessCheck
		if err := worker.ReadJSONFile(*previousCheckPath, worker.MaxReadinessJSONBytes, &check); err != nil {
			return errors.New("previous readiness check could not be read")
		}
		previousCheck = &check
	}
	clarification, err := readClarificationContext(*clarificationPath)
	if err != nil {
		return err
	}
	invoker, err := newModelInvoker(ctx, config.Models.Readiness.Assessor)
	if err != nil {
		return err
	}
	assessment, _, err := invoker.AssessReadiness(ctx, *attempt, previous, previousCheck, clarification, source, request, config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "worker: %s: %v\n", "readiness assessment failed", err)
		return errors.New("readiness assessment failed")
	}
	if err := worker.WriteJSONFileExclusive(*outputPath, assessment, worker.MaxReadinessJSONBytes); err != nil {
		return errors.New("readiness assessment artifact could not be written")
	}
	return nil
}

func runCheckReadiness(ctx context.Context, args []string) error {
	flags := commandFlags("check-readiness")
	configPath := flags.String("config", "", "")
	toolSHA := flags.String("tool-sha", "", "")
	ticketPath := flags.String("ticket", "", "")
	sourcePath := flags.String("source", "", "")
	assessmentPath := flags.String("assessment", "", "")
	clarificationPath := flags.String("clarification", "", "")
	outputPath := flags.String("out", "", "")
	if !parseFlags(flags, args) || !allPresent(*configPath, *toolSHA, *ticketPath, *sourcePath, *assessmentPath, *outputPath) || !worker.ValidToolSHA(*toolSHA) {
		return errors.New("check-readiness arguments are invalid")
	}
	config, request, source, err := readBoundInputs(*configPath, *toolSHA, *ticketPath, *sourcePath)
	if err != nil {
		return err
	}
	var assessment worker.ReadinessAssessment
	if err := worker.ReadJSONFile(*assessmentPath, worker.MaxReadinessJSONBytes, &assessment); err != nil {
		return errors.New("readiness assessment artifact could not be read")
	}
	clarification, err := readClarificationContext(*clarificationPath)
	if err != nil {
		return err
	}
	invoker, err := newModelInvoker(ctx, config.Models.Readiness.Checker)
	if err != nil {
		return err
	}
	check, _, err := invoker.CheckReadiness(ctx, assessment, clarification, source, request, config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "worker: %s: %v\n", "readiness check failed", err)
		return errors.New("readiness check failed")
	}
	if err := worker.WriteJSONFileExclusive(*outputPath, check, worker.MaxReadinessJSONBytes); err != nil {
		return errors.New("readiness check artifact could not be written")
	}
	return nil
}

func runDecideReadiness(args []string) error {
	flags := commandFlags("decide-readiness")
	configPath := flags.String("config", "", "")
	toolSHA := flags.String("tool-sha", "", "")
	ticketPath := flags.String("ticket", "", "")
	sourcePath := flags.String("source", "", "")
	outputPath := flags.String("out", "", "")
	var assessmentPaths stringList
	var checkPaths stringList
	flags.Var(&assessmentPaths, "assessment", "")
	flags.Var(&checkPaths, "check", "")
	if !parseFlags(flags, args) || !allPresent(*configPath, *toolSHA, *ticketPath, *sourcePath, *outputPath) ||
		!worker.ValidToolSHA(*toolSHA) || len(assessmentPaths) == 0 || len(assessmentPaths) != len(checkPaths) {
		return errors.New("decide-readiness arguments are invalid")
	}
	config, request, source, err := readBoundInputs(*configPath, *toolSHA, *ticketPath, *sourcePath)
	if err != nil {
		return err
	}
	assessments := make([]worker.ReadinessAssessment, 0, len(assessmentPaths))
	for _, filename := range assessmentPaths {
		var assessment worker.ReadinessAssessment
		if err := worker.ReadJSONFile(filename, worker.MaxReadinessJSONBytes, &assessment); err != nil {
			return errors.New("readiness assessment artifact could not be read")
		}
		assessments = append(assessments, assessment)
	}
	checks := make([]worker.ReadinessCheck, 0, len(checkPaths))
	for _, filename := range checkPaths {
		var check worker.ReadinessCheck
		if err := worker.ReadJSONFile(filename, worker.MaxReadinessJSONBytes, &check); err != nil {
			return errors.New("readiness check artifact could not be read")
		}
		checks = append(checks, check)
	}
	decision, err := worker.DecideReadiness(assessments, checks, source, request, config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "worker: %s: %v\n", "readiness decision was rejected", err)
		return errors.New("readiness decision was rejected")
	}
	if err := worker.WriteJSONFileExclusive(*outputPath, decision, worker.MaxReadinessJSONBytes); err != nil {
		return errors.New("readiness decision artifact could not be written")
	}
	return nil
}

func runReview(ctx context.Context, args []string) error {
	flags := commandFlags("review")
	configPath := flags.String("config", "", "")
	toolSHA := flags.String("tool-sha", "", "")
	ticketPath := flags.String("ticket", "", "")
	sourcePath := flags.String("source", "", "")
	candidatePath := flags.String("candidate", "", "")
	reviewerID := flags.String("reviewer", "", "")
	reviewClarificationPath := flags.String("clarification", "", "")
	outputPath := flags.String("out", "", "")
	if !parseFlags(flags, args) || !allPresent(*configPath, *toolSHA, *ticketPath, *sourcePath, *candidatePath, *reviewerID, *outputPath) || !worker.ValidToolSHA(*toolSHA) {
		return errors.New("review arguments are invalid")
	}
	config, request, source, err := readBoundInputs(*configPath, *toolSHA, *ticketPath, *sourcePath)
	if err != nil {
		return err
	}
	var candidate worker.Candidate
	if err := worker.ReadJSONFile(*candidatePath, worker.MaxArtifactJSONBytes, &candidate); err != nil {
		return errors.New("candidate artifact could not be read")
	}
	if err := candidate.Validate(source, request, config); err != nil {
		return errors.New("candidate artifact was rejected")
	}
	endpoint, ok := configuredEndpoint(config, *reviewerID, true)
	if !ok {
		return errors.New("reviewer is not configured")
	}
	invoker, err := newModelInvoker(ctx, endpoint)
	if err != nil {
		return err
	}
	clarification, err := readClarificationContext(*reviewClarificationPath)
	if err != nil {
		return err
	}
	review, _, err := invoker.ReviewCandidate(ctx, endpoint, candidate, clarification, source, request, config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "worker: %s: %v\n", "candidate review failed", err)
		return errors.New("candidate review failed")
	}
	if err := worker.WriteJSONFileExclusive(*outputPath, review, worker.MaxReviewJSONBytes); err != nil {
		return errors.New("review artifact could not be written")
	}
	return nil
}

func runDecide(args []string) error {
	flags := commandFlags("decide")
	configPath := flags.String("config", "", "")
	toolSHA := flags.String("tool-sha", "", "")
	ticketPath := flags.String("ticket", "", "")
	sourcePath := flags.String("source", "", "")
	candidatePath := flags.String("candidate", "", "")
	outputPath := flags.String("out", "", "")
	var reviewPaths stringList
	flags.Var(&reviewPaths, "review", "")
	if !parseFlags(flags, args) || !allPresent(*configPath, *toolSHA, *ticketPath, *sourcePath, *candidatePath, *outputPath) || !worker.ValidToolSHA(*toolSHA) || len(reviewPaths) == 0 {
		return errors.New("decide arguments are invalid")
	}
	config, request, source, err := readBoundInputs(*configPath, *toolSHA, *ticketPath, *sourcePath)
	if err != nil {
		return err
	}
	var candidate worker.Candidate
	if err := worker.ReadJSONFile(*candidatePath, worker.MaxArtifactJSONBytes, &candidate); err != nil {
		return errors.New("candidate artifact could not be read")
	}
	reviews, err := readReviews(reviewPaths)
	if err != nil {
		return err
	}
	decision, err := worker.DecideStage(candidate, reviews, source, request, config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "worker: %s: %v\n", "stage decision was rejected", err)
		return errors.New("stage decision was rejected")
	}
	if err := worker.WriteJSONFileExclusive(*outputPath, decision, worker.MaxDecisionJSONBytes); err != nil {
		return errors.New("decision artifact could not be written")
	}
	return nil
}

// runImpasseQuestion turns a nonconverged final stage into requester
// questions for the ask-and-resume rail, or records that the question rounds
// are spent so the workflow falls back to an honest nonconverged terminal.
func runImpasseQuestion(ctx context.Context, args []string) error {
	flags := commandFlags("impasse-question")
	configPath := flags.String("config", "", "")
	toolSHA := flags.String("tool-sha", "", "")
	ticketPath := flags.String("ticket", "", "")
	sourcePath := flags.String("source", "", "")
	candidatePath := flags.String("candidate", "", "")
	impasseClarificationPath := flags.String("clarification", "", "")
	outputPath := flags.String("out", "", "")
	var reviewPaths stringList
	flags.Var(&reviewPaths, "review", "")
	if !parseFlags(flags, args) || !allPresent(*configPath, *toolSHA, *ticketPath, *sourcePath, *candidatePath, *outputPath) || !worker.ValidToolSHA(*toolSHA) || len(reviewPaths) == 0 {
		return errors.New("impasse-question arguments are invalid")
	}
	config, request, source, err := readBoundInputs(*configPath, *toolSHA, *ticketPath, *sourcePath)
	if err != nil {
		return err
	}
	var candidate worker.Candidate
	if err := worker.ReadJSONFile(*candidatePath, worker.MaxArtifactJSONBytes, &candidate); err != nil {
		return errors.New("candidate artifact could not be read")
	}
	reviews, err := readReviews(reviewPaths)
	if err != nil {
		return err
	}
	clarification, err := readClarificationContext(*impasseClarificationPath)
	if err != nil {
		return err
	}
	invoker, err := newModelInvoker(ctx, config.Models.Readiness.Assessor)
	if err != nil {
		return err
	}
	decision, err := invoker.AskImpasse(ctx, candidate, reviews, clarification, source, request, config, time.Now().UTC())
	if err != nil {
		fmt.Fprintf(os.Stderr, "worker: %s: %v\n", "impasse question failed", err)
		return errors.New("impasse question failed")
	}
	if err := worker.WriteJSONFileExclusive(*outputPath, decision, worker.MaxReviewJSONBytes); err != nil {
		return errors.New("impasse decision could not be written")
	}
	return nil
}

// runComposeTrail renders the requester-facing record of a run from its
// sealed model history: rounds, findings, changed files, adopted decisions
// and the validation outcome. The result rides on the terminal report and the
// pull request body, so a ticket keeps durable evidence after run logs
// expire.
func runComposeTrail(args []string) error {
	flags := commandFlags("compose-trail")
	configPath := flags.String("config", "", "")
	toolSHA := flags.String("tool-sha", "", "")
	historyDir := flags.String("history", "", "")
	validationPath := flags.String("validation", "", "")
	trailClarificationPath := flags.String("clarification", "", "")
	outputPath := flags.String("out", "", "")
	if !parseFlags(flags, args) || !allPresent(*configPath, *toolSHA, *historyDir, *outputPath) || !worker.ValidToolSHA(*toolSHA) {
		return errors.New("compose-trail arguments are invalid")
	}
	config, err := worker.LoadConfig(*configPath)
	if err != nil {
		return errors.New("compose-trail config is invalid")
	}
	stages, err := worker.LoadTrailStages(*historyDir, config, *toolSHA)
	if err != nil {
		fmt.Fprintf(os.Stderr, "worker: %s: %v\n", "trail could not be composed", err)
		return errors.New("trail could not be composed")
	}
	clarification, err := readClarificationContext(*trailClarificationPath)
	if err != nil {
		return err
	}
	validationPassed := false
	if *validationPath != "" {
		var evidence worker.ValidationEvidence
		if err := worker.ReadJSONFile(*validationPath, worker.MaxValidationJSONBytes, &evidence); err == nil &&
			evidence.CandidateSHA256 == stages[len(stages)-1].Candidate.CandidateSHA256 {
			validationPassed = true
		}
	}
	trail := worker.ComposeTrail(stages, clarification, validationPassed)
	if err := writeRawFileExclusive(*outputPath, []byte(trail), worker.MaxTrailBytes); err != nil {
		return errors.New("trail could not be written")
	}
	return nil
}

// runPreserveAnswers renders the adopted answers of a resumed run into the
// record the instance keeps next to the knowledge its agents read. It writes
// nothing but the two artifacts named here; moving the render into the
// instance repository and committing it is the calling workflow's job.
func runPreserveAnswers(args []string) error {
	flags := commandFlags("preserve-answers")
	configPath := flags.String("config", "", "")
	toolSHA := flags.String("tool-sha", "", "")
	rawPath := flags.String("raw", "", "")
	clarificationPath := flags.String("clarification", "", "")
	metaOutPath := flags.String("meta-out", "", "")
	contentOutPath := flags.String("content-out", "", "")
	if !parseFlags(flags, args) ||
		!allPresent(*configPath, *toolSHA, *rawPath, *clarificationPath, *metaOutPath, *contentOutPath) ||
		!worker.ValidToolSHA(*toolSHA) {
		return errors.New("preserve-answers arguments are invalid")
	}
	config, err := readConfig(*configPath)
	if err != nil {
		return err
	}
	var raw worker.RawTicket
	if err := worker.ReadJSONFile(*rawPath, worker.MaxTicketJSONBytes, &raw); err != nil || raw.Validate(config) != nil || raw.ToolSHA != *toolSHA {
		return errors.New("raw ticket artifact was rejected")
	}
	clarification, err := readClarificationContext(*clarificationPath)
	if err != nil {
		return err
	}
	artifact, err := worker.PreserveAnswerKnowledge(config, raw, clarification)
	if err != nil {
		fmt.Fprintf(os.Stderr, "worker: %s: %v\n", "answers could not be preserved", err)
		return errors.New("answers could not be preserved")
	}
	if artifact.Enabled {
		if err := writeRawFileExclusive(*contentOutPath, artifact.Content, worker.MaxAnswerKnowledgeBytes); err != nil {
			return errors.New("answer knowledge could not be written")
		}
	}
	if err := worker.WriteJSONFileExclusive(*metaOutPath, artifact, worker.MaxArtifactJSONBytes); err != nil {
		return errors.New("answer knowledge could not be written")
	}
	return nil
}

func runApply(args []string, verifyOnly bool) error {
	name := "apply"
	if verifyOnly {
		name = "verify-applied"
	}
	flags := commandFlags(name)
	configPath := flags.String("config", "", "")
	toolSHA := flags.String("tool-sha", "", "")
	ticketPath := flags.String("ticket", "", "")
	sourcePath := flags.String("source", "", "")
	candidatePath := flags.String("candidate", "", "")
	repoRoot := flags.String("repo-root", "", "")
	if !parseFlags(flags, args) || !allPresent(*configPath, *toolSHA, *ticketPath, *sourcePath, *candidatePath, *repoRoot) || !worker.ValidToolSHA(*toolSHA) {
		return errors.New(name + " arguments are invalid")
	}
	config, request, source, err := readBoundInputs(*configPath, *toolSHA, *ticketPath, *sourcePath)
	if err != nil {
		return err
	}
	var candidate worker.Candidate
	if err := worker.ReadJSONFile(*candidatePath, worker.MaxArtifactJSONBytes, &candidate); err != nil {
		return errors.New("candidate artifact could not be read")
	}
	if verifyOnly {
		if err := worker.VerifyApplied(*repoRoot, candidate, source, request, config); err != nil {
			return errors.New("applied candidate verification failed")
		}
		return nil
	}
	if err := worker.ApplyCandidate(*repoRoot, candidate, source, request, config); err != nil {
		return errors.New("candidate apply failed")
	}
	return nil
}

func runPreflight(ctx context.Context, args []string) error {
	flags := commandFlags("preflight")
	configPath := flags.String("config", "", "")
	toolSHA := flags.String("tool-sha", "", "")
	endpointID := flags.String("endpoint", "", "")
	outputPath := flags.String("out", "", "")
	if !parseFlags(flags, args) || !allPresent(*configPath, *toolSHA, *endpointID, *outputPath) || !worker.ValidToolSHA(*toolSHA) {
		return errors.New("preflight arguments are invalid")
	}
	config, err := readConfig(*configPath)
	if err != nil {
		return err
	}
	endpoint, ok := configuredEndpoint(config, *endpointID, false)
	if !ok {
		return errors.New("preflight endpoint is not configured")
	}
	invoker, err := newModelInvoker(ctx, endpoint)
	if err != nil {
		return err
	}
	usage, err := invoker.Preflight(ctx, endpoint)
	if err != nil {
		fmt.Fprintf(os.Stderr, "worker: %s: %v\n", "model preflight failed", err)
		return errors.New("model preflight failed")
	}
	if err := worker.WriteJSONFileExclusive(*outputPath, usage, worker.MaxUsageJSONBytes); err != nil {
		return errors.New("preflight artifact could not be written")
	}
	return nil
}

func commandFlags(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	return flags
}

func parseFlags(flags *flag.FlagSet, args []string) bool {
	return flags.Parse(args) == nil && flags.NArg() == 0
}

func allPresent(values ...string) bool {
	for _, value := range values {
		if value == "" || strings.ContainsAny(value, "\r\n\x00") {
			return false
		}
	}
	return true
}

type stringList []string

func (values *stringList) String() string { return "" }

func (values *stringList) Set(value string) error {
	if value == "" || strings.ContainsAny(value, "\r\n\x00") || len(*values) >= 8 {
		return errors.New("list value is invalid")
	}
	*values = append(*values, value)
	return nil
}

func readConfig(filename string) (worker.Config, error) {
	var config worker.Config
	if err := worker.ReadJSONFile(filename, worker.MaxConfigJSONBytes, &config); err != nil {
		return worker.Config{}, errors.New("worker configuration could not be read")
	}
	if err := config.Validate(); err != nil {
		return worker.Config{}, errors.New("worker configuration was rejected")
	}
	return config, nil
}

func readConfigAndTicket(configPath, toolSHA, ticketPath string) (worker.Config, worker.TicketRequest, error) {
	config, err := readConfig(configPath)
	if err != nil {
		return worker.Config{}, worker.TicketRequest{}, err
	}
	var request worker.TicketRequest
	if err := worker.ReadJSONFile(ticketPath, worker.MaxTicketJSONBytes, &request); err != nil || request.Validate(config) != nil || request.ToolSHA != toolSHA {
		return worker.Config{}, worker.TicketRequest{}, errors.New("ticket artifact was rejected")
	}
	return config, request, nil
}

func readBoundInputs(configPath, toolSHA, ticketPath, sourcePath string) (worker.Config, worker.TicketRequest, worker.SourceSnapshot, error) {
	config, request, err := readConfigAndTicket(configPath, toolSHA, ticketPath)
	if err != nil {
		return worker.Config{}, worker.TicketRequest{}, worker.SourceSnapshot{}, err
	}
	var source worker.SourceSnapshot
	if err := worker.ReadJSONFile(sourcePath, worker.MaxArtifactJSONBytes, &source); err != nil || source.Validate(request, config) != nil {
		return worker.Config{}, worker.TicketRequest{}, worker.SourceSnapshot{}, errors.New("source artifact was rejected")
	}
	return config, request, source, nil
}

func readReviews(paths []string) ([]worker.Review, error) {
	reviews := make([]worker.Review, 0, len(paths))
	for _, filename := range paths {
		var review worker.Review
		if err := worker.ReadJSONFile(filename, worker.MaxReviewJSONBytes, &review); err != nil {
			return nil, errors.New("review artifact could not be read")
		}
		reviews = append(reviews, review)
	}
	return reviews, nil
}

func configuredEndpoint(config worker.Config, id string, reviewerOnly bool) (worker.ModelEndpoint, bool) {
	if !reviewerOnly {
		for _, endpoint := range []worker.ModelEndpoint{config.Models.Implementer, config.Models.Readiness.Assessor, config.Models.Readiness.Checker} {
			if endpoint.ID == id {
				return endpoint, true
			}
		}
	}
	for _, endpoint := range config.Models.Reviewers {
		if endpoint.ID == id {
			return endpoint, true
		}
	}
	return worker.ModelEndpoint{}, false
}

func newModelInvoker(_ context.Context, endpoint worker.ModelEndpoint) (*worker.ModelInvoker, error) {
	if err := worker.ValidateModelEndpoint(endpoint); err != nil {
		return nil, err
	}
	client, err := worker.NewGatewayClient(&http.Client{Timeout: worker.ModelInvocationTimeout})
	if err != nil {
		return nil, errors.New("model client could not be created")
	}
	invoker, err := worker.NewModelInvoker(client)
	if err != nil {
		return nil, errors.New("model client could not be created")
	}
	return invoker, nil
}

// readClarificationContext loads the optional sealed clarification artifact.
// An empty path means a revision-1 run.
func readClarificationContext(path string) (*worker.ClarificationContext, error) {
	if path == "" {
		return nil, nil
	}
	encoded, err := os.ReadFile(path)
	if err != nil || len(encoded) == 0 || len(encoded) > worker.MaxClarificationJSONBytes {
		return nil, errors.New("clarification artifact could not be read")
	}
	clarification, err := worker.DecodeClarificationContext(encoded)
	if err != nil {
		return nil, errors.New("clarification artifact is invalid")
	}
	return clarification, nil
}

func writeRawFileExclusive(path string, content []byte, maxBytes int) error {
	if len(content) == 0 || len(content) > maxBytes {
		return errors.New("artifact size is invalid")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}
