package main

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
