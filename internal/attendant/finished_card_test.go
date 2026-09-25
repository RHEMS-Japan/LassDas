package attendant

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
)

// A card that has stopped for good says WHEN it got there. Finished cards
// used to leave the running lane by themselves and say only what they had
// ended as, so a reader coming back to the board could not tell a run that
// ended minutes ago from one that ended the day before.
func TestAFinishedCardSaysWhenItGotThere(t *testing.T) {
	config := runtime.Config{Chain: runtime.ChainConfig{RunsRoot: t.TempDir()}}
	ended := time.Date(2026, 9, 25, 2, 0, 0, 0, time.UTC)
	run := state.RunOverview{
		DeliveryID: "delivery_finished", State: "terminal",
		TerminalCode: string(hook.TerminalModelFailed), CompletedAt: ended.UnixMilli(),
	}
	if err := os.MkdirAll(runDirectory(config, run.DeliveryID), 0o755); err != nil {
		t.Fatal(err)
	}
	got := classifyRun(config, run, nil)
	if got.Step != "failed" {
		t.Fatalf("step = %q, want failed", got.Step)
	}
	if !got.FinishedAt.Equal(ended) {
		t.Fatalf("finished_at = %v, want the moment the run was recorded as finished (%v)", got.FinishedAt, ended)
	}

	// A run still going has no such moment to show, and must not borrow one.
	running := classifyRun(config, state.RunOverview{DeliveryID: "delivery_live", State: "claimed"}, nil)
	if !running.FinishedAt.IsZero() {
		t.Fatalf("a running card claims to have finished at %v", running.FinishedAt)
	}
}

// Where the delivery wrote its own report, that is when the card reached the
// state it now shows: it comes after the run's own ending, and it is the
// report that put the card there.
func TestTheDeliverysOwnReportTimeIsWhenTheCardGotThere(t *testing.T) {
	config := runtime.Config{Chain: runtime.ChainConfig{RunsRoot: t.TempDir()}}
	ledgerEnd := time.Date(2026, 9, 25, 2, 0, 0, 0, time.UTC)
	reported := time.Date(2026, 9, 25, 6, 30, 0, 0, time.UTC)
	run := state.RunOverview{
		DeliveryID: "delivery_reported", State: "terminal",
		TerminalCode: string(hook.TerminalSuccess), CompletedAt: ledgerEnd.UnixMilli(),
	}
	dir := runDirectory(config, run.DeliveryID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRunFile(t, dir, runner.DeliverProductionReportFile,
		`{"verdict":"pass","screen_checked":true,"observed_at":"`+reported.Format(time.RFC3339)+`","pull_request_url":"https://example.test/pull/1"}`)

	got := classifyRun(config, run, nil)
	if got.Step != "done" {
		t.Fatalf("step = %q %q, want done", got.Step, got.StepTitle)
	}
	if !got.FinishedAt.Equal(reported) {
		t.Fatalf("finished_at = %v, want the report's own time (%v)", got.FinishedAt, reported)
	}
}

// A pull request closed without merging is an ending of its own. Nothing
// about a finished run with a published pull request changes when a person
// closes it, so the card used to ask to be merged for as long as it was
// kept — asking for something that was never going to happen.
func TestAPullRequestClosedWithoutMergingIsItsOwnEnding(t *testing.T) {
	root := t.TempDir()
	config := mergeObservationConfig(t, root)
	runDir := seedDeliveredRun(t, config, "delivery_"+strings.Repeat("f", 32), recordedRunDigest)
	run := state.RunOverview{
		RunID: "TICKET-90", DeliveryID: filepath.Base(runDir), State: "terminal",
		TerminalCode: string(hook.TerminalSuccess),
	}

	// Before anything is read, the card is still asking for a merge.
	var waiting RunStatus
	classifyAfterTerminal(&waiting, config, run, nil)
	if waiting.Step != "confirm" || waiting.StepTitle != "マージ待ち" {
		t.Fatalf("before the reading: step = %q %q", waiting.Step, waiting.StepTitle)
	}

	writeClosedReader(t, config.ControllerBin, recordedRunDigest)
	before := time.Now().UTC()
	recordFeatureMerge(context.Background(), config, run, runDir, &recordingLogger{})

	merge, ended := readFeatureMerge(runDir)
	if !ended || merge.Merged || merge.State != "closed" {
		t.Fatalf("the closing was not written down: %+v ended=%v", merge, ended)
	}
	if merge.ReadAt.Before(before) {
		t.Fatalf("the closing carries no time: %v", merge.ReadAt)
	}

	var closed RunStatus
	classifyAfterTerminal(&closed, config, run, nil)
	if closed.Step != "stopped" || closed.StepTitle != "PR は取り込まれずに閉じられました" {
		t.Fatalf("after the reading: step = %q %q", closed.Step, closed.StepTitle)
	}
	if !FinishedStep(closed.Step) {
		t.Fatal("a closed pull request left the run in a state the board never clears")
	}
	if !closed.FinishedAt.Equal(merge.ReadAt) {
		t.Fatalf("finished_at = %v, want %v", closed.FinishedAt, merge.ReadAt)
	}
	if strings.Contains(closed.Detail, "マージされ、") {
		t.Fatalf("a closed pull request is described as merged: %q", closed.Detail)
	}

	// Read again and the recorded ending stands: it is never asked about
	// twice, and the time on the card does not drift forward.
	recordFeatureMerge(context.Background(), config, run, runDir, &recordingLogger{})
	again, _ := readFeatureMerge(runDir)
	if !again.ReadAt.Equal(merge.ReadAt) {
		t.Fatalf("the time moved on a second reading: %v then %v", merge.ReadAt, again.ReadAt)
	}
}

// A pull request that is simply still open is no ending at all: it is not
// written down, and the next wake-up asks again.
func TestAnOpenPullRequestIsNotAnEnding(t *testing.T) {
	root := t.TempDir()
	config := mergeObservationConfig(t, root)
	runDir := seedDeliveredRun(t, config, "delivery_"+strings.Repeat("9", 32), recordedRunDigest)
	writeReaderAnswering(t, config.ControllerBin, recordedRunDigest, `{"state":"open","merged":false}`)

	recordFeatureMerge(context.Background(), config, state.RunOverview{RunID: "TICKET-91"}, runDir, &recordingLogger{})

	if _, ended := readFeatureMerge(runDir); ended {
		t.Fatal("an open pull request was written down as an ending")
	}
	if _, err := os.Stat(filepath.Join(runDir, featureMergeFile)); !os.IsNotExist(err) {
		t.Fatalf("an open pull request left a record: %v", err)
	}
}

// writeClosedReader stands in for the delivery binary's read-merged verb
// answering about a pull request somebody closed without merging.
func writeClosedReader(t *testing.T, path, accepts string) {
	t.Helper()
	writeReaderAnswering(t, path, accepts, `{"state":"closed","merged":false,"merge_commit_sha":""}`)
}

// writeReaderAnswering is writeFakeReader with the answer chosen by the
// caller, so one shape of reading can be told from another.
func writeReaderAnswering(t *testing.T, path, accepts, answer string) {
	t.Helper()
	script := "#!/bin/sh\n" +
		"recorded=''\n" +
		"out=''\n" +
		"while [ $# -gt 0 ]; do\n" +
		"  case \"$1\" in\n" +
		"    --config-sha256) recorded=\"$2\"; shift 2 ;;\n" +
		"    --out) out=\"$2\"; shift 2 ;;\n" +
		"    *) shift ;;\n" +
		"  esac\n" +
		"done\n" +
		"if [ \"$recorded\" != '" + accepts + "' ]; then\n" +
		"  echo 'controller: ticket_artifact_invalid' >&2\n" +
		"  exit 1\n" +
		"fi\n" +
		"printf '%s' '" + answer + "' > \"$out\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}
