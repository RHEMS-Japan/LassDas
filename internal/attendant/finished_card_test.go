package attendant

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/boardack"
	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
	"automation.internal/ticket-ingress/internal/ticketview"
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
	if !ticketview.IsFinished(closed.Step) {
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

// A card nobody has cleared away is never left off the board, however many
// there are. The snapshot used to cap the finished rows at thirty, so past
// that the oldest vanished with nobody having looked at them — the cards
// furthest behind being exactly the ones to disappear — and the count of
// what was waiting stopped at the cap and stayed there.
func TestNoUnclearedCardIsEverLeftOffTheBoard(t *testing.T) {
	const many = 35
	runs := make([]state.RunOverview, 0, many)
	for i := range many {
		runs = append(runs, state.RunOverview{
			DeliveryID: fmt.Sprintf("delivery_%02d", many-i), State: "terminal",
			TerminalCode: string(hook.TerminalSuccess), ClaimedAt: int64(many - i),
		})
	}
	none := map[string]boardack.Entry{}

	// Past twice the display limit is where the reading used to stop, so
	// the check has to reach past it or it proves nothing.
	deep := make([]state.RunOverview, 0, 2*terminalSnapshotLimit+5)
	for i := range 2*terminalSnapshotLimit + 5 {
		deep = append(deep, state.RunOverview{
			DeliveryID: fmt.Sprintf("deep_%03d", i), State: "terminal",
			TerminalCode: string(hook.TerminalSuccess), ClaimedAt: int64(i),
		})
	}
	if kept := worthClassifying(slices.Clone(deep), none); len(kept) != len(deep) {
		t.Fatalf("%d of %d finished runs were read; every one a person has not cleared is read", len(kept), len(deep))
	}
	rows := make([]RunStatus, 0, many)
	for _, run := range runs {
		rows = append(rows, RunStatus{DeliveryID: run.DeliveryID, Step: "done"})
	}
	if shown := trimClearedRows(slices.Clone(rows), none); len(shown) != many {
		t.Fatalf("%d of %d finished cards are shown; every one a person has not cleared is shown", len(shown), many)
	}
}

// The bound still holds where it may: the cards a person has already dealt
// with, oldest first.
func TestClearedCardsAreCappedOldestFirst(t *testing.T) {
	const many = terminalSnapshotLimit + 5
	cleared := map[string]boardack.Entry{}
	rows := make([]RunStatus, 0, many)
	// Newest first, the order the snapshot is in by the time it is trimmed.
	for i := range many {
		id := fmt.Sprintf("delivery_%02d", many-i)
		cleared[id] = boardack.Entry{At: time.Now().UTC()}
		rows = append(rows, RunStatus{DeliveryID: id, Step: "done"})
	}
	shown := trimClearedRows(slices.Clone(rows), cleared)
	if len(shown) != terminalSnapshotLimit {
		t.Fatalf("%d cleared cards shown, want %d", len(shown), terminalSnapshotLimit)
	}
	if shown[0].DeliveryID != rows[0].DeliveryID {
		t.Fatalf("the newest cleared card went: %q", shown[0].DeliveryID)
	}
	for _, row := range shown {
		if row.DeliveryID == rows[len(rows)-1].DeliveryID {
			t.Fatal("the oldest cleared card was kept over a newer one")
		}
	}
	// A run still going is never trimmed, cleared or not: only a finished
	// card can have been dealt with.
	live := []RunStatus{{DeliveryID: "delivery_01", Step: "implement"}}
	if got := trimClearedRows(live, cleared); len(got) != 1 {
		t.Fatal("a running card was trimmed as if somebody had cleared it")
	}
	// The pre-classification cap follows the same rule, at twice the depth.
	terminal := make([]state.RunOverview, 0, 2*terminalSnapshotLimit+5)
	clearedAll := map[string]boardack.Entry{}
	for i := range 2*terminalSnapshotLimit + 5 {
		id := fmt.Sprintf("cleared_%03d", i)
		clearedAll[id] = boardack.Entry{At: time.Now().UTC()}
		terminal = append(terminal, state.RunOverview{DeliveryID: id, State: "terminal"})
	}
	if kept := worthClassifying(terminal, clearedAll); len(kept) != 2*terminalSnapshotLimit {
		t.Fatalf("%d cleared runs read, want %d", len(kept), 2*terminalSnapshotLimit)
	}
}

// A row still going carries no times at all. A zero time.Time is not an
// empty one, so omitempty never dropped it and every running card on the
// board travelled with a finish time of 0001-01-01 on it.
func TestARunningRowCarriesNoZeroTimes(t *testing.T) {
	encoded, err := json.Marshal(RunStatus{DeliveryID: "delivery_live", State: "claimed", Step: "implement"})
	if err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	if err := json.Unmarshal(encoded, &row); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"finished_at", "report_at"} {
		if value, present := row[field]; present {
			t.Fatalf("a running row carries %s = %v: %s", field, value, encoded)
		}
	}
	// A row that does have one still carries it.
	ended := time.Date(2026, 9, 25, 2, 0, 0, 0, time.UTC)
	encoded, err = json.Marshal(RunStatus{DeliveryID: "delivery_done", Step: "done", FinishedAt: ended})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"finished_at":"2026-09-25T02:00:00Z"`) {
		t.Fatalf("a finished row lost its time: %s", encoded)
	}
}

// The board's record of what a person cleared away is read where the rows
// are built, and every decision it makes is exercised here without a ledger
// or a card wall behind it. Read outside this function, the wiring was the
// one part with no test at all: the read could disappear and every test
// still passed, while the snapshot grew without bound.
func TestTheRowsHonourTheRecordOfWhatWasClearedAway(t *testing.T) {
	config := runtime.Config{Chain: runtime.ChainConfig{RunsRoot: t.TempDir()}}
	statusDir := t.TempDir()
	// More cleared runs than the reading cap, so both filters have to look
	// at the record to arrive at the right number.
	const clearedCount = 2*terminalSnapshotLimit + 5
	const waitingCount = 5
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	record := map[string]boardack.Entry{}
	runs := []state.RunOverview{}
	terminal := func(id string, claimed int64) state.RunOverview {
		return state.RunOverview{
			DeliveryID: id, State: "terminal",
			TerminalCode: string(hook.TerminalSuccess), ClaimedAt: claimed,
		}
	}
	for i := range clearedCount {
		id := fmt.Sprintf("cleared_%03d", i)
		record[id] = boardack.Entry{At: start.Add(time.Duration(i) * time.Minute), User: "someone"}
		runs = append(runs, terminal(id, int64(i)))
	}
	for i := range waitingCount {
		runs = append(runs, terminal(fmt.Sprintf("waiting_%03d", i), int64(clearedCount+i)))
	}
	if err := boardack.Write(statusDir, record); err != nil {
		t.Fatal(err)
	}

	rows := boardRows(config, statusDir, slices.Clone(runs), nil)

	shown := map[string]bool{}
	clearedShown := 0
	for _, row := range rows {
		if !ticketview.IsFinished(row.Step) {
			t.Fatalf("%s is not a finished row: %q", row.DeliveryID, row.Step)
		}
		shown[row.DeliveryID] = true
		if strings.HasPrefix(row.DeliveryID, "cleared_") {
			clearedShown++
		}
	}
	// The record was read: without it nothing would have been capped.
	if clearedShown != terminalSnapshotLimit {
		t.Fatalf("%d cleared rows shown, want %d — the record of what was cleared away was not honoured", clearedShown, terminalSnapshotLimit)
	}
	// And what it says about the other cards is that they stay.
	for i := range waitingCount {
		if id := fmt.Sprintf("waiting_%03d", i); !shown[id] {
			t.Fatalf("%s was dropped although nobody cleared it away", id)
		}
	}
	// Oldest cleared first: the newest cleared card is kept, the oldest is not.
	if !shown[fmt.Sprintf("cleared_%03d", clearedCount-1)] {
		t.Fatal("the most recently cleared card went")
	}
	if shown["cleared_000"] {
		t.Fatal("the longest-cleared card was kept over a newer one")
	}
	if len(rows) != terminalSnapshotLimit+waitingCount {
		t.Fatalf("%d rows, want %d", len(rows), terminalSnapshotLimit+waitingCount)
	}

	// With no record at all, nothing has been cleared away and every card
	// is on the board, however many there are.
	none := boardRows(config, t.TempDir(), slices.Clone(runs), nil)
	if len(none) != len(runs) {
		t.Fatalf("%d rows without a record, want all %d", len(none), len(runs))
	}
}
