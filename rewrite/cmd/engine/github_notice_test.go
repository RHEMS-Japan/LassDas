package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"ticket-runner/internal/chain"
)

// Only the controller's failure detail is presentation text. A role's post
// continues through the ordinary scope unchanged (github_scope_test.go).
func TestGitHubStallNoticeQuotesFailureBeforeSavingAndPosting(t *testing.T) {
	for _, test := range []struct{ name, failure, want string }{
		{"mention", "failed for @example", "` failed for @example `"},
		{"one tick", "` @example", "`` ` @example ``"},
		{"two ticks", "`` @example ``", "``` `` @example `` ```"},
		{"three ticks", "``` @example ```", "```` ``` @example ``` ````"},
		{"mixed ticks", "`a` ``b`` ``` @example", "```` `a` ``b`` ``` @example ````"},
		{"markup", "<b>@example</b> &amp; \\`", "`` <b>@example</b> &amp; \\` ``"},
		{"process reason", "exit status 1\nintermediate @other\n`@example`", "`` exit status 1: `@example` ``"},
		{"credential", "@example synthetic-watch-key", "` @example [credential] `"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := githubConfiguration(t)
			start := time.Now().UTC().Add(-3 * time.Hour)
			root, directory := noticeJob(t, chain.State{History: []chain.Result{
				{Role: "implement", Speaker: "worker", Output: "previous work", StartedAt: start, FinishedAt: start.Add(time.Minute)},
				{Role: "router", Speaker: "runtime", Error: test.failure, StartedAt: start.Add(time.Hour), FinishedAt: start.Add(time.Hour)},
			}})
			queueRanSince(t, root, cfg, start)
			var posted []string
			useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodPost || r.URL.String() != cfg.GitHub.APIURL+"/repos/example/project/issues/51/comments" {
					return nil, fmt.Errorf("unexpected operation: %s %s", r.Method, r.URL)
				}
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					return nil, err
				}
				posted = append(posted, body["body"])
				return selectionReply(r, 201, githubCommentRow(cfg, 51, 901, 99, body["body"])), nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			n := requestNotices(cfg, sourceIssue{ID: 51, Key: "51"}, directory)
			for range 2 {
				if err := noteStall(ctx, cfg, n, directory, true, start); err != nil {
					t.Fatal(err)
				}
			}
			if err := n.flush(ctx); err != nil {
				t.Fatal(err)
			}
			records := readNotices(t, directory).Notices
			if len(posted) != 1 || len(records) != 1 || records[0].Text != posted[0] || records[0].CommentID != 901 {
				t.Fatalf("notice was changed, lost or repeated: posts=%v records=%+v", posted, records)
			}
			if !strings.HasSuffix(posted[0], "（直近の失敗: "+test.want+"）。") {
				t.Fatalf("failure escaped its code span: %q; want detail %q", posted[0], test.want)
			}
			encoded, err := json.Marshal(posted[0])
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("NOTICE_JSON=%s", encoded)
			if state := loadJobState(t, directory); state.History[1].Error != test.failure {
				t.Fatal("presentation changed the original failure record")
			}
		})
	}
}

func TestGitHubNoticeQuotesAfterScrubbingAndCuttingOnlyNonemptyDetails(t *testing.T) {
	cfg := githubConfiguration(t)
	if got := noticeDetail(cfg, " \n\t "); got != "" {
		t.Fatalf("an absent failure became a code span: %q", got)
	}
	// The credential itself contains a longer tick run. It is removed before
	// quoting; no partial credential or unbounded delimiter reaches the post.
	t.Setenv("WATCH_TEST_KEY", "synthetic-"+strings.Repeat("`", 230)+"-key")
	failure := "synthetic-" + strings.Repeat("`", 230) + "-key " + strings.Repeat("a", 300) + " @outside"
	want := "` [credential] " + strings.Repeat("a", 187) + " `"
	if got := noticeDetail(cfg, failure); got != want {
		t.Fatalf("scrub/cut/quote order changed: %q", got)
	}
	backlog := watchConfiguration(t)
	if got := noticeDetail(backlog, "` @example `"); got != "` @example `" {
		t.Fatalf("Backlog's existing presentation changed: %q", got)
	}
}
