package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The board used to move a finished card into 完了・終了 by itself, the moment
// the engine called the run finished, and the card then said only what it
// had ended as. A reader coming back could not tell a run that ended minutes
// ago from one that ended the day before, and nobody had looked at either.
// The checks below are that rule and its record.

// acknowledgeFixture lays out a status directory holding one finished run
// and one still going.
func acknowledgeFixture(t *testing.T) *boardServer {
	t.Helper()
	dir := t.TempDir()
	writeAcknowledgeBoard(t, dir, time.Now().UTC())
	return &boardServer{statusDir: dir, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func writeAcknowledgeBoard(t *testing.T, dir string, generatedAt time.Time) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"schema_version": 1,
		"generated_at":   generatedAt,
		"runs": []any{
			map[string]any{
				"delivery_id": "delivery_done", "issue_id": 11, "issue_key": "PROJ-1",
				"state": "terminal", "step": "failed", "step_title": "失敗で終了",
				"finished_at": "2026-09-25T02:00:00Z",
			},
			map[string]any{
				"delivery_id": "delivery_live", "issue_id": 12, "issue_key": "PROJ-2",
				"state": "claimed", "step": "implement", "step_title": "実装・検証中",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "board.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func acknowledgePost(t *testing.T, server *boardServer, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/acknowledge", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	server.serveAcknowledge(recorder, request)
	return recorder
}

// rowOf pulls one delivery's row out of the payload the browser receives.
func rowOf(t *testing.T, payload streamPayload, deliveryID string) map[string]any {
	t.Helper()
	var board struct {
		Runs []map[string]any `json:"runs"`
	}
	if err := json.Unmarshal(payload.Board, &board); err != nil {
		t.Fatalf("board is not JSON: %s", payload.Board)
	}
	for _, row := range board.Runs {
		if row["delivery_id"] == deliveryID {
			return row
		}
	}
	t.Fatalf("%s is not on the board: %s", deliveryID, payload.Board)
	return nil
}

// A finished run reaches the browser with no acknowledgement on it, which is
// what keeps it in the running lane; pressing the control writes one down,
// and a board that starts again still has it.
func TestClearingAFinishedRunIsRememberedAcrossARestart(t *testing.T) {
	server := acknowledgeFixture(t)
	before := server.payload()
	if before.SnapshotState != "ready" {
		t.Fatalf("snapshot state = %q", before.SnapshotState)
	}
	if _, cleared := rowOf(t, before, "delivery_done")["acknowledged_at"]; cleared {
		t.Fatal("a finished run arrived already cleared away")
	}
	if got := rowOf(t, before, "delivery_done")["finished_at"]; got != "2026-09-25T02:00:00Z" {
		t.Fatalf("finished_at = %v, want the time the attendant wrote", got)
	}

	response := acknowledgePost(t, server, `{"delivery_id":"delivery_done"}`, nil)
	if response.Code != 200 {
		t.Fatalf("status %d: %s", response.Code, response.Body)
	}
	var recorded acknowledgeResponse
	if err := json.Unmarshal(response.Body.Bytes(), &recorded); err != nil {
		t.Fatal(err)
	}
	if recorded.DeliveryID != "delivery_done" || recorded.AcknowledgedAt.IsZero() {
		t.Fatalf("the acknowledgement says nothing: %+v", recorded)
	}

	// A different server over the same directory is this board started
	// again: the record is a file, not something held in memory.
	restarted := &boardServer{statusDir: server.statusDir, logger: server.logger}
	after := restarted.payload()
	stamp, cleared := rowOf(t, after, "delivery_done")["acknowledged_at"]
	if !cleared || stamp != recorded.AcknowledgedAt.Format(time.RFC3339Nano) {
		t.Fatalf("acknowledged_at = %v (present=%v), want %s", stamp, cleared, recorded.AcknowledgedAt.Format(time.RFC3339Nano))
	}
	if _, cleared := rowOf(t, after, "delivery_live")["acknowledged_at"]; cleared {
		t.Fatal("clearing one card cleared another")
	}

	// Pressing twice is not two decisions: the time stays the one the
	// person actually looked, which is the whole point of recording it.
	again := acknowledgePost(t, server, `{"delivery_id":"delivery_done"}`, nil)
	if again.Code != 200 {
		t.Fatalf("second press: status %d: %s", again.Code, again.Body)
	}
	var second acknowledgeResponse
	if err := json.Unmarshal(again.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if !second.AcknowledgedAt.Equal(recorded.AcknowledgedAt) {
		t.Fatalf("a second press moved the time: %v then %v", recorded.AcknowledgedAt, second.AcknowledgedAt)
	}
}

// Clearing a card posts nothing to the tracker, so it does not wait on the
// requester credential most boards do not have.
func TestClearingAFinishedRunNeedsNoRequesterCredential(t *testing.T) {
	server := acknowledgeFixture(t)
	if server.poster != nil {
		t.Fatal("the fixture was supposed to have no way to post")
	}
	if got := server.payload(); got.ActionsEnabled || !got.AcknowledgeEnabled {
		t.Fatalf("actions_enabled = %v, acknowledge_enabled = %v", got.ActionsEnabled, got.AcknowledgeEnabled)
	}
	// The same request against the tracker actions is refused; this one is not.
	refused := httptest.NewRecorder()
	action := httptest.NewRequest(http.MethodPost, "/api/act", strings.NewReader(`{"action":"go","issue_id":11}`))
	action.Header.Set("Content-Type", "application/json")
	server.serveAct(refused, action)
	if refused.Code != http.StatusForbidden {
		t.Fatalf("a board with no credential posted an action: %d", refused.Code)
	}
	if response := acknowledgePost(t, server, `{"delivery_id":"delivery_done"}`, nil); response.Code != 200 {
		t.Fatalf("status %d: %s", response.Code, response.Body)
	}
}

// Only a delivery the board is showing, and only one that has stopped for
// good, can be cleared away.
func TestTheBoardRefusesToClearWhatItIsNotShowingAsFinished(t *testing.T) {
	server := acknowledgeFixture(t)
	for name, body := range map[string]string{
		"still running":     `{"delivery_id":"delivery_live"}`,
		"not on the board":  `{"delivery_id":"delivery_ghost"}`,
		"no delivery named": `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			if response := acknowledgePost(t, server, body, nil); response.Code != http.StatusForbidden {
				t.Fatalf("status %d: %s", response.Code, response.Body)
			}
		})
	}
	if runs := readAcknowledgements(server.statusDir); len(runs) != 0 {
		t.Fatalf("a refused press was written down: %v", runs)
	}
}

// The same two doors the tracker actions are closed with, because a page on
// another site must not be able to empty somebody's board either.
func TestClearingIsClosedToCrossSiteAndReadOnlyBoards(t *testing.T) {
	server := acknowledgeFixture(t)
	get := httptest.NewRecorder()
	server.serveAcknowledge(get, httptest.NewRequest(http.MethodGet, "/api/acknowledge", nil))
	if get.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status %d", get.Code)
	}
	form := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/acknowledge", strings.NewReader("delivery_id=delivery_done"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	server.serveAcknowledge(form, request)
	if form.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("form status %d", form.Code)
	}
	if response := acknowledgePost(t, server, `{"delivery_id":"delivery_done"}`,
		map[string]string{"Origin": "https://elsewhere.example"}); response.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status %d", response.Code)
	}
	// The local board takes no writes at all, and says so rather than
	// showing a control it would refuse.
	local := &boardServer{statusDir: server.statusDir, logger: server.logger, readOnly: true}
	if got := local.payload(); got.AcknowledgeEnabled {
		t.Fatal("a read-only board offered to clear cards away")
	}
	if response := acknowledgePost(t, local, `{"delivery_id":"delivery_done"}`, nil); response.Code != http.StatusForbidden {
		t.Fatalf("read-only status %d: %s", response.Code, response.Body)
	}
	if runs := readAcknowledgements(server.statusDir); len(runs) != 0 {
		t.Fatalf("a refused press was written down: %v", runs)
	}
}

// The per-run page reads the same two facts, so the two screens cannot give
// one ticket two different answers about whether it has been dealt with.
func TestTheTicketPageSeesTheSameAcknowledgement(t *testing.T) {
	server := acknowledgeFixture(t)
	if response := acknowledgePost(t, server, `{"delivery_id":"delivery_done"}`, nil); response.Code != 200 {
		t.Fatalf("status %d: %s", response.Code, response.Body)
	}
	// The run directory is not laid out here: the page falls back to the
	// board row alone, which is where both of these facts live.
	recorder := httptest.NewRecorder()
	server.serveTicketAPI(recorder, httptest.NewRequest(http.MethodGet, "/api/tickets/PROJ-1", nil))
	if recorder.Code != 200 {
		t.Fatalf("status %d: %s", recorder.Code, recorder.Body)
	}
	var payload struct {
		Board       map[string]any `json:"board"`
		Acknowledge bool           `json:"acknowledge_enabled"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Board["acknowledged_at"] == nil || payload.Board["finished_at"] != "2026-09-25T02:00:00Z" {
		t.Fatalf("the ticket page cannot say when: %v", payload.Board)
	}
	if !payload.Acknowledge {
		t.Fatal("the ticket page was told it cannot clear a card away")
	}
}

// A row that arrives already carrying an acknowledgement is not evidence of
// anything: the board's own record is the only thing that clears a card.
func TestAnAcknowledgementInTheSnapshotIsNotBelieved(t *testing.T) {
	server := acknowledgeFixture(t)
	raw, err := os.ReadFile(filepath.Join(server.statusDir, "board.json"))
	if err != nil {
		t.Fatal(err)
	}
	forged := strings.Replace(string(raw), `"finished_at":"2026-09-25T02:00:00Z"`,
		`"finished_at":"2026-09-25T02:00:00Z","acknowledged_at":"2026-09-25T09:00:00Z"`, 1)
	if forged == string(raw) {
		t.Fatal("the fixture changed shape; this check no longer forges anything")
	}
	if err := os.WriteFile(filepath.Join(server.statusDir, "board.json"), []byte(forged), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, cleared := rowOf(t, server.payload(), "delivery_done")["acknowledged_at"]; cleared {
		t.Fatal("a snapshot cleared a card the board never recorded")
	}
}

// The lanes and the counts line are JavaScript, and nothing in Go can see
// them. This runs the page's own code against small inputs; without node it
// says so rather than passing quietly.
func TestFinishedCardsWaitInTheRunningLaneUntilCleared(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; the lane rules were not checked")
	}
	output, err := exec.Command(node, "testdata/lanes_check.mjs", "board.html").CombinedOutput()
	ran := 0
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		switch {
		case strings.HasPrefix(line, "FAIL"):
			t.Error(line)
			ran++
		case strings.HasPrefix(line, "PASS"):
			ran++
		}
	}
	if err != nil && !strings.Contains(string(output), "FAIL") {
		t.Fatalf("the check could not run: %v\n%s", err, output)
	}
	// A harness that stops checking leaves no FAIL line and says nothing,
	// so the count is what says it ran.
	if ran < 7 {
		t.Fatalf("only %d checks ran; the harness is not checking what it claims to\n%s", ran, output)
	}
}
