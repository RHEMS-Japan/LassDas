package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/ticketview"
)

// ticketFixture lays out /data-like directories: status/board.json naming
// one run, and runs/<delivery>/ holding its records.
func ticketFixture(t *testing.T) boardServer {
	t.Helper()
	root := t.TempDir()
	statusDir := filepath.Join(root, "status")
	runDir := filepath.Join(root, "runs", "delivery_abc")
	for _, dir := range []string{statusDir, filepath.Join(runDir, "history", "stage-1")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	board := `{"runs":[{"delivery_id":"delivery_abc","issue_id":123,"issue_key":"PROJ-7","summary":"文言修正","step":"failed","step_title":"失敗","detail":"レビュー役の鍵が上限","can_go":false,"can_resolve":true,"terminal_code":"model_failure"}]}`
	if err := os.WriteFile(filepath.Join(statusDir, "board.json"), []byte(board), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "readiness-ticket.json"), []byte(`{"issue_key":"PROJ-7","summary":"文言修正","request":"x","repository":"example/repo"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "intake.json"), []byte(`{"read_at":"2026-09-15T01:00:00Z","rationale":"読んだ","invocation":{"cost_usd":0.25}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "history", "stage-1", "review-a-run.json"), []byte(`{"ran_at":"2026-09-15T01:05:00Z","transcript":"export TOKEN=sk-live-abcdefghijklmnopqrstuvwxyz0123456789\nno verdict"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// A record that exists on disk but is not on the page's allow-list.
	if err := os.WriteFile(filepath.Join(runDir, "secrets.env"), []byte("PRIVATE=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LASSDAS_RUNS_ROOT", "")
	return boardServer{statusDir: statusDir, trackerBase: "https://example.backlog.com", logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestTicketAPIResolvesTheKeyThroughTheBoardOnly(t *testing.T) {
	s := ticketFixture(t)
	rec := httptest.NewRecorder()
	s.serveTicketAPI(rec, httptest.NewRequest("GET", "/api/tickets/PROJ-7", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var got struct {
		Board       json.RawMessage `json:"board"`
		View        ticketview.View `json:"view"`
		TrackerBase string          `json:"tracker_base"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got.Board), `"terminal_code":"model_failure"`) || got.View.DeliveryID != "delivery_abc" || got.View.IssueKey != "PROJ-7" || got.TrackerBase == "" {
		t.Fatalf("payload not assembled: %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), "sk-live-abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Fatalf("token leaked through the API: %s", rec.Body)
	}
	for _, path := range []string{"/api/tickets/PROJ-8", "/api/tickets/delivery_abc", "/api/tickets/../PROJ-7", "/api/tickets/", "/api/tickets/PROJ-7/x"} {
		rec := httptest.NewRecorder()
		s.serveTicketAPI(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 404 {
			t.Fatalf("%s: status %d, want 404", path, rec.Code)
		}
	}
}

func TestTicketRecordsAreAllowListedAndMasked(t *testing.T) {
	s := ticketFixture(t)
	rec := httptest.NewRecorder()
	s.serveTicketAPI(rec, httptest.NewRequest("GET", "/api/tickets/PROJ-7/records/stage-1-review-a-run", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "[masked:") || strings.Contains(rec.Body.String(), "abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Fatalf("record should be served masked: %d %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("records are served as text, got %q", ct)
	}
	for _, name := range []string{"secrets.env", "../ticket", "ticket.json", "stage-1-review-a-run/", "deliver-production-report"} {
		rec := httptest.NewRecorder()
		s.serveTicketAPI(rec, httptest.NewRequest("GET", "/api/tickets/PROJ-7/records/"+name, nil))
		if rec.Code != 404 {
			t.Fatalf("records/%s: status %d, want 404", name, rec.Code)
		}
	}
}

func TestTicketPageIsServedForKeysOnly(t *testing.T) {
	s := ticketFixture(t)
	rec := httptest.NewRecorder()
	s.serveTicketPage(rec, httptest.NewRequest("GET", "/tickets/PROJ-7", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "/api/tickets/") || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatalf("page: %d %q", rec.Code, rec.Header())
	}
	for _, path := range []string{"/tickets/", "/tickets/PROJ-7/extra", "/tickets/lower-1"} {
		rec := httptest.NewRecorder()
		s.serveTicketPage(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 404 {
			t.Fatalf("%s: status %d, want 404", path, rec.Code)
		}
	}
}

func TestRunsRootFollowsTheEnvironmentThenTheStatusDir(t *testing.T) {
	s := boardServer{statusDir: "/data/status"}
	t.Setenv("LASSDAS_RUNS_ROOT", "")
	if got := s.runsRoot(); got != filepath.Join("/data", "runs") {
		t.Fatalf("default runs root = %q", got)
	}
	t.Setenv("LASSDAS_RUNS_ROOT", "/elsewhere/runs")
	if got := s.runsRoot(); got != "/elsewhere/runs" {
		t.Fatalf("env runs root = %q", got)
	}
}

func TestBoardHTMLLinksEachCardToItsTicketPage(t *testing.T) {
	page, err := os.ReadFile("board.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(page), `"/tickets/" + encodeURIComponent(run.issue_key)`) {
		t.Fatal("board cards must link to /tickets/<issue_key>")
	}
	_ = http.StatusOK
}

func TestTicketAPIPicksTheNewestRunOfAKey(t *testing.T) {
	for _, order := range [][2]string{{"delivery_old", "delivery_new"}, {"delivery_new", "delivery_old"}} {
		s := ticketFixture(t)
		rows := ""
		for i, id := range order {
			claimed := map[string]int64{"delivery_old": 100, "delivery_new": 200}[id]
			step := map[string]string{"delivery_old": "failed", "delivery_new": "review"}[id]
			if i > 0 {
				rows += ","
			}
			rows += fmt.Sprintf(`{"delivery_id":%q,"issue_id":123,"issue_key":"PROJ-7","step":%q,"claimed_at_ms":%d}`, id, step, claimed)
		}
		if err := os.WriteFile(filepath.Join(s.statusDir, "board.json"), []byte(`{"runs":[`+rows+`]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		s.serveTicketAPI(rec, httptest.NewRequest("GET", "/api/tickets/PROJ-7", nil))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"delivery_id":"delivery_new"`) || !strings.Contains(rec.Body.String(), `"step":"review"`) {
			t.Fatalf("order %v: newest run must win: %d %s", order, rec.Code, rec.Body)
		}
	}
}

func TestTicketKeysFollowTheEngineShape(t *testing.T) {
	for key, want := range map[string]bool{"A-1": true, "ABCDEFGHIJKLMNOPQRSTUVWXYZ_1-42": true, "PROJ-0": false, "proj-1": false, "PROJ-": false, "PROJ-1x": false} {
		if got := ticketKeyPattern.MatchString(key); got != want {
			t.Errorf("key %q accepted=%v, want %v", key, got, want)
		}
	}
}

func TestTicketRecordsAreMaskedBeforeAnyCut(t *testing.T) {
	s := ticketFixture(t)
	runDir := filepath.Join(filepath.Dir(s.statusDir), "runs", "delivery_abc")
	key := "sk-" + strings.Repeat("ABCDEFGHIJ", 4)
	// The key sits across the old 512 KiB cut and inside the read bound.
	body := strings.Repeat("x", (512<<10)-11) + "\n" + key + "\ntrailer text here\n"
	if err := os.WriteFile(filepath.Join(runDir, "m1-trail.txt"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.serveTicketAPI(rec, httptest.NewRequest("GET", "/api/tickets/PROJ-7/records/trail", nil))
	if rec.Code != 200 || strings.Contains(rec.Body.String(), key[3:]) || !strings.Contains(rec.Body.String(), "[masked:") || !strings.HasSuffix(strings.TrimSpace(rec.Body.String()), "trailer text here") {
		t.Fatalf("record must be masked whole and served whole: %d len=%d tail=%q", rec.Code, rec.Body.Len(), tailOf(rec.Body.String(), 60))
	}
	// A record longer than the serve bound is cut after masking, and says so.
	long := strings.Repeat("y", maxRecordServe+100)
	if err := os.WriteFile(filepath.Join(runDir, "m1-trail.txt"), []byte(long), 0o644); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	s.serveTicketAPI(rec, httptest.NewRequest("GET", "/api/tickets/PROJ-7/records/trail", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "以下略") || rec.Body.Len() > maxRecordServe+200 {
		t.Fatalf("a long record must be cut with a note: %d len=%d", rec.Code, rec.Body.Len())
	}
}

func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func TestTicketRoutesSitBehindTheAccessGate(t *testing.T) {
	s := ticketFixture(t)
	mux := http.NewServeMux()
	gate := newAuthGate("operator", "a-password-of-sixteen-plus", slog.New(slog.NewTextHandler(io.Discard, nil)))
	registerRoutes(mux, gate.wrap, &s)
	for _, path := range []string{"/tickets/PROJ-7", "/api/tickets/PROJ-7", "/api/tickets/PROJ-7/records/intake"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 401 {
			t.Fatalf("%s without credentials: %d, want 401", path, rec.Code)
		}
		req := httptest.NewRequest("GET", path, nil)
		req.SetBasicAuth("operator", "a-password-of-sixteen-plus")
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s with credentials: %d %s", path, rec.Code, rec.Body)
		}
	}
	// The mux cleans a dotted path before any handler sees it.
	req := httptest.NewRequest("GET", "/api/tickets/../PROJ-7", nil)
	req.SetBasicAuth("operator", "a-password-of-sixteen-plus")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code == 200 {
		t.Fatalf("a dotted path must not reach a ticket: %d", rec.Code)
	}
}

func TestTicketRecordsTooLargeToScanWholeAreRefused(t *testing.T) {
	if maxRecordServe >= maxRecordRead {
		t.Fatal("the serve bound must stay below the read bound")
	}
	s := ticketFixture(t)
	runDir := filepath.Join(filepath.Dir(s.statusDir), "runs", "delivery_abc")
	huge := make([]byte, maxRecordRead+1)
	for i := range huge {
		huge[i] = 'z'
	}
	if err := os.WriteFile(filepath.Join(runDir, "m1-trail.txt"), huge, 0o644); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.serveTicketAPI(rec, httptest.NewRequest("GET", "/api/tickets/PROJ-7/records/trail", nil))
	if rec.Code != http.StatusRequestEntityTooLarge || strings.Contains(rec.Body.String(), "zzzz") {
		t.Fatalf("a record too large to scan whole must be refused, not cut: %d len=%d", rec.Code, rec.Body.Len())
	}
}
