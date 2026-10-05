package tracker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"
)

func TestServiceErrorsAreShortUnicodeTextRedactedBeforeCutting(t *testing.T) {
	for _, kind := range []string{"github", "backlog"} {
		for _, sample := range []struct{ name, body, display string }{
			{"short", "service temporarily unavailable", ""},
			{"boundary", strings.Repeat("x", 200), ""},
			{"long", strings.Repeat("x", 4096), ""},
			{"unicode", strings.Repeat("日本語🙂", 300), ""},
			{"credential across cut", strings.Repeat("x", 193) + githubTestToken + strings.Repeat("終", 400), ""},
			{"flag", strings.Repeat("x", 199) + "🇯🇵 remainder", strings.Repeat("x", 199) + "…"},
			{"family", strings.Repeat("x", 199) + "👩‍👩‍👧‍👦 remainder", strings.Repeat("x", 199) + "…"},
			{"accent", strings.Repeat("x", 199) + "e\u0301 remainder", strings.Repeat("x", 199) + "…"},
			{"complete flag", strings.Repeat("x", 198) + "🇯🇵 remainder", strings.Repeat("x", 198) + "🇯🇵…"},
		} {
			t.Run(kind+"/"+sample.name, func(t *testing.T) {
				var err error
				var original string
				ctx := context.Background()
				if kind == "github" {
					g, _ := githubFixture(t, func(_ string, _ int32, w http.ResponseWriter, _ *http.Request) {
						w.WriteHeader(503)
						fmt.Fprint(w, sample.body)
					})
					_, _, err = g.call(ctx, "GET", g.APIURL+"/user", nil, 200, githubItemLimit)
					var refusal *githubError
					if !errors.As(err, &refusal) {
						t.Fatalf("error type lost: %v", err)
					}
					original = refusal.Body
				} else {
					t.Setenv("ERROR_TEST_KEY", githubTestToken)
					server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503); fmt.Fprint(w, sample.body) }))
					t.Cleanup(server.Close)
					b := Backlog{BaseURL: server.URL + "/api/v2", KeyEnv: "ERROR_TEST_KEY", Client: server.Client()}
					_, err = b.call(ctx, "GET", "/issues/EXAMPLE-1", nil, nil, 200)
					var refusal *trackerError
					if !errors.As(err, &refusal) {
						t.Fatalf("error type lost: %v", err)
					}
					original = refusal.Body
				}
				redacted := strings.ReplaceAll(sample.body, githubTestToken, "[credential]")
				if original != redacted {
					t.Fatal("internal refusal body was truncated or not redacted")
				}
				runes := []rune(redacted)
				want := redacted
				if len(runes) > 200 {
					want = string(runes[:200]) + "…"
				}
				if sample.display != "" {
					want = sample.display
				}
				if err.Error() != "tracker returned HTTP 503: "+want || !utf8.ValidString(err.Error()) {
					t.Fatalf("display did not preserve at most 200 complete redacted characters: %q", err)
				}
			})
		}
	}
}

func TestRedirectDisplayDoesNotSplitAGrapheme(t *testing.T) {
	prefix := "https://moved.example/" + strings.Repeat("x", 199-len("https://moved.example/"))
	g, calls := githubFixture(t, func(_ string, _ int32, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", prefix+"👩‍👩‍👧‍👦")
		w.WriteHeader(http.StatusTemporaryRedirect)
	})
	_, _, err := g.call(context.Background(), "GET", g.APIURL+"/user", nil, 200, githubItemLimit)
	if err == nil || calls.Load() != 1 || !strings.Contains(err.Error(), prefix+"…") || strings.Contains(err.Error(), "👩") {
		t.Fatalf("redirect was followed or split a displayed cluster: %v", err)
	}
}

func TestShortErrorDisplayKeepsTheFullBacklogRefusalForReadback(t *testing.T) {
	t.Setenv("ERROR_TEST_KEY", "synthetic-error-fixture")
	var reads atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PATCH" {
			w.WriteHeader(400)
			fmt.Fprint(w, strings.Repeat("padding ", 100)+"No comment content.")
			return
		}
		if r.Method != "GET" {
			t.Errorf("unexpected %s", r.Method)
		}
		reads.Add(1)
		fmt.Fprint(w, `{"summary":"already set"}`)
	}))
	defer server.Close()
	b := Backlog{BaseURL: server.URL + "/api/v2", KeyEnv: "ERROR_TEST_KEY", Client: server.Client()}
	confirmed := false
	err := b.patchIssue(context.Background(), "EXAMPLE-1", url.Values{"summary": {"already set"}}, func(data []byte) error {
		confirmed = string(data) == `{"summary":"already set"}`
		return nil
	})
	if err != nil || reads.Load() != 1 || !confirmed {
		t.Fatalf("refusal was no longer read back: reads=%d confirmed=%t err=%v", reads.Load(), confirmed, err)
	}
}
