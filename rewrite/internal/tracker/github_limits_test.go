package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMain fails the package when its tests leave behind anything the
// process keeps for a GitHub API: a later fake server given the same port
// would find it.
func TestMain(m *testing.M) {
	code := m.Run()
	githubStates.Lock()
	var left []string
	for key := range githubStates.shared {
		left = append(left, strings.ReplaceAll(key, "\x00", " "))
	}
	githubStates.Unlock()
	if code == 0 && len(left) > 0 {
		fmt.Fprintf(os.Stderr, "the tests left what GitHub said behind for %q\n", left)
		code = 1
	}
	os.Exit(code)
}

// githubClock stands in for the clock and the waiting, and gives the time it
// is, the waits asked for, and a way to move the clock on from another
// goroutine (a fake server's): it takes the clock's lock and records no wait.
func githubClock(t *testing.T) (*time.Time, *[]time.Duration, func(time.Duration)) {
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
	advance := func(by time.Duration) { mu.Lock(); defer mu.Unlock(); now = now.Add(by) }
	t.Cleanup(func() { githubNow, githubWait = previousNow, previousWait })
	return &now, &waits, advance
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
	now, _, _ := githubClock(t)
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
	now, _, _ := githubClock(t)
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
	_, waits, _ := githubClock(t)
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

// A limit that gives no time still to come waits a minute as one that gives
// none does, and no wait is longer than an hour. The end of an hourly
// allowance that is not spent is no time of a limit's.
func TestGitHubWaitsAMinuteForALimitWhoseTimeIsGoneAndNeverMoreThanAnHour(t *testing.T) {
	for name, test := range map[string]struct {
		status int
		header map[string]string
		body   string
		wait   time.Duration
	}{
		"reset already past":         {403, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1799999940"}, "API rate limit exceeded", time.Minute},
		"reset at zero":              {403, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "0"}, "API rate limit exceeded", time.Minute},
		"retry after zero":           {429, map[string]string{"Retry-After": "0"}, "", time.Minute},
		"retry after unreadable":     {429, map[string]string{"Retry-After": "soon"}, "", time.Minute},
		"retry after past int64":     {429, map[string]string{"Retry-After": "99999999999999999999"}, "", time.Minute},
		"older secondary wording":    {403, nil, "You have triggered an abuse detection mechanism.", time.Minute},
		"allowance not spent":        {403, map[string]string{"X-RateLimit-Remaining": "4000", "X-RateLimit-Reset": "1800002700"}, "You have exceeded a secondary rate limit.", time.Minute},
		"retry after out of measure": {429, map[string]string{"Retry-After": "9300000000"}, "", time.Hour},
		"reset a day ahead":          {200, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1800086400"}, "", time.Hour},
	} {
		t.Run(name, func(t *testing.T) {
			now, _, _ := githubClock(t)
			github, calls := githubFixture(t, func(base string, call int32, w http.ResponseWriter, r *http.Request) {
				if call == 1 {
					for key, value := range test.header {
						w.Header().Set(key, value)
					}
					w.WriteHeader(test.status)
					fmt.Fprint(w, test.body)
					return
				}
				fmt.Fprint(w, `{"id":900,"login":"engine-bot"}`)
			})
			ctx := context.Background()
			github.Myself(ctx)
			start := *now
			*now = start.Add(test.wait - time.Second)
			if _, err := github.Myself(ctx); err == nil || calls.Load() != 1 {
				t.Fatalf("asked again before the wait was over: calls=%d (%v)", calls.Load(), err)
			}
			*now = start.Add(test.wait)
			if _, err := github.Myself(ctx); err != nil || calls.Load() != 2 {
				t.Fatalf("not asked again once the wait was over: calls=%d (%v)", calls.Load(), err)
			}
		})
	}
}

// A failure that is not a limit is waited for as long as its Retry-After
// says, and no longer than an hour; one without it is not, and neither is a
// success that carries one.
func TestGitHubWaitsForTheRetryAfterOfAnyFailure(t *testing.T) {
	for name, test := range map[string]struct {
		status int
		after  string
		wait   time.Duration
	}{
		"unavailable for a minute":     {503, "60", time.Minute},
		"not found for thirty seconds": {404, "30", 30 * time.Second},
		"failed out of measure":        {500, "9300000000", time.Hour},
		"unavailable without a time":   {503, "", 0},
		"unavailable, unreadable time": {503, "soon", 0},
		"answered with a time":         {200, "60", 0},
	} {
		t.Run(name, func(t *testing.T) {
			now, _, _ := githubClock(t)
			github, calls := githubFixture(t, func(base string, call int32, w http.ResponseWriter, r *http.Request) {
				if call == 1 {
					if test.after != "" {
						w.Header().Set("Retry-After", test.after)
					}
					w.WriteHeader(test.status)
				}
				fmt.Fprint(w, `{"id":900,"login":"engine-bot"}`)
			})
			ctx := context.Background()
			github.Myself(ctx)
			start := *now
			if test.wait > 0 {
				*now = start.Add(test.wait - time.Second)
				if _, err := github.Myself(ctx); err == nil || calls.Load() != 1 {
					t.Fatalf("asked again before the wait was over: calls=%d (%v)", calls.Load(), err)
				}
			}
			*now = start.Add(test.wait)
			if _, err := github.Myself(ctx); err != nil || calls.Load() != 2 {
				t.Fatalf("not asked again once the wait was over: calls=%d (%v)", calls.Load(), err)
			}
		})
	}
}

// A success ends the doubling: the next limit that gives no time waits a
// minute again. The later of a refusal's seconds and the end of a spent
// allowance is the one waited for, whichever of the two it is.
func TestGitHubStartsAgainFromAMinuteAndWaitsForTheLaterTime(t *testing.T) {
	now, _, _ := githubClock(t)
	start := *now
	silent := func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"You have exceeded a secondary rate limit."}`)
	}
	answers := []func(w http.ResponseWriter){
		silent,
		func(w http.ResponseWriter) { fmt.Fprint(w, `{"id":900,"login":"engine-bot"}`) },
		silent,
		func(w http.ResponseWriter) {
			w.Header().Set("Retry-After", "30")
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(start.Add(10*time.Minute).Unix(), 10))
			w.WriteHeader(http.StatusTooManyRequests)
		},
		func(w http.ResponseWriter) { fmt.Fprint(w, `{"id":900,"login":"engine-bot"}`) },
		func(w http.ResponseWriter) {
			w.Header().Set("Retry-After", "600")
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(start.Add(10*time.Minute+30*time.Second).Unix(), 10))
			w.WriteHeader(http.StatusTooManyRequests)
		},
		func(w http.ResponseWriter) { fmt.Fprint(w, `{"id":900,"login":"engine-bot"}`) },
	}
	github, calls := githubFixture(t, func(base string, call int32, w http.ResponseWriter, r *http.Request) {
		answers[call-1](w)
	})
	ctx := context.Background()
	for i, step := range []struct {
		at      time.Duration
		reaches bool
	}{
		{0, true},                                // a limit that gives no time: a minute
		{time.Minute, true},                      // answered
		{time.Minute, true},                      // the same limit again: a minute, not two
		{2*time.Minute - time.Second, false},     //
		{2 * time.Minute, true},                  // thirty seconds, or until the allowance comes back
		{2*time.Minute + 31*time.Second, false},  // the later one holds
		{10 * time.Minute, true},                 // answered
		{10 * time.Minute, true},                 // ten minutes, or until the allowance comes back in thirty seconds
		{10*time.Minute + 31*time.Second, false}, // the later one holds again
		{20*time.Minute - time.Second, false},    //
		{20 * time.Minute, true},                 //
	} {
		*now = start.Add(step.at)
		before := calls.Load()
		github.Myself(ctx)
		if reached := calls.Load() > before; reached != step.reaches {
			t.Fatalf("step %d: reached the server %t, want %t", i, reached, step.reaches)
		}
	}
}

// A change waiting for its turn when GitHub says to wait is not sent once its
// turn comes.
func TestGitHubSendsNoChangeThatWaitedThroughALimit(t *testing.T) {
	githubClock(t)
	waiting, release := make(chan struct{}, 1), make(chan struct{})
	previousWait := githubWait
	githubWait = func(ctx context.Context, wait time.Duration) error {
		waiting <- struct{}{}
		<-release
		return previousWait(ctx, wait)
	}
	var mu sync.Mutex
	posts := 0
	github, _ := githubFixture(t, func(base string, call int32, w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			mu.Lock()
			posts++
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":1}`)
			return
		}
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	ctx := context.Background()
	address := github.base() + "/repos/octo-org/widgets/issues/12/comments"
	if _, _, err := github.call(ctx, http.MethodPost, address, map[string]string{"body": "first"}, http.StatusCreated, githubItemLimit); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := github.call(ctx, http.MethodPost, address, map[string]string{"body": "second"}, http.StatusCreated, githubItemLimit)
		done <- err
	}()
	<-waiting
	github.Myself(ctx)
	close(release)
	err := <-done
	mu.Lock()
	defer mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "nothing was sent") || posts != 1 {
		t.Fatalf("a change was sent after GitHub said to wait: posts=%d (%v)", posts, err)
	}
}

// Changes go one at a time, each at least a second after the answer to the
// one before; a slow answer moves the next change later, not earlier.
func TestGitHubSendsChangesOneAtATimeASecondAfterTheLastAnswer(t *testing.T) {
	_, waits, advance := githubClock(t)
	var mu sync.Mutex
	posts := 0
	hold := make(chan struct{})
	github, _ := githubFixture(t, func(base string, call int32, w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		posts++
		first := posts == 1
		mu.Unlock()
		if first {
			<-hold
			// The first answer takes a while to come back.
			advance(300 * time.Millisecond)
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":1}`)
	})
	ctx := context.Background()
	address := github.base() + "/repos/octo-org/widgets/issues/12/comments"
	send := func(done chan<- error) {
		_, _, err := github.call(ctx, http.MethodPost, address, map[string]string{"body": "words"}, http.StatusCreated, githubItemLimit)
		done <- err
	}
	first, second := make(chan error, 1), make(chan error, 1)
	go send(first)
	for {
		mu.Lock()
		arrived := posts
		mu.Unlock()
		if arrived == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	go send(second)
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	early := posts
	mu.Unlock()
	close(hold)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if early != 1 || posts != 2 || len(*waits) != 1 || (*waits)[0] != time.Second {
		t.Fatalf("a change went before the one before was answered, or less than a second after its answer: early=%d posts=%d waits=%v", early, posts, *waits)
	}
}

// A change that stops waiting for the second after the last answer is not
// sent and gives its turn back: the next change goes, a second after that
// answer.
func TestGitHubGivesTheTurnBackWhenAChangeStopsWaiting(t *testing.T) {
	_, waits, _ := githubClock(t)
	previousWait := githubWait
	stop := true
	githubWait = func(ctx context.Context, wait time.Duration) error {
		if stop {
			stop = false
			return context.Canceled
		}
		return previousWait(ctx, wait)
	}
	github, calls := githubFixture(t, func(base string, call int32, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":1}`)
	})
	address := github.base() + "/repos/octo-org/widgets/issues/12/comments"
	send := func(ctx context.Context) error {
		_, _, err := github.call(ctx, http.MethodPost, address, map[string]string{"body": "words"}, http.StatusCreated, githubItemLimit)
		return err
	}
	if err := send(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := send(context.Background()); !errors.Is(err, context.Canceled) || calls.Load() != 1 {
		t.Fatalf("a change that stopped waiting was sent, or failed otherwise: calls=%d (%v)", calls.Load(), err)
	}
	// A turn never given back holds the next change until its deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := send(ctx); err != nil || calls.Load() != 2 || len(*waits) != 1 || (*waits)[0] != time.Second {
		t.Fatalf("the turn was not given back: calls=%d waits=%v (%v)", calls.Load(), *waits, err)
	}
}

// A change carries no validator, and what the caller is handed is a copy:
// changing it changes nothing kept.
func TestGitHubKeepsItsOwnCopyAndSendsNoValidatorWithAChange(t *testing.T) {
	githubClock(t)
	var validators []string
	github, _ := githubFixture(t, func(base string, call int32, w http.ResponseWriter, r *http.Request) {
		validators = append(validators, r.Method+" "+r.Header.Get("If-None-Match"))
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Link", `<`+base+`/repositories/1/issues/12/comments?page=2>; rel="next"`)
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
		}
		fmt.Fprint(w, `[{"id":1}]`)
	})
	ctx := context.Background()
	address := github.base() + "/repos/octo-org/widgets/issues/12/comments"
	// The first answer is kept; the second and third are the kept one.
	for range 3 {
		data, header, err := github.call(ctx, http.MethodGet, address, nil, http.StatusOK, githubItemLimit)
		if err != nil || string(data) != `[{"id":1}]` || !strings.Contains(header.Get("Link"), "page=2") {
			t.Fatalf("read %q %q (%v)", data, header.Get("Link"), err)
		}
		data[0] = 'x'
		header.Set("Link", "changed")
	}
	if _, _, err := github.call(ctx, http.MethodPost, address, map[string]string{"body": "words"}, http.StatusCreated, githubItemLimit); err != nil {
		t.Fatal(err)
	}
	if strings.Join(validators, "|") != `GET |GET "v1"|GET "v1"|POST ` {
		t.Fatalf("validators sent: %q", validators)
	}
}

// A closed fake server leaves nothing for a later one given its port, such as
// a wait it was told of under the stand-in clock, which the real clock has
// not reached.
func TestGitHubFakeServersLeaveNothingForTheNextOnTheirPort(t *testing.T) {
	var base string
	t.Run("told to wait", func(t *testing.T) {
		githubClock(t)
		github, _ := githubFixture(t, func(base string, call int32, w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
		})
		github.Myself(context.Background())
		if _, err := github.Myself(context.Background()); err == nil || !strings.Contains(err.Error(), "nothing was sent") {
			t.Fatalf("the wait was not kept while the server was open: %v", err)
		}
		base = github.base()
	})
	githubStates.Lock()
	defer githubStates.Unlock()
	for key := range githubStates.shared {
		if strings.HasPrefix(key, base+"\x00") {
			t.Fatalf("the closed server left its state for the next one on its port: %q", key)
		}
	}
}
