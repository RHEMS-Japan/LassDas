package attendant

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
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

	_ "modernc.org/sqlite"
)

// One delivery, claimed, whose chain has finished: the publish card is done
// and the pull request is sealed. Everything past that is what this file
// measures, over a real ledger, a real report service and a board the test
// writes by hand.
type depthHarness struct {
	t          *testing.T
	config     runtime.Config
	services   *runtime.Services
	hermes     *runtime.Hermes
	runDir     string
	deliveryID string
	boardFile  string
	callsFile  string
	posted     *[]string
	comments   *[]backlogComment
	logger     *recordingLogger
}

// backlogComment is one comment as the tracker's API hands it back.
type backlogComment struct {
	ID          int64  `json:"id"`
	IssueID     int64  `json:"issueId"`
	Content     string `json:"content"`
	CreatedUser struct {
		ID int64 `json:"id"`
	} `json:"createdUser"`
	Created string `json:"created"`
}

// ticketComment is one comment on the delivery's ticket, by whoever wrote it.
func ticketComment(id, userID int64, content string) backlogComment {
	comment := backlogComment{ID: id, IssueID: depthIssueID, Content: content, Created: "2026-09-20T12:00:00Z"}
	comment.CreatedUser.ID = userID
	return comment
}

const (
	depthRunID       = "TICKET-901"
	depthIssueID     = int64(30)
	depthRequester   = int64(7)
	depthRepository  = "example/consumer"
	depthStagingHost = "https://staging.example.test"
	depthProdHost    = "https://www.example.test"
	depthMergeSHA    = "1111111111111111111111111111111111111111"
)

// newDepthHarness builds the delivery. delivery is what the destination
// asks for; deliverConfigured says whether this instance has the cards to
// carry it.
func newDepthHarness(t *testing.T, delivery string, deliverOn bool, goGate string) *depthHarness {
	t.Helper()
	root := t.TempDir()
	consumerConfig := filepath.Join(root, "consumer.json")
	body := fmt.Sprintf(`{"max_stages":3,"consumers":[{"repository":%q,"delivery":%q}]}`, depthRepository, delivery)
	if delivery == "" {
		body = fmt.Sprintf(`{"max_stages":3,"consumers":[{"repository":%q}]}`, depthRepository)
	}
	if err := os.WriteFile(consumerConfig, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	config := runtime.Config{
		ConsumerConfigPath: consumerConfig,
		Tracker: runtime.TrackerConfig{SpaceKey: "example", ProjectID: 42, ProjectKey: "TICKET",
			AllowedCreatorID: depthRequester, AllowedActivityType: 1},
		Identity: runtime.IdentityConfig{RepositoryID: 1, Repository: "o/r", WorkflowRef: "o/r/wf@main", EngineSHA: strings.Repeat("a", 40)},
		Chain: runtime.ChainConfig{RunsRoot: filepath.Join(root, "runs"), Profiles: runtime.ChainProfiles{
			Implementer: "impl", ReviewA: "ra", ReviewB: "rb", Validate: "val", Publish: "pub",
		}},
	}
	if deliverOn {
		config.Chain.Deliver = runtime.DeliverConfig{
			ChecksProfile: "checks", IntegrateProfile: "integrate", PromoteProfile: "promote",
			EnabledAfter: "2026-09-01T00:00:00Z", GoGate: goGate,
		}
	}

	ledger := filepath.Join(root, "ledger.db")
	store, err := state.NewLocalStore(ledger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	envelope, err := hook.SealSnapshot(hook.TicketSnapshot{
		SchemaVersion: hook.SnapshotSchemaVersion, SpaceKey: "example", ActivityID: 9001, ActivityType: 1,
		ProjectID: 42, ProjectKey: "TICKET", IssueID: depthIssueID, IssueKey: depthRunID, IssueKeyID: 901,
		CreatorID: depthRequester, RunID: depthRunID, CreatedAt: time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC),
		Target:    config.Target(),
		Untrusted: hook.UntrustedTicketData{Summary: "s", Description: "d"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Enqueue(context.Background(), hook.QueueRequest{Envelope: envelope, QueuedAt: time.Date(2026, 9, 15, 0, 0, 1, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	// Claimed through the store, not by hand: the claim is what writes the
	// engine identity into the run row, and every report and run comment
	// this delivery posts is refused unless the row carries it.
	claimedAt := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	if _, disposition, err := store.Pull(context.Background(), hook.PullClaimRequest{
		SpaceKey: "example", ProjectID: 42, ProjectKey: "TICKET",
		AllowedCreatorID: depthRequester, AllowedActivityType: 1, RunID: depthRunID,
		Target: config.Target(), Owner: config.Owner(chainOwnerRunID(envelope.DeliveryID)),
		IssuedAt: claimedAt, ClaimedAt: claimedAt, ClockSkew: 2 * time.Minute,
	}); err != nil || disposition != hook.PullAcquired {
		t.Fatalf("Pull() = %v, %v", disposition, err)
	}

	runDir := runDirectory(config, envelope.DeliveryID)
	if err := os.MkdirAll(filepath.Join(runDir, "history", "readiness"), 0o700); err != nil {
		t.Fatal(err)
	}
	encodedEnvelope, _ := json.Marshal(envelope)
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(runDir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("ticket-envelope.json", string(encodedEnvelope))
	write("ticket-draft.json", fmt.Sprintf(`{"repository":%q}`, depthRepository))
	write("history/readiness/decision.json", `{"request_kind":"change","needs_design":false}`)
	write("feature-pr.json", `{"payload":{"pull_request":{"HTMLURL":"https://github.com/example/consumer/pull/9"}}}`)
	write(runner.ChainOutcomeFile, `{"stage":1,"evidence":{"pull_request_url":"https://github.com/example/consumer/pull/9"}}`)
	write("m1-trail.txt", "trail\n")

	posted := &[]string{}
	comments := &[]backlogComment{}
	client, err := backlog.NewClient(backlog.Config{SpaceKey: "example", APIKey: "k", Origin: "https://example.backlog.com",
		Timeout: time.Second, MaxResponseBytes: 1 << 20},
		roundTripFunc(func(r *http.Request) (*http.Response, error) {
			switch {
			case r.Method == http.MethodPost:
				// The ticket keeps what was posted to it: the exactly-once
				// machinery finds its own comment by marker on the next
				// pass, so a fake that forgot them would post twice.
				body, _ := io.ReadAll(r.Body)
				form, _ := url.ParseQuery(string(body))
				content := form.Get("content")
				*posted = append(*posted, content)
				id := int64(900 + len(*posted))
				*comments = append(*comments, ticketComment(id, 1, content))
				encoded, _ := json.Marshal(map[string]any{"id": id, "issueId": depthIssueID,
					"content": content, "createdUser": map[string]any{"id": 1}, "created": "2026-09-20T12:00:00Z"})
				return jsonResponse(201, string(encoded)), nil
			case strings.HasSuffix(r.URL.Path, "/comments"):
				encoded, _ := json.Marshal(*comments)
				return jsonResponse(200, string(encoded)), nil
			}
			return jsonResponse(200, "[]"), nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	slogger := slog.New(slog.NewTextHandler(io.Discard, nil))
	target := config.Target()
	hookService, err := hook.NewService(hook.Config{
		SpaceKey: "example", ProjectID: 42, ProjectKey: "TICKET", AllowedCreatorID: depthRequester, AllowedActivityType: 1,
		RunMarker: "Automation-Run-ID", ExpectedRunID: depthRunID, Target: target, MaxEnvelopeBytes: 60 * 1024,
	}, client, store, slogger)
	if err != nil {
		t.Fatal(err)
	}
	route := hook.ReportRouteConfig{
		HMACKey: []byte(strings.Repeat("k", 32)), RepositoryID: 1,
		RepositorySHA256: hook.HashIdentity("o/r"), WorkflowRefSHA256: hook.HashIdentity("o/r/wf@main"),
		ExpectedRunID: depthRunID,
		Destinations: []hook.ReportDestination{{Repository: depthRepository, Delivery: destinationDelivery(delivery),
			StagingOrigin: depthStagingHost, ProductionOrigin: depthProdHost}},
		ClockSkew: 2 * time.Minute, LeaseDuration: 2 * time.Minute,
		SpaceKey: "example", ProjectID: 42, ProjectKey: "TICKET", AllowedCreatorID: depthRequester, AllowedActivityType: 1,
		Target: target, RunReferenceScheme: "local",
	}
	reportService, err := hook.NewTerminalReportService(route, store, client, slogger)
	if err != nil {
		t.Fatal(err)
	}
	if deliverOn {
		after, _ := config.Chain.Deliver.EnabledAfterTime()
		reportService.UseAutomaticDeliveryAfter(after)
	}
	tick, err := hook.NewQuestionTickService(route, store, client, reportService, hookService, readingStub{}, slogger)
	if err != nil {
		t.Fatal(err)
	}

	boardFile := filepath.Join(root, "board.json")
	callsFile := filepath.Join(root, "calls.log")
	counter := filepath.Join(root, "count")
	bin := filepath.Join(root, "hermes")
	script := `#!/bin/sh
{ printf '%s|' "$@"; echo; } >> "` + callsFile + `"
case "$2" in
  list) cat "` + boardFile + `" ;;
  create)
    n=$(cat "` + counter + `" 2>/dev/null || echo 0)
    n=$((n+1)); echo "$n" > "` + counter + `"
    printf '{"id":"t_d%s"}\n' "$n"
    ;;
  *) : ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	harness := &depthHarness{
		t: t, config: config, hermes: runtime.NewHermes(runtime.Config{HermesBin: bin, HermesBoard: "board"}),
		runDir: runDir, deliveryID: envelope.DeliveryID, boardFile: boardFile, callsFile: callsFile,
		posted: posted, comments: comments, logger: &recordingLogger{},
		services: &runtime.Services{Store: store, Backlog: client, Report: reportService, Tick: tick, Route: route},
	}
	harness.setBoard()
	return harness
}

func destinationDelivery(delivery string) string {
	if delivery == "" {
		return hook.DeliverProduction
	}
	return delivery
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(body))}
}

// setBoard writes the chain (publish done) plus whatever delivery cards the
// test has asked for.
func (h *depthHarness) setBoard(deliverCards ...runtime.BoardTask) {
	h.t.Helper()
	tasks := []runtime.BoardTask{}
	for _, stage := range []string{runtime.StageImplement, runtime.StageReviewA, runtime.StageReviewB, runtime.StageValidate, runtime.StagePublish} {
		tasks = append(tasks, runtime.BoardTask{ID: "t_" + stage, Status: "done",
			IdempotencyKey: runtime.ChainCardKey(h.deliveryID, stage, 1)})
	}
	tasks = append(tasks, deliverCards...)
	encoded, _ := json.Marshal(tasks)
	if err := os.WriteFile(h.boardFile, encoded, 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// card is one delivery card on the board, in the state a test needs it in.
func (h *depthHarness) card(stage, status string, attempt int) runtime.BoardTask {
	return runtime.BoardTask{ID: "t_" + stage, Status: status,
		IdempotencyKey: deliverCardKeyAt(h.deliveryID, stage, attempt)}
}

func (h *depthHarness) tick() {
	h.t.Helper()
	if err := SyncChains(context.Background(), h.config, h.services, h.hermes, h.logger); err != nil {
		h.t.Fatalf("SyncChains: %v (log: %v)", err, h.logger.lines)
	}
}

func (h *depthHarness) write(name, content string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.runDir, name), []byte(content), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// sealPhase writes the record a delivery card would have sealed.
func (h *depthHarness) sealPhase(file string, report runner.DeliverReport) {
	h.t.Helper()
	report.SchemaVersion = 1
	if report.ObservedAt.IsZero() {
		report.ObservedAt = time.Now().UTC()
	}
	encoded, _ := json.Marshal(report)
	h.write(file, string(encoded))
}

func (h *depthHarness) calls() string {
	raw, err := os.ReadFile(h.callsFile)
	if err != nil {
		return ""
	}
	return string(raw)
}

// runRow is the ledger's account of the delivery: its state and its ending.
func (h *depthHarness) runRow() state.RunOverview {
	h.t.Helper()
	runs, err := h.services.Store.ScanRuns(context.Background())
	if err != nil || len(runs) != 1 {
		h.t.Fatalf("ScanRuns() = %v, %v", runs, err)
	}
	return runs[0]
}

func (h *depthHarness) stagingPass() runner.DeliverReport {
	return runner.DeliverReport{Phase: "staging", Verdict: "pass", ScreenChecked: true,
		MergedSHA: depthMergeSHA, TargetURL: depthStagingHost + "/feature"}
}

func (h *depthHarness) productionPass() runner.DeliverReport {
	return runner.DeliverReport{Phase: "production", Verdict: "pass", ScreenChecked: true,
		TargetURL: depthProdHost + "/feature"}
}

// A destination that asks for production is carried there by the delivery
// itself: the run stays claimed through the CI wait, the merge, the staging
// observation and the promotion, and the success it finally reports carries
// the production page that was looked at.
func TestProductionDeliveryEndsWithProductionEvidence(t *testing.T) {
	h := newDepthHarness(t, "production", true, "")

	h.tick()
	if row := h.runRow(); row.State != "claimed" {
		t.Fatalf("the run ended at its pull request: state=%s code=%s", row.State, row.TerminalCode)
	}
	if !strings.Contains(h.calls(), "deliver:checks") {
		t.Fatalf("no checks card was created: %s", h.calls())
	}

	h.setBoard(h.card(deliverStageChecks, "done", 1))
	h.write(runner.DeliverChecksFile, `{"ok":true}`)
	h.tick()
	if !strings.Contains(h.calls(), "deliver:integrate") {
		t.Fatalf("no integrate card was created: %s", h.calls())
	}

	h.setBoard(h.card(deliverStageChecks, "done", 1), h.card(deliverStageIntegrate, "done", 1))
	h.sealPhase(runner.DeliverStagingReportFile, h.stagingPass())
	h.tick() // posts the staging report
	h.tick() // promotes without a Go
	if !strings.Contains(h.calls(), "deliver:promote") {
		t.Fatalf("no promote card was created without a Go: %s", h.calls())
	}
	if row := h.runRow(); row.State != "claimed" {
		t.Fatalf("the run ended before production: state=%s code=%s", row.State, row.TerminalCode)
	}

	h.setBoard(h.card(deliverStageChecks, "done", 1), h.card(deliverStageIntegrate, "done", 1), h.card(deliverStagePromote, "done", 1))
	h.sealPhase(runner.DeliverProductionReportFile, h.productionPass())
	h.tick() // posts the release report
	h.tick() // reports the success

	row := h.runRow()
	if row.State != "terminal" || row.TerminalCode != string(hook.TerminalSuccess) {
		t.Fatalf("run = %s / %s, want a terminal success (log: %v)", row.State, row.TerminalCode, h.logger.lines)
	}
	reached, evidence, _ := deliveryOutcome(h.runDir, depthRepository, depthPlanFor(t, h.runDir),
		map[string]string{"pull_request_url": "https://github.com/example/consumer/pull/9"})
	if reached != "production" {
		t.Fatalf("reached = %q, want production", reached)
	}
	for _, key := range []string{"commit_sha", "commit_url", "staging_evidence_url", "production_evidence_url"} {
		if evidence[key] == "" {
			t.Fatalf("the success carries no %s: %v", key, evidence)
		}
	}
	if evidence["production_evidence_url"] != depthProdHost+"/feature" {
		t.Fatalf("production evidence = %q", evidence["production_evidence_url"])
	}
}

func depthPlanFor(t *testing.T, runDir string) depthPlan {
	t.Helper()
	plan, ok := readDepthRecord(runDir)
	if !ok {
		t.Fatal("no delivery depth was recorded")
	}
	return plan
}

// go_gate: required brings back exactly what the promotion did before: the
// staging report goes up, and nothing is promoted until the requester's own
// 「Go」 is on the ticket.
func TestGoGateRequiredWaitsForTheRequestersGo(t *testing.T) {
	h := newDepthHarness(t, "production", true, runtime.GoGateRequired)
	h.setBoard(h.card(deliverStageChecks, "done", 1), h.card(deliverStageIntegrate, "done", 1))
	h.write(runner.DeliverChecksFile, `{"ok":true}`)
	h.sealPhase(runner.DeliverStagingReportFile, h.stagingPass())
	h.tick()
	h.tick()
	if strings.Contains(h.calls(), "deliver:promote") {
		t.Fatalf("the promotion ran without a Go while the gate was required: %s", h.calls())
	}
	if row := h.runRow(); row.State != "claimed" {
		t.Fatalf("the run ended while waiting for the Go: %s / %s", row.State, row.TerminalCode)
	}

	marker := hook.CommentMarker(string(hook.RunCommentStagingReport), depthRunID)
	*h.comments = append(*h.comments,
		ticketComment(950, depthRequester, "ステージング確認\n"+marker),
		ticketComment(951, depthRequester, "Go"))
	h.tick()
	if !strings.Contains(h.calls(), "deliver:promote") {
		t.Fatalf("the Go did not promote: %s", h.calls())
	}
}

// The requester's stop is honoured in the middle of the depth: nothing is
// promoted and the delivery ends as cancelled rather than as a success that
// never reached production.
func TestAStopMidDepthEndsTheDeliveryAsCancelled(t *testing.T) {
	h := newDepthHarness(t, "production", true, "")
	h.setBoard(h.card(deliverStageChecks, "done", 1), h.card(deliverStageIntegrate, "done", 1))
	h.write(runner.DeliverChecksFile, `{"ok":true}`)
	h.sealPhase(runner.DeliverStagingReportFile, h.stagingPass())
	h.tick() // posts the staging report
	*h.comments = append(*h.comments, ticketComment(952, depthRequester, "停止"))
	h.tick()

	row := h.runRow()
	if row.TerminalCode != string(hook.TerminalCancelled) {
		t.Fatalf("run = %s / %s, want cancelled (log: %v)", row.State, row.TerminalCode, h.logger.lines)
	}
	if strings.Contains(h.calls(), "deliver:promote") {
		t.Fatalf("a promote card was created after the stop: %s", h.calls())
	}
}

// A destination that asks only for a proposal is delivered exactly as it
// was: the success is reported the moment the publish card is done, and no
// delivery card is ever created — even on an instance that has them.
func TestPullRequestConsumerStopsAtTheProposal(t *testing.T) {
	h := newDepthHarness(t, "pull_request", true, "")
	h.tick()
	row := h.runRow()
	if row.State != "terminal" || row.TerminalCode != string(hook.TerminalSuccess) {
		t.Fatalf("run = %s / %s, want a terminal success (log: %v)", row.State, row.TerminalCode, h.logger.lines)
	}
	if strings.Contains(h.calls(), ":deliver:") {
		t.Fatalf("a delivery card was created for a proposal-only destination: %s", h.calls())
	}
}

// A destination that asks for production on an instance with no release
// path is delivered to its pull request and says what the rest would have
// needed. It never fails: the change is real and proposed, and the
// difference is a configuration an operator can supply.
func TestProductionWithoutAReleasePathReachesThePullRequest(t *testing.T) {
	h := newDepthHarness(t, "production", false, "")
	h.tick()

	row := h.runRow()
	if row.TerminalCode != string(hook.TerminalSuccess) {
		t.Fatalf("run = %s / %s, want a success rather than a failure code (log: %v)", row.State, row.TerminalCode, h.logger.lines)
	}
	plan := depthPlanFor(t, h.runDir)
	if plan.Configured != "production" || plan.Reached != "pull_request" || !plan.short() {
		t.Fatalf("depth record = %+v", plan)
	}
	if len(plan.Missing) == 0 || !strings.Contains(strings.Join(plan.Missing, " "), "chain.deliver") {
		t.Fatalf("the record does not say what is missing: %+v", plan)
	}
	reached, evidence, shortfall := deliveryOutcome(h.runDir, depthRepository, plan,
		map[string]string{"pull_request_url": "https://github.com/example/consumer/pull/9"})
	if reached != "pull_request" || shortfall == "" {
		t.Fatalf("reached = %q shortfall = %q", reached, shortfall)
	}
	if evidence["commit_sha"] != "" || evidence["staging_evidence_url"] != "" || evidence["production_evidence_url"] != "" {
		t.Fatalf("a proposal-only delivery claimed a deployment: %v", evidence)
	}
	if evidence["delivery_shortfall"] == "" {
		t.Fatalf("the report carries no shortfall line: %v", evidence)
	}
}

// A staging observation that did not pass is a failure like any other: no
// report goes up saying the delivery is over, the records of the steps that
// have to happen again are dropped, the card is retired, and the phase is
// dispatched a second time under its own key.
func TestAFailedObservationIsClimbedRatherThanReported(t *testing.T) {
	h := newDepthHarness(t, "production", true, "")
	h.config.Chain.RetryBackoffBaseSeconds = 1
	h.setBoard(h.card(deliverStageChecks, "done", 1), h.card(deliverStageIntegrate, "done", 1))
	h.write(runner.DeliverChecksFile, `{"ok":true}`)
	h.write(runner.DeliverMergeFile, `{"payload":{"merge":{"MergeSHA":"`+depthMergeSHA+`"}}}`)
	h.write(runner.DeliverStagingVisibleFile, `{"final_url":"x"}`)
	h.sealPhase(runner.DeliverStagingReportFile, runner.DeliverReport{Phase: "staging", Verdict: "observe_failed",
		TargetURL: depthStagingHost + "/feature"})

	// The ladder has no hand for an unnamed failure, so the first pass
	// enters the wait; the second, once the wait is over, dispatches.
	h.tick()
	if row := h.runRow(); row.TerminalCode != "" {
		t.Fatalf("a failed observation ended the delivery as %q", row.TerminalCode)
	}
	if len(*h.posted) != 0 {
		t.Fatalf("a failed observation was reported to the ticket: %d comments", len(*h.posted))
	}
	record := readLadderRecord(h.runDir, deliverStageIntegrate, deliverLadderRound)
	if record.LadderStep != rungWait {
		t.Fatalf("the phase did not reach the waiting rung: %+v", record)
	}

	time.Sleep(1100 * time.Millisecond)
	h.tick()
	if deliverFileExists(h.runDir, runner.DeliverStagingReportFile) {
		t.Fatal("the refused staging record survived the rebuild")
	}
	if deliverFileExists(h.runDir, runner.DeliverStagingVisibleFile) {
		t.Fatal("the observation that failed was not dropped")
	}
	if !deliverFileExists(h.runDir, runner.DeliverMergeFile) {
		t.Fatal("the merge that landed was dropped; it would be attempted again and refused")
	}
	if deliverAttempt(h.runDir, deliverStageIntegrate) != 2 {
		t.Fatalf("the phase is still on attempt %d", deliverAttempt(h.runDir, deliverStageIntegrate))
	}

	// The retired card is still on the board (archived cards stay in the
	// listing), and the next tick issues the second attempt beside it.
	h.setBoard(h.card(deliverStageChecks, "done", 1), h.card(deliverStageIntegrate, "archived", 1))
	h.tick()
	if !strings.Contains(h.calls(), deliverCardKeyAt(h.deliveryID, deliverStageIntegrate, 2)) {
		t.Fatalf("the second attempt was not dispatched: %s", h.calls())
	}
	if row := h.runRow(); row.TerminalCode != "" {
		t.Fatalf("the delivery ended as %q while the ladder was climbing", row.TerminalCode)
	}
}

// The delivery cards' keys must stay out of the chain's namespace, attempts
// included: the five-stage machinery filters the board by chain key, and a
// key it parsed would drag a delivery card into round archiving.
func TestDeliverAttemptKeysStayOutsideTheChainNamespace(t *testing.T) {
	const deliveryID = "delivery_0123456789abcdef0123456789abcdef"
	for _, stage := range []string{deliverStageChecks, deliverStageIntegrate, deliverStagePromote} {
		for attempt := 1; attempt <= 4; attempt++ {
			key := deliverCardKeyAt(deliveryID, stage, attempt)
			if _, _, _, ok := runtime.ParseChainCardKey(key); ok {
				t.Fatalf("%s parses as a chain card key", key)
			}
		}
		if first := deliverCardKeyAt(deliveryID, stage, 1); first != deliverCardKey(deliveryID, stage) {
			t.Fatalf("the first attempt changed key: %s", first)
		}
		if deliverCardKeyAt(deliveryID, stage, 2) == deliverCardKeyAt(deliveryID, stage, 3) {
			t.Fatalf("two attempts share a key for %s", stage)
		}
	}
}

// What a second attempt has to produce again, and what it must not touch.
func TestDeliverRetryDropsKeepWhatLanded(t *testing.T) {
	cases := map[string]struct {
		stage, verdict string
		want, absent   []string
	}{
		"a refused observation redoes the observation": {
			deliverStageIntegrate, "observe_failed",
			[]string{runner.DeliverStagingReportFile, runner.DeliverStagingVisibleFile},
			[]string{runner.DeliverMergeFile, runner.DeliverChecksFile},
		},
		"a deployment that failed redoes the deployment": {
			deliverStageIntegrate, "deploy_failed",
			[]string{runner.DeliverStagingReportFile, runner.DeliverStagingProofFile},
			[]string{runner.DeliverMergeFile},
		},
		"a red gate redoes the wait": {
			deliverStageChecks, "checks_failed",
			[]string{runner.DeliverStagingReportFile, runner.DeliverChecksFile},
			[]string{runner.DeliverMergeFile},
		},
		"a refused production screen redoes that screen": {
			deliverStagePromote, "observe_failed",
			[]string{runner.DeliverProductionReportFile, runner.DeliverProductionVisibleFile},
			[]string{runner.DeliverPromotionMergeFile, runner.DeliverStagingReportFile},
		},
	}
	for name, c := range cases {
		drops := strings.Join(deliverRetryDrops(c.stage, c.verdict), " ")
		for _, want := range c.want {
			if !strings.Contains(drops, want) {
				t.Errorf("%s: %s is not dropped (%s)", name, want, drops)
			}
		}
		for _, absent := range c.absent {
			if strings.Contains(drops, absent) {
				t.Errorf("%s: %s must survive (%s)", name, absent, drops)
			}
		}
	}
}

// The depth is read from the destination, and an instance that cannot carry
// it says which settings would have.
func TestPlanDeliveryDepthNamesWhatIsMissing(t *testing.T) {
	root := t.TempDir()
	consumer := filepath.Join(root, "consumer.json")
	runDir := filepath.Join(root, "run")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "ticket-draft.json"), []byte(`{"repository":"a/b"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	claimed := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	run := state.RunOverview{ClaimedAt: claimed.UnixMilli()}
	deliverOn := runtime.ChainConfig{Deliver: runtime.DeliverConfig{
		ChecksProfile: "c", IntegrateProfile: "i", PromoteProfile: "p", EnabledAfter: "2026-09-01T00:00:00Z"}}

	cases := map[string]struct {
		consumerJSON   string
		chain          runtime.ChainConfig
		run            state.RunOverview
		configured     string
		reached        string
		missingMention string
	}{
		"production with the cards configured": {
			`{"consumers":[{"repository":"a/b","delivery":"production"}]}`, deliverOn, run, "production", "production", "",
		},
		"production with no cards": {
			`{"consumers":[{"repository":"a/b","delivery":"production"}]}`, runtime.ChainConfig{}, run, "production", "pull_request", "checks_profile",
		},
		"claimed before the cut-off": {
			`{"consumers":[{"repository":"a/b","delivery":"integration"}]}`, deliverOn,
			state.RunOverview{ClaimedAt: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC).UnixMilli()},
			"integration", "pull_request", "enabled_after",
		},
		"a proposal-only destination": {
			`{"consumers":[{"repository":"a/b","delivery":"pull_request"}]}`, deliverOn, run, "pull_request", "pull_request", "",
		},
		"a destination that says nothing goes the whole way": {
			`{"consumers":[{"repository":"a/b"}]}`, deliverOn, run, "production", "production", "",
		},
		"a command-line destination that says nothing proposes": {
			`{"consumers":[{"repository":"a/b","kind":"cli"}]}`, deliverOn, run, "pull_request", "pull_request", "",
		},
	}
	for name, c := range cases {
		if err := os.WriteFile(consumer, []byte(c.consumerJSON), 0o600); err != nil {
			t.Fatal(err)
		}
		plan, err := planDeliveryDepth(runtime.Config{ConsumerConfigPath: consumer, Chain: c.chain}, c.run, runDir)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if plan.Configured != c.configured || plan.Reached != c.reached {
			t.Errorf("%s: configured=%s reached=%s, want %s/%s", name, plan.Configured, plan.Reached, c.configured, c.reached)
		}
		if c.missingMention == "" {
			if len(plan.Missing) != 0 || plan.shortfallText() != "" {
				t.Errorf("%s: nothing should be missing, got %v", name, plan.Missing)
			}
			continue
		}
		if !strings.Contains(strings.Join(plan.Missing, " "), c.missingMention) {
			t.Errorf("%s: missing = %v, want a mention of %s", name, plan.Missing, c.missingMention)
		}
		if !strings.Contains(plan.shortfallText(), c.missingMention) {
			t.Errorf("%s: the ticket line does not name it: %q", name, plan.shortfallText())
		}
	}
}

// A promotion the gate could not fulfil stops the delivery at staging and
// says why, instead of asking for a production that could only fail.
func TestAPromotionHoldStopsAtStagingAndSaysWhy(t *testing.T) {
	root := t.TempDir()
	runDir := filepath.Join(root, "run")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	report := h1StagingHold()
	encoded, _ := json.Marshal(report)
	if err := os.WriteFile(filepath.Join(runDir, runner.DeliverStagingReportFile), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	plan := depthPlan{SchemaVersion: depthSchemaVersion, Configured: "production", Reached: "production"}
	reached, evidence, shortfall := deliveryOutcome(runDir, depthRepository, plan,
		map[string]string{"pull_request_url": "https://github.com/example/consumer/pull/9"})
	if reached != "integration" {
		t.Fatalf("reached = %q, want integration", reached)
	}
	if evidence["production_evidence_url"] != "" {
		t.Fatalf("a held promotion claimed production evidence: %v", evidence)
	}
	if !strings.Contains(shortfall, "分岐") {
		t.Fatalf("the hold is not carried to the ticket: %q", shortfall)
	}
}

func h1StagingHold() runner.DeliverReport {
	return runner.DeliverReport{SchemaVersion: 1, Phase: "staging", Verdict: "pass", ScreenChecked: true,
		MergedSHA: depthMergeSHA, TargetURL: depthStagingHost + "/feature",
		PromotionHold: "本番にはステージングに無い変更が入っています（分岐状態）。", ObservedAt: time.Now().UTC()}
}
