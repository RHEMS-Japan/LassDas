package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"ticket-runner/internal/chain"
	"ticket-runner/internal/tracker"
)

func approvalConfiguration() config {
	var cfg config
	cfg.Router.Mode = "stages"
	cfg.Intake = &intakeConfig{ProjectID: 17, StopUserIDs: []int64{7}}
	cfg.Workflow = &chain.Workflow{Stages: []chain.Stage{
		{Name: "a", Kind: chain.ModelStage},
		{Name: "b", Kind: chain.CommandStage, OnFailure: "a"},
		{Name: "c", Kind: chain.CommandStage, OnFailure: "a", Approval: &chain.Approval{ApproverUserIDs: []int64{7}, Bundle: "ops/bundle", Show: []string{"dry-run.txt"}, ExpiryHours: 72}},
		{Name: "d", Kind: chain.ModelStage},
		{Name: "e", Kind: chain.CommandStage, OnFailure: "d"},
	}}
	for _, stage := range cfg.Workflow.Stages {
		p := chain.Process{Name: stage.Name, Command: []string{"/bin/true"}}
		if stage.Kind == chain.ModelStage {
			p.ModelEnv = "MODEL"
		}
		cfg.Roles = append(cfg.Roles, chain.Role{Name: stage.Name, Processes: []chain.Process{p}})
	}
	cfg.Roles[3].Processes[0].TrackerAccess = "comment"
	cfg.Roles[4].Processes[0].TrackerAccess = "read"
	return cfg
}

func approvalPair(t *testing.T, check func(config) error, reject func(*config)) {
	t.Helper()
	t.Run("accept", func(t *testing.T) {
		if err := check(approvalConfiguration()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("reject", func(t *testing.T) {
		cfg := approvalConfiguration()
		reject(&cfg)
		if err := check(cfg); err == nil {
			t.Fatal("accepted invalid configuration")
		}
	})
}

func TestApprovalCheck1Predecessor(t *testing.T) {
	approvalPair(t, validateApprovalPredecessor, func(c *config) { c.Workflow.Stages[1].Kind = chain.ModelStage })
	cfg := approvalConfiguration()
	cfg.Workflow.Stages = cfg.Workflow.Stages[2:]
	if err := validateApprovalPredecessor(cfg); err == nil {
		t.Fatal("accepted first-stage approval")
	}
}
func TestApprovalCheck2Failure(t *testing.T) {
	approvalPair(t, validateApprovalFailure, func(c *config) { c.Workflow.Stages[2].OnFailure = "d" })
	for _, target := range []string{"b", "c", "missing"} {
		cfg := approvalConfiguration()
		cfg.Workflow.Stages[2].OnFailure = target
		if validateApprovalFailure(cfg) == nil {
			t.Fatalf("accepted %s", target)
		}
	}
	// Check 2 independently rejects returning to the immediately preceding
	// model, even without running check 1 first.
	cfg := approvalConfiguration()
	cfg.Workflow.Stages[1].Kind = chain.ModelStage
	cfg.Workflow.Stages[2].OnFailure = "b"
	if validateApprovalFailure(cfg) == nil {
		t.Fatal("accepted immediately preceding model as failure target")
	}
}
func TestApprovalCheck3Secrets(t *testing.T) {
	t.Setenv("APPROVAL_TEST_A", "synthetic-a")
	t.Setenv("APPROVAL_TEST_B", "synthetic-b")
	t.Setenv("APPROVAL_TEST_ALIAS", "synthetic-a")
	for _, tt := range []struct {
		name   string
		role   int
		source string
		valid  bool
	}{
		{"separate", 0, "APPROVAL_TEST_B", true},
		{"separate pair", 2, "APPROVAL_TEST_B", true},
		{"same source other role", 0, "APPROVAL_TEST_A", false},
		{"same value other role", 3, "APPROVAL_TEST_ALIAS", false},
		{"same source pair", 2, "APPROVAL_TEST_A", false},
		{"same value pair", 2, "APPROVAL_TEST_ALIAS", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := approvalConfiguration()
			cfg.Roles[1].Processes[0].Secrets = map[string]string{"KEY": "APPROVAL_TEST_A"}
			cfg.Roles[tt.role].Processes[0].Secrets = map[string]string{"OTHER_KEY": tt.source}
			err := validateApprovalSecrets(cfg)
			if (err == nil) != tt.valid {
				t.Fatalf("valid=%t, error=%v", tt.valid, err)
			}
			if err != nil && (strings.Contains(err.Error(), "synthetic-") || strings.Contains(err.Error(), "APPROVAL_TEST_")) {
				t.Fatal("credential material in diagnostic")
			}
		})
	}
	t.Setenv("APPROVAL_TEST_A", "")
	t.Setenv("APPROVAL_TEST_B", "")
	cfg := approvalConfiguration()
	cfg.Roles[1].Processes[0].Secrets = map[string]string{"KEY": "APPROVAL_TEST_A"}
	cfg.Roles[2].Processes[0].Secrets = map[string]string{"KEY": "APPROVAL_TEST_B"}
	if err := validateApprovalSecrets(cfg); err != nil {
		t.Fatal("empty values are not shared credentials", err)
	}
	cfg.Roles[2].Processes[0].Secrets["KEY"] = "APPROVAL_TEST_A"
	if validateApprovalSecrets(cfg) == nil {
		t.Fatal("unset shared source accepted")
	}
}
func TestApprovalCheck4StopUsers(t *testing.T) {
	approvalPair(t, validateApprovalStopUsers, func(c *config) { c.Intake.StopUserIDs = []int64{8} })
}
func TestApprovalCheck5TrackerAccess(t *testing.T) {
	approvalPair(t, validateApprovalTrackerAccess, func(c *config) { c.Roles[0].Processes[0].TrackerAccess = "read" })
	for _, access := range []string{"read", "comment", "every-comment"} {
		for _, role := range []int{0, 1, 2} {
			cfg := approvalConfiguration()
			cfg.Roles[role].Processes[0].TrackerAccess = access
			if validateApprovalTrackerAccess(cfg) == nil {
				t.Fatalf("accepted role %d access %s", role, access)
			}
		}
	}
	for _, change := range []func(*config){
		func(c *config) { c.Intake.QuestionRole = "q" },
		func(c *config) { c.Workflow.Question = "q" },
		func(c *config) { c.Workflow.Stages[1].OnFailure = "d" },
		func(c *config) { c.Workflow.Stages[1].OnFailure = "d"; c.Roles[3].Processes[0].TrackerAccess = "" },
	} {
		cfg := approvalConfiguration()
		change(&cfg)
		if validateApprovalTrackerAccess(cfg) == nil {
			t.Fatal("accepted question or reachable tracker role")
		}
	}
}
func TestApprovalCheck6StopReport(t *testing.T) {
	approvalPair(t, validateApprovalStopReport, func(c *config) { c.Intake.StopReportRole = "b" })
	cfg := approvalConfiguration()
	cfg.Intake.StopReportRole = "d"
	if err := validateApprovalStopReport(cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Intake.StopReportRole = "c"
	if validateApprovalStopReport(cfg) == nil {
		t.Fatal("accepted approval role")
	}
}
func TestApprovalCheck7ProcessCount(t *testing.T) {
	approvalPair(t, validateApprovalProcessCount, func(c *config) { c.Roles[1].Processes = append(c.Roles[1].Processes, chain.Process{}) })
	for _, role := range []int{1, 2} {
		cfg := approvalConfiguration()
		cfg.Roles[role].Processes = nil
		if validateApprovalProcessCount(cfg) == nil {
			t.Fatal("accepted no process")
		}
	}
	cfg := approvalConfiguration()
	cfg.Roles = cfg.Roles[:2]
	if validateApprovalProcessCount(cfg) == nil {
		t.Fatal("accepted missing role")
	}
}
func TestApprovalCheck8Tracker(t *testing.T) {
	approvalPair(t, validateApprovalTracker, func(c *config) { c.GitHub = &tracker.GitHub{} })
	cfg := approvalConfiguration()
	cfg.Redmine = &tracker.Redmine{}
	if validateApprovalTracker(cfg) == nil {
		t.Fatal("accepted Redmine")
	}
}
func TestApprovalCheck9Values(t *testing.T) {
	approvalPair(t, validateApprovalValues, func(c *config) { c.Workflow.Stages[2].Approval.ExpiryHours = 0 })
}
func TestApprovalCheck10LaterFailures(t *testing.T) {
	approvalPair(t, validateApprovalLaterFailures, func(c *config) { c.Workflow.Stages[4].OnFailure = "a" })
	for _, target := range []string{"b", "c", "missing"} {
		cfg := approvalConfiguration()
		cfg.Workflow.Stages[4].OnFailure = target
		if validateApprovalLaterFailures(cfg) == nil {
			t.Fatalf("accepted target %s", target)
		}
	}
}

func TestApprovalUnavailableAfterAllChecks(t *testing.T) {
	const unavailable = "approval stages are not available yet"
	cfg := approvalConfiguration()
	if err := prepareStages(&cfg); err == nil || err.Error() != unavailable {
		t.Fatalf("gate: %v", err)
	}
	// Each check must run before the temporary gate, including the last check.
	changes := []func(*config){
		func(c *config) {
			c.Workflow.Stages[1].Kind = chain.ModelStage
			c.Roles[1].Processes[0].ModelEnv = "MODEL"
		},
		func(c *config) { c.Workflow.Stages[2].OnFailure = "d" },
		func(c *config) {
			c.Roles[1].Processes[0].Secrets = map[string]string{"K": "APPROVAL_TEST_SHARED"}
			c.Roles[2].Processes[0].Secrets = map[string]string{"K": "APPROVAL_TEST_SHARED"}
		},
		func(c *config) { c.Intake.StopUserIDs = nil },
		func(c *config) { c.Roles[0].Processes[0].TrackerAccess = "read" },
		func(c *config) { c.Intake.StopReportRole = "c" },
		func(c *config) { c.Roles[2].Processes = append(c.Roles[2].Processes, chain.Process{}) },
		func(c *config) { c.GitHub = &tracker.GitHub{} },
		func(c *config) { c.Workflow.Stages[2].Approval.Show = nil },
		func(c *config) { c.Workflow.Stages[4].OnFailure = "a" },
	}
	for i, change := range changes {
		cfg := approvalConfiguration()
		change(&cfg)
		if err := prepareStages(&cfg); err == nil || err.Error() == unavailable {
			t.Fatalf("check %d did not precede gate: %v", i+1, err)
		}
	}
	cfg = approvalConfiguration()
	cfg.Workflow.Stages[2].Approval = nil
	if err := prepareStages(&cfg); err != nil {
		t.Fatal("changed ordinary settings", err)
	}
}

func TestApprovalJSONAndCheckHaveNoRemoteReads(t *testing.T) {
	cfg := approvalConfiguration()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := readConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed.Workflow, cfg.Workflow) {
		t.Fatal("approval was not decoded")
	}
	for _, bad := range []string{
		strings.Replace(string(data), `"expiry_hours":72`, `"expiry_hour":72`, 1),
		strings.Replace(string(data), `"expiry_hours":72`, `"expiry_hours":72,"expiry_hours":1`, 1),
	} {
		if _, err := readConfig([]byte(bad)); err == nil {
			t.Fatal("unknown or duplicate approval field accepted")
		}
	}
	readCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		t.Error("check made a remote read")
		cancel()
		return nil, errors.New("unexpected read")
	})
	path := filepath.Join(t.TempDir(), "operator.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"check", "watch", "single", "child"} {
		t.Run(mode, func(t *testing.T) {
			var out bytes.Buffer
			ctx := readCtx
			args := []string{"--config", path}
			directory := filepath.Join(t.TempDir(), "unstarted")
			switch mode {
			case "check":
				args = append(args, "--check")
			case "watch":
				args = append(args, "--watch", "--run-dir", directory)
			default:
				args = append(args, "--request", "unread-request", "--run-dir", directory)
				if mode == "child" {
					ctx = context.WithValue(ctx, acceptedTimeFactsKey{}, "accepted limit")
				}
			}
			if err := run(ctx, args, &out, &out); err == nil || err.Error() != "approval stages are not available yet" {
				t.Fatalf("%s: %v", mode, err)
			}
			if strings.Contains(out.String(), "configuration is accepted") {
				t.Fatal("check accepted approval")
			}
			if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("rejected configuration created a run directory", err)
			}
		})
	}
}

func TestApprovalWatchStartup(t *testing.T) {
	for _, tt := range []struct {
		name         string
		myselfStatus int
		id           int64
		rule         string
		formatStatus int
		want         string
	}{
		{"Myself failure", 503, 9, "markdown", 200, "runtime account"},
		{"runtime approver", 200, 7, "markdown", 200, "runtime account"},
		{"non markdown", 200, 9, "backlog", 200, "markdown"},
		{"unknown format", 200, 9, "", 200, "markdown"},
		{"format read failure", 200, 9, "", 503, "text formatting"},
		{"allowed startup", 200, 9, "markdown", 200, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := approvalConfiguration()
			cfg.Backlog = watchConfiguration(t).Backlog
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			myself, formats := 0, 0
			useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/api/v2/users/myself":
					myself++
					return selectionReply(r, tt.myselfStatus, map[string]any{"id": tt.id}), nil
				case "/api/v2/projects/17":
					formats++
					if tt.want == "" {
						cancel()
					}
					return selectionReply(r, tt.formatStatus, map[string]any{"textFormattingRule": tt.rule}), nil
				default:
					t.Errorf("startup reached intake: %s", r.URL.Path)
					cancel()
					return nil, context.Canceled
				}
			})
			// Assign is false: approval still requires identity on every watch start.
			err := pollRequests(ctx, cfg, filepath.Join(t.TempDir(), "jobs"), time.Time{}, time.Second, 1, io.Discard)
			if tt.want == "" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("startup: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("startup: %v", err)
			}
			if myself != 1 {
				t.Fatalf("Myself calls=%d", myself)
			}
			wantFormats := 1
			if tt.myselfStatus != 200 || tt.id == 7 {
				wantFormats = 0
			}
			if formats != wantFormats {
				t.Fatalf("format calls=%d", formats)
			}
		})
	}
}

func TestApprovalSingleRunRefused(t *testing.T) {
	cfg := approvalConfiguration()
	if err := validateApprovalRun(cfg, false); err == nil || !strings.Contains(err.Error(), "single run") {
		t.Fatalf("single: %v", err)
	}
	if err := validateApprovalRun(cfg, true); err != nil {
		t.Fatal(err)
	}
	cfg.Workflow = nil
	if err := validateApprovalRun(cfg, false); err != nil {
		t.Fatal(err)
	}
}

func TestApprovalRecordingFlagOnlyBacklog(t *testing.T) {
	for _, kind := range []string{"backlog", "github", "redmine"} {
		for _, approval := range []bool{false, true} {
			cfg := approvalConfiguration()
			if kind == "github" {
				cfg.GitHub = &tracker.GitHub{}
			}
			if kind == "redmine" {
				cfg.Redmine = &tracker.Redmine{}
			}
			if !approval {
				cfg.Workflow.Stages[2].Approval = nil
			}
			cfg.recordComments = true // Recomputing also clears any stale flag.
			_ = prepareStages(&cfg)
			want := approval && kind == "backlog"
			if cfg.recordComments != want {
				t.Fatalf("%s approval=%t recording=%t", kind, approval, cfg.recordComments)
			}
			wrapped := reflect.TypeOf(cfg.source()) != reflect.TypeOf(cfg.unrecordedSource())
			if wrapped != want {
				t.Fatalf("%s approval=%t wrapped=%t", kind, approval, wrapped)
			}
		}
	}
}

func TestApprovalStoredChecksUseCurrentRolesOnEveryStart(t *testing.T) {
	cfg := approvalConfiguration()
	store, err := chain.Open(filepath.Join(t.TempDir(), "jobs", "51", "run"), "request")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := prepareStoredApprovals(&cfg, store); err != nil {
		t.Fatal("first start has no copy", err)
	}
	saved := approvalConfiguration().Workflow
	saved.Stages[2].Approval.ApproverUserIDs = nil
	if err := store.Save(chain.State{Request: "request", Workflow: saved}); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name   string
		change func(*config)
		want   string
	}{
		{"3 secrets", func(c *config) {
			c.Roles[1].Processes[0].Secrets = map[string]string{"K": "APPROVAL_TEST_SHARED"}
			c.Roles[0].Processes[0].Secrets = map[string]string{"K": "APPROVAL_TEST_SHARED"}
		}, "credentials"},
		{"5 tracker", func(c *config) { c.Roles[0].Processes[0].TrackerAccess = "read" }, "tracker_access"},
		{"6 report", func(c *config) { c.Intake.StopReportRole = "b" }, "stop_report_role"},
		{"7 processes", func(c *config) { c.Roles[2].Processes = nil }, "exactly one"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			current := approvalConfiguration()
			// Current order differs; protected names must come from the saved copy.
			current.Workflow.Stages[4].Approval = current.Workflow.Stages[2].Approval
			current.Workflow.Stages[2].Approval = nil
			tt.change(&current)
			for attempt := 0; attempt < 2; attempt++ {
				if err := prepareStoredApprovals(&current, store); err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("attempt %d: %v", attempt, err)
				}
			}
		})
	}
	if err := prepareStoredApprovals(&cfg, store); err == nil || err.Error() != "approval stages are not available yet" {
		t.Fatalf("saved copy: %v", err)
	}
	cfg.Workflow = nil
	if err := prepareStoredApprovals(&cfg, store); err != nil {
		t.Fatalf("removed current approval: %v", err)
	}
	cfg = approvalConfiguration()
	saved.Stages[2].Approval = nil
	if err := store.Save(chain.State{Request: "request", Workflow: saved}); err != nil {
		t.Fatal(err)
	}
	if err := prepareStoredApprovals(&cfg, store); err == nil || !strings.Contains(err.Error(), "new request") {
		t.Fatalf("old copy: %v", err)
	}
}

func TestApprovalAbsentLeavesHistoryRetriesToRun(t *testing.T) {
	cfg := approvalConfiguration()
	cfg.Workflow.Stages[2].Approval = nil
	// An ordinary run does not need a store at this preparation step.
	if err := prepareStoredApprovals(&cfg, nil); err != nil {
		t.Fatal(err)
	}
	store, err := chain.Open(filepath.Join(t.TempDir(), "run"), "request")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := os.WriteFile(filepath.Join(store.Dir, "history.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := prepareStoredApprovals(&cfg, store); err != nil {
		t.Fatal("ordinary run stopped before history retry", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	retries := 0
	engine := chain.Chain{
		Router: chain.OneRoleRouter{Role: "a"}, Executor: chain.Processes{}, Store: store,
		Workflow: cfg.Workflow, RetryDelay: time.Millisecond,
		Observe: func(message string) {
			if !strings.HasPrefix(message, "loading request history: ") {
				t.Errorf("unexpected observation: %s", message)
				cancel()
				return
			}
			retries++
			if err := store.Save(chain.State{Request: "request", Done: true}); err != nil {
				t.Error(err)
				cancel()
			}
		},
	}
	// Use the same prepareStoredApprovals -> Chain.Run sequence as main.
	if err := engine.Run(ctx); err != nil {
		t.Fatal("history retry did not recover", err)
	}
	if retries != 1 {
		t.Fatalf("history retries=%d, want 1", retries)
	}
}

func TestApprovalParentAndChildRecordSameJob(t *testing.T) {
	cfg := approvalConfiguration()
	cfg.Backlog = watchConfiguration(t).Backlog
	if err := prepareStages(&cfg); err == nil || err.Error() != "approval stages are not available yet" {
		t.Fatalf("gate: %v", err)
	}
	if !cfg.recordComments {
		t.Fatal("recording flag not wired")
	}
	root := t.TempDir()
	jobs := filepath.Join(root, "jobs")
	job := filepath.Join(jobs, "51")
	runDir := filepath.Join(job, "run")
	raw, err := json.Marshal(watchedIssue(51, "request", "2026-01-03T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	request, err := cfg.source().RequestText(raw)
	if err != nil {
		t.Fatal(err)
	}
	store, err := chain.Open(runDir, request)
	if err != nil {
		t.Fatal(err)
	}
	// Hold an existing request so the parent reads comments without launching a
	// child through the temporary gate. Exercise the child's run setup below.
	if err := store.Save(chain.State{Request: request, Waiting: true}); err != nil {
		t.Fatal(err)
	}
	store.Close()
	if err := os.WriteFile(filepath.Join(job, "issue.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(job, "question.json"), []byte(`{"after":700}`), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	commentReads := int64(0)
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api/v2/users/myself":
			return selectionReply(r, 200, map[string]any{"id": 9}), nil
		case "/api/v2/projects/17":
			return selectionReply(r, 200, map[string]any{"textFormattingRule": "markdown"}), nil
		case "/api/v2/issues":
			return selectionReply(r, 200, []any{}), nil
		case "/api/v2/issues/EXAMPLE-51/comments":
			commentReads++
			if commentReads == 1 {
				cancel()
			}
			return selectionReply(r, 200, []json.RawMessage{seenTestRaw(700+commentReads, 88, "observed comment", 0)}), nil
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			cancel()
			return nil, errors.New("unexpected request")
		}
	})
	cfg.commentJobs = filepath.Join(root, "wrong-parent")
	err = pollRequests(ctx, cfg, jobs, time.Time{}, time.Hour, 1, io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("poll: %v", err)
	}
	if got := readSeenTest(t, jobs); len(got) != 1 || got[0].ID != 701 {
		t.Fatalf("parent record: %+v", got)
	}
	// The shared run setup hands roleAccess a configuration copy. Read through
	// the source captured at that boundary as well as the runtime's later source.
	// Role HTTP forwarding is unchanged; it is outside the recording contract.
	store, err = chain.Open(runDir, request)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg.commentJobs = filepath.Join(root, "wrong-child")
	var roleSource tracker.Tracker
	prepareAccess, err := prepareRunAccess(&cfg, runDir+string(filepath.Separator), "EXAMPLE-51",
		func(bound config, assigned string, options ...func(*tracker.IssueScope)) (func(context.Context, chain.Process) (chain.Process, func(), error), error) {
			roleSource = bound.source()
			return roleAccess(bound, assigned, options...)
		})
	if err != nil || prepareAccess == nil || roleSource == nil {
		t.Fatalf("child access preparation: %v", err)
	}
	if err := prepareStoredApprovals(&cfg, store); err != nil {
		t.Fatal(err)
	}
	issue, err := cfg.source().ReadIssue(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := roleSource.Comments(context.Background(), issue); err != nil {
		t.Fatal("role access source", err)
	}
	if got := readSeenTest(t, jobs); len(got) != 2 || got[0].ID != 701 || got[1].ID != 702 {
		t.Fatalf("role access record: %+v", got)
	}
	if _, err := cfg.source().Comments(context.Background(), issue); err != nil {
		t.Fatal(err)
	}
	got := readSeenTest(t, jobs)
	if len(got) != 3 || got[0].ID != 701 || got[1].ID != 702 || got[2].ID != 703 {
		t.Fatalf("shared record: %+v", got)
	}
	for _, path := range []string{filepath.Join(root, "wrong-parent"), filepath.Join(root, "wrong-child"), filepath.Join(job, "51"), filepath.Join(runDir, "comments-seen.json")} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unexpected record location: %s (%v)", path, err)
		}
	}
}

func TestApprovalRunCommentDirectory(t *testing.T) {
	for _, path := range []string{"jobs/51/run", "jobs/51/run/", "jobs/51/./run"} {
		var cfg config
		setRunCommentJobs(&cfg, path)
		if cfg.commentJobs != "jobs" {
			t.Fatalf("%s: got %s", path, cfg.commentJobs)
		}
	}
}
