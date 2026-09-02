package attendant

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
)

// The board never decides anything, but it must not lie: each ledger/card/
// artifact constellation the sync machinery can produce maps to exactly one
// requester-facing step.
func TestClassifyRunNamesEveryPipelinePosition(t *testing.T) {
	root := t.TempDir()
	config := runtime.Config{}
	config.Chain.RunsRoot = root
	const delivery = "TKT-900:7"
	writeReport := func(t *testing.T, name, body string) {
		t.Helper()
		dir := filepath.Join(root, delivery)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	chainCard := func(stage string, round int, status string) runtime.BoardTask {
		return runtime.BoardTask{Status: status, IdempotencyKey: runtime.ChainCardKey(delivery, stage, round)}
	}
	deliverTask := func(stage, status string) runtime.BoardTask {
		return runtime.BoardTask{Status: status, IdempotencyKey: deliverCardKey(delivery, stage)}
	}
	cases := map[string]struct {
		run      state.RunOverview
		tasks    []runtime.BoardTask
		artifact map[string]string
		wantStep string
	}{
		"queued":            {run: state.RunOverview{State: "queued"}, wantStep: "intake"},
		"question":          {run: state.RunOverview{State: "awaiting_answer"}, wantStep: "question"},
		"claimed no chain":  {run: state.RunOverview{State: "claimed"}, wantStep: "intake"},
		"implement running": {run: state.RunOverview{State: "claimed"}, tasks: []runtime.BoardTask{chainCard("validate", 1, "done"), chainCard("publish", 1, "running")}, wantStep: "implement"},
		"review running":    {run: state.RunOverview{State: "claimed"}, tasks: []runtime.BoardTask{chainCard("publish", 2, "done"), chainCard("review-a", 2, "running"), chainCard("review-b", 2, "pending")}, wantStep: "review"},
		"cancelled":         {run: state.RunOverview{State: "terminal", TerminalCode: "cancelled"}, wantStep: "stopped"},
		"failed terminal":   {run: state.RunOverview{State: "terminal", TerminalCode: "validation_failed"}, wantStep: "failed"},
		"checks waiting":    {run: state.RunOverview{State: "terminal", TerminalCode: "success"}, tasks: []runtime.BoardTask{deliverTask("checks", "running")}, wantStep: "checks"},
		"integrate running": {run: state.RunOverview{State: "terminal", TerminalCode: "success"}, tasks: []runtime.BoardTask{deliverTask("checks", "done"), deliverTask("integrate", "running")}, wantStep: "staging"},
		"go awaited": {run: state.RunOverview{State: "terminal", TerminalCode: "success"},
			artifact: map[string]string{"deliver-staging-report.json": `{"verdict":"pass"}`}, wantStep: "confirm"},
		"promotion held": {run: state.RunOverview{State: "terminal", TerminalCode: "success"},
			artifact: map[string]string{"deliver-staging-report.json": `{"verdict":"pass","promotion_hold":"滞留"}`}, wantStep: "done"},
		"promoting": {run: state.RunOverview{State: "terminal", TerminalCode: "success"},
			tasks:    []runtime.BoardTask{deliverTask("promote", "running")},
			artifact: map[string]string{"deliver-staging-report.json": `{"verdict":"pass"}`}, wantStep: "production"},
		"production passed": {run: state.RunOverview{State: "terminal", TerminalCode: "success"},
			artifact: map[string]string{"deliver-production-report.json": `{"verdict":"pass"}`}, wantStep: "done"},
		"plain success": {run: state.RunOverview{State: "terminal", TerminalCode: "success"}, wantStep: "done"},
		// The posted-outcome seal outranks a stale staging-pass file: these
		// endings never reach an artifact and used to freeze the board on
		// "waiting for Go" (audit findings 1-1..1-3).
		"sealed expired outranks staged pass": {run: state.RunOverview{State: "terminal", TerminalCode: "success"},
			artifact: map[string]string{
				"deliver-staging-report.json": `{"verdict":"pass"}`,
				"board-outcome.json":          `{"phase":"release","verdict":"expired","at":"2026-09-01T00:00:00Z"}`,
			}, wantStep: "done"},
		"sealed stop before staging": {run: state.RunOverview{State: "terminal", TerminalCode: "success"},
			artifact: map[string]string{"board-outcome.json": `{"phase":"staging","verdict":"stopped","at":"2026-09-01T00:00:00Z"}`},
			wantStep: "stopped"},
		"sealed dead promote needs operator": {run: state.RunOverview{State: "terminal", TerminalCode: "success"},
			artifact: map[string]string{
				"deliver-staging-report.json": `{"verdict":"pass"}`,
				"board-outcome.json":          `{"phase":"release","verdict":"deploy_failed","at":"2026-09-01T00:00:00Z"}`,
			}, wantStep: "attention"},
		"staging observe failed is not a deploy failure": {run: state.RunOverview{State: "terminal", TerminalCode: "success"},
			artifact: map[string]string{"deliver-staging-report.json": `{"verdict":"observe_failed"}`}, wantStep: "failed"},
		"triage card is a human lane": {run: state.RunOverview{State: "claimed"},
			tasks: []runtime.BoardTask{chainCard("publish", 1, "triage")}, wantStep: "attention"},
		"production report beats staging seal": {run: state.RunOverview{State: "terminal", TerminalCode: "success"},
			artifact: map[string]string{
				"board-outcome.json":             `{"phase":"staging","verdict":"pass","at":"2026-09-01T00:00:00Z"}`,
				"deliver-production-report.json": `{"verdict":"pass"}`,
			}, wantStep: "done"},
		"report pending is not intake": {run: state.RunOverview{State: "terminal_report_pending"}, wantStep: "reporting"},
		"sealed stop during go wait": {run: state.RunOverview{State: "terminal", TerminalCode: "success"},
			artifact: map[string]string{
				"deliver-staging-report.json": `{"verdict":"pass"}`,
				"board-outcome.json":          `{"phase":"release","verdict":"stopped","at":"2026-09-01T00:00:00Z"}`,
			}, wantStep: "stopped"},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			if err := os.RemoveAll(filepath.Join(root, delivery)); err != nil {
				t.Fatal(err)
			}
			for file, body := range testCase.artifact {
				writeReport(t, file, body)
			}
			run := testCase.run
			run.DeliveryID = delivery
			got := classifyRun(config, run, testCase.tasks)
			if got.Step != testCase.wantStep {
				t.Fatalf("classifyRun() step = %q (%q), want %q", got.Step, got.StepTitle, testCase.wantStep)
			}
			if got.StepTitle == "" {
				t.Fatal("classifyRun() left the step title empty")
			}
		})
	}
}

// CanGo arms the board's Go button, so it must be true EXACTLY when the
// staging report comment is confirmed posted (sealed outcome): a
// file-only confirm can precede the post, and a Go written then is
// permanently ignored by the anchor rule.
func TestCanGoRequiresThePostedReport(t *testing.T) {
	root := t.TempDir()
	config := runtime.Config{}
	config.Chain.RunsRoot = root
	const delivery = "TKT-901:1"
	write := func(t *testing.T, name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, delivery), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, delivery, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run := state.RunOverview{DeliveryID: delivery, State: "terminal", TerminalCode: "success"}

	// File-only confirm: report written, post not yet confirmed.
	write(t, "deliver-staging-report.json", `{"verdict":"pass"}`)
	if got := classifyRun(config, run, nil); got.Step != "confirm" || got.CanGo {
		t.Fatalf("file-only: step=%q can_go=%v, want confirm without can_go", got.Step, got.CanGo)
	}
	// Sealed (posted) confirm arms the button.
	write(t, "board-outcome.json", `{"phase":"staging","verdict":"pass","at":"2026-09-01T00:00:00Z"}`)
	if got := classifyRun(config, run, nil); got.Step != "confirm" || !got.CanGo {
		t.Fatalf("sealed: step=%q can_go=%v, want confirm with can_go", got.Step, got.CanGo)
	}
	// A held pass never arms it.
	write(t, "board-outcome.json", `{"phase":"staging","verdict":"pass","note":"滞留","at":"2026-09-01T00:00:00Z"}`)
	if got := classifyRun(config, run, nil); got.CanGo {
		t.Fatalf("held: can_go=%v, want false", got.CanGo)
	}
}

// Events are the diff between consecutive snapshots: same step, no line;
// moved step, one line; and the snapshot file itself is replaced atomically.
func TestWriteBoardStatusAppendsOneEventPerMovedDelivery(t *testing.T) {
	dir := t.TempDir()
	first := BoardSnapshot{SchemaVersion: 1, GeneratedAt: time.Unix(1000, 0).UTC(), Runs: []RunStatus{
		{DeliveryID: "a", IssueKey: "TKT-1", Step: "implement", StepTitle: "実装・検証中"},
		{DeliveryID: "b", IssueKey: "TKT-2", Step: "checks", StepTitle: "自動検査 (CI) 待ち"},
	}}
	if err := WriteBoardStatus(dir, first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.GeneratedAt = time.Unix(2000, 0).UTC()
	second.Runs = []RunStatus{
		{DeliveryID: "a", IssueKey: "TKT-1", Step: "review", StepTitle: "レビュー中"},
		{DeliveryID: "b", IssueKey: "TKT-2", Step: "checks", StepTitle: "自動検査 (CI) 待ち"},
	}
	if err := WriteBoardStatus(dir, second); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	// First write: two appearances. Second write: only "a" moved.
	if len(lines) != 3 {
		t.Fatalf("events.jsonl has %d lines, want 3: %s", len(lines), raw)
	}
	var last StepEvent
	if err := json.Unmarshal([]byte(lines[2]), &last); err != nil {
		t.Fatal(err)
	}
	if last.DeliveryID != "a" || last.Step != "review" {
		t.Fatalf("last event = %+v, want delivery a moving to review", last)
	}
	board, err := os.ReadFile(filepath.Join(dir, "board.json"))
	if err != nil {
		t.Fatal(err)
	}
	var persisted BoardSnapshot
	if err := json.Unmarshal(board, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted.Runs) != 2 || persisted.Runs[0].Step != "review" {
		t.Fatalf("board.json = %+v, want the second snapshot", persisted)
	}
}
