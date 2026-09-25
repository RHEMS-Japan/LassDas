package runner

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
)

// ownerTestComments stands in for the tracker and nothing else. The ledger
// under these cases is the real one, so a refusal has to hold against the
// store's own gates rather than against a fake that accepts any owner.
type ownerTestComments struct{ reports, questions []string }

func (*ownerTestComments) FindExactComment(context.Context, int64, string) (int64, bool, error) {
	return 0, false, nil
}

func (*ownerTestComments) FindCommentWithMarker(context.Context, int64, string) (int64, bool, error) {
	return 0, false, nil
}

func (c *ownerTestComments) AddComment(_ context.Context, _ int64, body string) (int64, error) {
	c.reports = append(c.reports, body)
	return int64(900 + len(c.reports)), nil
}

func (c *ownerTestComments) AddCommentNotifying(_ context.Context, _ int64, body string, _ []int64) (int64, error) {
	c.questions = append(c.questions, body)
	return int64(700 + len(c.questions)), nil
}

// ownerTestExecution is the dispatch identity the fixture run is claimed
// under — a worker's own run id, the value a re-dispatch replaces.
const ownerTestExecution = 4242

// claimedTerminalFixture is one queued run claimed by one execution, with
// that execution's Terminal ready to end it: the real ledger, the real
// report and question services, and a run directory shaped the way a run
// leaves it. The claim request is returned so a case can re-claim the run
// for somebody else.
func claimedTerminalFixture(t *testing.T) (*Terminal, hook.PullClaimRequest, *ownerTestComments) {
	t.Helper()
	ctx := context.Background()
	config := runtime.Config{KnowledgeRoot: t.TempDir(), Identity: runtime.IdentityConfig{
		RepositoryID: 7, Repository: answersTestRepository,
		WorkflowRef: answersTestWorkflowRef, EngineSHA: strings.Repeat("c", 40),
	}}
	queuedAt := time.Now().UTC().Add(-5 * time.Minute)
	envelope, err := hook.SealSnapshot(hook.TicketSnapshot{
		SchemaVersion: hook.SnapshotSchemaVersion, SpaceKey: "space",
		ActivityID: 1, ActivityType: 1, ProjectID: 1, ProjectKey: "TICKET",
		IssueID: 2, IssueKey: "TICKET-3", IssueKeyID: 3, CreatorID: 1,
		RunID: "run_20260924_owner", CreatedAt: queuedAt, Target: config.Target(),
		Untrusted: hook.UntrustedTicketData{
			Summary: "Reword one visible label", Description: "Reword the heading on the settings page.\n"},
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.NewLocalStore(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if disposition, err := store.Enqueue(ctx, hook.QueueRequest{Envelope: envelope, QueuedAt: queuedAt}); err != nil ||
		disposition != hook.QueueCreated {
		t.Fatalf("Enqueue() = %s, %v", disposition, err)
	}
	pull := hook.PullClaimRequest{
		SpaceKey: "space", ProjectID: 1, ProjectKey: "TICKET",
		AllowedCreatorID: 1, AllowedActivityType: 1, RunID: envelope.Snapshot.RunID,
		Target: config.Target(), Owner: config.Owner(ownerTestExecution),
		IssuedAt: queuedAt.Add(time.Second), ClaimedAt: queuedAt.Add(time.Second), ClockSkew: time.Minute,
	}
	claimed, disposition, err := store.Pull(ctx, pull)
	if err != nil || disposition != hook.PullAcquired {
		t.Fatalf("Pull() = %s, %v", disposition, err)
	}
	route := runCloneRoute(claimed)
	comments := &ownerTestComments{}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	report, err := hook.NewTerminalReportService(route, store, comments, logger)
	if err != nil {
		t.Fatal(err)
	}
	question, err := hook.NewQuestionReportService(route, store, comments, logger)
	if err != nil {
		t.Fatal(err)
	}
	services := &runtime.Services{Config: config, Store: store, Report: report, Question: question, Route: route}
	terminal := NewTerminal(config, services, claimed, ownerTestExecution, runCloneWorkspace(t), trailTestLogger{})
	return terminal, pull, comments
}

func ownerTestRun(t *testing.T, terminal *Terminal) state.RunOverview {
	t.Helper()
	runs, err := terminal.services.Store.ScanRuns(context.Background())
	if err != nil || len(runs) != 1 {
		t.Fatalf("ScanRuns() = %+v, %v", runs, err)
	}
	return runs[0]
}

func ownerTestClaim(t *testing.T, terminal *Terminal) hook.PullOwner {
	t.Helper()
	route := terminal.services.Route
	route.ExpectedRunID = terminal.envelope.Snapshot.RunID
	owner, found, err := terminal.services.Store.ClaimOwner(context.Background(), route)
	if err != nil || !found {
		t.Fatalf("ClaimOwner() = %+v, %v, %v", owner, found, err)
	}
	return owner
}

// ownerTestReclaim is the recovery the attendant performs on a claim whose
// worker looks dead, followed by the claim the fresh dispatch then takes.
func ownerTestReclaim(t *testing.T, terminal *Terminal, pull hook.PullClaimRequest) {
	t.Helper()
	ctx := context.Background()
	run := ownerTestRun(t, terminal)
	at := pull.ClaimedAt.Add(time.Minute)
	if err := terminal.services.Store.RecoverLostClaim(ctx, run.Key, run.ClaimedAt, at); err != nil {
		t.Fatal(err)
	}
	pull.IssuedAt, pull.ClaimedAt = at.Add(time.Second), at.Add(time.Second)
	if _, disposition, err := terminal.services.Store.Pull(ctx, pull); err != nil || disposition != hook.PullAcquired {
		t.Fatalf("reclaim Pull() = %s, %v", disposition, err)
	}
}

// ownerTestQuestionFile is a clarification decision as the model stage
// leaves it, for the question half of these cases.
func ownerTestQuestionFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "decision.json")
	body := `{"outcome":"clarification_required","questions":[{"id":"Q1","dimension":"preapproved_scope_choice",` +
		`"question":"Which occurrences should change?","why_blocking":"The visible result differs.",` +
		`"choices":[{"id":"a","label":"Only the heading","effect":"The table keeps the old wording."},` +
		`{"id":"b","label":"Both occurrences","effect":"Heading and table change together."}]}],` +
		`"decision_sha256":"` + strings.Repeat("d", 64) + `"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// ownerTestOutcome is a delivered success, the outcome the live incident's
// earlier execution reached while a later one still held the claim.
func ownerTestOutcome() Outcome {
	return Outcome{Code: hook.TerminalSuccess, Evidence: map[string]string{
		"pull_request_url": "https://github.com/example/consumer/pull/13",
		"failed_step":      "review",
	}}
}

// An execution whose claim was recovered and taken by a later one ends
// nothing: not the ticket, not the ledger row, not its own run directory.
// Live 2026-09-24 it ended all three — the earlier execution read the
// later one's identity out of the claim and posted "success" under it
// while the later one was still running, and failing.
func TestTerminalRefusesWhenAnotherExecutionHoldsTheClaim(t *testing.T) {
	for _, operation := range []string{"report", "digest", "question"} {
		t.Run(operation, func(t *testing.T) {
			terminal, pull, comments := claimedTerminalFixture(t)
			// The dispatch that replaced this one, claiming after recovery.
			pull.Owner.WorkflowRunID = ownerTestExecution + 2
			ownerTestReclaim(t, terminal, pull)
			before := ownerTestRun(t, terminal)

			var err error
			switch operation {
			case "report":
				err = terminal.Report(context.Background(), hook.TerminalSuccess, ownerTestOutcome(), "example/consumer")
			case "digest":
				var digest string
				digest, err = terminal.ReportDigest(context.Background(), hook.TerminalSuccess, ownerTestOutcome(), "example/consumer")
				if digest != "" {
					t.Errorf("a replaced execution was given a seal digest: %s", digest)
				}
			case "question":
				err = terminal.AskQuestion(context.Background(), ownerTestQuestionFile(t))
			}
			if err == nil || !strings.Contains(err.Error(), "claim owner changed") {
				t.Fatalf("%s by a replaced execution = %v, want a refusal naming the changed claim", operation, err)
			}
			if !strings.Contains(err.Error(), "execution 4244, not 4242") {
				t.Errorf("the refusal does not say which execution holds the claim: %v", err)
			}

			if len(comments.reports)+len(comments.questions) != 0 {
				t.Fatalf("a replaced execution reached the tracker: reports=%v questions=%v", comments.reports, comments.questions)
			}
			if after := ownerTestRun(t, terminal); !reflect.DeepEqual(before, after) {
				t.Fatalf("the run row moved: before=%+v after=%+v", before, after)
			}
			if owner := ownerTestClaim(t, terminal); owner != pull.Owner {
				t.Fatalf("the claim changed: %+v, want the later execution's %+v", owner, pull.Owner)
			}
			ownerTestWorkspaceIntact(t, terminal.workspace)
		})
	}
}

// ownerTestWorkspaceIntact asserts the run directory is exactly as the
// fixture left it: every clone still there, every record still there with
// its original bytes, and no new record written on the way to the refusal.
func ownerTestWorkspaceIntact(t *testing.T, workspace string) {
	t.Helper()
	clones, records := runCloneState(t, workspace)
	if len(clones) != len(CloneDirectories) {
		t.Fatalf("clones kept = %v, want all of %v", clones, CloneDirectories)
	}
	if len(records) != len(runCloneRecords) {
		t.Fatalf("records kept = %d, want %d (%v)", len(records), len(runCloneRecords), records)
	}
	for name, want := range runCloneRecords {
		got, err := os.ReadFile(filepath.Join(workspace, filepath.FromSlash(name)))
		if err != nil || string(got) != want {
			t.Fatalf("record %s = %q, %v; want %q", name, got, err, want)
		}
	}
	for _, name := range []string{FailedStepFile, ModelFailureFile} {
		if _, err := os.Lstat(filepath.Join(workspace, name)); err == nil {
			t.Fatalf("a refused ending still wrote %s", name)
		}
	}
}

// An upgrade lands while a delivery is in flight: the run is still this
// execution's, so it ends under the identity it was claimed with and the
// engine revision alone is allowed to have moved.
func TestTerminalReportsWhenOnlyTheEngineRevisionChanged(t *testing.T) {
	for _, operation := range []string{"report", "digest", "question"} {
		t.Run(operation, func(t *testing.T) {
			terminal, pull, comments := claimedTerminalFixture(t)
			terminal.config.Identity.EngineSHA = strings.Repeat("e", 40)

			owner, err := terminal.owner(context.Background())
			if err != nil || owner != pull.Owner {
				t.Fatalf("owner() = %+v, %v; want the claim's %+v", owner, err, pull.Owner)
			}
			switch operation {
			case "report":
				if err := terminal.Report(context.Background(), hook.TerminalSuccess,
					ownerTestOutcome(), "example/consumer"); err != nil {
					t.Fatalf("Report() error = %v", err)
				}
				if len(comments.reports) != 1 || !strings.Contains(comments.reports[0], "/4242/attempts/1") {
					t.Fatalf("the report does not carry the claiming execution: %v", comments.reports)
				}
				if run := ownerTestRun(t, terminal); run.State != "terminal" || run.TerminalCode != "success" {
					t.Fatalf("the report did not seal: %+v", run)
				}
			case "digest":
				digest, err := terminal.ReportDigest(context.Background(), hook.TerminalSuccess,
					ownerTestOutcome(), "example/consumer")
				if err != nil || len(digest) != 64 {
					t.Fatalf("ReportDigest() = %q, %v", digest, err)
				}
			case "question":
				if err := terminal.AskQuestion(context.Background(), ownerTestQuestionFile(t)); err != nil {
					t.Fatalf("AskQuestion() error = %v", err)
				}
				if len(comments.questions) != 1 {
					t.Fatalf("questions posted = %d, want one", len(comments.questions))
				}
				// The question's run reference is sealed into the record, not
				// written into the comment the requester reads.
				run := ownerTestRun(t, terminal)
				if run.State != "awaiting_answer" {
					t.Fatalf("the question did not seal: %+v", run)
				}
				record, err := hook.DecodeQuestionRecord([]byte(run.QuestionRecordJSON))
				if err != nil {
					t.Fatalf("sealed question record: %v", err)
				}
				if record.WorkflowRunID != ownerTestExecution || record.RunAttempt != 1 ||
					!strings.Contains(record.RunURL, "/4242/attempts/1") {
					t.Fatalf("the question does not carry the claiming execution: %d/%d %s",
						record.WorkflowRunID, record.RunAttempt, record.RunURL)
				}
			}
		})
	}
}

// Only the engine revision may move. A claim under another engine
// repository, another workflow or another attempt is a different owner,
// not an upgrade, and the ending is refused the same way.
func TestTerminalOwnerAcceptsNoChangeButTheEngineRevision(t *testing.T) {
	for _, testcase := range []struct {
		name    string
		mistake string
		change  func(*Terminal, *hook.PullClaimRequest)
	}{
		{name: "repository_id", mistake: "another engine repository",
			change: func(terminal *Terminal, _ *hook.PullClaimRequest) {
				terminal.config.Identity.RepositoryID = 8
			}},
		{name: "repository", mistake: "another engine repository",
			change: func(terminal *Terminal, _ *hook.PullClaimRequest) {
				terminal.config.Identity.Repository = "example/other-engine"
			}},
		{name: "workflow", mistake: "another workflow",
			change: func(terminal *Terminal, _ *hook.PullClaimRequest) {
				terminal.config.Identity.WorkflowRef = answersTestWorkflowRef + "-other"
			}},
		{name: "run_attempt", mistake: "attempt 2, not 1",
			change: func(_ *Terminal, pull *hook.PullClaimRequest) {
				pull.Owner.RunAttempt = 2
			}},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			terminal, pull, comments := claimedTerminalFixture(t)
			testcase.change(terminal, &pull)
			if pull.Owner.RunAttempt != 1 {
				ownerTestReclaim(t, terminal, pull)
			}
			before := ownerTestRun(t, terminal)

			owner, err := terminal.owner(context.Background())
			if err == nil || !strings.Contains(err.Error(), "claim owner changed") {
				t.Fatalf("owner() with a changed %s = %+v, %v; want a refusal", testcase.name, owner, err)
			}
			if !strings.Contains(err.Error(), testcase.mistake) {
				t.Errorf("the refusal does not name the difference %q: %v", testcase.mistake, err)
			}
			if err := terminal.Report(context.Background(), hook.TerminalSuccess,
				ownerTestOutcome(), "example/consumer"); err == nil ||
				!strings.Contains(err.Error(), "claim owner changed") {
				t.Fatalf("Report() with a changed %s = %v; want a refusal", testcase.name, err)
			}
			if len(comments.reports)+len(comments.questions) != 0 {
				t.Fatalf("a changed %s still reached the tracker: %+v", testcase.name, comments)
			}
			if after := ownerTestRun(t, terminal); !reflect.DeepEqual(before, after) {
				t.Fatalf("the run row moved: before=%+v after=%+v", before, after)
			}
			ownerTestWorkspaceIntact(t, terminal.workspace)
		})
	}
}
