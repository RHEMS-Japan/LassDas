package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/worker"
)

// returnedPipeline is a run directory with a stand-in worker that records
// the arguments each verb was rendered with.
func returnedPipeline(t *testing.T) (*Pipeline, string) {
	t.Helper()
	record := filepath.Join(t.TempDir(), "argv.txt")
	script := filepath.Join(t.TempDir(), "worker")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+record+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	consumerPath := filepath.Join(workspace, "consumer.json")
	if err := os.WriteFile(consumerPath, []byte(`{"models":{"reviewers":[{"id":"review-a"},{"id":"review-b"}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config := runtime.Config{WorkerBin: script, ConsumerConfigPath: consumerPath}
	config.Identity.EngineSHA = strings.Repeat("ab", 20)
	return &Pipeline{Config: config, Workspace: workspace, Logger: trailTestLogger{}}, record
}

// The round that was handed back is rendered again carrying the engine's
// own answer — and it is this round's record, not the previous round's. A
// return does not start a new round, so the first round, which has no
// previous one at all, has to carry it too.
func TestAReturnedRoundsInstructionCarriesTheAnswerToThisRound(t *testing.T) {
	pipeline, record := returnedPipeline(t)
	answer := worker.AnswerReturn("鍵が渡されていません。", nil, time.Now().UTC())
	if err := RecordReturn(pipeline.Workspace, 1, answer); err != nil {
		t.Fatalf("RecordReturn: %v", err)
	}
	if err := pipeline.RenderImplementInstruction(context.Background(), 1); err != nil {
		t.Fatalf("RenderImplementInstruction: %v", err)
	}
	argv, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the worker was not run: %v", err)
	}
	want := "--returned " + ReturnRecordFile(pipeline.Workspace, 1)
	if !strings.Contains(string(argv), want) {
		t.Fatalf("the round was rendered again without %q:\n%s", want, argv)
	}
}

// A round nobody handed back is rendered exactly as it was before this
// record existed.
func TestARoundNobodyHandedBackCarriesNoAnswer(t *testing.T) {
	pipeline, record := returnedPipeline(t)
	if err := pipeline.RenderImplementInstruction(context.Background(), 1); err != nil {
		t.Fatalf("RenderImplementInstruction: %v", err)
	}
	argv, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the worker was not run: %v", err)
	}
	if strings.Contains(string(argv), "--returned") {
		t.Fatalf("an instruction carried an answer to nothing:\n%s", argv)
	}
}

// Every answer is kept, oldest first, and reading one back gives the one
// the round is running under now.
func TestEachAnswerIsAddedToTheRoundsRecord(t *testing.T) {
	pipeline, _ := returnedPipeline(t)
	const report = "この依頼は、いまのままでは実現できません。"
	for attempt := 1; attempt <= 2; attempt++ {
		previous, err := ReadReturns(pipeline.Workspace, 1)
		if err != nil {
			t.Fatalf("ReadReturns: %v", err)
		}
		if err := RecordReturn(pipeline.Workspace, 1, worker.AnswerReturn(report, previous, time.Now().UTC())); err != nil {
			t.Fatalf("RecordReturn: %v", err)
		}
	}
	record, err := ReadReturns(pipeline.Workspace, 1)
	if err != nil || record == nil {
		t.Fatalf("the round's record = %+v %v", record, err)
	}
	if len(record.Returns) != 2 {
		t.Fatalf("the record holds %d returns, want both", len(record.Returns))
	}
	if latest := record.Latest(); latest == nil || latest.Attempt != 2 || !latest.Repeated {
		t.Fatalf("the newest answer = %+v, want the second and a repeat", record.Latest())
	}
}
