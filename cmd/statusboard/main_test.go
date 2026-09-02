package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
