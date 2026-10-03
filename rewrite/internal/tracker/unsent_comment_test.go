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

func TestInvalidCommentBytesAreKnownNotToHaveBeenSent(t *testing.T) {
	for _, kind := range []string{"native", "github scope", "backlog scope"} {
		for _, body := range []string{"bad\xff", "\xc0\xaf", "\xed\xa0\x80", "unfinished\xf0\x9f"} {
			t.Run(fmt.Sprintf("%s/%x", kind, []byte(body)), func(t *testing.T) {
				calls := 0
				var worker Backlog
				issue := "EXAMPLE-1"
				accept := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					w.WriteHeader(http.StatusCreated)
					fmt.Fprint(w, `{"id":41}`)
				})
				switch kind {
				case "native":
					t.Setenv("UNSENT_TEST_KEY", "synthetic-comment-fixture")
					server := httptest.NewTLSServer(accept)
					t.Cleanup(server.Close)
					worker = Backlog{BaseURL: server.URL, KeyEnv: "UNSENT_TEST_KEY", Client: server.Client()}
				case "backlog scope":
					worker, _ = scopedFixture(t, accept, true)
				case "github scope":
					_, worker, _, _ = githubScopeFixture(t, func(base string, _ int32, w http.ResponseWriter, r *http.Request) {
						calls++
						w.WriteHeader(http.StatusCreated)
						json.NewEncoder(w).Encode(githubScopeComment(base, 41, "accepted"))
					}, true)
					issue = "7"
				}
				data, err := worker.AddComment(context.Background(), issue, body)
				if err == nil || data != nil || calls != 0 || !strings.Contains(err.Error(), "invalid UTF-8; it was not sent") || strings.Contains(err.Error(), "may already") || strings.Contains(err.Error(), "inspect comments") {
					t.Fatalf("known unsent post was sent or called ambiguous: calls=%d data=%s error=%v", calls, data, err)
				}
			})
		}
	}
}

func TestLiteralReplacementCharacterStillPostsUnchanged(t *testing.T) {
	const text = "日本語 � 🙂\nunchanged prose"
	worker, _ := scopedFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ParseForm() != nil || r.PostForm.Get("content") != text {
			t.Error("well-formed prose was changed")
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":41}`)
	}), true)
	if _, err := worker.AddComment(context.Background(), "EXAMPLE-1", text); err != nil {
		t.Fatalf("literal replacement character refused: %v", err)
	}
}
