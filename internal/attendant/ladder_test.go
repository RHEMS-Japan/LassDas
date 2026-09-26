package attendant

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/backlog"
	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"

	_ "modernc.org/sqlite"
)

// exhaustTheLadder puts one stage of one round at a configured limit on its
// attempts, which after this change is the only way a failed card still
// ends a delivery. Tests that measure an ending rather than a climb say so
// with this.
func exhaustTheLadder(t *testing.T, config *runtime.Config, runDir, stage string, round int) {
	t.Helper()
	config.Chain.RetryMaxAttempts = 1
	writeLadderRecord(runDir, stage, round, ladderRecord{Attempts: 1}, &pendingTestLogger{})
}

// ladderSetup is a delivery whose card has blocked, with the card's own
// account of why sealed beside the round's records: everything before it
// finished, everything after it is waiting on it.
type ladderSetup struct {
	fixture  pendingFixture
	config   runtime.Config
	envelope hook.DispatchEnvelope
	view     chainView
	runDir   string
	stage    string
	hermes   *runtime.Hermes
	calls    string
	tracker  *fakeConfirmationSource
	logger   *recordingLogger
	// claimedAt is when the ledger says this delivery was claimed, which
	// is what its deadline is measured from. A minute ago by default, so a
	// test about a remedy is not also a test about the clock; a test about
	// the clock moves it.
	claimedAt time.Time
}

// newLadderSetup seals the failure the card would have sealed and puts the
// board in the state that failure leaves it in.
func newLadderSetup(t *testing.T, failure runner.StageFailure) *ladderSetup {
	t.Helper()
	fixture := newPendingFixture(t, "")
	fixture.writeRunDir(t, "example/consumer")
	runDir := runDirectory(fixture.config, fixture.deliveryID)
	// The apply card exists only in a design-backed chain, so a delivery
	// whose apply card failed is one of those — even though the card's own
	// records belong to an implementation round, like every card after it.
	design := runtime.IsDesignStage(failure.Stage)
	designBacked := design || failure.Stage == runtime.StageApply
	history := filepath.Join(runDir, "history", "stage-1")
	if design {
		history = filepath.Join(runDir, "history", "design-1")
	}
	if err := os.MkdirAll(history, 0o755); err != nil {
		t.Fatal(err)
	}
	sealCardFailure(t, runDir, failure)
	// The shape the reception decided, which is what says which cards this
	// delivery's chain has. Nothing downstream can rebuild a stage without
	// it.
	if err := os.MkdirAll(filepath.Join(runDir, "history", "readiness"), 0o755); err != nil {
		t.Fatal(err)
	}
	shape := `{"request_kind":"change","needs_design":false}`
	if designBacked {
		shape = `{"request_kind":"change","needs_design":true}`
	}
	if err := os.WriteFile(filepath.Join(runDir, "history", "readiness", "decision.json"), []byte(shape), 0o600); err != nil {
		t.Fatal(err)
	}

	config := fixture.config
	config.Chain.Profiles = designTestProfiles()
	if err := os.WriteFile(config.ConsumerConfigPath,
		[]byte(`{"max_stages":3,"agents":{"applier":{"command":"launch"}},"models":{"reviewers":[{"id":"review-a"},{"id":"review-b"}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A tracker that answers "no comments", for the paths that reach the
	// real client through the services.
	fixture.services.Backlog = quietTracker(t)

	var envelope hook.DispatchEnvelope
	if err := json.Unmarshal([]byte(fixture.run.EnvelopeJSON), &envelope); err != nil {
		t.Fatal(err)
	}
	card := func(id, stage, status string) runtime.BoardTask {
		return runtime.BoardTask{ID: id, Status: status, IdempotencyKey: runtime.ChainCardKey(fixture.deliveryID, stage, 1)}
	}
	tasks := []runtime.BoardTask{
		card("t_impl", runtime.StageImplement, "done"),
		card("t_ra", runtime.StageReviewA, "todo"),
		card("t_rb", runtime.StageReviewB, "todo"),
		card("t_v", runtime.StageValidate, "todo"),
		card("t_p", runtime.StagePublish, "todo"),
	}
	if designBacked {
		tasks = append([]runtime.BoardTask{
			card("t_inv", runtime.StageInvestigate, "done"),
			card("t_dra", runtime.StageDesignReviewA, "todo"),
			card("t_drb", runtime.StageDesignReviewB, "todo"),
			card("t_dd", runtime.StageDesignDecide, "todo"),
			card("t_apply", runtime.StageApply, "todo"),
		}, tasks...)
	}
	for index := range tasks {
		_, stage, _, _ := runtime.ParseChainCardKey(tasks[index].IdempotencyKey)
		if stage == failure.Stage {
			tasks[index].Status = "blocked"
		}
	}
	hermes, calls := fakeBoard(t)
	return &ladderSetup{
		fixture: fixture, config: config, envelope: envelope,
		view: chainViewFor(tasks, fixture.deliveryID), runDir: runDir, stage: failure.Stage,
		hermes: hermes, calls: calls, tracker: &fakeConfirmationSource{}, logger: &recordingLogger{},
		claimedAt: time.Now().UTC().Add(-time.Minute),
	}
}

func (s *ladderSetup) run() state.RunOverview {
	run := state.RunOverview{DeliveryID: s.fixture.deliveryID, RunID: "TKT-4242", IssueID: 4242, IssueKey: "TKT-4242"}
	if !s.claimedAt.IsZero() {
		run.ClaimedAt = s.claimedAt.UnixMilli()
	}
	return run
}

// climb is one tick's pass over the failed card, with the fake tracker in
// place of the real client.
func (s *ladderSetup) climb(t *testing.T) (ladderVerdict, error) {
	t.Helper()
	shape := runtime.ShapeImplement
	// The apply card exists only in a design-backed chain, so a delivery
	// whose apply card failed is one of those whatever the stage name's
	// own classification says about which round it belongs to.
	if runtime.IsDesignStage(s.stage) || s.stage == runtime.StageApply {
		shape = runtime.ShapeDesign
	}
	climb := newClimb(s.config, s.fixture.services, s.hermes, s.envelope, s.run(), s.view,
		runtime.ChainPlan{Shape: shape}, s.stage, s.logger)
	climb.tracker = s.tracker
	return climbLadder(context.Background(), climb)
}

// climbWithoutTracker is the same tick for a delivery whose ticket cannot
// be reached at all.
func (s *ladderSetup) climbWithoutTracker(t *testing.T) (ladderVerdict, error) {
	t.Helper()
	shape := runtime.ShapeImplement
	if runtime.IsDesignStage(s.stage) {
		shape = runtime.ShapeDesign
	}
	climb := newClimb(s.config, s.fixture.services, s.hermes, s.envelope, s.run(), s.view,
		runtime.ChainPlan{Shape: shape}, s.stage, s.logger)
	climb.tracker = nil
	return climbLadder(context.Background(), climb)
}

func (s *ladderSetup) record() ladderRecord {
	round := 1
	return readLadderRecord(s.runDir, s.stage, round)
}

// sealFailureRecord writes the card's account of its own failure exactly
// where the card writes it, through the runner's own sealing path, so what
// the ladder reads is what a card produces.
func sealCardFailure(t *testing.T, runDir string, failure runner.StageFailure) {
	t.Helper()
	if err := runner.SealStageFailureRecord(runDir, failure); err != nil {
		t.Fatal(err)
	}
	if _, ok := runner.ReadStageFailure(runDir, failure.Stage, failure.Round); !ok {
		t.Fatalf("the failure record for %s round %d did not read back", failure.Stage, failure.Round)
	}
}

func quietTracker(t *testing.T) *backlog.Client {
	t.Helper()
	client, err := backlog.NewClient(backlog.Config{
		SpaceKey: "example", APIKey: "k", Origin: "https://example.backlog.com",
		Timeout: time.Second, MaxResponseBytes: 1 << 20,
	}, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("[]")), Header: http.Header{}}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func ladderBoardLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
}

// archivedCards are the board ids the tick asked to be archived, in order.
func archivedCards(lines []string) []string {
	var archived []string
	for _, line := range lines {
		fields := strings.Split(line, "|")
		for index, field := range fields {
			if field == "archive" && index+1 < len(fields) {
				archived = append(archived, fields[index+1])
			}
		}
	}
	return archived
}

// createdStages are the stage names the tick asked the board to create.
// Every physical line is scanned rather than the one holding "create": a
// card body carries newlines, so one command's record spans several lines
// and its idempotency key is rarely on the first of them.
func createdStages(lines []string) []string {
	var created []string
	for _, line := range lines {
		fields := strings.Split(line, "|")
		for index, field := range fields {
			if field != "--idempotency-key" || index+1 >= len(fields) {
				continue
			}
			if _, stage, _, ok := runtime.ParseChainCardKey(fields[index+1]); ok {
				created = append(created, stage)
			}
		}
	}
	return created
}

// The risk the plan names first: the kanban treats an archived parent as
// satisfied, so a stage rebuilt underneath a card that is still waiting
// would be handed straight to dispatch and run beside the chain's living
// remainder on the one run directory they share.
//
// So the failed stage and everything after it go, and nothing before it
// does — the finished implement card keeps its sealed candidate, which is
// what the review being dispatched again reads.
func TestOnlyTheFailedStageAndTheOnesAfterItAreRebuilt(t *testing.T) {
	setup := newLadderSetup(t, runner.StageFailure{
		Stage: runtime.StageReviewA, Round: 1, Class: runner.FailureClassNetwork, Error: "connection reset by peer",
	})
	verdict, err := setup.climb(t)
	if err != nil || verdict != ladderHandled {
		t.Fatalf("verdict = %v err = %v, want the failure handled", verdict, err)
	}
	lines := ladderBoardLines(t, setup.calls)
	archived := archivedCards(lines)
	if !slices.Equal(archived, []string{"t_ra", "t_rb", "t_v", "t_p"}) {
		t.Fatalf("archived = %v, want the failed stage and the ones after it, in order", archived)
	}
	if slices.Contains(archived, "t_impl") {
		t.Fatal("the finished implement card was archived; its sealed candidate is what the review reads")
	}
	created := createdStages(lines)
	if !slices.Equal(created, []string{runtime.StageReviewA, runtime.StageReviewB, runtime.StageValidate, runtime.StagePublish}) {
		t.Fatalf("created = %v, want exactly the archived stages back", created)
	}
	if len(setup.fixture.comments.posted) != 0 || setup.fixture.store.begins != 0 {
		t.Fatalf("the delivery was reported instead of climbed: %q", setup.fixture.comments.posted)
	}
}

// Every hand is played once. A stage that keeps meeting a full volume
// sweeps the finished deliveries' copies, then gives back its own
// verification sandbox, and then has nothing left to change — it never
// sweeps twice, which is the loop this replaces.
func TestTheSameHandIsNeverPlayedTwice(t *testing.T) {
	setup := newLadderSetup(t, runner.StageFailure{
		Stage: runtime.StageReviewA, Round: 1, Class: runner.FailureClassDisk, Error: "no space left on device",
	})
	var steps []int
	for pass := 0; pass < 4; pass++ {
		verdict, err := setup.climb(t)
		if err != nil || verdict != ladderHandled {
			t.Fatalf("pass %d: verdict = %v err = %v", pass, verdict, err)
		}
		steps = append(steps, setup.record().LadderStep)
	}
	if tried := setup.record().Tried; !slices.Equal(tried, []string{"reclaim:finished-runs", "reclaim:own-sandbox"}) {
		t.Fatalf("tried = %v, want each reclaiming hand exactly once", tried)
	}
	if !slices.Equal(steps, []int{rungReclaim, rungReclaim, rungWait, rungWait}) {
		t.Fatalf("rungs = %v, want both reclaiming hands and then the waiting rung", steps)
	}
}

// The order the plan insists on: other deliveries' leavings before this
// delivery's own. What a finished run left will never be read again; the
// sandbox belongs to a delivery that is still running and costs it a fresh
// clone to take.
func TestADiskFailureSweepsTheFinishedRunsBeforeItsOwnSandbox(t *testing.T) {
	setup := newLadderSetup(t, runner.StageFailure{
		Stage: runtime.StageReviewA, Round: 1, Class: runner.FailureClassDisk, Error: "write /data: no space left on device",
	})
	// A delivery that ended with its copies of the destination still in its
	// directory, which is what fills a volume.
	finished := seedFinishedRunWithClones(t, setup)
	// And this delivery's own sandbox, which the verification makes again.
	sandbox := filepath.Join(setup.runDir, "validation-target")
	if err := os.MkdirAll(filepath.Join(sandbox, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := setup.climb(t); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(finished, "target-repo")); !os.IsNotExist(err) {
		t.Fatalf("the finished delivery's copies were not swept first: %v", err)
	}
	if _, err := os.Stat(sandbox); err != nil {
		t.Fatalf("this delivery's sandbox was taken on the first hand: %v", err)
	}
	if _, err := setup.climb(t); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sandbox); !os.IsNotExist(err) {
		t.Fatalf("the sandbox was not given back on the second hand: %v", err)
	}
	// Nothing sealed is ever touched: the round's history is what the
	// delivery is made of, and the working copy is what no stage rebuilds.
	for _, kept := range []string{"history/stage-1", "ticket-envelope.json"} {
		if _, err := os.Stat(filepath.Join(setup.runDir, kept)); err != nil {
			t.Fatalf("%s was swept: %v", kept, err)
		}
	}
	if tried := setup.record().Tried; !slices.Equal(tried, []string{"reclaim:finished-runs", "reclaim:own-sandbox"}) {
		t.Fatalf("tried = %v", tried)
	}
}

// seedFinishedRunWithClones puts another delivery in the ledger, ended,
// still holding its copies of the destination, in a directory the sweep
// will accept as that delivery's own.
func seedFinishedRunWithClones(t *testing.T, setup *ladderSetup) string {
	t.Helper()
	ledger := filepath.Join(t.TempDir(), "ledger.db")
	store, err := state.NewLocalStore(ledger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	setup.fixture.services.Store = store

	envelope := sweepEnvelope(t, setup.config, 777)
	if _, err := store.Enqueue(context.Background(), hook.QueueRequest{Envelope: envelope, QueuedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	sweepState(t, ledger, envelope.DeliveryID, finishedRunState)
	directory := runDirectory(setup.config, envelope.DeliveryID)
	stageEnvelope(t, directory, envelope)
	for _, clone := range runner.CloneDirectories {
		if err := os.MkdirAll(filepath.Join(directory, clone), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return directory
}

// The waits are the first wait doubled once for each wait already spent,
// up to the longest, and the hands played above the waiting rung do not
// lengthen them: they were different remedies, not repetitions.
func TestTheWaitBetweenAttemptsDoublesUpToTheLongest(t *testing.T) {
	chain := runtime.ChainConfig{RetryBackoffBaseSeconds: 60, RetryBackoffMaxSeconds: 1800}
	for attempts, want := range map[int]time.Duration{
		0: time.Minute, 1: 2 * time.Minute, 2: 4 * time.Minute, 3: 8 * time.Minute,
		4: 16 * time.Minute, 5: 30 * time.Minute, 9: 30 * time.Minute, 40: 30 * time.Minute,
	} {
		if got := ladderWait(chain, ladderRecord{Attempts: attempts}); got != want {
			t.Fatalf("wait after %d attempts = %v, want %v", attempts, got, want)
		}
	}
	spent := ladderRecord{Attempts: 3, Tried: []string{"reclaim:finished-runs", "reclaim:own-sandbox"}}
	if got := ladderWait(chain, spent); got != 2*time.Minute {
		t.Fatalf("after two hands and one wait the next wait = %v, want the second wait", got)
	}
	// A configuration that says nothing gets the intended shape.
	if got := ladderWait(runtime.ChainConfig{}, ladderRecord{}); got != time.Minute {
		t.Fatalf("default first wait = %v", got)
	}
}

// The record is a file under the run directory, which is the volume, so a
// pod replaced mid-climb comes back knowing which hands are spent instead
// of starting again from the top.
func TestTheClimbSurvivesAPodBeingReplaced(t *testing.T) {
	setup := newLadderSetup(t, runner.StageFailure{
		Stage: runtime.StageReviewA, Round: 1, Class: runner.FailureClassDisk, Error: "no space left on device",
	})
	if _, err := setup.climb(t); err != nil {
		t.Fatal(err)
	}
	// Everything the process held is gone; only the volume is left. The
	// record is read off the disk exactly as a new pod would read it.
	restarted := readLadderRecord(setup.runDir, runtime.StageReviewA, 1)
	if restarted.Attempts != 1 || !slices.Equal(restarted.Tried, []string{"reclaim:finished-runs"}) {
		t.Fatalf("after a restart the record = %+v, want one attempt and the first hand spent", restarted)
	}
	if _, err := setup.climb(t); err != nil {
		t.Fatal(err)
	}
	continued := readLadderRecord(setup.runDir, runtime.StageReviewA, 1)
	if continued.Attempts != 2 || !slices.Equal(continued.Tried, []string{"reclaim:finished-runs", "reclaim:own-sandbox"}) {
		t.Fatalf("the climb restarted instead of continuing: %+v", continued)
	}
	// A record naming another stage or another round is not this climb's,
	// and reads as a climb that has not started rather than as this one.
	if other := readLadderRecord(setup.runDir, runtime.StageReviewA, 2); other.Attempts != 0 || len(other.Tried) != 0 {
		t.Fatalf("another round read this round's climb: %+v", other)
	}
	if other := readLadderRecord(setup.runDir, runtime.StageReviewB, 1); other.Attempts != 0 {
		t.Fatalf("another stage read this stage's climb: %+v", other)
	}
}

// The record is read back off a volume that outlives the pod, so the reader
// is held to the same terms as the round's own records: it refuses one that
// is not this stage's and this round's, one too large to be ours, and one
// that will not parse. Every refusal reads as a climb that has not started,
// which costs at most one hand replayed and never leaves a delivery stuck.
func TestReadLadderRecordRefusesWhatIsNotThisClimbs(t *testing.T) {
	runDir := t.TempDir()
	logger := &pendingTestLogger{}
	spent := ladderRecord{Attempts: 2, Tried: []string{"reclaim:finished-runs", "reclaim:own-sandbox"}, LadderStep: rungWait}
	writeLadderRecord(runDir, runtime.StageReviewA, 1, spent, logger)
	if got := readLadderRecord(runDir, runtime.StageReviewA, 1); got.Attempts != 2 || len(got.Tried) != 2 {
		t.Fatalf("this climb did not read back: %+v", got)
	}
	for _, tc := range []struct {
		name  string
		stage string
		round int
	}{
		{"another stage", runtime.StageReviewB, 1},
		{"another round", runtime.StageReviewA, 2},
	} {
		if got := readLadderRecord(runDir, tc.stage, tc.round); got.Attempts != 0 || len(got.Tried) != 0 {
			t.Fatalf("%s read this climb as its own: %+v", tc.name, got)
		}
	}
	// A record whose contents name somewhere else, at the right path. The
	// path proves nothing; the record has to say where it belongs.
	misbound := ladderRecord{SchemaVersion: ladderSchemaVersion, Stage: runtime.StagePublish, Round: 9, Attempts: 5}
	encoded, err := json.Marshal(misbound)
	if err != nil {
		t.Fatal(err)
	}
	path := ladderRecordFile(runDir, runtime.StageReviewA, 1)
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readLadderRecord(runDir, runtime.StageReviewA, 1); got.Attempts != 0 {
		t.Fatalf("a record naming another stage and round was read as this one: %+v", got)
	}
	// Too large to be ours, and unparseable. Both read as a climb that has
	// not started rather than stopping the delivery.
	oversized := append([]byte(`{"schema_version":1,"stage":"review-a","round":1,"attempts":7,"last_reason":"`),
		append(bytes.Repeat([]byte("x"), maxLadderRecordBytes), []byte(`"}`)...)...)
	if err := os.WriteFile(path, oversized, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readLadderRecord(runDir, runtime.StageReviewA, 1); got.Attempts != 0 {
		t.Fatalf("a record past the bound was read: %+v", got)
	}
	if err := os.WriteFile(path, []byte(`{"schema_version":1,"stage":"review-a","round":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readLadderRecord(runDir, runtime.StageReviewA, 1); got.Attempts != 0 {
		t.Fatalf("a record cut short was read: %+v", got)
	}
	// And a record from a shape this engine no longer writes.
	stale := map[string]any{"schema_version": ladderSchemaVersion + 1, "stage": runtime.StageReviewA, "round": 1, "attempts": 4}
	encoded, err = json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readLadderRecord(runDir, runtime.StageReviewA, 1); got.Attempts != 0 {
		t.Fatalf("a record of another shape was read: %+v", got)
	}
}

// A pod being replaced sends every card a signal, and for a verb that
// spends a model turn that arrives as a model failure — from inside the
// process it is one. Counted as such it would walk the delivery down the
// remedies for a model that will not answer, a rung at a time, over a
// rolling restart. So it is not counted: the stage is dispatched again and
// nothing is spent.
func TestAnInterruptedCardIsNotCountedAsAModelFailure(t *testing.T) {
	setup := newLadderSetup(t, runner.StageFailure{
		Stage: runtime.StageReviewA, Round: 1, Class: runner.FailureClassModel,
		Error: "the agent-review step could not run: context canceled", Interrupted: true,
	})
	for pass := 0; pass < 3; pass++ {
		verdict, err := setup.climb(t)
		if err != nil || verdict != ladderHandled {
			t.Fatalf("pass %d: verdict = %v err = %v", pass, verdict, err)
		}
	}
	record := setup.record()
	if record.Interruptions != 3 {
		t.Fatalf("interruptions = %d, want all three recorded", record.Interruptions)
	}
	if record.Attempts != 0 || len(record.Tried) != 0 || record.LadderStep == rungWait {
		t.Fatalf("an interrupted card spent the ladder: %+v", record)
	}
	if len(setup.tracker.added) != 0 {
		t.Fatalf("the ticket was told about a restart: %q", setup.tracker.added)
	}
	// The same card failing for real does spend a hand — this fixture's
	// seats have nobody else in them, so the hand is the rebuilt
	// instruction — which is what makes the difference the flag and not
	// the class.
	sealCardFailure(t, setup.runDir, runner.StageFailure{
		Stage: runtime.StageReviewA, Round: 1, Class: runner.FailureClassModel, Error: "the provider gave up",
	})
	if _, err := setup.climb(t); err != nil {
		t.Fatal(err)
	}
	if got := setup.record(); got.Attempts != 1 || !slices.Contains(got.Tried, "prompt:shorten") {
		t.Fatalf("a real model failure spent nothing: %+v", got)
	}
}

// With no configured alternative launch, a key at its limit goes straight
// to the waiting rung. A shorter prompt cannot restore credit. The ticket
// is told once, and the stage resumes when its next attempt can answer.
func TestAKeyAtItsLimitWaitsIsToldOnceAndResumes(t *testing.T) {
	setup := newLadderSetup(t, runner.StageFailure{
		Stage: runtime.StageReviewA, Round: 1, Class: runner.FailureClassCredit,
		Error: "the agent-review step exited 1",
	})
	setup.config.Chain.RetryBackoffBaseSeconds = 1

	for tick := 0; tick < 3; tick++ {
		verdict, err := setup.climb(t)
		if err != nil || verdict != ladderHandled {
			t.Fatalf("tick %d: verdict = %v err = %v", tick, verdict, err)
		}
	}
	record := setup.record()
	if record.LadderStep != rungWait || len(record.Tried) != 0 || record.Attempts != 0 {
		t.Fatalf("a key at its limit did not go straight to the waiting rung: %+v", record)
	}
	if len(setup.tracker.added) != 1 {
		t.Fatalf("the ticket was told %d times across three ticks, want once: %q", len(setup.tracker.added), setup.tracker.added)
	}
	notice := setup.tracker.added[0]
	if !strings.Contains(notice, "利用枠の上限") || !strings.Contains(notice, "依頼は止まっていません") {
		t.Fatalf("the notice does not say what it is or that the delivery continues:\n%s", notice)
	}
	if len(setup.fixture.comments.posted) != 0 || setup.fixture.store.begins != 0 {
		t.Fatal("the delivery was ended instead of kept")
	}
	if archived := archivedCards(ladderBoardLines(t, setup.calls)); len(archived) != 0 {
		t.Fatalf("the stage was dispatched during the wait: %v", archived)
	}
	// The wait passes and the stage is dispatched again; the key answers,
	// the card finishes, and the delivery carries on from where it was.
	time.Sleep(1100 * time.Millisecond)
	if _, err := setup.climb(t); err != nil {
		t.Fatal(err)
	}
	if got := setup.record().Attempts; got != 1 {
		t.Fatalf("attempts after the wait = %d, want the stage dispatched once", got)
	}
	if archived := archivedCards(ladderBoardLines(t, setup.calls)); !slices.Contains(archived, "t_ra") {
		t.Fatalf("the stage was not dispatched again: archived = %v", archived)
	}
	if len(setup.tracker.added) != 1 {
		t.Fatalf("the ticket was told again on the dispatch: %q", setup.tracker.added)
	}
}

// The requester's stop is one of the only two endings a delivery is allowed
// to have, and before the ladder a failed card ended the delivery within
// the minute — so the stop was only ever needed at a round boundary. A
// delivery that can now spend hours inside one stage has to hear it.
func TestAStopAskedForDuringTheClimbEndsTheDelivery(t *testing.T) {
	setup := newLadderSetup(t, runner.StageFailure{
		Stage: runtime.StageReviewA, Round: 1, Class: runner.FailureClassNetwork, Error: "connection refused",
	})
	setup.config.Tracker.AllowedCreatorID = 7
	setup.tracker.comments = []hook.BacklogComment{{CommentID: 1, UserID: 7, Body: "停止"}}
	verdict, err := setup.climb(t)
	if err != nil || verdict != ladderStopped {
		t.Fatalf("verdict = %v err = %v, want the stop honoured", verdict, err)
	}
	if archived := archivedCards(ladderBoardLines(t, setup.calls)); len(archived) != 0 {
		t.Fatalf("the stage was dispatched after a stop: %v", archived)
	}
}

// And it is heard during the wait, not only when the wait is over. The
// waits reach half an hour, so a stop read only at the next dispatch would
// leave a requester who wrote 停止 a minute into one waiting out the rest of
// it with the delivery still holding its claim.
func TestAStopIsHeardWhileTheStageIsWaitingNotOnlyAtTheNextDispatch(t *testing.T) {
	setup := newLadderSetup(t, runner.StageFailure{
		Stage: runtime.StageReviewA, Round: 1, Class: runner.FailureClassCredit,
		Error: "the agent-review step exited 1",
	})
	setup.config.Tracker.AllowedCreatorID = 7
	// An hour between attempts, so nothing here could be the wait elapsing.
	setup.config.Chain.RetryBackoffBaseSeconds = 3600
	setup.config.Chain.RetryBackoffMaxSeconds = 3600

	if verdict, err := setup.climb(t); err != nil || verdict != ladderHandled {
		t.Fatalf("entering the wait: verdict = %v err = %v", verdict, err)
	}
	if got := setup.record().LadderStep; got != rungWait {
		t.Fatalf("the stage is not waiting: rung %d", got)
	}
	// The requester writes 停止 a moment into the hour.
	setup.tracker.comments = []hook.BacklogComment{{CommentID: 1, UserID: 7, Body: "停止"}}
	// No clock is moved. The very next pass is what has to hear it: the
	// stop read used to run on the wait's own minute-long throttle, and a
	// requester who wrote 停止 could watch six passes go by without the
	// ticket being read at all (measured 2026-09-26).
	verdict, err := setup.climb(t)
	if err != nil || verdict != ladderStopped {
		t.Fatalf("verdict = %v err = %v, want the stop heard during the wait", verdict, err)
	}
	if got := setup.record().Attempts; got != 0 {
		t.Fatalf("attempts = %d: the stop was heard only because the wait had elapsed", got)
	}
}

// A delivery that reaches the waiting rung with a stop already on its
// ticket is obeyed there. A key at its limit arrives on the first tick
// after its card failed, and the longest waits would otherwise leave the
// request unheard until the first dispatch, half an hour away.
func TestAStopAlreadyOnTheTicketIsHeardOnEnteringTheWait(t *testing.T) {
	setup := newLadderSetup(t, runner.StageFailure{
		Stage: runtime.StageReviewA, Round: 1, Class: runner.FailureClassCredit,
		Error: "the agent-review step exited 1",
	})
	setup.config.Tracker.AllowedCreatorID = 7
	setup.config.Chain.RetryBackoffBaseSeconds = 3600
	setup.tracker.comments = []hook.BacklogComment{{CommentID: 1, UserID: 7, Body: "停止"}}
	verdict, err := setup.climb(t)
	if err != nil || verdict != ladderStopped {
		t.Fatalf("verdict = %v err = %v, want the stop heard as the wait began", verdict, err)
	}
	if len(setup.tracker.added) != 0 {
		t.Fatalf("a delivery being stopped was told it was still going: %q", setup.tracker.added)
	}
}

// And it is read on every pass, not on a clock of its own.
//
// The wait belongs to the failure — how long before this stage is worth
// another attempt — and a requester's 停止 has nothing to do with it. While
// the two shared a clock, the engine's promise that a stop is honoured on
// the next tick was false for the whole of every wait: six consecutive
// passes over a waiting card never read the ticket (measured 2026-09-26).
// The cost of the fix is one comment listing per waiting card per tick.
func TestTheStopIsReadOnEveryPassWhileWaiting(t *testing.T) {
	setup := newLadderSetup(t, runner.StageFailure{
		Stage: runtime.StageReviewA, Round: 1, Class: runner.FailureClassCredit,
		Error: "the agent-review step exited 1",
	})
	setup.config.Tracker.AllowedCreatorID = 7
	setup.config.Chain.RetryBackoffBaseSeconds = 3600
	setup.config.Chain.RetryBackoffMaxSeconds = 3600

	if verdict, err := setup.climb(t); err != nil || verdict != ladderHandled {
		t.Fatalf("entering the wait: verdict = %v err = %v", verdict, err)
	}
	// What entering costs is one stop read and the notice's own look for
	// its marker; what matters below is that neither happens again.
	entering := setup.tracker.listings
	if entering == 0 {
		t.Fatal("entering the wait read nothing, so a stop already on the ticket would wait for the first dispatch")
	}
	// Six more ticks inside the same minute, each of which reads.
	for tick := 0; tick < 6; tick++ {
		if verdict, err := setup.climb(t); err != nil || verdict != ladderHandled {
			t.Fatalf("tick %d: verdict = %v err = %v", tick, verdict, err)
		}
	}
	if setup.tracker.listings != entering+6 {
		t.Fatalf("listings after six ticks in the same minute = %d, want one per tick on top of entering's %d",
			setup.tracker.listings, entering)
	}
	// The tick that dispatches reads as well: it is the last thing between
	// a stop and a card that starts spending again.
	setup.tracker.comments = []hook.BacklogComment{{CommentID: 1, UserID: 7, Body: "停止"}}
	record := setup.record()
	record.LastAt = time.Now().UTC().Add(-2 * time.Hour)
	writeLadderRecord(setup.runDir, setup.stage, 1, record, setup.logger)
	if verdict, err := setup.climb(t); err != nil || verdict != ladderStopped {
		t.Fatalf("the dispatching tick did not read the stop: verdict = %v err = %v", verdict, err)
	}
}

// A delivery whose ticket cannot be reached at all can be neither told that
// it is still going nor stopped, so it would climb on in silence. It says
// so once per stage — once, because a line repeated every tick for hours is
// the shape an operator learns to scroll past.
func TestAClimbWithNoTrackerSaysSoOncePerStage(t *testing.T) {
	setup := newLadderSetup(t, runner.StageFailure{
		Stage: runtime.StageReviewA, Round: 1, Class: runner.FailureClassCredit,
		Error: "the agent-review step exited 1",
	})
	setup.config.Chain.RetryBackoffBaseSeconds = 3600
	for tick := 0; tick < 3; tick++ {
		if verdict, err := setup.climbWithoutTracker(t); err != nil || verdict != ladderHandled {
			t.Fatalf("tick %d: verdict = %v err = %v", tick, verdict, err)
		}
	}
	said := 0
	for _, line := range setup.logger.lines {
		if strings.Contains(line, "no tracker is configured for this delivery") {
			said++
		}
	}
	if said != 1 {
		t.Fatalf("the missing tracker was said %d times across three ticks, want once:\n%s", said, strings.Join(setup.logger.lines, "\n"))
	}
	if !setup.record().LoggedNoTracker {
		t.Fatal("the record does not remember that it was said, so a restart would say it again every tick")
	}
	if len(setup.tracker.added) != 0 || len(setup.fixture.comments.posted) != 0 {
		t.Fatal("something was posted for a delivery with no tracker")
	}
}

// The three endings that said "something broke and the delivery is over".
// None of them was a decision about the request, and nothing on a failed
// card's path produces them now. They stay in the vocabulary: ledger rows
// and comments already posted name them.
func TestTheThreeBrokenEndingsAreNoLongerProduced(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stage string
		class runner.FailureClass
	}{
		{"a model that would not answer", runtime.StageReviewA, runner.FailureClassModel},
		{"the machinery's own breakdown", runtime.StageImplement, runner.FailureClassUnknown},
		{"a delivery that would not go out", runtime.StagePublish, runner.FailureClassNetwork},
		{"a design review that would not answer", runtime.StageDesignReviewA, runner.FailureClassModel},
		{"a design the machinery could not carry out", runtime.StageDesignDecide, runner.FailureClassUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup := newLadderSetup(t, runner.StageFailure{
				Stage: tc.stage, Round: 1, Class: tc.class, Error: "whatever it was",
			})
			run, fixture := setup.run(), setup.fixture
			var err error
			if runtime.IsDesignStage(tc.stage) {
				var handled bool
				handled, err = handleDesignChainFailure(context.Background(), setup.config, fixture.services, setup.hermes,
					setup.envelope, run, setup.view, runtime.ChainPlan{Shape: runtime.ShapeDesign}, tc.stage, setup.logger)
				if !handled {
					t.Fatal("the design side did not take its own card")
				}
			} else {
				err = handleChainFailure(context.Background(), setup.config, fixture.services, setup.hermes,
					setup.envelope, run, setup.view, tc.stage, setup.logger)
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, posted := range fixture.comments.posted {
				for _, retired := range retiredCodes() {
					if strings.Contains(posted, string(retired)) {
						t.Fatalf("the delivery still ends as %s:\n%s", retired, posted)
					}
				}
			}
			if fixture.store.begins != 0 {
				t.Fatalf("a terminal report was begun %d times", fixture.store.begins)
			}
			if got := setup.record(); got.Attempts == 0 && got.LadderStep != rungWait {
				t.Fatalf("the failure was neither climbed nor waited on: %+v", got)
			}
			// The codes are still words the engine knows: a ledger row or a
			// comment posted before this change still reads.
			for _, retired := range retiredCodes() {
				if !retired.Valid() {
					t.Fatalf("%s left the vocabulary; rows and comments already name it", retired)
				}
			}
		})
	}
}

func retiredCodes() []hook.TerminalCode {
	return []hook.TerminalCode{hook.TerminalModelFailed, hook.TerminalInternalFailed, hook.TerminalReleaseFailed}
}

// A step cut off by its own wall clock is a failure, not an interruption.
//
// Both arrive as a cancelled context, and while they were filed as the same
// thing a card that could not finish inside its wall was replayed for free,
// for ever: measured 2026-09-26, ten consecutive wall expiries against
// retry_max_attempts=1 spent nothing, counted nothing and rebuilt forty
// cards. Nothing about a wall that expires is free — the same seat on the
// same prompt meets the same wall — so it is counted, climbed, and bounded
// like every other failure.
func TestAStepThatRanOutOfItsOwnTimeIsCountedAgainstTheLimit(t *testing.T) {
	failure := runner.StageFailure{
		Stage: runtime.StageReviewA, Round: 1, Class: runner.FailureClassTimeout,
		Error: "the agent-review step was stopped part-way: context deadline exceeded",
	}
	setup := newLadderSetup(t, failure)
	// One attempt allowed, and it is spent: the operator's own bound, on
	// the far side of which a delivery ends on the failure.
	setup.config.Chain.RetryMaxAttempts = 1
	writeLadderRecord(setup.runDir, setup.stage, 1, ladderRecord{Attempts: 1}, setup.logger)

	for pass := 0; pass < 10; pass++ {
		sealCardFailure(t, setup.runDir, failure)
		verdict, err := setup.climb(t)
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if verdict != ladderSpent {
			t.Fatalf("pass %d: verdict = %v, want the delivery ended on the operator's bound", pass, verdict)
		}
	}
	record := setup.record()
	if record.Interruptions != 0 {
		t.Fatalf("interruptions = %d: a wall that expired was counted as a pod being replaced", record.Interruptions)
	}
	if created := createdStages(ladderBoardLines(t, setup.calls)); len(created) != 0 {
		t.Fatalf("the stage was dispatched again past the bound: %v", created)
	}
}

// And below the bound it is climbed like any other failure: the seat moves,
// which is the remedy for a role that cannot answer in the time it is given.
// Without this the fix could be "end every delivery that meets a wall".
func TestAStepThatRanOutOfItsOwnTimeIsClimbedBeforeTheBound(t *testing.T) {
	setup := newLadderSetup(t, runner.StageFailure{
		Stage: runtime.StageReviewA, Round: 1, Class: runner.FailureClassTimeout,
		Error: "the agent-review step was stopped part-way: context deadline exceeded",
	})
	verdict, err := setup.climb(t)
	if err != nil || verdict != ladderHandled {
		t.Fatalf("verdict = %v err = %v, want the wall climbed", verdict, err)
	}
	record := setup.record()
	if record.Attempts != 1 || record.LadderStep != rungSeat {
		t.Fatalf("record = %+v, want one attempt spent on the seat's rung", record)
	}
	if created := createdStages(ladderBoardLines(t, setup.calls)); len(created) == 0 {
		t.Fatal("the stage was not dispatched again")
	}
}

// The audit's own reproduction, inverted.
//
// It kept one stage of one round failing for the same reason — a key at its
// limit, a model that would not answer — with the wait always elapsed and
// no bound configured, and measured twenty passes: nineteen attempts,
// seventy-six cards rebuilt, the round still at one, and no final report.
// Every one of those passes is still a pass; what has changed is that the
// night ends.
func TestTheRepeatedFailureTheAuditMeasuredNowReachesAnEnd(t *testing.T) {
	for _, class := range []runner.FailureClass{runner.FailureClassCredit, runner.FailureClassModel} {
		t.Run(string(class), func(t *testing.T) {
			setup := newLadderSetup(t, runner.StageFailure{
				Stage: runtime.StageReviewA, Round: 1, Class: class, Error: "persistent refusal",
			})
			if setup.config.Chain.RetryMaxAttempts != 0 {
				t.Fatal("the fixture configures a bound; this measures the delivery without one")
			}
			// On a row that carries a claim, as the audit's did: a fixture
			// with no claim time is one whose deadline is never read, and
			// the twenty passes below would then prove nothing about it.
			if setup.run().ClaimedAt <= 0 {
				t.Fatal("the fixture's ledger row carries no claim time, so no deadline is read from it")
			}
			for pass := 0; pass < 20; pass++ {
				// The wait moved into the past, as the audit did it: what is
				// measured is the climbing, not how long a wait is.
				record := setup.record()
				if record.Attempts > 0 || record.LadderStep == rungWait {
					record.LastAt = time.Now().UTC().Add(-48 * time.Hour)
					writeLadderRecord(setup.runDir, setup.stage, 1, record, setup.logger)
				}
				verdict, err := setup.climb(t)
				if err != nil || verdict != ladderHandled {
					t.Fatalf("pass %d: verdict = %v err = %v, want the failure still being climbed", pass, verdict, err)
				}
			}
			if attempts := setup.record().Attempts; attempts < 10 {
				t.Fatalf("attempts = %d: the delivery was not climbing at all", attempts)
			}
			if len(setup.fixture.comments.posted) != 0 {
				t.Fatalf("the delivery reported inside its deadline: %q", setup.fixture.comments.posted)
			}

			// And then the night is over.
			setup.claimedAt = time.Now().UTC().Add(-9 * time.Hour)
			verdict, err := setup.climb(t)
			if err != nil || verdict != ladderDeadlineReached {
				t.Fatalf("verdict = %v err = %v, want the delivery out of the time it was given", verdict, err)
			}
		})
	}
}
