package hook

import (
	"context"
	"testing"
)

type fakeBoard struct {
	calls []struct {
		IssueID int64
		Phase   BoardPhase
	}
	err error
}

func (f *fakeBoard) ProjectBoardPhase(_ context.Context, issueID int64, phase BoardPhase) error {
	f.calls = append(f.calls, struct {
		IssueID int64
		Phase   BoardPhase
	}{issueID, phase})
	return f.err
}

func TestProcessProjectsAnAcceptedTicketAsRunning(t *testing.T) {
	board := &fakeBoard{}
	service := newTestService(t, &fakeBacklog{activity: testActivity(), issue: testIssue()}, &fakeStore{}, nil)
	service.UseBoard(board)

	result := service.Process(context.Background(), testHint())
	if result.Code != "queue_created" {
		t.Fatalf("Process() = %+v", result)
	}
	if len(board.calls) != 1 || board.calls[0].Phase != BoardRunning || board.calls[0].IssueID != testIssue().ID {
		t.Fatalf("board calls = %+v", board.calls)
	}
}

func TestTerminalReportProjectsTheEndingByItsCode(t *testing.T) {
	for code, expected := range map[TerminalCode]BoardPhase{
		TerminalSuccess:              BoardDelivered,
		TerminalCancelled:            BoardNeedsAttention,
		TerminalClarificationExpired: BoardNeedsAttention,
		TerminalModelFailed:          BoardNeedsAttention,
	} {
		store := &terminalFakeStore{
			beginBindings:     []TerminalBinding{{IssueID: 404, IssueKey: "TICKET-505"}},
			beginDispositions: []TerminalBeginDisposition{TerminalBeginAcquired},
			completeResults:   []TerminalCompleteDisposition{TerminalCompleted},
		}
		board := &fakeBoard{}
		service := newTerminalTestService(t, store, &terminalFakeComments{addIDs: []int64{808}}, nil)
		service.UseBoard(board)

		result := service.ProcessTerminalReport(context.Background(), terminalTestRequest(code))
		if result.Code != "terminal_report_recorded" {
			t.Fatalf("%s: result = %+v", code, result)
		}
		if len(board.calls) != 1 || board.calls[0].Phase != expected || board.calls[0].IssueID != 404 {
			t.Fatalf("%s: board calls = %+v", code, board.calls)
		}
	}
}

// The board is a view: a projection that fails must leave the decision that
// triggered it untouched, in every service that projects.
func TestABoardFailureNeverChangesTheDecision(t *testing.T) {
	board := &fakeBoard{err: NewExternalFailure("backlog", FailureRetryable, "status_update_failed")}

	service := newTestService(t, &fakeBacklog{activity: testActivity(), issue: testIssue()}, &fakeStore{}, nil)
	service.UseBoard(board)
	if result := service.Process(context.Background(), testHint()); result.Code != "queue_created" {
		t.Fatalf("enqueue with failing board = %+v", result)
	}

	store := &terminalFakeStore{
		beginBindings:     []TerminalBinding{{IssueID: 404, IssueKey: "TICKET-505"}},
		beginDispositions: []TerminalBeginDisposition{TerminalBeginAcquired},
		completeResults:   []TerminalCompleteDisposition{TerminalCompleted},
	}
	terminal := newTerminalTestService(t, store, &terminalFakeComments{addIDs: []int64{808}}, nil)
	terminal.UseBoard(board)
	if result := terminal.ProcessTerminalReport(context.Background(), terminalTestRequest(TerminalSuccess)); result.Code != "terminal_report_recorded" {
		t.Fatalf("terminal with failing board = %+v", result)
	}
	if len(board.calls) != 2 {
		t.Fatalf("board calls = %+v", board.calls)
	}
}
