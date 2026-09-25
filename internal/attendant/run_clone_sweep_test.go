package attendant

import (
	"context"
	"database/sql"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"

	_ "modernc.org/sqlite"
)

// sweepRecords are the files a finished run is kept for. The sweep must
// leave every one of them where it is.
var sweepRecords = map[string]string{
	"m1-trail.txt":                  "第1周: 実装しました\n",
	"spend.json":                    `{"total_usd":1.5}`,
	"history/stage-1/decision.json": `{"outcome":"approved"}`,
}

// sweepReception is a reception whose ledger holds one run per state asked
// for, each with a directory of its own, and a board it can be told what
// to say.
type sweepReception struct {
	config     runtime.Config
	services   *runtime.Services
	hermes     *runtime.Hermes
	logger     *recordingLogger
	tasksFile  string
	ids        []string                         // the runs in the order they were seeded
	dirs       map[string]string                // run id -> the delivery's directory under the runs root
	deliveries map[string]string                // run id -> delivery id
	envelopes  map[string]hook.DispatchEnvelope // run id -> the envelope its preparation sealed
}

// newSweepReception seeds one run per state (TICKET-500, TICKET-501, …).
// The ledger state is written straight into the row: the sweep reads
// nothing else of a run, and reaching awaiting_answer or an ending through
// the protocols would stage a question that has no bearing on the clones.
func newSweepReception(t *testing.T, orchestration string, states ...string) *sweepReception {
	t.Helper()
	root := t.TempDir()
	config := runtime.Config{
		Orchestration: orchestration,
		Tracker:       runtime.TrackerConfig{SpaceKey: "example", ProjectID: 42, ProjectKey: "TICKET", AllowedCreatorID: 7, AllowedActivityType: 1},
		Identity:      runtime.IdentityConfig{RepositoryID: 1, Repository: "o/r", WorkflowRef: "o/r/wf@main", EngineSHA: strings.Repeat("a", 40)},
		Chain:         runtime.ChainConfig{RunsRoot: filepath.Join(root, "runs")},
	}
	ledger := filepath.Join(root, "ledger.db")
	store, err := state.NewLocalStore(ledger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	tasksFile := filepath.Join(root, "tasks.json")
	if err := os.WriteFile(tasksFile, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "hermes")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ncase \"$2\" in list) cat "+tasksFile+" ;; *) : ;; esac\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	reception := &sweepReception{
		config: config, services: &runtime.Services{Store: store}, hermes: runtime.NewHermes(runtime.Config{HermesBin: bin}),
		logger: &recordingLogger{}, tasksFile: tasksFile,
		dirs: map[string]string{}, deliveries: map[string]string{}, envelopes: map[string]hook.DispatchEnvelope{},
	}
	for index, runState := range states {
		envelope := sweepEnvelope(t, config, int64(500+index))
		if _, err := store.Enqueue(context.Background(), hook.QueueRequest{Envelope: envelope, QueuedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		sweepState(t, ledger, envelope.DeliveryID, runState)
		id := envelope.Snapshot.RunID
		reception.ids = append(reception.ids, id)
		reception.dirs[id] = runDirectory(config, envelope.DeliveryID)
		reception.deliveries[id] = envelope.DeliveryID
		reception.envelopes[id] = envelope
	}
	return reception
}

// tick runs the sweep the way the attendant's tick runs it.
func (r *sweepReception) tick(t *testing.T) {
	t.Helper()
	if err := SweepFinishedRunClones(context.Background(), r.config, r.services, r.hermes, r.logger); err != nil {
		t.Fatalf("the sweep failed the tick: %v", err)
	}
}

// stage lays a run's directory out where the caller says, records and
// sealed envelope and all, the way its preparation left it.
func (r *sweepReception) stage(t *testing.T, id, directory string) {
	t.Helper()
	for name, body := range sweepRecords {
		path := filepath.Join(directory, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stageEnvelope(t, directory, r.envelopes[id])
}

// board is what the kanban answers from now on.
func (r *sweepReception) board(t *testing.T, tasks []runtime.BoardTask) {
	t.Helper()
	encoded, err := json.Marshal(tasks)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.tasksFile, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (r *sweepReception) complaints() []string {
	lines := []string{}
	for _, line := range r.logger.lines {
		if strings.HasPrefix(line, "ERROR ") {
			lines = append(lines, line)
		}
	}
	return lines
}

func sweepEnvelope(t *testing.T, config runtime.Config, number int64) hook.DispatchEnvelope {
	t.Helper()
	key := "TICKET-" + strconv.FormatInt(number, 10)
	envelope, err := hook.SealSnapshot(hook.TicketSnapshot{
		SchemaVersion: hook.SnapshotSchemaVersion, SpaceKey: "example", ActivityID: 9000 + number, ActivityType: 1,
		ProjectID: 42, ProjectKey: "TICKET", IssueID: 30 + number, IssueKey: key, IssueKeyID: number, CreatorID: 7,
		RunID: key, CreatedAt: time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC), Target: config.Target(),
		Untrusted: hook.UntrustedTicketData{Summary: "s", Description: "d"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

// stageEnvelope writes the sealed envelope the preparation leaves in a run
// directory before it makes the first clone.
func stageEnvelope(t *testing.T, directory string, envelope hook.DispatchEnvelope) {
	t.Helper()
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "ticket-envelope.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func sweepState(t *testing.T, ledger, deliveryID, runState string) {
	t.Helper()
	code := ""
	if runState == finishedRunState {
		// The ending that leaves the clones behind: the reception's own
		// question tick sealed it, and it holds no run directory.
		code = string(hook.TerminalClarificationExpired)
	}
	db, err := sql.Open("sqlite", ledger+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	result, err := db.Exec(
		`UPDATE ledger SET attrs = json_set(attrs, '$.state', ?, '$.terminal_code', ?, '$.terminal_completed_at', ?)
		 WHERE pk LIKE 'run#%' AND json_extract(attrs, '$.record_type') = 'run' AND json_extract(attrs, '$.delivery_id') = ?`,
		runState, code, time.Now().Add(-time.Hour).UnixMilli(), deliveryID)
	if err != nil {
		t.Fatal(err)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		t.Fatalf("the run was not seeded into %s: %d rows, %v", runState, affected, err)
	}
	if runState != finishedRunState && runState != "awaiting_answer" {
		return
	}
	// Both of those states release the reception's one-at-a-time slot as
	// they are reached, which is what lets a second run be seeded at all.
	if _, err := db.Exec(`DELETE FROM ledger WHERE json_extract(attrs, '$.record_type') = 'pending'`); err != nil {
		t.Fatal(err)
	}
}

// stageClones lays down the three copies of the destination as the run
// left them. The base tree is written back unwritable, which is how the
// model workspace shaping leaves it and the reason a plain removal is not
// enough.
func stageClones(t *testing.T, directory string) {
	t.Helper()
	for _, clone := range runner.CloneDirectories {
		deep := filepath.Join(directory, clone, "src", "nested")
		if err := os.MkdirAll(deep, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(deep, "file.txt"), []byte(clone), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	base := filepath.Join(directory, "target-base")
	for _, shut := range []string{filepath.Join(base, "src", "nested"), filepath.Join(base, "src"), base} {
		if err := os.Chmod(shut, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	// The harness cannot clear an unwritable tree either, so a base tree a
	// case deliberately leaves behind is opened up before it tries.
	t.Cleanup(func() {
		_ = filepath.WalkDir(base, func(path string, entry fs.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				_ = os.Chmod(path, 0o755)
			}
			return nil
		})
	})
}

// clonesLeft names the copies of the destination still in the directory.
func clonesLeft(t *testing.T, directory string) []string {
	t.Helper()
	left := []string{}
	for _, clone := range runner.CloneDirectories {
		if _, err := os.Lstat(filepath.Join(directory, clone)); err == nil {
			left = append(left, clone)
		}
	}
	return left
}

// recordsLeft names the files the run is kept for that are still there.
func recordsLeft(t *testing.T, directory string) []string {
	t.Helper()
	left := []string{}
	for name := range sweepRecords {
		if _, err := os.Lstat(filepath.Join(directory, filepath.FromSlash(name))); err == nil {
			left = append(left, name)
		}
	}
	return left
}

// A question that passes its answer deadline is sealed by the reception's
// question tick, which holds no run directory, so nothing cleared the
// run's copies of the destination as it ended — and the same is true of
// every run that ended before anything cleared them at all. A tick clears
// them, and everything the finished run is read back for stays where it
// is, ledger row included.
func TestATickClearsTheClonesOfARunSealedWithoutADirectory(t *testing.T) {
	reception := newSweepReception(t, "cards", finishedRunState)
	directory := reception.dirs["TICKET-500"]
	reception.stage(t, "TICKET-500", directory)
	stageClones(t, directory)

	reception.tick(t)

	if left := clonesLeft(t, directory); len(left) != 0 {
		t.Fatalf("a finished run kept its clones: %v", left)
	}
	if kept := recordsLeft(t, directory); len(kept) != len(sweepRecords) {
		t.Fatalf("records kept = %v, want all %d", kept, len(sweepRecords))
	}
	trail, err := os.ReadFile(filepath.Join(directory, "m1-trail.txt"))
	if err != nil || !strings.Contains(string(trail), "実装しました") {
		t.Fatalf("the trail did not survive the sweep: %q, %v", trail, err)
	}
	runs, err := reception.services.Store.ScanRuns(context.Background())
	if err != nil || len(runs) != 1 || runs[0].State != finishedRunState ||
		runs[0].TerminalCode != string(hook.TerminalClarificationExpired) {
		t.Fatalf("the sweep disturbed the ledger: %+v, %v", runs, err)
	}
	if lines := reception.complaints(); len(lines) != 0 {
		t.Fatalf("the sweep complained: %v", lines)
	}
}

// A run that has not ended still needs its working copies: the question it
// asked will be answered and the same directory carries on, a claim may
// still be recovered, and a report the store would not seal is re-sent
// from the same directory.
func TestATickLeavesTheClonesOfARunThatHasNotEnded(t *testing.T) {
	// One reception per state: the ledger lends one run at a time per
	// project, and four of these five still hold that slot.
	for _, runState := range []string{"queued", "claimed", "question_report_pending", "awaiting_answer", "terminal_report_pending"} {
		t.Run(runState, func(t *testing.T) {
			reception := newSweepReception(t, "cards", runState)
			directory := reception.dirs["TICKET-500"]
			reception.stage(t, "TICKET-500", directory)
			stageClones(t, directory)

			reception.tick(t)

			if left := clonesLeft(t, directory); len(left) != len(runner.CloneDirectories) {
				t.Fatalf("a run that is %s lost clones: kept %v", runState, left)
			}
			if kept := recordsLeft(t, directory); len(kept) != len(sweepRecords) {
				t.Fatalf("records kept = %v, want all %d", kept, len(sweepRecords))
			}
			if len(reception.logger.lines) != 0 {
				t.Fatalf("the sweep touched a run that is %s: %v", runState, reception.logger.lines)
			}
		})
	}
}

// A clone that will not go is written down once for the run and changes
// nothing else. The next tick finds the same directory and tries again,
// which is the whole of the retry: a volume that cannot be cleared is a
// reason to say so every minute, not a reason to stop receiving tickets.
func TestACloneThatRefusedIsSweptAgainOnTheNextTick(t *testing.T) {
	if os.Geteuid() == 0 {
		// The refusal this case needs is a permission one, and root has no
		// permissions to be refused by.
		t.Skip("the unremovable clone cannot be staged as root")
	}
	reception := newSweepReception(t, "cards", finishedRunState)
	directory := reception.dirs["TICKET-500"]
	reception.stage(t, "TICKET-500", directory)
	// Two of the three are there and neither can be removed: an unwritable
	// parent refuses the unlink of the directory itself.
	for _, clone := range []string{"target-repo", "validation-target"} {
		if err := os.MkdirAll(filepath.Join(directory, clone, "src"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(directory, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(directory, 0o755) })

	reception.tick(t)

	if left := clonesLeft(t, directory); len(left) != 2 {
		t.Fatalf("the fixture did not hold the clones in place: %v", left)
	}
	if lines := reception.complaints(); len(lines) != 1 {
		t.Fatalf("a refusal is written down once for the run, got %d: %v", len(lines), lines)
	}

	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	reception.tick(t)

	if left := clonesLeft(t, directory); len(left) != 0 {
		t.Fatalf("the next tick did not try again: %v", left)
	}
	if lines := reception.complaints(); len(lines) != 1 {
		t.Fatalf("the retry complained: %v", lines)
	}
	if kept := recordsLeft(t, directory); len(kept) != len(sweepRecords) {
		t.Fatalf("records kept = %v, want all %d", kept, len(sweepRecords))
	}
}

// Almost every finished run the reception looks at on a tick was cleared
// long ago, so looking must cost three stats and nothing else.
func TestARunWithNoClonesCostsTheTickNoRemoval(t *testing.T) {
	reception := newSweepReception(t, "cards", finishedRunState)
	directory := reception.dirs["TICKET-500"]
	reception.stage(t, "TICKET-500", directory)
	swept := 0
	removal := sweepRunClones
	sweepRunClones = func(ctx context.Context, workspace string) runner.CloneSweep {
		swept++
		return removal(ctx, workspace)
	}
	t.Cleanup(func() { sweepRunClones = removal })

	reception.tick(t)

	if swept != 0 {
		t.Fatalf("a run with nothing to remove was swept anyway: %d times", swept)
	}
	if len(reception.logger.lines) != 0 {
		t.Fatalf("a run with nothing to remove was written about: %v", reception.logger.lines)
	}
	if kept := recordsLeft(t, directory); len(kept) != len(sweepRecords) {
		t.Fatalf("records kept = %v, want all %d", kept, len(sweepRecords))
	}
}

// In the runner orchestration the run works in the directory its card
// names, which is not the delivery's directory under the runs root; a run
// whose card is gone is looked for under the root instead, the way the
// merge observation looks for it.
func TestTheRunnerOrchestrationSweepsTheDirectoryTheCardNames(t *testing.T) {
	reception := newSweepReception(t, "runner", finishedRunState, finishedRunState)
	carded := filepath.Join(t.TempDir(), "workspace")
	reception.stage(t, "TICKET-500", carded)
	stageClones(t, carded)
	reception.board(t, []runtime.BoardTask{{
		ID: "task-1", Status: "done", IdempotencyKey: reception.deliveries["TICKET-500"], WorkspacePath: carded,
	}})
	uncarded := reception.dirs["TICKET-501"]
	reception.stage(t, "TICKET-501", uncarded)
	stageClones(t, uncarded)

	reception.tick(t)

	for _, directory := range []string{carded, uncarded} {
		if left := clonesLeft(t, directory); len(left) != 0 {
			t.Fatalf("%s kept its clones: %v", directory, left)
		}
		if kept := recordsLeft(t, directory); len(kept) != len(sweepRecords) {
			t.Fatalf("%s: records kept = %v, want all %d", directory, kept, len(sweepRecords))
		}
	}
	if lines := reception.complaints(); len(lines) != 0 {
		t.Fatalf("the sweep complained: %v", lines)
	}
}

// A card's workspace is a field on a board and names any directory on the
// volume; the sweep removes rather than reads, so it asks the directory
// for this delivery's sealed envelope before it takes anything out of it.
// Pointed at a directory that is not the run's, it takes nothing.
func TestADirectoryThatIsNotTheRunsIsLeftWhole(t *testing.T) {
	for _, testcase := range []struct {
		name  string
		stage func(*testing.T, *sweepReception, string)
	}{
		{name: "it holds no envelope at all", stage: func(*testing.T, *sweepReception, string) {}},
		{
			name: "it holds another delivery's envelope",
			stage: func(t *testing.T, reception *sweepReception, directory string) {
				stageEnvelope(t, directory, sweepEnvelope(t, reception.config, 777))
			},
		},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			reception := newSweepReception(t, "runner", finishedRunState)
			elsewhere := filepath.Join(t.TempDir(), "someone-elses-directory")
			keep := filepath.Join(elsewhere, "target-repo", "src", "keep.txt")
			if err := os.MkdirAll(filepath.Dir(keep), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(keep, []byte("not this run's"), 0o600); err != nil {
				t.Fatal(err)
			}
			testcase.stage(t, reception, elsewhere)
			reception.board(t, []runtime.BoardTask{{
				ID: "task-1", Status: "done", IdempotencyKey: reception.deliveries["TICKET-500"], WorkspacePath: elsewhere,
			}})

			reception.tick(t)

			if _, err := os.Lstat(keep); err != nil {
				t.Fatalf("the sweep emptied a directory that was not the run's: %v", err)
			}
			if left := clonesLeft(t, elsewhere); len(left) != 1 {
				t.Fatalf("the sweep removed from a directory that was not the run's: %v", left)
			}
			if lines := reception.complaints(); len(lines) != 1 {
				t.Fatalf("the refusal was not written down once: %v", reception.logger.lines)
			}
		})
	}
}

// The envelope is what shows whose directory this is, so it has to be an
// envelope and not a file that merely names the delivery. Anything that
// can write into a directory could put the second one there, and the
// sweep would then take the directory's contents on its word; the whole
// sealed shape is measured instead, the way a runner claim measures the
// same file. The run's own envelope in the same place is swept.
func TestAFileThatOnlyNamesTheDeliveryIsNotTheRunsEnvelope(t *testing.T) {
	reception := newSweepReception(t, "cards", finishedRunState)
	directory := reception.dirs["TICKET-500"]
	stageClones(t, directory)
	forged := []byte(`{"delivery_id":"` + reception.deliveries["TICKET-500"] + `"}`)
	if err := os.WriteFile(filepath.Join(directory, "ticket-envelope.json"), forged, 0o600); err != nil {
		t.Fatal(err)
	}

	reception.tick(t)

	if left := clonesLeft(t, directory); len(left) != len(runner.CloneDirectories) {
		t.Fatalf("a file naming the delivery was taken for the run's envelope: %v left", left)
	}
	if lines := reception.complaints(); len(lines) != 1 {
		t.Fatalf("the refusal was not written down once: %v", reception.logger.lines)
	}

	stageEnvelope(t, directory, reception.envelopes["TICKET-500"])
	reception.tick(t)

	if left := clonesLeft(t, directory); len(left) != 0 {
		t.Fatalf("the run's own directory was not swept: %v left", left)
	}
}

// A run directory that is a symbolic link carries the three names into
// whatever it points at, and a removal follows them there. The envelope in
// the link's target is this run's, so the link itself is the only thing
// refusing it.
func TestARunDirectoryThatIsALinkIsLeftAlone(t *testing.T) {
	reception := newSweepReception(t, "cards", finishedRunState)
	target := filepath.Join(t.TempDir(), "outside-the-runs-root")
	reception.stage(t, "TICKET-500", target)
	stageClones(t, target)
	link := reception.dirs["TICKET-500"]
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	reception.tick(t)

	if left := clonesLeft(t, target); len(left) != len(runner.CloneDirectories) {
		t.Fatalf("the sweep followed a link out of the runs root: %v left", left)
	}
	if lines := reception.complaints(); len(lines) != 1 {
		t.Fatalf("the refusal was not written down once: %v", reception.logger.lines)
	}
}

// A reclaim waits on the lend lock the whole runs root shares, which a
// live launch can hold for minutes. The tick passes over every run it
// knows, so what it hands a removal is a deadline, and the budget behind
// that deadline is the tick's: the second run of a tick whose first run
// spent it is swept without asking rather than waiting again.
func TestTheSweepWillNotWaitPastItsBudget(t *testing.T) {
	reception := newSweepReception(t, "cards", finishedRunState, finishedRunState)
	for _, id := range reception.ids {
		reception.stage(t, id, reception.dirs[id])
		stageClones(t, reception.dirs[id])
	}
	budget := reclaimBudget
	reclaimBudget = 300 * time.Millisecond
	t.Cleanup(func() { reclaimBudget = budget })
	removal := sweepRunClones
	spent := []bool{}
	sweepRunClones = func(ctx context.Context, workspace string) runner.CloneSweep {
		// A launcher that never hands the tree back. The second arm is the
		// harness's own stop, so a deadline that never arrives fails the
		// case instead of hanging it.
		spent = append(spent, ctx.Err() != nil)
		select {
		case <-ctx.Done():
		case <-time.After(30 * time.Second):
		}
		return runner.CloneSweep{Refused: []runner.CloneRefusal{{Directory: "target-repo", Err: ctx.Err()}}}
	}
	t.Cleanup(func() { sweepRunClones = removal })

	started := time.Now()
	reception.tick(t)
	waited := time.Since(started)

	if waited > 5*time.Second {
		t.Fatalf("the tick waited past its budget: %s", waited)
	}
	if len(spent) != 2 || spent[0] || !spent[1] {
		t.Fatalf("the budget was not one tick's, shared by the runs in it: %v", spent)
	}
	if lines := reception.complaints(); len(lines) != 2 {
		t.Fatalf("each run's refusal is written down once: %v", lines)
	}
}

// The sweep removes rather than reads, so a directory it cannot name
// safely is left alone rather than guessed at.
func TestTheSweepWillNotNameADirectoryOutsideTheRunsRoot(t *testing.T) {
	delivery := "delivery_" + strings.Repeat("a", 32)
	for _, testcase := range []struct{ name, root, delivery string }{
		{name: "no root was configured", root: "", delivery: delivery},
		{name: "the root is relative", root: "runs", delivery: delivery},
		{name: "the root is the whole volume", root: "/", delivery: delivery},
		{name: "the root is the whole volume, spelled long", root: "/../..", delivery: delivery},
		{name: "no delivery", root: "/var/lib/reception/runs", delivery: ""},
		{name: "the delivery climbs out", root: "/var/lib/reception/runs", delivery: "../../etc"},
		{name: "the delivery is a path", root: "/var/lib/reception/runs", delivery: "a/b"},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			config := runtime.Config{Chain: runtime.ChainConfig{RunsRoot: testcase.root}}
			if directory := finishedRunDirectory(config, testcase.delivery); directory != "" {
				t.Fatalf("named %q", directory)
			}
		})
	}
	config := runtime.Config{Chain: runtime.ChainConfig{RunsRoot: "/var/lib/reception/runs"}}
	if directory := finishedRunDirectory(config, delivery); directory != filepath.Join(config.Chain.RunsRoot, delivery) {
		t.Fatalf("an ordinary delivery was refused: %q", directory)
	}
}
