package backlog

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/hook"
)

func TestUpdateIssueStatusPatchesTheIssueAndVerifiesTheResult(t *testing.T) {
	var seen *http.Request
	var seenBody string
	client := testClient(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		seen = request
		body, _ := io.ReadAll(request.Body)
		seenBody = string(body)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(
			`{"id": 164195673, "status": {"id": 439072}}`,
		))}, nil
	}), 1024*1024)

	if err := client.UpdateIssueStatus(context.Background(), 164195673, 439072); err != nil {
		t.Fatalf("UpdateIssueStatus() error = %v", err)
	}
	if seen.Method != http.MethodPatch || seen.URL.Path != "/api/v2/issues/164195673" {
		t.Fatalf("request = %s %s", seen.Method, seen.URL.Path)
	}
	if seenBody != "statusId=439072" {
		t.Fatalf("body = %q", seenBody)
	}
}

// A response that names another issue or another status means the update did
// not do what was asked; treating it as success would show a board state that
// never happened.
func TestUpdateIssueStatusRefusesAMismatchedResponse(t *testing.T) {
	for name, body := range map[string]string{
		"another issue":  `{"id": 1, "status": {"id": 439072}}`,
		"another status": `{"id": 164195673, "status": {"id": 2}}`,
	} {
		client := testClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
		}), 1024*1024)
		err := client.UpdateIssueStatus(context.Background(), 164195673, 439072)
		if class, kind := hook.FailureDetails(err); class != hook.FailureRejected || kind != "invalid_response" {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}

func TestBoardProjectionMapsEveryPhaseAndOnlyThose(t *testing.T) {
	var patched []string
	client := testClient(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(request.Body)
		patched = append(patched, string(body))
		status := strings.TrimPrefix(string(body), "statusId=")
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(
			`{"id": 7, "status": {"id": ` + status + `}}`,
		))}, nil
	}), 1024*1024)
	projection, err := NewBoardProjection(client, BoardStatusMap{Running: 11, AwaitingAnswer: 12, Delivered: 3, NeedsAttention: 1})
	if err != nil {
		t.Fatalf("NewBoardProjection() error = %v", err)
	}

	for phase, expected := range map[hook.BoardPhase]string{
		hook.BoardRunning:        "statusId=11",
		hook.BoardAwaitingAnswer: "statusId=12",
		hook.BoardDelivered:      "statusId=3",
		hook.BoardNeedsAttention: "statusId=1",
	} {
		patched = nil
		if err := projection.ProjectBoardPhase(context.Background(), 7, phase); err != nil {
			t.Fatalf("%s: %v", phase, err)
		}
		if len(patched) != 1 || patched[0] != expected {
			t.Fatalf("%s: patched = %v", phase, patched)
		}
	}
	if err := projection.ProjectBoardPhase(context.Background(), 7, hook.BoardPhase("unheard-of")); err == nil {
		t.Fatal("an unknown phase must be refused, not guessed")
	}
}

func TestBoardProjectionRefusesAnIncompleteMap(t *testing.T) {
	client := testClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("no request must be sent")
		return nil, nil
	}), 1024)
	if _, err := NewBoardProjection(client, BoardStatusMap{Running: 11, AwaitingAnswer: 12, Delivered: 3}); err == nil {
		t.Fatal("an incomplete map must be refused at construction")
	}
}
