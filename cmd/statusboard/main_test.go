package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
