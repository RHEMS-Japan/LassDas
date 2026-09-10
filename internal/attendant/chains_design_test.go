package attendant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/backlog"
	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
)

func TestChainViewForSeparatesDesignRounds(t *testing.T) {
	tasks := []runtime.BoardTask{
		{ID: "t1", Status: "done", IdempotencyKey: "delivery-1:investigate:d1"},
		{ID: "t2", Status: "done", IdempotencyKey: "delivery-1:design-review-a:d1"},
		{ID: "t3", Status: "failed", IdempotencyKey: "delivery-1:design-decide:d1"},
		{ID: "t4", Status: "todo", IdempotencyKey: "delivery-1:investigate:d2"},
		{ID: "t5", Status: "todo", IdempotencyKey: "delivery-1:apply:r1"},
		{ID: "t6", Status: "todo", IdempotencyKey: "delivery-1:review-a:r1"},
		{ID: "t7", Status: "archived", IdempotencyKey: "delivery-1:publish:r1"},
		{ID: "t8", Status: "todo", IdempotencyKey: "delivery-2:investigate:d1"},
	}
	view := chainViewFor(tasks, "delivery-1")
	if view.designRound != 2 || view.round != 1 || !view.hasChain() {
		t.Fatalf("rounds: design %d implement %d", view.designRound, view.round)
	}
	if len(view.designCards) != 1 || view.designCards[runtime.StageInvestigate].ID != "t4" {
		t.Errorf("design cards of the newest round: %+v", view.designCards)
	}
	if len(view.cards) != 2 || view.cards[runtime.StageApply].ID != "t5" {
		t.Errorf("implementation cards: %+v", view.cards)
	}
	if len(view.all) != 6 {
		t.Errorf("live cards = %d", len(view.all))
	}
	keys := view.existingKeys("delivery-1")
	for _, want := range []string{"delivery-1:investigate:d2", "delivery-1:apply:r1", "delivery-1:review-a:r1"} {
		if _, ok := keys[want]; !ok {
			t.Errorf("existing keys lack %s: %v", want, keys)
		}
	}
	if task, ok := view.card(runtime.StageInvestigate); !ok || task.ID != "t4" {
		t.Errorf("card(investigate) = %+v %v", task, ok)
	}
	if rounds := view.rounds(); rounds.Design != 2 || rounds.Implement != 1 {
		t.Errorf("rounds() = %+v", rounds)
	}
	if chainViewFor(nil, "delivery-1").hasChain() {
		t.Error("empty board has a chain")
	}
}

func TestConsumerDesignMaxRoundsReadsTheLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "consumer.json")
	if err := os.WriteFile(path, []byte(`{"max_stages":3,"design_max_rounds":2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := consumerDesignMaxRounds(path); got != 2 {
		t.Errorf("limit = %d", got)
	}
	if err := os.WriteFile(path, []byte(`{"max_stages":3}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := consumerDesignMaxRounds(path); got != defaultDesignMaxRounds {
		t.Errorf("default = %d", got)
	}
	if got := consumerDesignMaxRounds(filepath.Join(t.TempDir(), "missing.json")); got != defaultDesignMaxRounds {
		t.Errorf("missing file = %d", got)
	}
}

func TestDesignObjectionRecorded(t *testing.T) {
	runDir := t.TempDir()
	if objected, _ := designObjectionRecorded(runDir, 1); objected {
		t.Error("objection reported with no record")
	}
	if err := os.MkdirAll(filepath.Join(runDir, "history", "design-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "history", "design-1", "objection.json"), []byte(`{"reason":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if objected, _ := designObjectionRecorded(runDir, 1); !objected {
		t.Error("sealed objection not seen")
	}
	if objected, _ := designObjectionRecorded(runDir, 2); objected {
		t.Error("objection of another round reported")
	}
	if err := os.MkdirAll(filepath.Join(runDir, "history", "stage-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"review-a.json": `{"verdict":"revise","findings":[{"code":"style","message":"a smaller thing"}]}`,
		"review-b.json": `{"verdict":"revise","findings":[{"code":"design-wrong","message":"the cause is elsewhere"}]}`,
	} {
		if err := os.WriteFile(filepath.Join(runDir, "history", "stage-1", name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The signal is read from whichever reviewer carries it, so the walk
	// must not stop at the first record that does not.
	flagged, err := reviewsFlagDesignWrong(runDir, 1, []string{"review-a", "review-b"})
	if err != nil || !flagged {
		t.Errorf("design-wrong finding not read from the sealed reviews: %v, %v", flagged, err)
	}
	// Another round's records are not this round's answer. Asking about a
	// round whose reviews were never sealed is a reason to stop, not a no:
	// the caller only ever asks about a round that sealed a revise decision,
	// which proves the records were there (review of #123).
	if flagged, err := reviewsFlagDesignWrong(runDir, 2, []string{"review-a", "review-b"}); err == nil || flagged {
		t.Errorf("another round's absent reviews were read as an answer: %v, %v", flagged, err)
	}
}

// A sealed review that cannot be read is not a review that found nothing,
// and it is not a judgement about the design either: it is a reason to stop.
// Read as "no design-wrong", it sent the delivery back to the applier with a
// design a reviewer may have called wrong, and wrote nothing anywhere
// (audit, 2026-09-09).
func TestSealedReviewsThatCannotBeReadAreAReasonToStop(t *testing.T) {
	place := func(t *testing.T, write func(dir string)) string {
		t.Helper()
		runDir := t.TempDir()
		dir := filepath.Join(runDir, "history", "stage-1")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		write(dir)
		return runDir
	}
	cases := map[string]func(dir string){
		"truncated": func(dir string) { writeReview(t, dir, `{"findings":[`) },
		"empty":     func(dir string) { writeReview(t, dir, ``) },
		"not json":  func(dir string) { writeReview(t, dir, `not json at all`) },
		// Opens as a directory rather than a file.
		"a directory in its place": func(dir string) {
			if err := os.MkdirAll(filepath.Join(dir, "review-a.json"), 0o755); err != nil {
				t.Fatal(err)
			}
		},
		// Reaching this point proves every review was there and parsed, so
		// one that is now missing was removed after — as a dangling symlink
		// is, and as a renamed reviewer id makes every record look.
		"missing": func(string) {},
		"a dangling symlink": func(dir string) {
			if err := os.Symlink(filepath.Join(dir, "nowhere.json"), filepath.Join(dir, "review-a.json")); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, write := range cases {
		runDir := place(t, write)
		flagged, err := reviewsFlagDesignWrong(runDir, 1, []string{"review-a", "review-b"})
		if err == nil {
			t.Errorf("%s: gave no reason to record", name)
		}
		if flagged {
			t.Errorf("%s: answered a question about the design", name)
		}
	}
}

// A configuration that names no reviewer reads every review record as
// nothing at all. It is the same silent no the rest of this closes.
func TestAConfigurationWithNoReviewerIsAReasonToStop(t *testing.T) {
	runDir := t.TempDir()
	dir := filepath.Join(runDir, "history", "stage-1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeReview(t, dir, `{"verdict":"revise","findings":[{"code":"design-wrong"}]}`)
	flagged, err := reviewsFlagDesignWrong(runDir, 1, nil)
	if err == nil || flagged {
		t.Fatalf("no configured reviewer: %v, %v", flagged, err)
	}
}

func writeReview(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "review-a.json"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPlaceDesignStageShowsInvestigationAndDesign(t *testing.T) {
	view := chainViewFor([]runtime.BoardTask{
		{ID: "t1", Status: "in_progress", IdempotencyKey: "delivery-1:investigate:d1"},
		{ID: "t2", Status: "todo", IdempotencyKey: "delivery-1:design-review-a:d1"},
	}, "delivery-1")
	var status RunStatus
	if !placeDesignStage(&status, view) || status.Step != "investigate" {
		t.Errorf("investigating: %+v", status)
	}
	view = chainViewFor([]runtime.BoardTask{
		{ID: "t1", Status: "done", IdempotencyKey: "delivery-1:investigate:d1"},
		{ID: "t2", Status: "in_progress", IdempotencyKey: "delivery-1:design-review-a:d1"},
	}, "delivery-1")
	status = RunStatus{}
	if !placeDesignStage(&status, view) || status.Step != "design" {
		t.Errorf("designing: %+v", status)
	}
	view = chainViewFor([]runtime.BoardTask{
		{ID: "t1", Status: "done", IdempotencyKey: "delivery-1:investigate:d1"},
		{ID: "t2", Status: "done", IdempotencyKey: "delivery-1:design-decide:d1"},
		{ID: "t3", Status: "todo", IdempotencyKey: "delivery-1:apply:r1"},
	}, "delivery-1")
	status = RunStatus{}
	if placeDesignStage(&status, view) {
		t.Error("a finished design round still shows as design")
	}
	// A revision archives the old implementation cards one at a time.
	// A leftover publication card is not evidence that publication started.
	for _, tail := range [][]runtime.BoardTask{
		{{ID: "publish", Status: "todo", IdempotencyKey: "delivery-1:publish:r1"}},
		nil,
	} {
		cards := append([]runtime.BoardTask{
			{ID: "investigate", Status: "done", IdempotencyKey: "delivery-1:investigate:d1"},
			{ID: "decide", Status: "done", IdempotencyKey: "delivery-1:design-decide:d1"},
		}, tail...)
		status = RunStatus{}
		if !placeDesignStage(&status, chainViewFor(cards, "delivery-1")) || status.Step != "design" || status.StepTitle != "次の工程を準備中" {
			t.Errorf("design transition with tail %+v: %+v", tail, status)
		}
	}
	if placeDesignStage(&status, chainViewFor(nil, "delivery-1")) {
		t.Error("no design round shows as design")
	}
}

// --- fake board for the design-round transitions -------------------------

type recordingLogger struct{ lines []string }

func (l *recordingLogger) Info(msg string, kv ...any) {
	l.lines = append(l.lines, "INFO "+msg+fmt.Sprint(kv...))
}
func (l *recordingLogger) Error(msg string, kv ...any) {
	l.lines = append(l.lines, "ERROR "+msg+fmt.Sprint(kv...))
}

// fakeBoard is a stand-in Hermes CLI that records every call and answers
// create with an incrementing id, like the runtime package's own stub.
func fakeBoard(t *testing.T) (*runtime.Hermes, string) {
	t.Helper()
	dir := t.TempDir()
	callLog := filepath.Join(dir, "calls.log")
	counter := filepath.Join(dir, "count")
	bin := filepath.Join(dir, "hermes")
	script := `#!/bin/sh
{ printf '%s|' "$@"; echo; } >> "` + callLog + `"
case "$2" in
  create)
    n=$(cat "` + counter + `" 2>/dev/null || echo 0)
    n=$((n+1))
    echo "$n" > "` + counter + `"
    printf '{"id":"t_%s"}\n' "$n"
    ;;
  *) : ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return runtime.NewHermes(runtime.Config{HermesBin: bin, HermesBoard: "lassdas"}), callLog
}

func designRunConfig(t *testing.T, designMaxRounds int) (runtime.Config, string) {
	t.Helper()
	root := t.TempDir()
	consumer := filepath.Join(root, "consumer.json")
	if err := os.WriteFile(consumer, []byte(fmt.Sprintf(`{"max_stages":3,"design_max_rounds":%d}`, designMaxRounds)), 0o644); err != nil {
		t.Fatal(err)
	}
	config := runtime.Config{ConsumerConfigPath: consumer, Chain: runtime.ChainConfig{RunsRoot: filepath.Join(root, "runs"), Profiles: runtime.ChainProfiles{
		Implementer: "lassdas-implementer", ReviewA: "lassdas-review-a", ReviewB: "lassdas-review-b", Validate: "lassdas-validate", Publish: "lassdas-publish",
		Investigate: "lassdas-investigate", DesignReviewA: "lassdas-design-review-a", DesignReviewB: "lassdas-design-review-b", DesignDecide: "lassdas-design-decide", Applier: "lassdas-applier",
	}}}
	runDir := runtime.RunDirectory(config.Chain, "delivery-1")
	if err := os.MkdirAll(filepath.Join(runDir, "history", "design-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	return config, runDir
}

// objectedBoard is the board after the applier objected in implementation
// round 1 of design round 1: the design cards are done, the apply card is
// done, the sealing review card failed, the tail never started.
func objectedBoard() chainView {
	card := func(id, stage, status string, round int) runtime.BoardTask {
		return runtime.BoardTask{ID: id, Status: status, IdempotencyKey: runtime.ChainCardKey("delivery-1", stage, round)}
	}
	tasks := []runtime.BoardTask{
		card("t_i1", runtime.StageInvestigate, "done", 1), card("t_a1", runtime.StageDesignReviewA, "done", 1),
		card("t_b1", runtime.StageDesignReviewB, "done", 1), card("t_d1", runtime.StageDesignDecide, "done", 1),
		card("t_apply", runtime.StageApply, "done", 1), card("t_ra", runtime.StageReviewA, "blocked", 1),
		card("t_rb", runtime.StageReviewB, "todo", 1), card("t_v", runtime.StageValidate, "todo", 1), card("t_p", runtime.StagePublish, "todo", 1),
	}
	return chainViewFor(tasks, "delivery-1")
}

// objectedOnApplyBoard is the board after the applier objected from its own
// card: the apply card sealed the objection and failed, the tail never
// started (issue #103).
func objectedOnApplyBoard() chainView {
	card := func(id, stage, status string, round int) runtime.BoardTask {
		return runtime.BoardTask{ID: id, Status: status, IdempotencyKey: runtime.ChainCardKey("delivery-1", stage, round)}
	}
	tasks := []runtime.BoardTask{
		card("t_i1", runtime.StageInvestigate, "done", 1), card("t_a1", runtime.StageDesignReviewA, "done", 1),
		card("t_b1", runtime.StageDesignReviewB, "done", 1), card("t_d1", runtime.StageDesignDecide, "done", 1),
		card("t_apply", runtime.StageApply, "blocked", 1), card("t_ra", runtime.StageReviewA, "todo", 1),
		card("t_rb", runtime.StageReviewB, "todo", 1), card("t_v", runtime.StageValidate, "todo", 1), card("t_p", runtime.StagePublish, "todo", 1),
	}
	return chainViewFor(tasks, "delivery-1")
}

func boardCalls(t *testing.T, callLog string) (archived, created []string) {
	t.Helper()
	raw, _ := os.ReadFile(callLog)
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		parts := strings.Split(line, "|")
		if len(parts) > 2 && parts[1] == "archive" {
			archived = append(archived, parts[2])
		}
		for i, p := range parts {
			if p == "--idempotency-key" && i+1 < len(parts) {
				created = append(created, parts[i+1])
			}
		}
	}
	return archived, created
}

func TestDesignObjectionReopensDesignRound(t *testing.T) {
	config, runDir := designRunConfig(t, 3)
	if err := os.WriteFile(filepath.Join(runDir, "history", "design-1", "objection.json"), []byte(`{"reason":"the label is not in that file","section":"files"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	hermes, callLog := fakeBoard(t)
	logger := &recordingLogger{}
	view := objectedBoard()
	handled, err := handleDesignChainFailure(context.Background(), config, nil, hermes, hook.DispatchEnvelope{},
		state.RunOverview{DeliveryID: "delivery-1", RunID: "run-1"}, view, runtime.ChainPlan{Shape: runtime.ShapeDesign}, runtime.StageReviewA, logger)
	if !handled || err != nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	archived, created := boardCalls(t, callLog)
	// The done apply card and the whole tail go; the done design cards stay.
	for _, want := range []string{"t_apply", "t_ra", "t_rb", "t_v", "t_p"} {
		if !containsID(archived, want) {
			t.Errorf("%s was not archived: %v", want, archived)
		}
	}
	for _, keep := range []string{"t_i1", "t_a1", "t_b1", "t_d1"} {
		if containsID(archived, keep) {
			t.Errorf("done design card %s was archived", keep)
		}
	}
	// Design round 2 is created; the implementation round stays 1, so the
	// runner's stage count (no decision sealed) and the board agree.
	wantKeys := []string{"delivery-1:investigate:d2", "delivery-1:design-review-a:d2", "delivery-1:design-review-b:d2", "delivery-1:design-decide:d2",
		"delivery-1:apply:r1", "delivery-1:review-a:r1", "delivery-1:review-b:r1", "delivery-1:validate:r1", "delivery-1:publish:r1"}
	if strings.Join(created, " ") != strings.Join(wantKeys, " ") {
		t.Errorf("created keys = %v, want %v", created, wantKeys)
	}
}

func TestDesignRoundsStopAtLimit(t *testing.T) {
	config, runDir := designRunConfig(t, 1)
	if err := os.WriteFile(filepath.Join(runDir, "history", "design-1", "objection.json"), []byte(`{"reason":"x","section":"files"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	hermes, callLog := fakeBoard(t)
	err := nextDesignRound(context.Background(), hermes, config, state.RunOverview{DeliveryID: "delivery-1", RunID: "run-1"}, objectedBoard(), runtime.ChainPlan{Shape: runtime.ShapeDesign}, "objection", &recordingLogger{})
	if !errors.Is(err, errDesignRoundLimit) {
		t.Fatalf("at the limit: %v", err)
	}
	if archived, created := boardCalls(t, callLog); len(archived) != 0 || len(created) != 0 {
		t.Errorf("the board was touched at the limit: archived %v created %v", archived, created)
	}
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func TestInterruptedObjectionTransitionIsResumedNotHealed(t *testing.T) {
	// After the objection transition archived the implementation cards but
	// died before creating design round 2: only the done design cards remain.
	card := func(id, stage string, round int) runtime.BoardTask {
		return runtime.BoardTask{ID: id, Status: "done", IdempotencyKey: runtime.ChainCardKey("delivery-1", stage, round)}
	}
	view := chainViewFor([]runtime.BoardTask{
		card("t_i1", runtime.StageInvestigate, 1), card("t_a1", runtime.StageDesignReviewA, 1),
		card("t_b1", runtime.StageDesignReviewB, 1), card("t_d1", runtime.StageDesignDecide, 1),
	}, "delivery-1")
	if view.round != 0 || view.designRound != 1 {
		t.Fatalf("view rounds: %+v", view.rounds())
	}
	config, runDir := designRunConfig(t, 3)
	if err := os.WriteFile(filepath.Join(runDir, "history", "design-1", "objection.json"), []byte(`{"reason":"x","section":"files"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	hermes, callLog := fakeBoard(t)
	// The resumed transition creates design round 2 and a fresh apply round 1,
	// never an apply card under the objected design alone.
	err := nextDesignRound(context.Background(), hermes, config, state.RunOverview{DeliveryID: "delivery-1", RunID: "run-1"}, view, runtime.ChainPlan{Shape: runtime.ShapeDesign}, "resumed", &recordingLogger{})
	if err != nil {
		t.Fatal(err)
	}
	_, created := boardCalls(t, callLog)
	if len(created) == 0 || created[0] != "delivery-1:investigate:d2" || !containsID(created, "delivery-1:apply:r1") {
		t.Errorf("resumed transition created %v", created)
	}
}

// The round's incomplete.json is what the terminal report carries to the
// requester: both keys are read from a record shaped like the worker's,
// and a missing or broken record yields no evidence rather than an error.
func TestIncompleteEvidenceReadsTheRoundRecord(t *testing.T) {
	runDir := t.TempDir()
	dir := filepath.Join(runDir, "history", "design-2")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	record := `{"last_refused_answer":"{\"design\":{}}","last_refused_objection":"the design was refused: design file \"docs/x.md\" change 12 is 304 bytes (limit 300)","reason":"the model's design kept failing the checks: no design file contains the wording promised to disappear"}`
	if err := os.WriteFile(filepath.Join(dir, "incomplete.json"), []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}
	evidence := incompleteEvidence(runDir, 2)
	if evidence["incomplete_reason"] != "the model's design kept failing the checks: no design file contains the wording promised to disappear" ||
		evidence["incomplete_objection"] != `the design was refused: design file "docs/x.md" change 12 is 304 bytes (limit 300)` {
		t.Errorf("evidence = %v", evidence)
	}
	// A round the board no longer names falls back to the newest record.
	if got := incompleteEvidence(runDir, 1); got["incomplete_objection"] == "" {
		t.Errorf("the newest record was not found without the board: %v", got)
	}
	if got := incompleteEvidence(runDir, 0); got["incomplete_reason"] == "" {
		t.Errorf("a view without cards found no record: %v", got)
	}
	if got := incompleteEvidence(t.TempDir(), 2); len(got) != 0 {
		t.Errorf("a run without records gave evidence: %v", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "incomplete.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := incompleteEvidence(runDir, 2); len(got) != 0 {
		t.Errorf("a broken record gave evidence: %v", got)
	}
}

// The refusal reaches the ticket through both paths that end an
// incomplete run: the design chain's failure handler and a resubmission of
// a pending terminal report. Dropping either wiring posts the budget text.
func TestIncompleteRunPostsTheRefusalThroughBothPaths(t *testing.T) {
	record := `{"last_refused_answer":"{}","last_refused_objection":"the design was refused: no design file contains the wording promised to disappear: absent_text names wording a design file carries at the baseline","reason":"the model's design kept failing the checks: no design file contains the wording promised to disappear"}`
	assertRefusalPosted := func(t *testing.T, posted []string) {
		t.Helper()
		if len(posted) != 1 {
			t.Fatalf("comments posted = %d, want one: %q", len(posted), posted)
		}
		if !strings.Contains(posted[0], "最後に拒否された点 (規則の原文): the design was refused: no design file contains the wording promised to disappear") ||
			!strings.Contains(posted[0], "自動検査の規則に合わず") || strings.Contains(posted[0], "範囲を絞って再度起票") {
			t.Fatalf("the posted comment does not name the refusal:\n%s", posted[0])
		}
	}
	t.Run("design chain failure", func(t *testing.T) {
		fixture := newPendingFixture(t, "")
		runDir := runDirectory(fixture.config, fixture.deliveryID)
		if err := os.MkdirAll(filepath.Join(runDir, "history", "design-1"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(runDir, "history", "design-1", "incomplete.json"), []byte(record), 0o600); err != nil {
			t.Fatal(err)
		}
		var envelope hook.DispatchEnvelope
		if err := json.Unmarshal([]byte(fixture.run.EnvelopeJSON), &envelope); err != nil {
			t.Fatal(err)
		}
		terminal := runner.NewTerminal(fixture.config, fixture.services, envelope, chainOwnerRunID(fixture.deliveryID), runDir, &pendingTestLogger{})
		digest, err := terminal.ReportDigest(context.Background(), hook.TerminalInvestigationIncomplete, runner.Outcome{Code: hook.TerminalInvestigationIncomplete}, "")
		if err != nil {
			t.Fatal(err)
		}
		fixture.store.expected = digest
		hermes, _ := fakeBoard(t)
		card := runtime.BoardTask{ID: "t_i1", Status: "failed", IdempotencyKey: runtime.ChainCardKey(fixture.deliveryID, runtime.StageInvestigate, 1)}
		view := chainViewFor([]runtime.BoardTask{card}, fixture.deliveryID)
		run := state.RunOverview{DeliveryID: fixture.deliveryID, RunID: "TKT-4242", IssueID: 4242, IssueKey: "TKT-4242"}
		handled, err := handleDesignChainFailure(context.Background(), fixture.config, fixture.services, hermes, envelope, run, view,
			runtime.ChainPlan{Shape: runtime.ShapeDesign}, runtime.StageInvestigate, &recordingLogger{})
		if !handled || err != nil {
			t.Fatalf("handled=%v err=%v", handled, err)
		}
		assertRefusalPosted(t, fixture.comments.posted)
	})
	t.Run("pending terminal resubmission", func(t *testing.T) {
		fixture := newPendingFixture(t, "")
		fixture.writeRunDir(t, "")
		runDir := runDirectory(fixture.config, fixture.deliveryID)
		if err := os.MkdirAll(filepath.Join(runDir, "history", "design-1"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(runDir, "history", "design-1", "incomplete.json"), []byte(record), 0o600); err != nil {
			t.Fatal(err)
		}
		var envelope hook.DispatchEnvelope
		if err := json.Unmarshal([]byte(fixture.run.EnvelopeJSON), &envelope); err != nil {
			t.Fatal(err)
		}
		terminal := runner.NewTerminal(fixture.config, fixture.services, envelope, chainOwnerRunID(fixture.deliveryID), runDir, &pendingTestLogger{})
		digest, err := terminal.ReportDigest(context.Background(), hook.TerminalInvestigationIncomplete, runner.Outcome{Code: hook.TerminalInvestigationIncomplete}, "")
		if err != nil {
			t.Fatal(err)
		}
		fixture.store.expected = digest
		fixture.run.TerminalCode = string(hook.TerminalInvestigationIncomplete)
		fixture.run.TerminalReportSHA256 = digest
		// No cards on the board: the record is found under history/ alone.
		if err := resubmitPendingTerminal(context.Background(), fixture.config, fixture.services, nil, fixture.run, chainViewFor(nil, fixture.deliveryID), &pendingTestLogger{}); err != nil {
			t.Fatal(err)
		}
		assertRefusalPosted(t, fixture.comments.posted)
	})
}

// The apply card fails with the objection already sealed (the applier wrote
// it at the root of its working copy and run-instruction sealed it, issue
// #103): the attendant reopens the design exactly as it does when the
// sealing review card found the objection.
func TestApplyCardObjectionReopensDesignRound(t *testing.T) {
	config, runDir := designRunConfig(t, 3)
	hermes, callLog := fakeBoard(t)
	logger := &recordingLogger{}
	view := objectedOnApplyBoard()
	// Without the sealed record the failed apply card is an ordinary failure.
	handled, err := handleDesignChainFailure(context.Background(), config, nil, hermes, hook.DispatchEnvelope{},
		state.RunOverview{DeliveryID: "delivery-1", RunID: "run-1"}, view, runtime.ChainPlan{Shape: runtime.ShapeDesign}, runtime.StageApply, logger)
	if handled || err != nil {
		t.Fatalf("without an objection record: handled=%v err=%v", handled, err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "history", "design-1", "objection.json"), []byte(`{"reason":"the label is not in that file","section":"files"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	handled, err = handleDesignChainFailure(context.Background(), config, nil, hermes, hook.DispatchEnvelope{},
		state.RunOverview{DeliveryID: "delivery-1", RunID: "run-1"}, view, runtime.ChainPlan{Shape: runtime.ShapeDesign}, runtime.StageApply, logger)
	if !handled || err != nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	archived, created := boardCalls(t, callLog)
	for _, want := range []string{"t_apply", "t_ra", "t_rb", "t_v", "t_p"} {
		if !containsID(archived, want) {
			t.Errorf("%s was not archived: %v", want, archived)
		}
	}
	for _, keep := range []string{"t_i1", "t_a1", "t_b1", "t_d1"} {
		if containsID(archived, keep) {
			t.Errorf("done design card %s was archived", keep)
		}
	}
	if !containsID(created, runtime.ChainCardKey("delivery-1", runtime.StageInvestigate, 2)) {
		t.Fatalf("design round 2 was not opened: %v", created)
	}
}

// The decision the attendant actually makes, measured without a board or a
// tracker: where an implementation round goes next, and what it records.
// The call site had no test at all — disabling the branch that sends a
// delivery back to the designer, or the line that records why a run was
// stopped, failed nothing (review of #123).
func TestWhereAnImplementationRoundGoesNext(t *testing.T) {
	config := func(t *testing.T, body string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "consumer.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	twoReviewers := `{"models":{"reviewers":[{"id":"review-a"},{"id":"review-b"}]}}`
	for name, c := range map[string]struct {
		reviews  map[string]string
		config   string
		wantBack bool
		wantStop bool
	}{
		"a reviewer found the design wrong": {
			reviews:  map[string]string{"review-a.json": `{"findings":[]}`, "review-b.json": `{"findings":[{"code":"design-wrong"}]}`},
			config:   twoReviewers,
			wantBack: true,
		},
		"the reviews found other things": {
			reviews: map[string]string{"review-a.json": `{"findings":[{"code":"style"}]}`, "review-b.json": `{"findings":[]}`},
			config:  twoReviewers,
		},
		"one review will not parse": {
			reviews:  map[string]string{"review-a.json": `{"findings":[`, "review-b.json": `{"findings":[]}`},
			config:   twoReviewers,
			wantStop: true,
		},
		"one review was removed": {
			reviews:  map[string]string{"review-b.json": `{"findings":[{"code":"design-wrong"}]}`},
			config:   twoReviewers,
			wantStop: true,
		},
		"the configuration names no reviewer": {
			reviews:  map[string]string{"review-a.json": `{"findings":[{"code":"design-wrong"}]}`},
			config:   `{"models":{}}`,
			wantStop: true,
		},
		"the configuration cannot be read": {
			reviews:  map[string]string{"review-a.json": `{"findings":[{"code":"design-wrong"}]}`},
			config:   `not json`,
			wantStop: true,
		},
	} {
		runDir := t.TempDir()
		dir := filepath.Join(runDir, "history", "stage-1")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for file, body := range c.reviews {
			if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		back, err := designWrongForRound(runDir, config(t, c.config), 1)
		if (err != nil) != c.wantStop {
			t.Errorf("%s: stop = %v, want %v", name, err, c.wantStop)
		}
		if back != c.wantBack {
			t.Errorf("%s: back to the designer = %v, want %v", name, back, c.wantBack)
		}
	}
}

// A run the attendant stops itself says why on the ticket. The reason names
// no file and no error text: those are the operator's, and the requester's
// question is only whether their own ticket is at fault.
func TestTheReasonAStoppedRunCarriesNamesNothingInternal(t *testing.T) {
	code, reason := unreadableReviewsOutcome(2)
	// Not a model failure: no model was asked anything on this path, and the
	// code decides both the comment the requester reads and whether the
	// failure counts toward the hold on new work.
	if code != hook.TerminalInternalFailed {
		t.Fatalf("the run ends as %s", code)
	}
	if !strings.Contains(reason, "2 巡目") || !strings.Contains(reason, "依頼の内容とは別のところ") {
		t.Fatalf("the reason does not say what happened: %q", reason)
	}
	for _, internal := range []string{".json", "review-a", "unexpected end", "/"} {
		if strings.Contains(reason, internal) {
			t.Errorf("the reason names something internal (%q): %q", internal, reason)
		}
	}
}

// Where an implementation round goes is decided in handleChainFailure, and
// nothing measured it: disabling the branch that stops a run, changing the
// code it stops with, or deleting the sentence its requester is told, all
// left every test green (review of #123). This drives the real function
// against a board and watches what it does with the cards and the run.
func TestAFailedRoundGoesWhereTheReviewsSay(t *testing.T) {
	const designWrong = `{"verdict":"revise","findings":[{"code":"design-wrong","message":"the cause is elsewhere"}]}`
	const otherFindings = `{"verdict":"revise","findings":[{"code":"style","message":"a smaller thing"}]}`
	for name, c := range map[string]struct {
		reviewA, reviewB  string
		wantNewDesign     bool
		wantNextImplement bool
		wantStopped       bool
	}{
		"a reviewer found the design wrong": {reviewA: otherFindings, reviewB: designWrong, wantNewDesign: true},
		// The re-apply route needs a sealed design this fixture does not
		// build, and says so — which is itself the proof it took that route
		// rather than stopping or opening a new design round.
		"the reviews found other things": {reviewA: otherFindings, reviewB: otherFindings, wantNextImplement: true},
		"a review will not parse":        {reviewA: `{"findings":[`, reviewB: otherFindings, wantStopped: true},
		"a review was removed":           {reviewB: otherFindings, wantStopped: true},
	} {
		config, runDir := designRunConfigWithReviewers(t)
		stage1 := filepath.Join(runDir, "history", "stage-1")
		if err := os.MkdirAll(stage1, 0o755); err != nil {
			t.Fatal(err)
		}
		// A sealed revise decision is what brings a round here at all.
		if err := os.WriteFile(filepath.Join(stage1, "decision.json"), []byte(`{"outcome":"revise"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		for file, body := range map[string]string{"review-a.json": c.reviewA, "review-b.json": c.reviewB} {
			if body == "" {
				continue
			}
			if err := os.WriteFile(filepath.Join(stage1, file), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		hermes, callLog := fakeBoard(t)
		envelope := hook.DispatchEnvelope{DeliveryID: "delivery-1", Snapshot: hook.TicketSnapshot{IssueID: 4242}}
		logger := &recordingLogger{}
		err := handleChainFailure(context.Background(), config, quietServices(t), hermes, envelope,
			state.RunOverview{DeliveryID: "delivery-1", RunID: "run-1"}, failedValidateBoard(), runtime.StageValidate, logger)
		_, created := boardCalls(t, callLog)
		newDesign := containsID(created, runtime.ChainCardKey("delivery-1", runtime.StageInvestigate, 2))
		nextImplement := containsID(created, runtime.ChainCardKey("delivery-1", runtime.StageApply, 2)) ||
			(err != nil && strings.Contains(err.Error(), "no approved design to re-apply"))
		reason, _ := os.ReadFile(filepath.Join(runDir, "delivery-stop-reason.txt"))
		stopped := len(reason) > 0
		if newDesign != c.wantNewDesign || nextImplement != c.wantNextImplement || stopped != c.wantStopped {
			t.Errorf("%s: new design=%v next implement=%v stopped=%v (err=%v)", name, newDesign, nextImplement, stopped, err)
		}
		if c.wantStopped {
			if !strings.Contains(string(reason), "レビュー結果を読めなかった") {
				t.Errorf("%s: the requester was told %q", name, reason)
			}
			if len(logger.lines) == 0 {
				t.Errorf("%s: the run stopped without saying why anywhere", name)
			}
		}
	}
}

// designRunConfigWithReviewers is designRunConfig with the two reviewers the
// cards orchestration runs, which is what the sealed reviews are named for.
func designRunConfigWithReviewers(t *testing.T) (runtime.Config, string) {
	t.Helper()
	config, runDir := designRunConfig(t, 3)
	if err := os.WriteFile(config.ConsumerConfigPath, []byte(`{"max_stages":3,"design_max_rounds":3,`+
		`"models":{"reviewers":[{"id":"review-a"},{"id":"review-b"}]},`+
		`"agents":{"applier":{"command":"true","timeout_seconds":60}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// The shape comes from the readiness decision: this is the delivery the
	// investigating designer produced a design for.
	readiness := filepath.Join(runDir, "history", "readiness")
	if err := os.MkdirAll(readiness, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(readiness, "decision.json"),
		[]byte(`{"outcome":"ready","request_kind":"change","needs_design":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return config, runDir
}

// failedValidateBoard is a design-backed delivery whose validate card failed
// on its first implementation round: the one shape that reaches the question
// of where the round goes next.
func failedValidateBoard() chainView {
	card := func(id, stage, status string, round int) runtime.BoardTask {
		return runtime.BoardTask{ID: id, Status: status, IdempotencyKey: runtime.ChainCardKey("delivery-1", stage, round)}
	}
	return chainViewFor([]runtime.BoardTask{
		card("t_i1", runtime.StageInvestigate, "done", 1), card("t_a1", runtime.StageDesignReviewA, "done", 1),
		card("t_b1", runtime.StageDesignReviewB, "done", 1), card("t_d1", runtime.StageDesignDecide, "done", 1),
		card("t_apply", runtime.StageApply, "done", 1), card("t_ra", runtime.StageReviewA, "done", 1),
		card("t_rb", runtime.StageReviewB, "done", 1), card("t_v", runtime.StageValidate, "blocked", 1),
		card("t_p", runtime.StagePublish, "todo", 1),
	}, "delivery-1")
}

// quietServices is the services a chain needs to answer "the requester has
// not asked us to stop": a real Backlog client whose transport answers every
// comment lookup with none.
func quietServices(t *testing.T) *runtime.Services {
	t.Helper()
	client, err := backlog.NewClient(backlog.Config{
		SpaceKey: "space", APIKey: "k", Origin: "https://space.backlog.com", Timeout: time.Second, MaxResponseBytes: 1 << 20,
	}, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader("[]"))}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return &runtime.Services{Backlog: client}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The answer must not depend on the order the reviewers are configured in.
// Stopping at the first design-wrong left a later unreadable record unread,
// so the same pair of records sent the delivery to the designer one way
// round and stopped the run the other (review of #123).
func TestTheAnswerDoesNotDependOnTheOrderOfTheReviewers(t *testing.T) {
	for name, broken := range map[string]string{"unreadable": `{"findings":[`, "removed": ""} {
		runDir := t.TempDir()
		dir := filepath.Join(runDir, "history", "stage-1")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "review-a.json"),
			[]byte(`{"verdict":"revise","findings":[{"code":"design-wrong"}]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if broken != "" {
			if err := os.WriteFile(filepath.Join(dir, "review-b.json"), []byte(broken), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		for _, order := range [][]string{{"review-a", "review-b"}, {"review-b", "review-a"}} {
			flagged, err := reviewsFlagDesignWrong(runDir, 1, order)
			if err == nil || flagged {
				t.Errorf("%s, order %v: flagged=%v err=%v; a record that cannot be read is not an answer", name, order, flagged, err)
			}
		}
	}
}
