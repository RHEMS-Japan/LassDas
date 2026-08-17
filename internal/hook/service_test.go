package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

const testRunID = "run_20260802_alpha"

var testTime = time.Date(2026, 8, 2, 3, 4, 5, 0, time.UTC)

type fakeBacklog struct {
	mu            sync.Mutex
	activity      CanonicalActivity
	issue         CanonicalIssue
	activityErr   error
	issueErr      error
	activityCalls int
	issueCalls    int
}

func (f *fakeBacklog) GetActivity(_ context.Context, _ int64) (CanonicalActivity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.activityCalls++
	return f.activity, f.activityErr
}

func (f *fakeBacklog) GetIssue(_ context.Context, _ int64) (CanonicalIssue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issueCalls++
	return f.issue, f.issueErr
}

type fakeStore struct {
	mu          sync.Mutex
	disposition QueueDisposition
	enqueueErr  error
	requests    []QueueRequest
}

func (f *fakeStore) Enqueue(_ context.Context, request QueueRequest) (QueueDisposition, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, request)
	if f.enqueueErr != nil {
		return "", f.enqueueErr
	}
	if f.disposition == "" {
		return QueueCreated, nil
	}
	return f.disposition, nil
}

func (f *fakeStore) Pull(context.Context, PullClaimRequest) (DispatchEnvelope, PullDisposition, error) {
	return DispatchEnvelope{}, PullEmpty, nil
}

func testTarget() DeliveryTarget {
	return DeliveryTarget{RepositoryID: 123456, WorkflowRefSHA256: strings.Repeat("a", 64)}
}

func testConfig() Config {
	return Config{
		SpaceKey: "example-space", ProjectID: 101, ProjectKey: "TICKET",
		AllowedCreatorID: 202, AllowedActivityType: 1,
		RunMarker: "Automation-Run-ID", ExpectedRunID: testRunID,
		Target: testTarget(), MaxEnvelopeBytes: 32 * 1024,
	}
}

func testActivity() CanonicalActivity {
	return CanonicalActivity{
		ID: 303, Type: 1, ProjectID: 101, ProjectKey: "TICKET", CreatorID: 202,
		IssueID: 404, IssueKeyID: 505, Summary: "sample ticket",
		Description: "Automation-Run-ID: " + testRunID + "\n\nuntrusted instructions", CreatedAt: testTime,
	}
}

func testIssue() CanonicalIssue {
	return CanonicalIssue{ID: 404, ProjectID: 101, IssueKey: "TICKET-505", KeyID: 505, CreatorID: 202, CreatedAt: testTime}
}

func testHint() WebhookHint {
	activity := testActivity()
	return WebhookHint{
		ActivityID: activity.ID, ActivityType: activity.Type,
		ProjectID: activity.ProjectID, ProjectKey: activity.ProjectKey,
		CreatorID: activity.CreatorID, IssueID: activity.IssueID, IssueKeyID: activity.IssueKeyID,
	}
}

func newTestService(t *testing.T, backlog *fakeBacklog, store *fakeStore, logBuffer *bytes.Buffer) *Service {
	t.Helper()
	if backlog == nil {
		backlog = &fakeBacklog{activity: testActivity(), issue: testIssue()}
	}
	if store == nil {
		store = &fakeStore{}
	}
	if logBuffer == nil {
		logBuffer = &bytes.Buffer{}
	}
	service, err := NewService(testConfig(), backlog, store, slog.New(slog.NewTextHandler(logBuffer, nil)))
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	service.now = func() time.Time { return testTime }
	return service
}

func TestProcessQueuesImmutableActivitySnapshot(t *testing.T) {
	backlog := &fakeBacklog{activity: testActivity(), issue: testIssue()}
	store := &fakeStore{}
	result := newTestService(t, backlog, store, nil).Process(context.Background(), testHint())

	if result.Decision != DecisionAccepted || result.Code != "queue_created" {
		t.Fatalf("Process() = %+v", result)
	}
	if len(store.requests) != 1 {
		t.Fatalf("enqueue calls = %d", len(store.requests))
	}
	request := store.requests[0]
	if !request.QueuedAt.Equal(testTime) {
		t.Fatalf("queued at = %v", request.QueuedAt)
	}
	if err := ValidateEnvelope(request.Envelope); err != nil {
		t.Fatalf("ValidateEnvelope() error = %v", err)
	}
	snapshot := request.Envelope.Snapshot
	if snapshot.Untrusted.Summary != backlog.activity.Summary || snapshot.Untrusted.Description != backlog.activity.Description {
		t.Fatal("snapshot did not use immutable activity content")
	}
	if snapshot.Target != testTarget() || snapshot.SchemaVersion != SnapshotSchemaVersion {
		t.Fatalf("snapshot target/schema = %+v/%d", snapshot.Target, snapshot.SchemaVersion)
	}
}

func TestSealSnapshotOverwritesCallerBindings(t *testing.T) {
	snapshot := TicketSnapshot{
		SchemaVersion: SnapshotSchemaVersion, DeliveryID: "delivery_00000000000000000000000000000000",
		SpaceKey: "example-space", ActivityID: 9001, ActivityType: 1, ProjectID: 101, ProjectKey: "TICKET",
		IssueID: 8001, IssueKey: "TICKET-501", IssueKeyID: 501, CreatorID: 202,
		RunID: testRunID, CreatedAt: testTime, InputSHA256: strings.Repeat("0", 64), Target: testTarget(),
		Untrusted: UntrustedTicketData{Summary: "summary", Description: "description"},
	}
	envelope, err := SealSnapshot(snapshot)
	if err != nil {
		t.Fatalf("SealSnapshot() error = %v", err)
	}
	if envelope.DeliveryID == snapshot.DeliveryID || envelope.Snapshot.InputSHA256 == snapshot.InputSHA256 {
		t.Fatal("caller-controlled bindings were preserved")
	}
	if err := ValidateEnvelope(envelope); err != nil {
		t.Fatalf("ValidateEnvelope() error = %v", err)
	}
}

func TestProcessRejectsEveryWebhookCanonicalMismatch(t *testing.T) {
	tests := map[string]func(*WebhookHint){
		"activity id": func(h *WebhookHint) { h.ActivityID++ }, "activity type": func(h *WebhookHint) { h.ActivityType++ },
		"project id": func(h *WebhookHint) { h.ProjectID++ }, "project key": func(h *WebhookHint) { h.ProjectKey = "OTHER" },
		"creator id": func(h *WebhookHint) { h.CreatorID++ }, "issue id": func(h *WebhookHint) { h.IssueID++ },
		"issue key id": func(h *WebhookHint) { h.IssueKeyID++ },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			backlog := &fakeBacklog{activity: testActivity(), issue: testIssue()}
			store := &fakeStore{}
			hint := testHint()
			mutate(&hint)
			result := newTestService(t, backlog, store, nil).Process(context.Background(), hint)
			if result.Decision != DecisionIgnored || result.Code != "activity_mismatch" || backlog.issueCalls != 0 || len(store.requests) != 0 {
				t.Fatalf("result=%+v issueCalls=%d enqueueCalls=%d", result, backlog.issueCalls, len(store.requests))
			}
		})
	}
}

// TestProcessAcceptsAnyBodyBecauseTheTicketNamesTheRun fixes the removal of the
// reserved first line. The run is identified by the ticket, so prose that never
// mentions the automation is queued like anything else. Requiring a magic line
// both demanded a format and, because the record key was built from a single
// configured value, allowed exactly one ticket for the life of the deployment.
func TestProcessAcceptsAnyBodyBecauseTheTicketNamesTheRun(t *testing.T) {
	bodies := map[string]string{
		"ordinary prose":        "ログイン画面の「送信」を「ログイン」に変えてください。",
		"headings and a table":  "## 背景\n\n| 項目 | 値 |\n|---|---|\n| 用語 | RPM制限 |",
		"a stale magic line":    "Automation-Run-ID: run_20260802_other\n\n本文",
		"the line further down": "example\nAutomation-Run-ID: whatever",
	}
	for name, description := range bodies {
		t.Run(name, func(t *testing.T) {
			activity, issue := testActivity(), testIssue()
			activity.Description = description
			hint := WebhookHint{ActivityID: activity.ID, ActivityType: activity.Type, ProjectID: activity.ProjectID, ProjectKey: activity.ProjectKey, CreatorID: activity.CreatorID, IssueID: activity.IssueID, IssueKeyID: activity.IssueKeyID}
			store := &fakeStore{}
			result := newTestService(t, &fakeBacklog{activity: activity, issue: issue}, store, nil).Process(context.Background(), hint)
			if result.Decision != DecisionAccepted || len(store.requests) != 1 {
				t.Fatalf("a ticket must not be refused for how it is written: result=%+v enqueued=%d", result, len(store.requests))
			}
			if got := store.requests[0].Envelope.Snapshot.RunID; got != issue.IssueKey {
				t.Fatalf("the run must be identified by the ticket, got %q want %q", got, issue.IssueKey)
			}
		})
	}
}

func TestProcessDefaultDenyBeforeQueue(t *testing.T) {
	tests := []struct {
		name     string
		activity func(*CanonicalActivity)
		issue    func(*CanonicalIssue)
		code     string
	}{
		{name: "activity type", activity: func(v *CanonicalActivity) { v.Type++ }, code: "activity_not_allowed"},
		{name: "configured project", activity: func(v *CanonicalActivity) { v.ProjectID++ }, code: "activity_not_allowed"},
		{name: "current issue id", issue: func(v *CanonicalIssue) { v.ID++ }, code: "issue_not_allowed"},
		{name: "current project", issue: func(v *CanonicalIssue) { v.ProjectID++ }, code: "issue_not_allowed"},
		{name: "current creator", issue: func(v *CanonicalIssue) { v.CreatorID++ }, code: "issue_not_allowed"},
		{name: "issue key", issue: func(v *CanonicalIssue) { v.IssueKey += "-extra" }, code: "issue_not_allowed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			activity, issue := testActivity(), testIssue()
			hint := testHint()
			if tt.activity != nil {
				tt.activity(&activity)
				hint = WebhookHint{ActivityID: activity.ID, ActivityType: activity.Type, ProjectID: activity.ProjectID, ProjectKey: activity.ProjectKey, CreatorID: activity.CreatorID, IssueID: activity.IssueID, IssueKeyID: activity.IssueKeyID}
			}
			if tt.issue != nil {
				tt.issue(&issue)
			}
			store := &fakeStore{}
			result := newTestService(t, &fakeBacklog{activity: activity, issue: issue}, store, nil).Process(context.Background(), hint)
			if result.Code != tt.code || len(store.requests) != 0 {
				t.Fatalf("result=%+v enqueueCalls=%d", result, len(store.requests))
			}
		})
	}
}

func TestProcessMapsQueueDispositions(t *testing.T) {
	tests := map[QueueDisposition]struct {
		decision Decision
		code     string
	}{
		QueueCreated: {DecisionAccepted, "queue_created"}, QueueDuplicate: {DecisionAccepted, "duplicate_queued"},
		QueueClaimed: {DecisionAccepted, "duplicate_claimed"}, QueueConflict: {DecisionIgnored, "queue_conflict"},
	}
	for disposition, want := range tests {
		t.Run(string(disposition), func(t *testing.T) {
			result := newTestService(t, nil, &fakeStore{disposition: disposition}, nil).Process(context.Background(), testHint())
			if result.Decision != want.decision || result.Code != want.code {
				t.Fatalf("Process() = %+v", result)
			}
		})
	}
}

func TestProcessQueueFailureRequestsRetry(t *testing.T) {
	store := &fakeStore{enqueueErr: errors.New("ambiguous write")}
	result := newTestService(t, nil, store, nil).Process(context.Background(), testHint())
	if result.Decision != DecisionRetryRequested || result.Code != "queue_failed" {
		t.Fatalf("Process() = %+v", result)
	}
}

func TestSnapshotBindingChangesWithImmutableActivityContent(t *testing.T) {
	firstStore := &fakeStore{}
	newTestService(t, nil, firstStore, nil).Process(context.Background(), testHint())
	activity := testActivity()
	activity.Description += "\nchanged at creation"
	secondStore := &fakeStore{}
	newTestService(t, &fakeBacklog{activity: activity, issue: testIssue()}, secondStore, nil).Process(context.Background(), testHint())
	first, second := firstStore.requests[0].Envelope, secondStore.requests[0].Envelope
	if first.Snapshot.InputSHA256 == second.Snapshot.InputSHA256 || first.DeliveryID == second.DeliveryID {
		t.Fatal("different snapshots shared a binding")
	}
}

func TestValidateEnvelopeRejectsTampering(t *testing.T) {
	store := &fakeStore{}
	newTestService(t, nil, store, nil).Process(context.Background(), testHint())
	original := store.requests[0].Envelope
	tests := map[string]func(*DispatchEnvelope){
		"outer delivery":    func(v *DispatchEnvelope) { v.DeliveryID = "delivery_00000000000000000000000000000000" },
		"snapshot delivery": func(v *DispatchEnvelope) { v.Snapshot.DeliveryID = "delivery_00000000000000000000000000000000" },
		"ticket content":    func(v *DispatchEnvelope) { v.Snapshot.Untrusted.Description += "tampered" },
		"digest":            func(v *DispatchEnvelope) { v.Snapshot.InputSHA256 = strings.Repeat("0", 64) },
		"target":            func(v *DispatchEnvelope) { v.Snapshot.Target.RepositoryID++ },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			value := original
			mutate(&value)
			if ValidateEnvelope(value) == nil {
				t.Fatal("tampering was accepted")
			}
		})
	}
}

func TestProcessDoesNotLogExternalErrorOrTicketBody(t *testing.T) {
	const sentinel = "SENTINEL-SHOULD-NOT-LEAK"
	activity := testActivity()
	activity.Description += "\n" + sentinel
	buffer := &bytes.Buffer{}
	result := newTestService(t, &fakeBacklog{activity: activity, issue: testIssue(), issueErr: errors.New(sentinel)}, nil, buffer).Process(context.Background(), testHint())
	if result.Decision != DecisionRetryRequested || strings.Contains(buffer.String(), sentinel) {
		t.Fatalf("result=%+v log=%q", result, buffer.String())
	}
}

func TestConfigFailsClosed(t *testing.T) {
	tests := map[string]func(*Config){
		"space": func(c *Config) { c.SpaceKey = "" }, "project id": func(c *Config) { c.ProjectID = 0 },
		"creator": func(c *Config) { c.AllowedCreatorID = 0 }, "run id": func(c *Config) { c.ExpectedRunID = "" },
		"target repository": func(c *Config) { c.Target.RepositoryID = 0 },
		"target workflow":   func(c *Config) { c.Target.WorkflowRefSHA256 = "" }, "size": func(c *Config) { c.MaxEnvelopeBytes = 0 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			config := testConfig()
			mutate(&config)
			if config.Validate() == nil {
				t.Fatal("invalid configuration was accepted")
			}
		})
	}
}

func TestValidateEnvelopeBindsTheClarificationToTheSameInput(t *testing.T) {
	clarification := clarificationTestRecord(t)
	sealed, err := MarshalClarificationRecord(clarification)
	if err != nil {
		t.Fatalf("MarshalClarificationRecord() error = %v", err)
	}
	snapshot := TicketSnapshot{
		SchemaVersion: SnapshotSchemaVersion,
		SpaceKey:      "example-space", ActivityID: 9001, ActivityType: 1, ProjectID: 101, ProjectKey: "TICKET",
		IssueID: 8001, IssueKey: "TICKET-501", IssueKeyID: 501, CreatorID: 202,
		RunID: testRunID, CreatedAt: testTime, Target: testTarget(),
		Untrusted: UntrustedTicketData{Summary: "summary", Description: "description"},
	}
	base, err := SealSnapshot(snapshot)
	if err != nil {
		t.Fatalf("SealSnapshot() error = %v", err)
	}
	plain, err := json.Marshal(base)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if strings.Contains(string(plain), "clarification_json") {
		t.Fatal("a never-resumed envelope must not mention the clarification field")
	}

	// A clarification for a different delivery or input never validates.
	carried := base
	carried.ClarificationJSON = string(sealed)
	if err := ValidateEnvelope(carried); err == nil {
		t.Fatal("a foreign clarification was accepted")
	}
	carried.ClarificationJSON = `{"forged":true}`
	if err := ValidateEnvelope(carried); err == nil {
		t.Fatal("a corrupted clarification was accepted")
	}
}

func TestProcessRequiresTheOptInCategory(t *testing.T) {
	marked := testIssue()
	marked.CategoryIDs = []int64{31, 77}
	unmarked := testIssue()
	unmarked.CategoryIDs = []int64{31}
	bare := testIssue()

	tests := map[string]struct {
		required int64
		issue    CanonicalIssue
		decision Decision
		code     string
	}{
		"gate off queues an unmarked issue":  {0, bare, DecisionAccepted, "queue_created"},
		"marked issue passes the gate":       {77, marked, DecisionAccepted, "queue_created"},
		"other categories do not pass":       {77, unmarked, DecisionIgnored, "category_not_allowed"},
		"issue without categories is denied": {77, bare, DecisionIgnored, "category_not_allowed"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			config := testConfig()
			config.RequiredCategoryID = test.required
			store := &fakeStore{}
			service, err := NewService(config, &fakeBacklog{activity: testActivity(), issue: test.issue}, store, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
			if err != nil {
				t.Fatalf("NewService() error = %v", err)
			}
			service.now = func() time.Time { return testTime }
			result := service.Process(context.Background(), testHint())
			if result.Decision != test.decision || result.Code != test.code {
				t.Fatalf("Process() = %s/%s, want %s/%s", result.Decision, result.Code, test.decision, test.code)
			}
			queued := len(store.requests) > 0
			if queued != (test.decision == DecisionAccepted) {
				t.Fatalf("enqueued = %d for decision %s", len(store.requests), test.decision)
			}
		})
	}
}

func TestConfigRejectsNegativeRequiredCategory(t *testing.T) {
	config := testConfig()
	config.RequiredCategoryID = -1
	if config.Validate() == nil {
		t.Fatal("negative required category id was accepted")
	}
}

// A ticket filed without the opt-in category is ignored conclusively: adding
// the category afterwards fires no accepted activity, so nothing re-ingests it
// on its own. The documented recovery is to mark the issue and replay the
// creation webhook - the gate reads the issue's current categories, so the
// replayed delivery must queue. This test pins that recovery contract.
func TestProcessReplayAdmitsAnIssueMarkedAfterCreation(t *testing.T) {
	config := testConfig()
	config.RequiredCategoryID = 77
	backlog := &fakeBacklog{activity: testActivity(), issue: testIssue()}
	store := &fakeStore{}
	service, err := NewService(config, backlog, store, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	service.now = func() time.Time { return testTime }

	first := service.Process(context.Background(), testHint())
	if first.Decision != DecisionIgnored || first.Code != "category_not_allowed" || len(store.requests) != 0 {
		t.Fatalf("unmarked issue: %s/%s, enqueued=%d", first.Decision, first.Code, len(store.requests))
	}

	backlog.issue.CategoryIDs = []int64{77}
	replayed := service.Process(context.Background(), testHint())
	if replayed.Decision != DecisionAccepted || replayed.Code != "queue_created" || len(store.requests) != 1 {
		t.Fatalf("replay after marking: %s/%s, enqueued=%d", replayed.Decision, replayed.Code, len(store.requests))
	}
}

type fakeDispatcher struct {
	calls int
	err   error
}

func (f *fakeDispatcher) DispatchWork(_ context.Context) error {
	f.calls++
	return f.err
}

func TestProcessDispatchesOnlyWhenAQueueIsCreated(t *testing.T) {
	tests := map[string]struct {
		disposition QueueDisposition
		wantCalls   int
	}{
		"created wakes the worker": {QueueCreated, 1},
		"duplicate does not":       {QueueDuplicate, 0},
		"already claimed does not": {QueueClaimed, 0},
		"conflicting does not":     {QueueConflict, 0},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			dispatcher := &fakeDispatcher{}
			store := &fakeStore{disposition: test.disposition}
			service := newTestService(t, nil, store, nil)
			service.UseDispatcher(dispatcher)
			service.Process(context.Background(), testHint())
			if dispatcher.calls != test.wantCalls {
				t.Fatalf("dispatch calls = %d, want %d", dispatcher.calls, test.wantCalls)
			}
		})
	}
}

func TestProcessKeepsItsDecisionWhenTheDispatchFails(t *testing.T) {
	dispatcher := &fakeDispatcher{err: errors.New("github is down")}
	store := &fakeStore{disposition: QueueCreated}
	logBuffer := &bytes.Buffer{}
	service := newTestService(t, nil, store, logBuffer)
	service.UseDispatcher(dispatcher)
	result := service.Process(context.Background(), testHint())
	if result.Decision != DecisionAccepted || result.Code != "queue_created" {
		t.Fatalf("Process() = %s/%s after a dispatch failure", result.Decision, result.Code)
	}
	if !strings.Contains(logBuffer.String(), "dispatch_failed") {
		t.Fatal("a failed dispatch left no log trail")
	}
}

type fakeTick struct {
	calls    int
	result   Result
	lastTick QuestionTickRequest
}

func (f *fakeTick) ProcessQuestionTick(_ context.Context, request QuestionTickRequest) Result {
	f.calls++
	f.lastTick = request
	return f.result
}

func commentHint() WebhookHint {
	hint := testHint()
	hint.ActivityType = commentActivityType
	return hint
}

// A comment from the answerer is a wake-up call: the tick runs with the
// service's own identity, and a resumed question dispatches the worker at
// once. The comment body itself is never interpreted here.
func TestProcessTreatsAnAnswerCommentAsATickSignal(t *testing.T) {
	tick := &fakeTick{result: Result{Decision: DecisionAccepted, Code: "question_tick_resumed", DeliveryID: "delivery_x"}}
	dispatcher := &fakeDispatcher{}
	store := &fakeStore{}
	service := newTestService(t, nil, store, nil)
	service.UseAnswerSignal(tick)
	service.UseDispatcher(dispatcher)

	result := service.Process(context.Background(), commentHint())

	if result.Decision != DecisionAccepted || result.Code != "answer_signal_resumed" {
		t.Fatalf("Process() = %s/%s", result.Decision, result.Code)
	}
	if tick.calls != 1 || tick.lastTick.Protocol != QuestionTickProtocol ||
		tick.lastTick.AutomationRunID != service.config.ExpectedRunID {
		t.Fatalf("tick call = %d, request = %+v", tick.calls, tick.lastTick)
	}
	if dispatcher.calls != 1 {
		t.Fatalf("dispatch calls = %d, want 1", dispatcher.calls)
	}
	if len(store.requests) != 0 {
		t.Fatal("a comment signal must never enqueue a ticket")
	}
}

func TestProcessAnswerSignalWithoutAResumeStaysQuiet(t *testing.T) {
	tick := &fakeTick{result: Result{Decision: DecisionAccepted, Code: "question_tick_idle"}}
	dispatcher := &fakeDispatcher{}
	service := newTestService(t, nil, &fakeStore{}, nil)
	service.UseAnswerSignal(tick)
	service.UseDispatcher(dispatcher)

	result := service.Process(context.Background(), commentHint())

	if result.Code != "answer_signal_ticked" || dispatcher.calls != 0 {
		t.Fatalf("result = %+v, dispatch calls = %d", result, dispatcher.calls)
	}
}

func TestProcessAnswerSignalChecksTheAllowlists(t *testing.T) {
	strangers := map[string]func(*WebhookHint){
		"another creator":     func(h *WebhookHint) { h.CreatorID = 999 },
		"another project":     func(h *WebhookHint) { h.ProjectID = 999 },
		"another project key": func(h *WebhookHint) { h.ProjectKey = "OTHER" },
	}
	for name, mutate := range strangers {
		t.Run(name, func(t *testing.T) {
			tick := &fakeTick{}
			service := newTestService(t, nil, &fakeStore{}, nil)
			service.UseAnswerSignal(tick)
			hint := commentHint()
			mutate(&hint)
			result := service.Process(context.Background(), hint)
			if result.Decision != DecisionIgnored || tick.calls != 0 {
				t.Fatalf("result = %+v, tick calls = %d", result, tick.calls)
			}
		})
	}
}

// Without the signal wired, comments keep their old fate: rejected by the
// activity-type allowlist after the API check, exactly as before.
func TestProcessWithoutAnswerSignalIgnoresComments(t *testing.T) {
	activity := testActivity()
	activity.Type = commentActivityType
	service := newTestService(t, &fakeBacklog{activity: activity, issue: testIssue()}, &fakeStore{}, nil)
	hint := commentHint()
	result := service.Process(context.Background(), hint)
	if result.Decision != DecisionIgnored {
		t.Fatalf("result = %+v", result)
	}
}

// A failing tick is wrapped as an accepted signal: the webhook sender gets no
// retry-bait, the inner code goes to the log, and nothing is dispatched. The
// schedule remains the safety net for whatever the tick could not do.
func TestProcessAnswerSignalWrapsATickFailure(t *testing.T) {
	tick := &fakeTick{result: Result{Decision: DecisionRetryRequested, Code: "question_tick_load"}}
	dispatcher := &fakeDispatcher{}
	service := newTestService(t, nil, &fakeStore{}, nil)
	service.UseAnswerSignal(tick)
	service.UseDispatcher(dispatcher)

	result := service.Process(context.Background(), commentHint())

	if result.Decision != DecisionAccepted || result.Code != "answer_signal_ticked" || dispatcher.calls != 0 {
		t.Fatalf("result = %+v, dispatch calls = %d", result, dispatcher.calls)
	}
}

func TestConfigRejectsCommentTypeAsAllowedActivity(t *testing.T) {
	config := testConfig()
	config.AllowedActivityType = commentActivityType
	if config.Validate() == nil {
		t.Fatal("comment activity type was accepted as the ticket activity type")
	}
}

// A deleted issue answers 404 forever: that is a settled "nothing to process",
// not a dependency outage. Every other rejection keeps stalling so an auth
// outage can never advance past real tickets.
func TestProcessSettlesADeletedIssueAndStallsOnOtherRejections(t *testing.T) {
	deleted := newTestService(t, &fakeBacklog{
		activity: testActivity(),
		issueErr: NewExternalFailure("backlog", FailureRejected, "not_found"),
	}, &fakeStore{}, nil)
	if result := deleted.Process(context.Background(), testHint()); result.Decision != DecisionIgnored || result.Code != "issue_gone" {
		t.Fatalf("deleted issue: %+v", result)
	}

	authBroken := newTestService(t, &fakeBacklog{
		activity: testActivity(),
		issueErr: NewExternalFailure("backlog", FailureRejected, "authentication_failed"),
	}, &fakeStore{}, nil)
	if result := authBroken.Process(context.Background(), testHint()); result.Decision != DecisionDependencyFailed {
		t.Fatalf("auth failure: %+v", result)
	}
}
