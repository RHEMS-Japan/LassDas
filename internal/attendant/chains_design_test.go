package attendant

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/hook"
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
	if err := os.WriteFile(filepath.Join(runDir, "history", "stage-1", "review-b.json"), []byte(`{"verdict":"revise","findings":[{"code":"design-wrong","message":"the cause is elsewhere"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !reviewsFlagDesignWrong(runDir, 1, []string{"review-a", "review-b"}) || reviewsFlagDesignWrong(runDir, 2, []string{"review-a", "review-b"}) {
		t.Error("design-wrong finding not read from the sealed reviews")
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
