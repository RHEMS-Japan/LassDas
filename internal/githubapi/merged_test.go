package githubapi

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// The debug role's merge wait observes a HUMAN decision, so these tests pin
// the three properties that matter: only the contracted merge shape is
// accepted, a refusal (closed unmerged) stops the wait immediately, and a
// transient transport failure never does — the deadline is the only bound.

func mergeWaitPull() PullRequest {
	return PullRequest{Number: 41, BaseRef: "stg", HeadRef: "feature/x", HeadSHA: shaB}
}

func mergeWaitOptions() WaitOptions {
	return WaitOptions{PollInterval: time.Millisecond, Timeout: time.Second}
}

func pullStep(body string) requestStep {
	return requestStep{method: http.MethodGet, path: "/repos/example/consumer/pulls/41", body: body}
}

func commitStep(body string) requestStep {
	return requestStep{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + shaC, body: body}
}

func openPullBody() string {
	return `{"number":41,"state":"open","merged":false,"merge_commit_sha":""}`
}

func mergedPullBody() string {
	return `{"number":41,"state":"closed","merged":true,"merge_commit_sha":"` + shaC + `"}`
}

func mergeCommitBody(parents ...string) string {
	body := `{"sha":"` + shaC + `","tree":{"sha":"` + shaD + `"},"parents":[`
	for index, parent := range parents {
		if index > 0 {
			body += ","
		}
		body += `{"sha":"` + parent + `"}`
	}
	return body + `]}`
}

func assertInvariantCode(t *testing.T, err error, code string) {
	t.Helper()
	var violation *InvariantError
	if !errors.As(err, &violation) || violation.Code != code {
		t.Fatalf("error = %v, want invariant %s", err, code)
	}
}

func TestAwaitFeatureMergeReturnsTheObservedHumanMerge(t *testing.T) {
	controller, transport := newTestController(t, []requestStep{
		pullStep(openPullBody()),
		pullStep(mergedPullBody()),
		commitStep(mergeCommitBody(shaA, shaB)),
	}, true)
	merge, err := controller.AwaitFeatureMerge(context.Background(), mergeWaitPull(), mergeWaitOptions())
	if err != nil {
		t.Fatalf("AwaitFeatureMerge() error = %v", err)
	}
	want := MergeResult{
		PullRequestNumber: 41,
		BaseBranch:        "stg", BaseSHA: shaA,
		HeadBranch: "feature/x", HeadSHA: shaB,
		MergeSHA: shaC, TreeSHA: shaD,
	}
	if merge != want {
		t.Fatalf("merge = %+v, want %+v", merge, want)
	}
	transport.done()
}

// The merge API can name a commit the git commits API does not serve yet;
// that 404 must be polled through, not treated as the permanent kind.
func TestAwaitFeatureMergeOutlivesTheMergeCommitVisibilityLag(t *testing.T) {
	controller, transport := newTestController(t, []requestStep{
		pullStep(mergedPullBody()),
		{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + shaC, status: http.StatusNotFound, body: `{}`},
		pullStep(mergedPullBody()),
		commitStep(mergeCommitBody(shaA, shaB)),
	}, true)
	merge, err := controller.AwaitFeatureMerge(context.Background(), mergeWaitPull(), mergeWaitOptions())
	if err != nil || merge.MergeSHA != shaC {
		t.Fatalf("AwaitFeatureMerge() = %+v, %v", merge, err)
	}
	transport.done()
}

func TestAwaitFeatureMergeOutlivesTransientReadFailures(t *testing.T) {
	controller, transport := newTestController(t, []requestStep{
		{method: http.MethodGet, path: "/repos/example/consumer/pulls/41", err: errors.New("connection reset")},
		{method: http.MethodGet, path: "/repos/example/consumer/pulls/41", status: http.StatusInternalServerError, body: `{}`},
		pullStep(mergedPullBody()),
		{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + shaC, status: http.StatusBadGateway, body: `{}`},
		pullStep(mergedPullBody()),
		commitStep(mergeCommitBody(shaA, shaB)),
	}, true)
	merge, err := controller.AwaitFeatureMerge(context.Background(), mergeWaitPull(), mergeWaitOptions())
	if err != nil || merge.MergeSHA != shaC {
		t.Fatalf("AwaitFeatureMerge() = %+v, %v", merge, err)
	}
	transport.done()
}

// A credential loss or a vanished pull request never heals: the wait must
// stop with the actual cause instead of polling it away for 72 hours.
func TestAwaitFeatureMergeStopsOnPermanentReadFailures(t *testing.T) {
	for status, code := range map[int]string{
		http.StatusNotFound:  "not_found",
		http.StatusForbidden: "authentication_failed",
	} {
		controller, transport := newTestController(t, []requestStep{
			{method: http.MethodGet, path: "/repos/example/consumer/pulls/41", status: status, body: `{}`},
		}, true)
		_, err := controller.AwaitFeatureMerge(context.Background(), mergeWaitPull(), mergeWaitOptions())
		var failure *APIError
		if !errors.As(err, &failure) || failure.Code != code {
			t.Fatalf("status %d: error = %v, want the permanent %s to stop the wait", status, err, code)
		}
		transport.done()
	}
}

func TestAwaitFeatureMergeRefusesAPullRequestClosedUnmerged(t *testing.T) {
	controller, transport := newTestController(t, []requestStep{
		pullStep(`{"number":41,"state":"closed","merged":false,"merge_commit_sha":""}`),
	}, true)
	_, err := controller.AwaitFeatureMerge(context.Background(), mergeWaitPull(), mergeWaitOptions())
	assertInvariantCode(t, err, "pull_request_closed_unmerged")
	transport.done()
}

func TestAwaitFeatureMergeRefusesAForeignMergeShape(t *testing.T) {
	// A single parent means the repository squashed despite the contract.
	controller, transport := newTestController(t, []requestStep{
		pullStep(mergedPullBody()),
		commitStep(mergeCommitBody(shaA)),
	}, true)
	_, err := controller.AwaitFeatureMerge(context.Background(), mergeWaitPull(), mergeWaitOptions())
	assertInvariantCode(t, err, "unsupported_merge_shape")
	transport.done()

	// Two parents whose second is not the delivered head: someone else's
	// branch rode the merge commit.
	controller, transport = newTestController(t, []requestStep{
		pullStep(mergedPullBody()),
		commitStep(mergeCommitBody(shaA, shaE)),
	}, true)
	_, err = controller.AwaitFeatureMerge(context.Background(), mergeWaitPull(), mergeWaitOptions())
	assertInvariantCode(t, err, "unsupported_merge_shape")
	transport.done()
}

func TestAwaitFeatureMergeTimesOutHonestly(t *testing.T) {
	// Probes land at 0ms and 80ms; the 120ms deadline then beats the next
	// 160ms poll with 40ms to spare on both sides — exactly two scripted
	// reads, then the honest timeout.
	controller, transport := newTestController(t, []requestStep{
		pullStep(openPullBody()),
		pullStep(openPullBody()),
	}, true)
	_, err := controller.AwaitFeatureMerge(context.Background(), mergeWaitPull(), WaitOptions{
		PollInterval: 80 * time.Millisecond, Timeout: 120 * time.Millisecond,
	})
	assertInvariantCode(t, err, "merge_wait_timeout")
	transport.done()
}

func TestAwaitFeatureMergeValidatesItsInput(t *testing.T) {
	controller, transport := newTestController(t, nil, true)
	if _, err := controller.AwaitFeatureMerge(context.Background(), PullRequest{Number: 0, HeadSHA: shaB}, mergeWaitOptions()); err == nil {
		t.Fatal("a pull request without a number was accepted")
	}
	if _, err := controller.AwaitFeatureMerge(context.Background(), PullRequest{Number: 41, HeadSHA: "short"}, mergeWaitOptions()); err == nil {
		t.Fatal("a pull request with an invalid head SHA was accepted")
	}
	transport.done()

	unverified, transport := newTestController(t, nil, false)
	if _, err := unverified.AwaitFeatureMerge(context.Background(), mergeWaitPull(), mergeWaitOptions()); err == nil {
		t.Fatal("an unverified repository was accepted")
	}
	transport.done()
}
