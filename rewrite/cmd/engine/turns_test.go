package main

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func TestTheStartIsWorthACommentOnlyAfterAWait(t *testing.T) {
	cfg := watchConfiguration(t)
	cfg.Intake.Announce = true
	var mu sync.Mutex
	var comments []string
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "watch-tracker.example" {
			return nil, http.ErrNotSupported
		}
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			return selectionReply(r, 200, []any{}), nil
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			if err := r.ParseForm(); err != nil {
				return nil, err
			}
			comments = append(comments, r.PostForm.Get("content"))
			return selectionReply(r, 201, map[string]any{"id": 700 + len(comments), "content": r.PostForm.Get("content")}), nil
		}
		return catalogReply(r, 503, "not under test"), nil
	})
	var log bytes.Buffer
	observe := func(message string) { log.WriteString(message + "\n") }
	// Told the work starts at once, the requester hears no second comment.
	atOnce := t.TempDir()
	issue := sourceIssue{ID: 51, ProjectID: 17, Key: "EXAMPLE-51"}
	acceptTurn(context.Background(), cfg, issue, atOnce, 0, observe)
	acceptTurn(context.Background(), cfg, issue, atOnce, 3, observe)
	if startDeservesNotice(cfg, issue, atOnce) {
		t.Fatal("a request accepted with nothing ahead of it was to hear that it started")
	}
	// Told to wait, the requester hears when the work starts.
	waited := t.TempDir()
	acceptTurn(context.Background(), cfg, issue, waited, 2, observe)
	acceptTurn(context.Background(), cfg, issue, waited, 0, observe)
	if !startDeservesNotice(cfg, issue, waited) {
		t.Fatal("a request accepted behind others was not to hear that it started")
	}
	// An acceptance that was never announced leaves the start to be said.
	if !startDeservesNotice(cfg, issue, t.TempDir()) {
		t.Fatal("a request whose acceptance was never announced was to stay silent")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(comments) != 2 || !strings.HasPrefix(comments[0], "受け付けました。すぐに") || !strings.HasPrefix(comments[1], "受け付けました。前に 2 件あり") || log.Len() != 0 {
		t.Fatalf("acceptances announced: %q log: %s", comments, log.String())
	}
}
