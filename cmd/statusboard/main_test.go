package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/backlog"
)

type boardTestTransport func(*http.Request) (*http.Response, error)

func (f boardTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestResolvePostsOnlyTheExistingFixedCommandForTheDisplayedDelivery(t *testing.T) {
	for _, test := range []struct {
		name, step, delivery, action string
		canResolve                   bool
		want                         int
	}{
		{"allowed", "attention", "delivery-example", "resolve", true, 200},
		{"unavailable", "attention", "delivery-example", "resolve", false, 403},
		{"already closed", "done", "delivery-example", "resolve", true, 403},
		{"stale delivery", "attention", "old-delivery", "resolve", true, 403},
		{"missing delivery", "attention", "", "resolve", true, 403},
		{"not a Go", "attention", "delivery-example", "go", true, 403},
	} {
		t.Run(test.name, func(t *testing.T) {
			posts := 0
			poster, err := backlog.NewClient(backlog.Config{SpaceKey: "example", Origin: "https://example.backlog.com", APIKey: "test-only", Timeout: time.Second, MaxResponseBytes: 1024}, boardTestTransport(func(r *http.Request) (*http.Response, error) {
				posts++
				if r.Method != "POST" || r.URL.Path != "/api/v2/issues/123/comments" {
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				if err := r.ParseForm(); err != nil {
					t.Fatal(err)
				}
				content := r.Form.Get("content")
				if !strings.HasPrefix(content, "確認済み\n") || strings.Contains(content, "injected") {
					t.Fatalf("not the fixed confirmation: %q", content)
				}
				response, err := json.Marshal(map[string]any{"id": 456, "issueId": 123, "content": content})
				if err != nil {
					t.Fatal(err)
				}
				return &http.Response{StatusCode: 201, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(response))}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			s := boardServer{statusDir: t.TempDir(), poster: poster, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			board, err := json.Marshal(map[string]any{"runs": []boardRun{{DeliveryID: "delivery-example", IssueID: 123, Step: test.step, CanResolve: test.canResolve}}})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(s.statusDir, "board.json"), board, 0o600); err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(actRequest{Action: test.action, IssueID: 123, DeliveryID: test.delivery, Text: "injected"})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest("POST", "https://board.example/api/act", bytes.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Origin", "https://board.example")
			request.SetBasicAuth("viewer", "test-only")
			response := httptest.NewRecorder()
			s.serveAct(response, request)
			if response.Code != test.want {
				t.Fatalf("got %d, want %d: %s", response.Code, test.want, response.Body.String())
			}
			if test.want == 200 {
				if posts != 1 {
					t.Fatalf("posts = %d", posts)
				}
				records := tailJSONL(filepath.Join(s.statusDir, "actions.jsonl"), 1)
				var record actRecord
				if len(records) != 1 || json.Unmarshal(records[0], &record) != nil || record.DeliveryID != test.delivery || record.User != "viewer" || record.Action != "resolve" {
					t.Fatalf("missing action audit: %s", records)
				}
			} else if posts != 0 {
				t.Fatalf("denied action posted %d comments", posts)
			}
			if got, err := os.ReadFile(filepath.Join(s.statusDir, "board.json")); err != nil || !bytes.Equal(got, board) {
				t.Fatal("action changed the run snapshot")
			}
		})
	}
}

func TestBoardAccessDefaultsToBasicAndLocalIsReadOnly(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, mode := range []string{"", "basic", "unknown"} {
		if _, err := boardAccess(mode, "", "", logger); err == nil {
			t.Fatalf("mode %q accepted absent credentials", mode)
		}
	}
	for _, mode := range []string{"", "basic"} {
		wrap, err := boardAccess(mode, "viewer", "artificial-password", logger)
		if err != nil {
			t.Fatal(err)
		}
		for _, authenticated := range []bool{false, true} {
			r := httptest.NewRequest(http.MethodGet, "http://board.example/", nil)
			if authenticated {
				r.SetBasicAuth("viewer", "artificial-password")
			}
			w := httptest.NewRecorder()
			wrap(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }).ServeHTTP(w, r)
			want := http.StatusUnauthorized
			if authenticated {
				want = http.StatusOK
			}
			if w.Code != want {
				t.Fatalf("basic mode %q: got %d, want %d", mode, w.Code, want)
			}
		}
	}
	wrap, err := boardAccess("local", "", "", logger)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		url, method string
		want        int
	}{
		{"http://127.0.0.1:9200/", http.MethodGet, http.StatusOK},
		{"http://localhost:9200/api/board", http.MethodGet, http.StatusOK},
		{"http://[::1]:9200/", http.MethodHead, http.StatusOK},
		{"http://127.0.0.1:9200/api/act", http.MethodPost, http.StatusMethodNotAllowed},
		{"http://127.0.0.1:9200/webhook/unused", http.MethodPost, http.StatusMethodNotAllowed},
		{"http://board.example/", http.MethodGet, http.StatusForbidden},
		{"http://localhost.example/", http.MethodGet, http.StatusForbidden},
		{"http://192.0.2.1/", http.MethodGet, http.StatusForbidden},
	} {
		w := httptest.NewRecorder()
		wrap(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }).ServeHTTP(w, httptest.NewRequest(test.method, test.url, nil))
		if w.Code != test.want || w.Header().Get("WWW-Authenticate") != "" {
			t.Fatalf("local %s %s: got %d, want %d; challenge %q", test.method, test.url, w.Code, test.want, w.Header().Get("WWW-Authenticate"))
		}
	}
}

// The embedded demo page is a copy (go:embed cannot reach outside the
// package); the committed mockup under docs/mockups stays the source.
func TestDemoPageIsTheCommittedMockup(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "docs", "mockups", "status-board-mock-ghost.html"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(source, demoHUDPage) {
		t.Fatal("cmd/statusboard/demo/hud.html drifted from docs/mockups/status-board-mock-ghost.html; copy it again")
	}
}

func TestServeDemoServesOnlyTheKnownSkins(t *testing.T) {
	recorder := httptest.NewRecorder()
	serveDemo(recorder, httptest.NewRequest(http.MethodGet, "/demo/hud", nil))
	if recorder.Code != http.StatusOK || !bytes.Equal(recorder.Body.Bytes(), demoHUDPage) {
		t.Fatalf("the demo page was not served: %d", recorder.Code)
	}
	for _, header := range []string{"Content-Security-Policy", "X-Content-Type-Options", "X-Frame-Options"} {
		if recorder.Header().Get(header) == "" {
			t.Fatalf("%s is missing on the demo page", header)
		}
	}

	for _, path := range []string{"/demo/", "/demo/other", "/demo/hud/"} {
		recorder := httptest.NewRecorder()
		serveDemo(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("%s was served (%d)", path, recorder.Code)
		}
	}

	recorder = httptest.NewRecorder()
	serveDemo(recorder, httptest.NewRequest(http.MethodPost, "/demo/hud", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("a POST to the demo page was accepted (%d)", recorder.Code)
	}
}

// The snapshot's stage hint is what keeps an attention state from
// rendering as an empty rail; the page must read it and have a held style
// to show it with.
func TestBoardPageLightsTheStageAnAttentionStateStoppedAt(t *testing.T) {
	page, err := os.ReadFile("board.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"run.stage", "STEP_INDEX[stage]", ".node.hold .tick"} {
		if !strings.Contains(string(page), needle) {
			t.Fatalf("board.html lacks %q", needle)
		}
	}
}

// The attendant's intake-hold banner rides in the snapshot as `notice`; the
// page must render it.
func TestBoardPageRendersTheIntakeHoldNotice(t *testing.T) {
	page, err := os.ReadFile("board.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{`id="notice-banner"`, "latestBoard.notice"} {
		if !strings.Contains(string(page), needle) {
			t.Fatalf("board.html lacks %q", needle)
		}
	}
}
