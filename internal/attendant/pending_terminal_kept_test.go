package attendant

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runner"
)

// CloseUnreproducibleTerminal is the ledger side of an ending that can no
// longer be rebuilt: the row is closed against the comment that reported
// it, without the digest a report would have to reproduce.
func (f *pendingFakeStore) CloseUnreproducibleTerminal(_ context.Context, request hook.TerminalRecoveryCloseRequest) (hook.TerminalCompleteDisposition, error) {
	f.recoveries = append(f.recoveries, request)
	for _, earlier := range f.recoveries[:len(f.recoveries)-1] {
		if earlier.CommentID == request.CommentID {
			return hook.TerminalAlreadyComplete, nil
		}
	}
	return hook.TerminalCompleted, nil
}

// keptPendingFixture is a run whose ending really happened: the report was
// decided, the closing comment written into the run directory, the ledger
// told and the ticket commented. Then everything after the run directory is
// taken away again, which is the state a pod that stopped mid-report leaves
// behind — the row pending, the comment kept, the ticket silent.
func keptPendingFixture(t *testing.T, code hook.TerminalCode, repository string, evidence map[string]string) pendingFixture {
	t.Helper()
	fixture := newPendingFixture(t, repository)
	fixture.writeRunDir(t, repository)
	runDir := runDirectory(fixture.config, fixture.deliveryID)
	envelope, err := pendingEnvelope(runDir, fixture.run)
	if err != nil {
		t.Fatal(err)
	}
	terminal := runner.NewTerminal(fixture.config, fixture.services, envelope,
		chainOwnerRunID(fixture.deliveryID), runDir, &pendingTestLogger{})
	outcome := runner.Outcome{Code: code, Evidence: evidence}
	digest, err := terminal.ReportDigest(context.Background(), code, outcome, repository)
	if err != nil {
		t.Fatal(err)
	}
	fixture.store.expected = digest
	fixture.run.TerminalCode = string(code)
	fixture.run.TerminalReportSHA256 = digest
	if err := terminal.Report(context.Background(), code, outcome, repository); err != nil {
		t.Fatal(err)
	}
	if len(fixture.comments.posted) != 1 {
		t.Fatalf("the ending posted %d comments, want one", len(fixture.comments.posted))
	}
	fixture.firstComment = fixture.comments.posted[0]
	// Everything the pod had done past the run directory is lost.
	fixture.comments.posted = nil
	fixture.store.begins, fixture.store.completes = 0, 0
	return fixture
}

// The audit's reproduction, inverted. A pending success whose chain cards
// are gone used to be rebuilt from those cards, fail, and wait for a person
// with nothing on the ticket. It now ends from the comment the run wrote
// down when it decided the ending: one comment, the same words, and the
// submission recorded in the ledger.
func TestAPendingSuccessWithoutItsCardsEndsFromTheCommentItKept(t *testing.T) {
	fixture := keptPendingFixture(t, hook.TerminalSuccess, "example/consumer", map[string]string{
		"pull_request_url": "https://github.com/example/consumer/pull/12",
	})
	logger := &pendingTestLogger{}
	for tick := 0; tick < 2; tick++ {
		if err := resubmitPendingTerminal(context.Background(), fixture.config, fixture.services, nil,
			fixture.run, chainViewFor(nil, fixture.deliveryID), logger); err != nil {
			t.Fatalf("tick %d: %v", tick+1, err)
		}
	}
	if len(fixture.comments.posted) != 1 {
		t.Fatalf("comments posted = %d across two ticks, want exactly one: %q", len(fixture.comments.posted), fixture.comments.posted)
	}
	if fixture.comments.posted[0] != fixture.firstComment {
		t.Fatalf("the re-sent comment is not the one the run decided:\nkept: %q\nsent: %q", fixture.firstComment, fixture.comments.posted[0])
	}
	if fixture.store.begins == 0 || fixture.store.completes == 0 {
		t.Fatalf("store begins/completes = %d/%d, want the submission recorded", fixture.store.begins, fixture.store.completes)
	}
	for _, digest := range fixture.store.digests {
		if digest != fixture.run.TerminalReportSHA256 {
			t.Fatalf("a report with another digest reached the store: %s", digest)
		}
	}
	if log := strings.Join(logger.lines, "\n"); strings.Contains(log, "cannot be rebuilt") {
		t.Fatalf("the rebuild was attempted for a run that kept its comment: %q", logger.lines)
	}
}

// The other half of the reproduction: a row whose digest nothing can
// reproduce. The kept comment belongs to a different ending, so it is not
// posted; the ending is reported from what the ledger holds instead, once,
// and the row is closed so the project's queue moves again.
func TestAPendingReportWhoseDigestCannotBeReproducedIsStillReported(t *testing.T) {
	fixture := keptPendingFixture(t, hook.TerminalModelFailed, "", nil)
	fixture.run.TerminalReportSHA256 = strings.Repeat("f", 64)
	logger := &pendingTestLogger{}
	for tick := 0; tick < 2; tick++ {
		if err := resubmitPendingTerminal(context.Background(), fixture.config, fixture.services, nil,
			fixture.run, chainViewFor(nil, fixture.deliveryID), logger); err != nil {
			t.Fatalf("tick %d: %v", tick+1, err)
		}
	}
	if len(fixture.comments.posted) != 1 {
		t.Fatalf("comments posted = %d across two ticks, want exactly one: %q", len(fixture.comments.posted), fixture.comments.posted)
	}
	comment := fixture.comments.posted[0]
	if !strings.Contains(comment, "model_failed") || !strings.Contains(comment, "復元できませんでした") {
		t.Fatalf("the comment does not say how the run ended and that the record was lost: %q", comment)
	}
	if !strings.Contains(comment, "本番の状態: 不明") {
		t.Fatalf("the comment claims to know the production state it cannot read: %q", comment)
	}
	if fixture.store.begins != 0 {
		t.Fatalf("a report was sent for a digest nothing reproduces: %v", fixture.store.digests)
	}
	if len(fixture.store.recoveries) != 2 {
		t.Fatalf("ledger closes = %d, want the submission recorded on both ticks", len(fixture.store.recoveries))
	}
	closed := fixture.store.recoveries[0]
	if closed.ReportSHA256 != fixture.run.TerminalReportSHA256 || closed.Code != hook.TerminalModelFailed ||
		closed.RunID != "TKT-4242" || closed.CommentID <= 0 {
		t.Fatalf("the row was closed against something else: %+v", closed)
	}
	if fixture.store.recoveries[1].CommentID != closed.CommentID {
		t.Fatalf("the second tick closed against another comment: %+v", fixture.store.recoveries[1])
	}
}

// The comment the ending decided is posted exactly as it was written, and
// the marker on its last line is what keeps a second attempt from posting
// it twice — the same marker the lost report carried.
func TestTheKeptClosingCommentIsPostedUnchangedAndOnlyOnce(t *testing.T) {
	fixture := keptPendingFixture(t, hook.TerminalModelFailed, "", nil)
	runDir := runDirectory(fixture.config, fixture.deliveryID)
	kept, ok := runner.ReadTerminalComment(runDir)
	if !ok {
		t.Fatal("the ending wrote down no closing comment")
	}
	if kept.Body != fixture.firstComment {
		t.Fatalf("the kept comment is not the posted one:\nkept: %q\nposted: %q", kept.Body, fixture.firstComment)
	}
	if kept.ReportSHA256 != fixture.run.TerminalReportSHA256 {
		t.Fatalf("the kept comment names digest %s, the row %s", kept.ReportSHA256, fixture.run.TerminalReportSHA256)
	}
	if kept.Marker != hook.ExtractCommentMarker(kept.Body) || !strings.Contains(kept.Marker, kept.ReportSHA256) {
		t.Fatalf("the kept marker is not the one on the comment: %q", kept.Marker)
	}
	logger := &pendingTestLogger{}
	if err := resubmitPendingTerminal(context.Background(), fixture.config, fixture.services, nil,
		fixture.run, chainViewFor(nil, fixture.deliveryID), logger); err != nil {
		t.Fatal(err)
	}
	// The tracker now holds it. A further attempt finds it by the marker.
	before := len(fixture.comments.posted)
	if err := resubmitPendingTerminal(context.Background(), fixture.config, fixture.services, nil,
		fixture.run, chainViewFor(nil, fixture.deliveryID), logger); err != nil {
		t.Fatal(err)
	}
	if before != 1 || len(fixture.comments.posted) != 1 {
		t.Fatalf("comments posted = %d then %d, want one that stays one", before, len(fixture.comments.posted))
	}
	if fixture.comments.posted[0] != kept.Body {
		t.Fatalf("the posted comment drifted from the kept one: %q", fixture.comments.posted[0])
	}
}

// A run that ended under an engine from before the comment was kept has no
// file to send, and is still rebuilt and re-sent exactly as it was.
func TestAnEndingFromAnEarlierEngineIsStillRebuiltAndResent(t *testing.T) {
	fixture := newPendingFixture(t, "")
	fixture.writeRunDir(t, "example/consumer")
	logger := &pendingTestLogger{}
	if _, kept := runner.ReadTerminalComment(runDirectory(fixture.config, fixture.deliveryID)); kept {
		t.Fatal("this fixture is meant to have no kept comment")
	}
	if err := resubmitPendingTerminal(context.Background(), fixture.config, fixture.services, nil,
		fixture.run, chainViewFor(nil, fixture.deliveryID), logger); err != nil {
		t.Fatal(err)
	}
	if len(fixture.comments.posted) != 1 || fixture.store.completes != 1 || len(fixture.store.recoveries) != 0 {
		t.Fatalf("comments = %d, completes = %d, recoveries = %d; want the rebuilt report completed once",
			len(fixture.comments.posted), fixture.store.completes, len(fixture.store.recoveries))
	}
	if !strings.Contains(fixture.comments.posted[0], "model_failed") {
		t.Fatalf("the rebuilt report is not this run's: %q", fixture.comments.posted[0])
	}
}

// A run directory that ends up holding a closing comment for some other
// ending than the row was begun with must not have it posted: the requester
// would be told about an ending this run never had. The ledger's own
// account is reported instead.
func TestAKeptCommentForAnotherEndingIsNotPosted(t *testing.T) {
	fixture := keptPendingFixture(t, hook.TerminalModelFailed, "", nil)
	kept, ok := runner.ReadTerminalComment(runDirectory(fixture.config, fixture.deliveryID))
	if !ok {
		t.Fatal("the ending wrote down no closing comment")
	}
	fixture.run.TerminalReportSHA256 = strings.Repeat("a", 64)
	logger := &pendingTestLogger{}
	if err := resubmitPendingTerminal(context.Background(), fixture.config, fixture.services, nil,
		fixture.run, chainViewFor(nil, fixture.deliveryID), logger); err != nil {
		t.Fatal(err)
	}
	if len(fixture.comments.posted) != 1 {
		t.Fatalf("comments = %d, want the ledger's account posted once", len(fixture.comments.posted))
	}
	if fixture.comments.posted[0] == kept.Body {
		t.Fatal("the kept comment was posted against a row it does not belong to")
	}
	if fixture.store.begins != 0 || len(fixture.store.recoveries) != 1 {
		t.Fatalf("begins = %d, recoveries = %d", fixture.store.begins, len(fixture.store.recoveries))
	}
}

// The words are bound to the ending, not merely marked with it. A kept
// file whose body was swapped for other text — keeping the marker line
// that makes it look like this report's — is refused, and the ticket gets
// the ledger's own account rather than the swapped words.
func TestAKeptCommentWhoseBodyWasSwappedIsNotPosted(t *testing.T) {
	fixture := keptPendingFixture(t, hook.TerminalModelFailed, "", nil)
	runDir := runDirectory(fixture.config, fixture.deliveryID)
	kept, ok := runner.ReadTerminalComment(runDir)
	if !ok {
		t.Fatal("the ending wrote down no closing comment")
	}
	marker := kept.Body[strings.LastIndex(strings.TrimRight(kept.Body, "\n"), "\n")+1:]
	swapped := "自動処理の最終結果: model_failed\n本番環境へ反映済みです。ご確認ください。\n" + marker
	// Only the body is touched: the report beside it, and so the digest
	// the ledger holds, is exactly what the run sealed. A generic re-encode
	// of the file would move the run id through a float and change the
	// digest, which would refuse the file for the wrong reason.
	edited := kept
	edited.Body = swapped
	raw, err := json.Marshal(edited)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, runner.TerminalCommentFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, readable := runner.ReadTerminalComment(runDir); readable {
		t.Fatal("a swapped body was read back as this run's closing comment")
	}
	logger := &pendingTestLogger{}
	if err := resubmitPendingTerminal(context.Background(), fixture.config, fixture.services, nil,
		fixture.run, chainViewFor(nil, fixture.deliveryID), logger); err != nil {
		t.Fatal(err)
	}
	if len(fixture.comments.posted) != 1 {
		t.Fatalf("comments = %d, want the ledger's account posted once", len(fixture.comments.posted))
	}
	if strings.Contains(fixture.comments.posted[0], "本番環境へ反映済みです") {
		t.Fatalf("the swapped words reached the ticket: %q", fixture.comments.posted[0])
	}
	// With the file refused the run falls to the older paths, and this
	// one's report can still be rebuilt, so the ticket gets that. Which of
	// the two speaks is not the point; that neither of them says what the
	// swapped file said is.
	if !strings.Contains(fixture.comments.posted[0], "model_failed") {
		t.Fatalf("the posted comment is not this run's ending: %q", fixture.comments.posted[0])
	}
	if fixture.store.begins != 1 && len(fixture.store.recoveries) != 1 {
		t.Fatalf("begins = %d, recoveries = %d; want the ending recorded once",
			fixture.store.begins, len(fixture.store.recoveries))
	}
}

// A deployment with a landing recorded says so rather than claiming
// production is untouched, which is what the seven-item footer would have
// said if the recovery comment reused the ordinary wording.
func TestTheRecoveredEndingNamesTheLandingWhenTheDeliveryRecordedOne(t *testing.T) {
	request := hook.TerminalRecoveryRequest{
		AutomationRunID: "TKT-4242", IssueID: 4242, Code: hook.TerminalCancelled,
		ReportSHA256: strings.Repeat("b", 64), Reached: hook.DeliverProduction,
	}
	comment := hook.TerminalRecoveredCommentContent(request)
	if !strings.Contains(comment, "本番環境への反映") || !strings.Contains(comment, "本番の状態: 確認済み") {
		t.Fatalf("a delivery that reached production is not described: %q", comment)
	}
	request.Reached = ""
	if unknown := hook.TerminalRecoveredCommentContent(request); !strings.Contains(unknown, "本番の状態: 不明") {
		t.Fatalf("an unrecorded landing is not admitted: %q", unknown)
	}
	if err := hook.ValidateCommentContract(comment, hook.ExtractCommentMarker(comment)); err != nil {
		t.Fatalf("the recovery comment breaks the comment contract: %v", err)
	}
}

// The engine reports the ending from the ledger only after it has posted
// the comment; a tracker it cannot reach leaves the row alone for the next
// tick rather than closing a run nobody was told about.
func TestTheRowIsNotClosedWhenTheCommentCouldNotBePosted(t *testing.T) {
	fixture := keptPendingFixture(t, hook.TerminalModelFailed, "", nil)
	fixture.run.TerminalReportSHA256 = strings.Repeat("f", 64)
	fixture.comments.addFailure = errDeliberate
	logger := &pendingTestLogger{}
	err := resubmitPendingTerminal(context.Background(), fixture.config, fixture.services, nil,
		fixture.run, chainViewFor(nil, fixture.deliveryID), logger)
	if err == nil {
		t.Fatal("an unreachable tracker was reported as an ending")
	}
	if len(fixture.store.recoveries) != 0 {
		t.Fatalf("the row was closed with nothing on the ticket: %+v", fixture.store.recoveries)
	}
	fixture.comments.addFailure = nil
	if err := resubmitPendingTerminal(context.Background(), fixture.config, fixture.services, nil,
		fixture.run, chainViewFor(nil, fixture.deliveryID), logger); err != nil {
		t.Fatal(err)
	}
	if len(fixture.comments.posted) != 1 || len(fixture.store.recoveries) != 1 {
		t.Fatalf("comments = %d, recoveries = %d after the tracker came back",
			len(fixture.comments.posted), len(fixture.store.recoveries))
	}
}

// A run directory the engine cannot write the comment into is not a
// delivery that ends in silence: the ending is still submitted, the failed
// write is reported, and the older rebuild carries any re-send.
func TestAnEndingIsStillSentWhenTheClosingCommentCannotBeWrittenDown(t *testing.T) {
	fixture := newPendingFixture(t, "")
	fixture.writeRunDir(t, "")
	runDir := runDirectory(fixture.config, fixture.deliveryID)
	// The path the record goes to is taken by a directory that cannot be
	// removed, so every attempt to write it fails.
	blocked := filepath.Join(runDir, runner.TerminalCommentFile)
	if err := os.MkdirAll(filepath.Join(blocked, "occupied"), 0o755); err != nil {
		t.Fatal(err)
	}
	envelope, err := pendingEnvelope(runDir, fixture.run)
	if err != nil {
		t.Fatal(err)
	}
	logger := &pendingTestLogger{}
	terminal := runner.NewTerminal(fixture.config, fixture.services, envelope,
		chainOwnerRunID(fixture.deliveryID), runDir, logger)
	if err := terminal.Report(context.Background(), hook.TerminalModelFailed,
		runner.Outcome{Code: hook.TerminalModelFailed}, ""); err != nil {
		t.Fatalf("the ending was not sent: %v", err)
	}
	if len(fixture.comments.posted) != 1 {
		t.Fatalf("comments = %d, want the ending posted anyway", len(fixture.comments.posted))
	}
	if log := strings.Join(logger.lines, "\n"); !strings.Contains(log, "closing comment not written down") {
		t.Fatalf("the failed write was not reported: %q", logger.lines)
	}
	if _, kept := runner.ReadTerminalComment(runDir); kept {
		t.Fatal("a comment was read back from a path that could not be written")
	}
}
