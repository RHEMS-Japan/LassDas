package hook

import (
	"context"
	"strings"
	"testing"
	"time"
)

// keptDeliveryReport is a success that has only opened its pull request,
// on a destination whose delivery carries on afterwards — the one shape
// where an ending is reported while something is still to happen.
func keptDeliveryReport() TerminalReportRequest {
	report := terminalTestRequest(TerminalSuccess)
	report.CommitSHA, report.CommitURL = "", ""
	report.StagingEvidenceURL, report.ProductionEvidenceURL = "", ""
	return report
}

// How far the delivery has got is a fact about the run, so the board reads
// it the same way whichever text reports the run: a delivery with more to
// do is shown as running, not as delivered, when its closing comment is
// re-sent from the copy the run kept.
func TestAKeptCommentLeavesTheBoardWhereTheRunIs(t *testing.T) {
	claim := functionURLTestNow.Add(-time.Minute)
	for _, tc := range []struct {
		name  string
		after time.Time
		phase BoardPhase
	}{
		{"the delivery carries on", claim, BoardRunning},
		{"the delivery is over", time.Time{}, BoardDelivered},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := keptDeliveryReport()
			store := &terminalFakeStore{
				beginBindings:     []TerminalBinding{{IssueID: 404, ClaimedAtMillis: claim.UnixMilli()}},
				beginDispositions: []TerminalBeginDisposition{TerminalBeginAcquired},
				completeResults:   []TerminalCompleteDisposition{TerminalCompleted},
			}
			comments := &terminalFakeComments{addIDs: []int64{808}}
			board := &fakeBoard{}
			service := newTerminalTestService(t, store, comments, nil)
			service.config.Destinations[0].Delivery = DeliverPullRequest
			service.UseAutomaticDeliveryAfter(tc.after)
			service.UseBoard(board)

			record, err := MarshalTerminalReportRecord(report)
			if err != nil {
				t.Fatal(err)
			}
			digest := TerminalReportDigest(record)
			kept := TerminalCommentContent(report, digest)
			if result := service.ProcessPersistedTerminalReport(context.Background(), report, kept); result.Decision != DecisionAccepted {
				t.Fatalf("the kept comment was refused: %+v", result)
			}
			if len(comments.addContents) != 1 || comments.addContents[0] != kept {
				t.Fatalf("the kept comment was not posted as it was: %q", comments.addContents)
			}
			// The words are the closing ones either way. By the time a
			// kept comment is re-sent, whatever was still to come has
			// already happened, so a notice promising it would be wrong.
			if !strings.Contains(kept, "最終結果") || strings.Contains(kept, "自動処理を継続") {
				t.Fatalf("the kept comment is not the closing one: %q", kept)
			}
			if len(board.calls) != 1 || board.calls[0].Phase != tc.phase || board.calls[0].IssueID != 404 {
				t.Fatalf("board = %+v, want phase %s", board.calls, tc.phase)
			}
		})
	}
}

// A kept comment ending in some other report's marker cannot be
// recognised on a later attempt, so it would be posted again every tick.
// It is refused instead, and so is one the tracker would not take.
func TestAKeptCommentThatCouldNotBeRecognisedAgainIsRefused(t *testing.T) {
	report := keptDeliveryReport()
	record, err := MarshalTerminalReportRecord(report)
	if err != nil {
		t.Fatal(err)
	}
	kept := TerminalCommentContent(report, TerminalReportDigest(record))
	elsewhere := strings.TrimRight(kept, "\n")
	elsewhere = elsewhere[:strings.LastIndex(elsewhere, "\n")+1] + CommentMarker("terminal", "TICKET-3") + "\n"
	for name, body := range map[string]string{
		"another report's marker": elsewhere,
		"nothing at all":          "",
		"more than the tracker takes": strings.Repeat("あ", MaxTrackerCommentBytes/3) +
			strings.Repeat("!", MaxTrackerCommentBytes),
	} {
		store := &terminalFakeStore{
			beginBindings:     []TerminalBinding{{IssueID: 404}},
			beginDispositions: []TerminalBeginDisposition{TerminalBeginAcquired},
			completeResults:   []TerminalCompleteDisposition{TerminalCompleted},
		}
		comments := &terminalFakeComments{addIDs: []int64{808}}
		service := newTerminalTestService(t, store, comments, nil)
		service.config.Destinations[0].Delivery = DeliverPullRequest
		if result := service.ProcessPersistedTerminalReport(context.Background(), report, body); result.Decision != DecisionInvalid {
			t.Fatalf("%s was accepted: %+v", name, result)
		}
		if len(comments.addContents) != 0 {
			t.Fatalf("%s reached the ticket: %q", name, comments.addContents)
		}
	}
}
