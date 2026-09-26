package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadsOriginalProseWithoutModelReception(t *testing.T) {
	t.Setenv("TRACKER_TEST_KEY", "synthetic+/=token")
	const original = "Please finish this.\nExample JSON: {\"gaps\":null,\"unknown\":true}\nKeep the original acceptance conditions."
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/issues/EXAMPLE-1" || r.URL.Query().Get("apiKey") != "synthetic+/=token" {
			t.Error("wrong source request")
		}
		json.NewEncoder(w).Encode(map[string]any{"issueKey": "EXAMPLE-1", "summary": "A plain request", "description": original, "newAPIField": true})
	}))
	defer server.Close()
	request, err := (Backlog{BaseURL: server.URL + "/api/v2", KeyEnv: "TRACKER_TEST_KEY", Client: server.Client()}).Request(context.Background(), "EXAMPLE-1")
	if err != nil || !strings.HasSuffix(request, original) {
		t.Fatalf("request=%q err=%v", request, err)
	}
}

func TestTrackerErrorKeepsReasonButNotKeyAndDoesNotRedirect(t *testing.T) {
	t.Setenv("TRACKER_TEST_KEY", "synthetic+/=token")
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Location", "/other")
		w.WriteHeader(http.StatusFound)
		fmt.Fprint(w, "upstream detail synthetic+/=token synthetic%2B%2F%3Dtoken")
	}))
	defer server.Close()
	_, err := (Backlog{BaseURL: server.URL, KeyEnv: "TRACKER_TEST_KEY", Client: server.Client()}).Request(context.Background(), "EXAMPLE-1")
	if err == nil || !strings.Contains(err.Error(), "upstream detail") || strings.Contains(err.Error(), "synthetic") || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}
