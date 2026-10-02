package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// githubClock stands in for the clock and the waiting, and gives the time it
// is and the waits asked for.
func githubClock(t *testing.T) (*time.Time, *[]time.Duration) {
	t.Helper()
	now := time.Unix(1_800_000_000, 0)
	var waits []time.Duration
	var mu sync.Mutex
	previousNow, previousWait := githubNow, githubWait
	githubNow = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	githubWait = func(_ context.Context, wait time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		waits = append(waits, wait)
		now = now.Add(wait)
		return nil
	}
	t.Cleanup(func() { githubNow, githubWait = previousNow, previousWait })
	return &now, &waits
}

func TestGitHubAsksAgainWithTheValidatorAndKeepsTheAnswerWhenNothingChanged(t *testing.T) {
	var conditional []string
	github, calls := githubFixture(t, func(base string, call int32, w http.ResponseWriter, r *http.Request) {
		conditional = append(conditional, r.Header.Get("If-None-Match"))
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		fmt.Fprint(w, `{"id":900,"login":"engine-bot"}`)
	})
	for range 2 {
		if me, err := github.Myself(context.Background()); err != nil || me != (Account{ID: 900, Login: "engine-bot"}) {
			t.Fatalf("myself: %+v (%v)", me, err)
		}
	}
	if calls.Load() != 2 || strings.Join(conditional, "|") != `|"v1"` {
		t.Fatalf("calls=%d validators sent: %q", calls.Load(), conditional)
	}
}

// A full first page that has not changed still leads to the page after it,
// where the hundred-and-first comment is: the page it named, kept with the
// answer, or the following one when it named none.
func TestGitHubReadsOnPastAFullPageThatDidNotChange(t *testing.T) {
	for _, named := range []bool{true, false} {
		t.Run(fmt.Sprintf("next page named=%v", named), func(t *testing.T) {
			comments := 100
			var asked []string
			github, _ := githubFixture(t, func(base string, call int32, w http.ResponseWriter, r *http.Request) {
				second := "page=2"
				if named {
					// A cursor the following page number would not reach.
					second = "after=cursor-1"
				}
				query := r.URL.Query()
				query.Del("per_page")
				part := query.Encode()
				asked = append(asked, part+" "+r.Header.Get("If-None-Match"))
				records := []any{}
				for id := 1; id <= comments; id++ {
					records = append(records, map[string]any{"id": id, "body": fmt.Sprintf("comment %d", id), "issue_url": base + "/repos/octo-org/widgets/issues/12"})
				}
				switch {
				case part == "":
					// The first hundred do not change.
					if r.Header.Get("If-None-Match") == `"first"` {
						w.WriteHeader(http.StatusNotModified)
						return
					}
					if named {
						w.Header().Set("Link", fmt.Sprintf(`<%s/repositories/1/issues/12/comments?per_page=100&after=cursor-1>; rel="next"`, base))
					}
					w.Header().Set("ETag", `"first"`)
					records = records[:100]
				case part == second:
					w.Header().Set("ETag", fmt.Sprintf(`"second of %d"`, comments))
					records = records[100:]
				default:
					records = []any{}
				}
				json.NewEncoder(w).Encode(records)
			})
			issue := Issue{ID: 12, Key: "12"}
			rows, err := github.Comments(context.Background(), issue)
			if err != nil || len(rows) != 100 {
				t.Fatalf("first read: %d (%v)", len(rows), err)
			}
			comments = 101
			rows, err = github.Comments(context.Background(), issue)
			if err != nil || len(rows) != 101 {
				t.Fatalf("second read: %d (%v), asked %q", len(rows), err, asked)
			}
			if comment, err := github.ReadComment(rows[100], issue); err != nil || comment.Body != "comment 101" {
				t.Fatalf("the hundred-and-first comment: %+v (%v)", comment, err)
			}
			second := "page=2"
			if named {
				second = "after=cursor-1"
			}
			if want := " |" + second + " | \"first\"|" + second + ` "second of 100"`; strings.Join(asked, "|") != want {
				t.Fatalf("asked %q, want %q", strings.Join(asked, "|"), want)
			}
		})
	}
}

func TestGitHubSendsNothingUntilTheAllowanceComesBack(t *testing.T) {
	now, _ := githubClock(t)
	reset := now.Add(10 * time.Minute)
	github, calls := githubFixture(t, func(base string, call int32, w http.ResponseWriter, r *http.Request) {
		if call == 1 {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		}
		fmt.Fprint(w, `{"id":900,"login":"engine-bot"}`)
	})
	ctx := context.Background()
	if _, err := github.Myself(ctx); err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Duration{0, 9*time.Minute + 59*time.Second} {
		*now = reset.Add(-10 * time.Minute).Add(at)
		if _, err := github.Myself(ctx); err == nil || !strings.Contains(err.Error(), "nothing was sent") {
			t.Fatalf("asked again %s after the allowance ran out: %v", at, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("requests reached the server while it said to wait: %d", calls.Load())
	}
	*now = reset
	if _, err := github.Myself(ctx); err != nil || calls.Load() != 2 {
		t.Fatalf("not asked again once the allowance came back: calls=%d (%v)", calls.Load(), err)
	}
}

func TestGitHubWaitsAsLongAsALimitSays(t *testing.T) {
	now, _ := githubClock(t)
	answers := []func(w http.ResponseWriter){
		func(w http.ResponseWriter) {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
		},
		func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"You have exceeded a secondary rate limit."}`)
		},
		func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"You have exceeded a secondary rate limit."}`)
		},
		func(w http.ResponseWriter) { fmt.Fprint(w, `{"id":900,"login":"engine-bot"}`) },
		func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"Resource not accessible by personal access token"}`)
		},
		func(w http.ResponseWriter) { fmt.Fprint(w, `{"id":900,"login":"engine-bot"}`) },
	}
	github, calls := githubFixture(t, func(base string, call int32, w http.ResponseWriter, r *http.Request) {
		answers[call-1](w)
	})
	ctx := context.Background()
	// Each step: how long after the previous request the next is tried, and
	// whether it reaches the server.
	for i, step := range []struct {
		after   time.Duration
		reaches bool
	}{
		{0, true},                  // 429, wait 30 seconds
		{29 * time.Second, false},  // still waiting
		{time.Second, true},        // a secondary limit that says nothing: a minute
		{59 * time.Second, false},  //
		{time.Second, true},        // the same again: two minutes
		{119 * time.Second, false}, //
		{time.Second, true},        // answered
		{0, true},                  // refused for want of permission: no waiting
		{0, true},                  // answered at once
	} {
		*now = now.Add(step.after)
		before := calls.Load()
		github.Myself(ctx)
		if reached := calls.Load() > before; reached != step.reaches {
			t.Fatalf("step %d: reached the server %t, want %t", i, reached, step.reaches)
		}
	}
}

func TestGitHubSpacesChangesASecondApart(t *testing.T) {
	_, waits := githubClock(t)
	github, calls := githubFixture(t, func(base string, call int32, w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
		}
		fmt.Fprintf(w, `{"id":%d,"login":"engine-bot"}`, call)
	})
	ctx := context.Background()
	address := github.base() + "/repos/octo-org/widgets/issues/12/comments"
	for range 2 {
		if _, _, err := github.call(ctx, http.MethodPost, address, map[string]string{"body": "words"}, http.StatusCreated, githubItemLimit); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := github.Myself(ctx); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || len(*waits) != 1 || (*waits)[0] != time.Second {
		t.Fatalf("calls=%d waits=%v: changes were not a second apart, or a read waited", calls.Load(), *waits)
	}
}

func TestGitHubKeepsABoundedAmountOfAnswers(t *testing.T) {
	shared := &githubShared{kept: map[string]githubKept{}}
	for i := range githubKeptCount + 10 {
		shared.keep(fmt.Sprint(i), githubKept{etag: `"x"`, data: []byte("answer")})
	}
	if _, found := shared.recall("0"); found || len(shared.kept) != githubKeptCount {
		t.Fatalf("kept %d answers, the oldest among them: %t", len(shared.kept), found)
	}
	shared.keep("big", githubKept{etag: `"x"`, data: make([]byte, githubKeptItem+1)})
	if _, found := shared.recall("big"); found {
		t.Fatal("an answer longer than the bound was kept")
	}
	for i := range 10 {
		shared.keep(fmt.Sprint("large", i), githubKept{etag: `"x"`, data: make([]byte, githubKeptItem)})
	}
	if shared.size > githubKeptSize {
		t.Fatalf("kept %d bytes", shared.size)
	}
	shared.keep("large9", githubKept{data: []byte("no validator")})
	if _, found := shared.recall("large9"); found {
		t.Fatal("an answer without a validator replaced nothing")
	}
}
