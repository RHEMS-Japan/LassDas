package main

import (
	"context"
	"errors"
	"os"

	"automation.internal/ticket-ingress/internal/worker"
)

// maxAnswerBodyFileBytes bounds the comment read from disk. A requester's
// comment is already bounded where it is fetched; this is the second bound,
// on the path a file takes into a prompt.
const maxAnswerBodyFileBytes = 1 << 16

// runReadAnswer reads one comment a requester left and writes what it means
// for the questions that were asked.
//
// The comment and the questions travel as files, never as arguments: the
// comment is whatever a person typed, and nothing a person typed belongs on
// a command line.
func runReadAnswer(ctx context.Context, args []string) error {
	flags := commandFlags("read-answer")
	configPath := flags.String("config", "", "")
	questionsPath := flags.String("questions", "", "")
	bodyPath := flags.String("body", "", "")
	outputPath := flags.String("out", "", "")
	if !parseFlags(flags, args) || !allPresent(*configPath, *questionsPath, *bodyPath, *outputPath) {
		return errors.New("read-answer arguments are invalid")
	}
	config, err := readConfig(*configPath)
	if err != nil {
		return err
	}
	questions, err := os.ReadFile(*questionsPath)
	if err != nil || len(questions) == 0 || int64(len(questions)) > worker.MaxReadinessJSONBytes {
		return errors.New("the questions could not be read")
	}
	body, err := os.ReadFile(*bodyPath)
	if err != nil || len(body) == 0 || len(body) > maxAnswerBodyFileBytes {
		return errors.New("the comment could not be read")
	}
	endpoint := config.Models.Readiness.Assessor
	invoker, err := newModelInvoker(ctx, endpoint)
	if err != nil {
		return err
	}
	reading, usage, err := invoker.ReadAnswer(ctx, endpoint, string(questions), string(body))
	if err != nil {
		return err
	}
	record := struct {
		worker.AnswerReading
		Usage worker.InvocationUsage `json:"usage"`
	}{AnswerReading: reading, Usage: usage}
	if err := worker.WriteJSONFileExclusive(*outputPath, record, maxAnswerBodyFileBytes); err != nil {
		return errors.New("the reading could not be written")
	}
	return nil
}
