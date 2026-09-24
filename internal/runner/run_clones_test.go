package runner

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runtime"
)

// runCloneRecords are the files a finished run is kept for. They are read
// back long after the run ends — the requester's comment quotes the trail,
// the board reads the round history — and together they are a rounding
// error beside one clone of the destination.
var runCloneRecords = map[string]string{
	"m1-trail.txt":                   "第1周: 実装しました\n",
	"spend.json":                     `{"total_usd":1.5}`,
	"validation.json":                `{"status":"passed"}`,
	"feature-pr.json":                `{"payload":{"pull_request":{"Number":7}}}`,
	"history/stage-1/candidate.json": `{"files":[]}`,
	"history/stage-1/decision.json":  `{"outcome":"approved"}`,
}

// runCloneWorkspace lays out a run directory as it stands the moment the
// run ends: the three clones of the destination beside the records. The
// base tree is written back unwritable, which is how the model workspace
// shaping leaves it and the reason a plain removal is not enough.
func runCloneWorkspace(t *testing.T) string {
	t.Helper()
	workspace := t.TempDir()
	for _, clone := range runCloneDirectories {
		deep := filepath.Join(workspace, clone, "src", "nested")
		if err := os.MkdirAll(deep, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(deep, "file.txt"), []byte(clone), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	base := filepath.Join(workspace, "target-base")
	for _, directory := range []string{filepath.Join(base, "src", "nested"), filepath.Join(base, "src"), base} {
		if err := os.Chmod(directory, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	// The harness cannot clear an unwritable tree either, so a base tree
	// this case deliberately leaves behind is opened up before it tries.
	t.Cleanup(func() { _ = forceRemoveAll(base) })
	for name, body := range runCloneRecords {
		path := filepath.Join(workspace, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return workspace
}

// runCloneRoute is the report route the fixture run was claimed under.
func runCloneRoute(envelope hook.DispatchEnvelope) hook.ReportRouteConfig {
	return hook.ReportRouteConfig{
		HMACKey: bytes.Repeat([]byte("k"), 32), RepositoryID: 7,
		RepositorySHA256: hook.HashIdentity(answersTestRepository), WorkflowRefSHA256: hook.HashIdentity(answersTestWorkflowRef),
		ExpectedRunID: envelope.Snapshot.RunID,
		Destinations: []hook.ReportDestination{{Repository: "example/consumer", Delivery: hook.DeliverPullRequest,
			StagingOrigin: "https://staging.example.test", ProductionOrigin: "https://www.example.test"}},
		ClockSkew: time.Minute, LeaseDuration: time.Minute, SpaceKey: "space", ProjectID: 1, ProjectKey: "TICKET",
		AllowedCreatorID: 1, AllowedActivityType: 1, RunReferenceScheme: "local",
		Target: hook.DeliveryTarget{RepositoryID: 7, WorkflowRefSHA256: hook.HashIdentity(answersTestWorkflowRef)},
	}
}

func runCloneServices(t *testing.T, config runtime.Config, envelope hook.DispatchEnvelope, store hook.TerminalReportStore) *runtime.Services {
	t.Helper()
	route := runCloneRoute(envelope)
	report, err := hook.NewTerminalReportService(route, store, &answersFakeComments{}, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return &runtime.Services{Config: config, Report: report, Route: route}
}

func runCloneState(t *testing.T, workspace string) (clones []string, records []string) {
	t.Helper()
	for _, name := range runCloneDirectories {
		if _, err := os.Lstat(filepath.Join(workspace, name)); err == nil {
			clones = append(clones, name)
		}
	}
	for name := range runCloneRecords {
		if _, err := os.Lstat(filepath.Join(workspace, filepath.FromSlash(name))); err == nil {
			records = append(records, name)
		}
	}
	return clones, records
}

// The volume filled because nothing cleared a finished run's copies of the
// destination: nine of them left 19 GB behind and the tenth run died in
// its first git operation (live 2026-09-25). A run that has reported is
// finished, so its clones go and everything it is kept for stays.
func TestATerminalRunLeavesNoCloneDirectoriesBehind(t *testing.T) {
	for _, testcase := range []struct {
		code       hook.TerminalCode
		repository string
		evidence   map[string]string
	}{
		{code: hook.TerminalSuccess, repository: "example/consumer",
			evidence: map[string]string{"pull_request_url": "https://github.com/example/consumer/pull/13"}},
		{code: hook.TerminalValidationFailed, repository: "example/consumer"},
		{code: hook.TerminalModelFailed, repository: "example/consumer"},
		{code: hook.TerminalInternalFailed},
		{code: hook.TerminalInvestigated, repository: "example/consumer"},
	} {
		t.Run(string(testcase.code), func(t *testing.T) {
			config, envelope := preservedAnswersFixture(t)
			workspace := runCloneWorkspace(t)
			services := runCloneServices(t, config, envelope, answersFakeStore{})
			terminal := NewTerminal(config, services, envelope, 4242, workspace, trailTestLogger{})
			outcome := Outcome{Code: testcase.code, Evidence: testcase.evidence}
			if err := terminal.Report(context.Background(), testcase.code, outcome, testcase.repository); err != nil {
				t.Fatalf("Report() error = %v", err)
			}
			clones, records := runCloneState(t, workspace)
			if len(clones) != 0 {
				t.Fatalf("a finished run kept its clones: %v", clones)
			}
			if len(records) != len(runCloneRecords) {
				t.Fatalf("records kept = %d, want %d (%v)", len(records), len(runCloneRecords), records)
			}
			trail, err := os.ReadFile(filepath.Join(workspace, "m1-trail.txt"))
			if err != nil || !strings.Contains(string(trail), "実装しました") {
				t.Fatalf("the trail did not survive the pruning: %q, %v", trail, err)
			}
		})
	}
}

// runCloneRefusingStore answers every terminal report with a conflict: the
// report is not sealed and the run stays claimed for the next attempt.
type runCloneRefusingStore struct{}

func (runCloneRefusingStore) BeginTerminal(context.Context, hook.TerminalBeginRequest) (hook.TerminalBinding, hook.TerminalBeginDisposition, error) {
	return hook.TerminalBinding{}, hook.TerminalBeginConflict, nil
}

func (runCloneRefusingStore) CompleteTerminal(context.Context, hook.TerminalCompleteRequest) (hook.TerminalCompleteDisposition, error) {
	return hook.TerminalCompleteConflict, nil
}

// runCloneQuestionFakes accept the question a run posts instead of a report.
type runCloneQuestionFakes struct{ posted []string }

func (f *runCloneQuestionFakes) BeginQuestion(context.Context, hook.QuestionBeginRequest) (hook.TerminalBinding, hook.QuestionBeginDisposition, error) {
	return hook.TerminalBinding{IssueID: 2, IssueKey: "TICKET-3"}, hook.QuestionBeginAcquired, nil
}

func (f *runCloneQuestionFakes) CompleteQuestion(context.Context, hook.QuestionCompleteRequest) (hook.QuestionCompleteDisposition, error) {
	return hook.QuestionCompleted, nil
}

func (f *runCloneQuestionFakes) FindExactComment(context.Context, int64, string) (int64, bool, error) {
	return 0, false, nil
}

func (f *runCloneQuestionFakes) AddCommentNotifying(_ context.Context, _ int64, content string, _ []int64) (int64, error) {
	f.posted = append(f.posted, content)
	return int64(700 + len(f.posted)), nil
}

// A run that has not finished still needs its working copies: the question
// it asked will be answered and the same directory carries on, and a
// report the store would not seal is re-sent from the same directory. Both
// keep everything.
func TestARunThatIsNotTerminalKeepsItsClones(t *testing.T) {
	for _, testcase := range []struct {
		name string
		act  func(*testing.T, *Terminal, string)
	}{
		{
			name: "a question is posted instead of a report",
			act: func(t *testing.T, terminal *Terminal, workspace string) {
				decision := filepath.Join(workspace, "history", "question", "decision.json")
				if err := os.MkdirAll(filepath.Dir(decision), 0o755); err != nil {
					t.Fatal(err)
				}
				body := `{"outcome":"clarification_required","questions":[{"id":"Q1","dimension":"user_visible_behavior",` +
					`"question":"どちらにしますか","why_blocking":"表示が変わります",` +
					`"choices":[{"id":"a","label":"見出しだけ","effect":"表は元のまま"},{"id":"b","label":"両方","effect":"見出しと表が揃う"}]}],` +
					`"decision_sha256":"` + strings.Repeat("d", 64) + `"}`
				if err := os.WriteFile(decision, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := terminal.AskQuestion(context.Background(), decision); err != nil {
					t.Fatalf("AskQuestion() error = %v", err)
				}
			},
		},
		{
			name: "the report was refused",
			act: func(t *testing.T, terminal *Terminal, _ string) {
				if err := terminal.Report(context.Background(), hook.TerminalModelFailed,
					Outcome{Code: hook.TerminalModelFailed}, ""); err == nil {
					t.Fatal("a refused report was reported as sealed")
				}
			},
		},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			config, envelope := preservedAnswersFixture(t)
			workspace := runCloneWorkspace(t)
			services := runCloneServices(t, config, envelope, runCloneRefusingStore{})
			poster := &runCloneQuestionFakes{}
			question, err := hook.NewQuestionReportService(services.Route, poster, poster, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
			if err != nil {
				t.Fatal(err)
			}
			services.Question = question
			terminal := NewTerminal(config, services, envelope, 4242, workspace, trailTestLogger{})
			testcase.act(t, terminal, workspace)
			clones, records := runCloneState(t, workspace)
			if len(clones) != len(runCloneDirectories) {
				t.Fatalf("a run that is not finished lost clones: kept %v", clones)
			}
			if len(records) != len(runCloneRecords) {
				t.Fatalf("records kept = %d, want %d (%v)", len(records), len(runCloneRecords), records)
			}
		})
	}
}

// Pruning is not allowed to end a run that already ended: a clone that
// cannot be removed is written down, and a directory that was never made
// is not a failure at all.
func TestPruningNeitherFailsNorNeedsEveryCloneToBeThere(t *testing.T) {
	if os.Geteuid() == 0 {
		// The refusal this case needs is a permission one, and root has no
		// permissions to be refused by.
		t.Skip("the unremovable clone cannot be staged as root")
	}
	config, envelope := preservedAnswersFixture(t)
	workspace := t.TempDir()
	// Only one of the three is there, and it cannot be removed: an
	// unwritable parent refuses the unlink of the directory itself.
	stuck := filepath.Join(workspace, "target-repo")
	if err := os.MkdirAll(filepath.Join(stuck, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(workspace, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(workspace, 0o755) })
	services := runCloneServices(t, config, envelope, answersFakeStore{})
	terminal := NewTerminal(config, services, envelope, 4242, workspace, trailTestLogger{})
	if err := terminal.Report(context.Background(), hook.TerminalModelFailed,
		Outcome{Code: hook.TerminalModelFailed}, "example/consumer"); err != nil {
		t.Fatalf("a clone that could not be removed failed the terminal report: %v", err)
	}
	if _, err := os.Lstat(stuck); err != nil {
		t.Fatalf("the fixture did not hold the clone in place: %v", err)
	}
}
