package tracker

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Submission is accelerated; cleanup uses the real clock and write spacing.
// This tests the documented limit, not a promise that every old post is gone.
func TestGitHubLaunchCleanupLeavesEarlierPostsAfterItsTimeLimit(t *testing.T) {
	previousWait := githubWait
	githubWait = func(context.Context, time.Duration) error { return nil }
	t.Cleanup(func() { githubWait = previousWait })
	var mu sync.Mutex
	last := int64(0)
	remaining := map[int64]bool{80: true} // A pre-existing requester's comment.
	g, calls := githubFixture(t, func(base string, _ int32, w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPost:
			last++
			remaining[last] = true
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(githubScopeComment(base, last, "posted report"))
		case http.MethodGet, http.MethodDelete:
			id, _ := strconv.ParseInt(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], 10, 64)
			if id <= 0 || id >= 26 || !remaining[id] {
				t.Errorf("cleanup touched a retained or unowned comment: %d", id)
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if r.Method == http.MethodDelete {
				delete(remaining, id)
				w.WriteHeader(http.StatusNoContent)
				return
			}
			json.NewEncoder(w).Encode(githubScopeComment(base, id, "earlier report"))
		default:
			t.Errorf("unexpected cleanup request: %s", r.Method)
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	access, err := ServeIssue(ctx, g, "7", true, KeepLatestPost)
	if err != nil {
		t.Fatal(err)
	}
	defer access.Close()
	client, err := CertificateClient(access.Certificate)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	t.Setenv("CLEANUP_TEST_WORKER", access.Key)
	worker := Backlog{BaseURL: access.URL, KeyEnv: "CLEANUP_TEST_WORKER", Client: client}
	for range 26 {
		if _, err := worker.AddComment(ctx, "7", "posted report"); err != nil {
			t.Fatal(err)
		}
	}
	githubWait = previousWait
	started := time.Now()
	access.Close()
	elapsed := time.Since(started)
	mu.Lock()
	left, keepsLast, keepsRequester := len(remaining)-2, remaining[26], remaining[80]
	mu.Unlock()
	if elapsed < 19*time.Second || elapsed > 30*time.Second || left < 1 || left >= 25 || !keepsLast || !keepsRequester {
		t.Fatalf("cleanup limit changed: elapsed=%s earlier left=%d last=%v requester=%v", elapsed, left, keepsLast, keepsRequester)
	}
	before := calls.Load()
	access.Close()
	if calls.Load() != before {
		t.Fatal("closing a second time retried expired cleanup")
	}
	t.Logf("cleanup elapsed=%s; %d earlier comments remain; last and requester retained", elapsed, left)
}
