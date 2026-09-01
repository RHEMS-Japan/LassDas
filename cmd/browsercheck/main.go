package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"automation.internal/ticket-ingress/internal/releaseproof"
	"automation.internal/ticket-ingress/internal/visiblecheck"
	"automation.internal/ticket-ingress/internal/worker"
)

type commandConfig struct {
	configPath          string
	ticketPath          string
	sourcePath          string
	candidatePath       string
	decisionPath        string
	validationPath      string
	reviewPaths         []string
	stagingProofPath    string
	productionProofPath string
	priorEvidencePath   string
	priorScreenshotPath string
	environment         string
	toolSHA             string
	evidenceOut         string
	screenshotOut       string
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		// The reason is operational gold: every input here is a sealed
		// artifact, and "which seal refused" is the whole diagnosis.
		_, _ = fmt.Fprintln(os.Stderr, "browsercheck: verification failed:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if ctx == nil {
		return errors.New("browser command is invalid")
	}
	command, err := parseCommand(args)
	if err != nil {
		return err
	}
	var config worker.Config
	if err := worker.ReadJSONFile(command.configPath, worker.MaxConfigJSONBytes, &config); err != nil || config.Validate() != nil {
		return errors.New("browser config is invalid")
	}
	var request worker.TicketRequest
	if err := worker.ReadJSONFile(command.ticketPath, worker.MaxTicketJSONBytes, &request); err != nil ||
		request.Validate(config) != nil || request.ToolSHA != command.toolSHA {
		return errors.New("browser ticket is invalid")
	}
	var source worker.SourceSnapshot
	if err := worker.ReadJSONFile(command.sourcePath, worker.MaxArtifactJSONBytes, &source); err != nil || source.Validate(request, config) != nil {
		return errors.New("browser source is invalid")
	}
	var candidate worker.Candidate
	if err := worker.ReadJSONFile(command.candidatePath, worker.MaxArtifactJSONBytes, &candidate); err != nil ||
		candidate.Validate(source, request, config) != nil {
		return errors.New("browser candidate is invalid")
	}
	var decision worker.StageDecision
	if err := worker.ReadJSONFile(command.decisionPath, worker.MaxDecisionJSONBytes, &decision); err != nil {
		return errors.New("browser decision is invalid")
	}
	var validation worker.ValidationEvidence
	if err := worker.ReadJSONFile(command.validationPath, worker.MaxValidationJSONBytes, &validation); err != nil ||
		validation.Validate(candidate, source, request, config) != nil {
		return errors.New("browser validation is invalid")
	}
	reviews := make([]worker.Review, len(command.reviewPaths))
	for index, filename := range command.reviewPaths {
		if err := worker.ReadJSONFile(filename, worker.MaxReviewJSONBytes, &reviews[index]); err != nil {
			return errors.New("browser review is invalid")
		}
	}
	var staging releaseproof.StagingProof
	if err := worker.ReadJSONFile(command.stagingProofPath, worker.MaxArtifactJSONBytes, &staging); err != nil {
		return errors.New("browser staging proof is invalid")
	}
	input := releaseproof.StagingInputs{
		Request: request, Config: config, Source: source, Candidate: candidate,
		Reviews: reviews, Decision: decision, Validation: validation,
		Baseline: staging.Baseline, PublishedFeature: staging.PublishedFeature,
		FeaturePullRequest: staging.FeaturePullRequest, FeatureChecks: staging.FeatureChecks,
		FeatureMerge: staging.FeatureMerge, StagingDeployment: staging.StagingDeployment,
	}
	if err := staging.Validate(input); err != nil {
		return errors.New("browser staging proof was rejected")
	}

	// The consoles render a login page to a credential-free profile, so the
	// sealed observations carry the same operator-provisioned session the
	// debug role uses. Missing or unreadable sessions degrade to none — the
	// observation then fails honestly on the login page.
	cookies, _ := visiblecheck.LoadSessionCookies(os.Getenv("LASSDAS_E2E_SESSION_FILE"))
	var evidence visiblecheck.Evidence
	var screenshot []byte
	if command.environment == "staging" {
		evidence, screenshot, err = visiblecheck.ObserveAndSealStaging(ctx, staging, input, cookies)
	} else {
		var production releaseproof.ProductionProof
		if err := worker.ReadJSONFile(command.productionProofPath, worker.MaxArtifactJSONBytes, &production); err != nil {
			return errors.New("browser production proof is invalid")
		}
		var prior visiblecheck.Evidence
		if err := worker.ReadJSONFile(command.priorEvidencePath, worker.MaxArtifactJSONBytes, &prior); err != nil {
			return errors.New("browser prior evidence is invalid")
		}
		priorScreenshot, readErr := readBoundedRegularFile(command.priorScreenshotPath, visiblecheck.MaxScreenshotBytes)
		if readErr != nil {
			return errors.New("browser prior screenshot is invalid")
		}
		evidence, screenshot, err = visiblecheck.ObserveAndSealProduction(
			ctx, production, staging, prior, priorScreenshot, input, cookies,
		)
	}
	if err != nil {
		return errors.New("browser evidence was rejected")
	}
	if err := writeScreenshotExclusive(command.screenshotOut, screenshot); err != nil {
		return errors.New("browser screenshot could not be written")
	}
	if err := worker.WriteJSONFileExclusive(command.evidenceOut, evidence, worker.MaxArtifactJSONBytes); err != nil {
		_ = os.Remove(command.screenshotOut)
		return errors.New("browser evidence could not be written")
	}
	return nil
}

func parseCommand(args []string) (commandConfig, error) {
	if len(args) < 2 || len(args)%2 != 0 || len(args) > 40 {
		return commandConfig{}, errors.New("browser arguments are invalid")
	}
	allowed := map[string]bool{
		"--config": false, "--ticket": false, "--source": false, "--candidate": false,
		"--decision": false, "--validation": false, "--review": true,
		"--staging-proof": false, "--production-proof": false,
		"--prior-evidence": false, "--prior-screenshot": false,
		"--environment": false, "--tool-sha": false,
		"--evidence-out": false, "--screenshot-out": false,
	}
	values := make(map[string][]string, len(allowed))
	for index := 0; index < len(args); index += 2 {
		name, value := args[index], args[index+1]
		repeated, exists := allowed[name]
		if !exists || value == "" || strings.ContainsAny(name+value, "\x00\r\n") || (!repeated && len(values[name]) != 0) {
			return commandConfig{}, errors.New("browser arguments are invalid")
		}
		for _, previous := range values[name] {
			if previous == value {
				return commandConfig{}, errors.New("browser arguments are invalid")
			}
		}
		values[name] = append(values[name], value)
	}
	common := []string{
		"--config", "--ticket", "--source", "--candidate", "--decision", "--validation",
		"--staging-proof", "--environment", "--tool-sha", "--evidence-out", "--screenshot-out",
	}
	for _, name := range common {
		if len(values[name]) != 1 {
			return commandConfig{}, errors.New("browser arguments are invalid")
		}
	}
	if len(values["--review"]) != 2 || !worker.ValidToolSHA(values["--tool-sha"][0]) {
		return commandConfig{}, errors.New("browser arguments are invalid")
	}
	environment := values["--environment"][0]
	productionNames := []string{"--production-proof", "--prior-evidence", "--prior-screenshot"}
	for _, name := range productionNames {
		expected := 0
		if environment == "production" {
			expected = 1
		}
		if len(values[name]) != expected {
			return commandConfig{}, errors.New("browser arguments are invalid")
		}
	}
	if environment != "staging" && environment != "production" {
		return commandConfig{}, errors.New("browser arguments are invalid")
	}
	pathNames := []string{
		"--config", "--ticket", "--source", "--candidate", "--decision", "--validation", "--review",
		"--staging-proof", "--production-proof", "--prior-evidence", "--prior-screenshot", "--evidence-out", "--screenshot-out",
	}
	seenPaths := make(map[string]struct{})
	for _, name := range pathNames {
		for _, value := range values[name] {
			if !validPathArgument(value) {
				return commandConfig{}, errors.New("browser arguments are invalid")
			}
			if _, duplicate := seenPaths[value]; duplicate {
				return commandConfig{}, errors.New("browser path arguments are invalid")
			}
			seenPaths[value] = struct{}{}
		}
	}
	one := func(name string) string {
		if len(values[name]) == 1 {
			return values[name][0]
		}
		return ""
	}
	return commandConfig{
		configPath: one("--config"), ticketPath: one("--ticket"), sourcePath: one("--source"),
		candidatePath: one("--candidate"), decisionPath: one("--decision"), validationPath: one("--validation"),
		reviewPaths: append([]string(nil), values["--review"]...), stagingProofPath: one("--staging-proof"),
		productionProofPath: one("--production-proof"), priorEvidencePath: one("--prior-evidence"),
		priorScreenshotPath: one("--prior-screenshot"), environment: environment, toolSHA: one("--tool-sha"),
		evidenceOut: one("--evidence-out"), screenshotOut: one("--screenshot-out"),
	}, nil
}

func validPathArgument(value string) bool {
	return value != "" && filepath.IsAbs(value) && filepath.Clean(value) == value && !strings.ContainsAny(value, "\x00\r\n")
}

func readBoundedRegularFile(filename string, maximum int) ([]byte, error) {
	if !validPathArgument(filename) || maximum < 1 {
		return nil, errors.New("input path is invalid")
	}
	entry, err := os.Lstat(filename)
	if err != nil || !entry.Mode().IsRegular() || entry.Mode()&os.ModeSymlink != 0 || entry.Size() < 1 || entry.Size() > int64(maximum) {
		return nil, errors.New("input file is invalid")
	}
	file, err := os.Open(filename)
	if err != nil {
		return nil, errors.New("input file is invalid")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(entry, opened) || opened.Size() != entry.Size() {
		return nil, errors.New("input file changed")
	}
	encoded, err := io.ReadAll(io.LimitReader(file, int64(maximum)+1))
	if err != nil || len(encoded) != int(entry.Size()) || len(encoded) > maximum {
		return nil, errors.New("input file is invalid")
	}
	return encoded, nil
}

func writeScreenshotExclusive(filename string, encoded []byte) error {
	if !validPathArgument(filename) || len(encoded) < 64 || len(encoded) > visiblecheck.MaxScreenshotBytes {
		return errors.New("screenshot output is invalid")
	}
	parent := filepath.Dir(filename)
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("screenshot directory is invalid")
	}
	file, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("screenshot output exists")
	}
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = os.Remove(filename)
		}
	}()
	if _, err := file.Write(encoded); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	keep = true
	return nil
}
