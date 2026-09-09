package githubapi

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// Two ways the staging wait can run out, and they are not the same fact. A
// run that appeared and never finished is a deployment that did not
// complete. A run that never appeared is a destination that started no
// deployment for this commit — nothing failed, and nothing deployed. Told
// as one, the second reads as "the deploy did not finish" about a deploy
// that was never going to run.
func TestTheStagingWaitSeparatesADeployThatNeverStartedFromOneThatNeverFinished(t *testing.T) {
	merge := MergeResult{
		PullRequestNumber: 7, BaseBranch: "stg", BaseSHA: shaA,
		HeadBranch: "automation/sample", HeadSHA: shaF, MergeSHA: sha4, TreeSHA: sha3,
	}
	runsQuery := "branch=stg&event=push&head_sha=" + sha4 + "&per_page=100"
	for _, tc := range []struct {
		name string
		runs string
		want string
	}{
		{
			name: "no run was ever created",
			runs: `{"total_count":0,"workflow_runs":[]}`,
			want: WorkflowRunAbsentCode,
		},
		{
			name: "a run was created and never completed",
			runs: workflowRunsJSON(41, 262913062, "Deploy API/Client (stg)", ".github/workflows/deploy-stg.yml", "stg", sha4, "in_progress", ""),
			want: "workflow_run_timeout",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps := []requestStep{
				{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + sha4, body: gitCommitJSON(sha4, sha3, shaA, shaF)},
				{method: http.MethodGet, path: "/repos/example/consumer/actions/workflows/262913062/runs", query: runsQuery, body: tc.runs},
			}
			controller, transport := newTestController(t, steps, true)
			// The window closes on the first wait rather than after fifty
			// minutes of real polling.
			controller.client.sleep = func(context.Context, time.Duration) error { return context.DeadlineExceeded }

			_, err := controller.AwaitStaging(context.Background(), merge,
				WaitOptions{PollInterval: time.Millisecond, Timeout: time.Minute}, DigestCommitPolicy{})
			if err == nil {
				t.Fatal("the wait returned no error")
			}
			if !IsInvariant(err, tc.want) {
				t.Fatalf("error = %v, want the invariant %q", err, tc.want)
			}
			transport.done()
		})
	}
}
