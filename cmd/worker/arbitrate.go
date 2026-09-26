package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"automation.internal/ticket-ingress/internal/worker"
)

// runArbitrate rules on a round that has stopped moving.
//
// The verb is shaped like the other stage verbs on purpose: it takes the
// round's sealed records, writes one more sealed record beside them, and
// decides nothing that is not derivable from what it was given. What it
// produces is read twice — by the decide verb, which counts the verdict
// again without the objections the ruling set aside, and by the next round's
// instruction, which carries what the ruling told the implementer.
func runArbitrate(ctx context.Context, args []string) error {
	flags := commandFlags("arbitrate")
	configPath := flags.String("config", "", "")
	toolSHA := flags.String("tool-sha", "", "")
	ticketPath := flags.String("ticket", "", "")
	sourcePath := flags.String("source", "", "")
	candidatePath := flags.String("candidate", "", "")
	clarificationPath := flags.String("clarification", "", "")
	validationFailurePath := flags.String("validation-failure", "", "")
	historyDir := flags.String("history", "", "")
	repoRoot := flags.String("repo-root", "", "")
	outputPath := flags.String("out", "", "")
	var reviewPaths stringList
	flags.Var(&reviewPaths, "review", "")
	if !parseFlags(flags, args) || !allPresent(*configPath, *toolSHA, *ticketPath, *sourcePath, *candidatePath, *outputPath) ||
		!worker.ValidToolSHA(*toolSHA) || len(reviewPaths) == 0 {
		return errors.New("arbitrate arguments are invalid")
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
	clarification, err := readClarificationContext(*clarificationPath)
	if err != nil {
		return err
	}
	// Given for the deadlock that has no objections in it: the seats passed
	// the change and the destination's own commands refused it, and what
	// those commands printed is the whole of what the arbiter has to read.
	refused, err := readValidationFailure(*validationFailurePath)
	if err != nil {
		return err
	}
	history, err := worker.LoadArbitrationHistory(*historyDir, candidate.Stage, request, config)
	if err != nil {
		return err
	}
	invoker, err := newModelInvoker(ctx, config.Models.ArbiterEndpoint())
	if err != nil {
		return err
	}
	var ruling worker.Ruling
	if *repoRoot == "" {
		ruling, err = invoker.Arbitrate(ctx, candidate, reviews, clarification, refused, source, request, config, time.Now().UTC(), history)
	} else {
		repository := worker.ArbitrationRepository{Root: *repoRoot, RecordsPath: filepath.Join(filepath.Dir(*outputPath), "arbitration-measurements.jsonl")}
		ruling, err = invoker.ArbitrateWithRepository(ctx, candidate, reviews, clarification, refused, source, request, config, time.Now().UTC(), repository, history)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "worker: %s: %v\n", "arbitration failed", err)
		return errors.New("arbitration failed")
	}
	if err := worker.WriteJSONFileExclusive(*outputPath, ruling, worker.MaxReviewJSONBytes); err != nil {
		return errors.New("ruling artifact could not be written")
	}
	return nil
}

// readRuling loads the ruling the decide verb is to count the verdict under,
// or nil when the flag was not given — which is every round nobody had to
// rule on, and so nearly all of them.
//
// A path that was given and cannot be read is a failure rather than an
// absence. The round is being decided again because of what is in that file;
// deciding without it would seal the same verdict that made the ruling
// necessary.
func readRuling(path string) (*worker.Ruling, error) {
	if path == "" {
		return nil, nil
	}
	var ruling worker.Ruling
	if err := worker.ReadJSONFile(path, worker.MaxReviewJSONBytes, &ruling); err != nil {
		return nil, errors.New("the round's ruling could not be read")
	}
	return &ruling, nil
}
