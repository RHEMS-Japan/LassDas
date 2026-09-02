package githubapi

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAmbiguousMutationErrorClassification(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "request transport", err: &APIError{Code: "request_failed"}, want: true},
		{name: "accepted response read", err: &APIError{Status: http.StatusCreated, Code: "response_read_failed"}, want: true},
		{name: "accepted response too large", err: &APIError{Status: http.StatusOK, Code: "response_too_large"}, want: true},
		{name: "accepted invalid response", err: &APIError{Status: http.StatusCreated, Code: "invalid_response"}, want: true},
		{name: "unprocessable", err: &APIError{Status: http.StatusUnprocessableEntity, Code: "unprocessable"}},
		{name: "server error", err: &APIError{Status: http.StatusBadGateway, Code: "server_error"}, want: true},
		{name: "invalid response on failure status", err: &APIError{Status: http.StatusBadGateway, Code: "invalid_response"}},
		{name: "invariant", err: invariant("test")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := isAmbiguousMutationError(test.err); got != test.want {
				t.Fatalf("isAmbiguousMutationError(%v) = %t, want %t", test.err, got, test.want)
			}
		})
	}
}

func TestCreateExactBranchRefReconcilesTransportFailureWithExactRef(t *testing.T) {
	steps := []requestStep{
		{method: http.MethodPost, path: "/repos/example/consumer/git/refs", err: io.ErrUnexpectedEOF},
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/automation/sample", body: refJSON("automation/sample", shaF)},
	}
	controller, transport := newTestController(t, steps, true)

	if err := controller.createExactBranchRef(context.Background(), "automation/sample", shaF); err != nil {
		t.Fatalf("createExactBranchRef() error = %v", err)
	}
	transport.done()
}

func TestCreateExactBranchRefReconcilesAcceptedInvalidResponse(t *testing.T) {
	steps := []requestStep{
		{method: http.MethodPost, path: "/repos/example/consumer/git/refs", status: http.StatusCreated, body: "{"},
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/automation/sample", body: refJSON("automation/sample", shaF)},
	}
	controller, transport := newTestController(t, steps, true)

	if err := controller.createExactBranchRef(context.Background(), "automation/sample", shaF); err != nil {
		t.Fatalf("createExactBranchRef() error = %v", err)
	}
	transport.done()
}

func TestCreateExactBranchRefReconcilesAcceptedSemanticMismatch(t *testing.T) {
	steps := []requestStep{
		{method: http.MethodPost, path: "/repos/example/consumer/git/refs", status: http.StatusCreated, body: `{}`},
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/automation/sample", body: refJSON("automation/sample", shaF)},
	}
	controller, transport := newTestController(t, steps, true)

	if err := controller.createExactBranchRef(context.Background(), "automation/sample", shaF); err != nil {
		t.Fatalf("createExactBranchRef() error = %v", err)
	}
	transport.done()
}

func TestCreateExactBranchRefReconcilesAcceptedResponseReadFailure(t *testing.T) {
	steps := []requestStep{
		{method: http.MethodPost, path: "/repos/example/consumer/git/refs", status: http.StatusCreated, readErr: io.ErrUnexpectedEOF},
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/automation/sample", body: refJSON("automation/sample", shaF)},
	}
	controller, transport := newTestController(t, steps, true)

	if err := controller.createExactBranchRef(context.Background(), "automation/sample", shaF); err != nil {
		t.Fatalf("createExactBranchRef() error = %v", err)
	}
	transport.done()
}

func TestCreateExactBranchRefReconcilesServerAndExistingResourceResponses(t *testing.T) {
	for _, status := range []int{http.StatusConflict, http.StatusUnprocessableEntity, http.StatusBadGateway} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			steps := []requestStep{
				{method: http.MethodPost, path: "/repos/example/consumer/git/refs", status: status, body: `{}`},
				{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/automation/sample", body: refJSON("automation/sample", shaF)},
			}
			controller, transport := newTestController(t, steps, true)

			if err := controller.createExactBranchRef(context.Background(), "automation/sample", shaF); err != nil {
				t.Fatalf("createExactBranchRef() error = %v", err)
			}
			transport.done()
		})
	}
}

func TestCreateExactBranchRefRetriesTransientReconciliationRead(t *testing.T) {
	steps := []requestStep{
		{method: http.MethodPost, path: "/repos/example/consumer/git/refs", status: http.StatusBadGateway, body: `{}`},
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/automation/sample", status: http.StatusBadGateway, body: `{}`},
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/automation/sample", body: refJSON("automation/sample", shaF)},
	}
	controller, transport := newTestController(t, steps, true)

	if err := controller.createExactBranchRef(context.Background(), "automation/sample", shaF); err != nil {
		t.Fatalf("createExactBranchRef() error = %v", err)
	}
	transport.done()
}

func TestCreateExactBranchRefRejectsInexactExistingResource(t *testing.T) {
	steps := []requestStep{
		{method: http.MethodPost, path: "/repos/example/consumer/git/refs", status: http.StatusUnprocessableEntity, body: `{}`},
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/automation/sample", body: refJSON("automation/sample", shaE)},
	}
	controller, transport := newTestController(t, steps, true)

	err := controller.createExactBranchRef(context.Background(), "automation/sample", shaF)
	if err == nil || !isStatus(err, http.StatusUnprocessableEntity) {
		t.Fatalf("createExactBranchRef() error = %v", err)
	}
	transport.done()
}

func TestCreateExactBranchRefDoesNotReinterpretOtherHTTPFailure(t *testing.T) {
	steps := []requestStep{{method: http.MethodPost, path: "/repos/example/consumer/git/refs", status: http.StatusBadRequest, body: `{}`}}
	controller, transport := newTestController(t, steps, true)

	err := controller.createExactBranchRef(context.Background(), "automation/sample", shaF)
	if err == nil || isAmbiguousMutationError(err) || isExistingResourceMutationError(err) || !isStatus(err, http.StatusBadRequest) {
		t.Fatalf("createExactBranchRef() error = %v", err)
	}
	transport.done()
}

func TestCreateFeaturePullRequestReconcilesTransportFailureWithUniqueExactPull(t *testing.T) {
	query := "base=stg&head=example%3Aautomation%2Fsample&per_page=100&state=open"
	steps := []requestStep{
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/stg", body: refJSON("stg", shaA)},
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/automation/sample", body: refJSON("automation/sample", shaF)},
		{method: http.MethodPost, path: "/repos/example/consumer/pulls", err: io.ErrUnexpectedEOF},
		{method: http.MethodGet, path: "/repos/example/consumer/pulls", query: query, body: `[` + pullJSON(7, "open", "automation/sample", shaF, "stg", shaA, nil) + `]`},
	}
	controller, transport := newTestController(t, steps, true)

	pull, err := controller.CreateFeaturePullRequest(context.Background(), PublishedFeature{
		Base: Snapshot{Branch: "stg", SHA: shaA}, Branch: "automation/sample", HeadSHA: shaF,
	}, PullRequestSpec{Title: "sample", Body: "ticket"})
	if err != nil {
		t.Fatalf("CreateFeaturePullRequest() error = %v", err)
	}
	if pull.Number != 7 || pull.HeadSHA != shaF || pull.BaseSHA != shaA {
		t.Fatalf("pull = %+v", pull)
	}
	transport.done()
}

func TestCreateFeaturePullRequestReconcilesServerAndExistingResourceResponses(t *testing.T) {
	query := "base=stg&head=example%3Aautomation%2Fsample&per_page=100&state=open"
	for _, status := range []int{http.StatusConflict, http.StatusUnprocessableEntity, http.StatusBadGateway} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			steps := []requestStep{
				{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/stg", body: refJSON("stg", shaA)},
				{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/automation/sample", body: refJSON("automation/sample", shaF)},
				{method: http.MethodPost, path: "/repos/example/consumer/pulls", status: status, body: `{}`},
				{method: http.MethodGet, path: "/repos/example/consumer/pulls", query: query, body: `[` + pullJSON(7, "open", "automation/sample", shaF, "stg", shaA, nil) + `]`},
			}
			controller, transport := newTestController(t, steps, true)

			pull, err := controller.CreateFeaturePullRequest(context.Background(), PublishedFeature{
				Base: Snapshot{Branch: "stg", SHA: shaA}, Branch: "automation/sample", HeadSHA: shaF,
			}, PullRequestSpec{Title: "sample", Body: "ticket"})
			if err != nil {
				t.Fatalf("CreateFeaturePullRequest() error = %v", err)
			}
			if pull.Number != 7 || pull.HTMLURL != "https://github.com/example/consumer/pull/7" {
				t.Fatalf("pull = %+v", pull)
			}
			transport.done()
		})
	}
}

func TestCreateFeaturePullRequestRejectsInexactReconciliation(t *testing.T) {
	query := "base=stg&head=example%3Aautomation%2Fsample&per_page=100&state=open"
	inexact := strings.Replace(pullJSON(7, "open", "automation/sample", shaF, "stg", shaA, nil), `"title":"sample"`, `"title":"other"`, 1)
	steps := []requestStep{
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/stg", body: refJSON("stg", shaA)},
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/automation/sample", body: refJSON("automation/sample", shaF)},
		{method: http.MethodPost, path: "/repos/example/consumer/pulls", status: http.StatusUnprocessableEntity, body: `{}`},
		{method: http.MethodGet, path: "/repos/example/consumer/pulls", query: query, body: `[` + inexact + `]`},
	}
	controller, transport := newTestController(t, steps, true)

	_, err := controller.CreateFeaturePullRequest(context.Background(), PublishedFeature{
		Base: Snapshot{Branch: "stg", SHA: shaA}, Branch: "automation/sample", HeadSHA: shaF,
	}, PullRequestSpec{Title: "sample", Body: "ticket"})
	if err == nil || !isStatus(err, http.StatusUnprocessableEntity) {
		t.Fatalf("CreateFeaturePullRequest() error = %v", err)
	}
	transport.done()
}

func TestCreatePromotionPullRequestReconcilesAcceptedInvalidResponse(t *testing.T) {
	acceptanceHash := strings.Repeat("9", 64)
	proof := testPromotionProof(acceptanceHash)
	steps := promotionProofSteps()
	steps = append(steps,
		requestStep{method: http.MethodPost, path: "/repos/example/consumer/pulls", status: http.StatusCreated, body: `{}`},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/pulls", query: "base=prod&head=example%3Astg&per_page=100&state=open", status: http.StatusBadGateway, body: `{}`},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/pulls", query: "base=prod&head=example%3Astg&per_page=100&state=open", body: `[]`},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/pulls", query: "base=prod&head=example%3Astg&per_page=100&state=open", body: `[` + pullJSON(9, "open", "stg", shaD, "prod", sha2, nil) + `]`},
	)
	controller, transport := newTestController(t, steps, true)

	pull, err := controller.CreatePromotionPullRequest(context.Background(), proof, testStagingDigestPolicy(), PullRequestSpec{
		Title: "promote sample", Body: "verified staging evidence " + acceptanceHash,
	})
	if err != nil {
		t.Fatalf("CreatePromotionPullRequest() error = %v", err)
	}
	if pull.Number != 9 || pull.HeadSHA != shaD || pull.BaseSHA != sha2 {
		t.Fatalf("pull = %+v", pull)
	}
	transport.done()
}

func TestMergeReconcilesAmbiguousMutationWithExactMergedState(t *testing.T) {
	tests := []struct {
		name     string
		mutation requestStep
	}{
		{name: "transport failure after apply", mutation: requestStep{err: io.ErrUnexpectedEOF}},
		{name: "accepted invalid response after apply", mutation: requestStep{status: http.StatusOK, body: "{"}},
		{name: "accepted semantic mismatch after apply", mutation: requestStep{status: http.StatusOK, body: `{}`}},
		{name: "server failure after apply", mutation: requestStep{status: http.StatusBadGateway, body: `{}`}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pull := newFeaturePull()
			steps := exactMergePrefix(t, pull)
			test.mutation.method = http.MethodPut
			test.mutation.path = "/repos/example/consumer/pulls/7/merge"
			steps = append(steps, test.mutation,
				requestStep{method: http.MethodGet, path: "/repos/example/consumer/pulls/7", body: mergedPullJSON(pull, sha4)},
				requestStep{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + sha4, body: gitCommitJSON(sha4, sha3, shaA, shaF)},
				requestStep{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/stg", body: refJSON("stg", sha4)},
			)
			controller, transport := newTestController(t, steps, true)

			merged, err := controller.MergeFeaturePullRequest(context.Background(), pull,
				testGateEvidence(pull), MergeSpec{CommitTitle: "sample"},
				WaitOptions{PollInterval: time.Millisecond, Timeout: time.Second})
			if err != nil {
				t.Fatalf("MergeFeaturePullRequest() error = %v", err)
			}
			if merged.MergeSHA != sha4 || merged.TreeSHA != sha3 || merged.BaseSHA != shaA || merged.HeadSHA != shaF {
				t.Fatalf("merged = %+v", merged)
			}
			transport.done()
		})
	}
}

func TestMergeReconcilesAcceptedButIncorrectMergeSHA(t *testing.T) {
	pull := newFeaturePull()
	steps := exactMergePrefix(t, pull)
	steps = append(steps,
		requestStep{method: http.MethodPut, path: "/repos/example/consumer/pulls/7/merge", body: `{"sha":"` + sha5 + `","merged":true,"message":"merged"}`},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + sha5, body: gitCommitJSON(sha5, shaE, shaA, shaF)},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/pulls/7", body: mergedPullJSON(pull, sha4)},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + sha4, body: gitCommitJSON(sha4, sha3, shaA, shaF)},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/stg", body: refJSON("stg", sha4)},
	)
	controller, transport := newTestController(t, steps, true)

	merged, err := controller.MergeFeaturePullRequest(context.Background(), pull,
		testGateEvidence(pull), MergeSpec{CommitTitle: "sample"},
		WaitOptions{PollInterval: time.Millisecond, Timeout: time.Second})
	if err != nil || merged.MergeSHA != sha4 {
		t.Fatalf("MergeFeaturePullRequest() = %+v, %v", merged, err)
	}
	transport.done()
}

func TestMergeRejectsAmbiguousMutationWithoutExactCommitProof(t *testing.T) {
	pull := newFeaturePull()
	steps := exactMergePrefix(t, pull)
	steps = append(steps,
		requestStep{method: http.MethodPut, path: "/repos/example/consumer/pulls/7/merge", err: io.ErrUnexpectedEOF},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/pulls/7", body: mergedPullJSON(pull, sha4)},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + sha4, body: gitCommitJSON(sha4, shaE, shaA, shaF)},
	)
	controller, transport := newTestController(t, steps, true)

	_, err := controller.MergeFeaturePullRequest(context.Background(), pull,
		testGateEvidence(pull), MergeSpec{CommitTitle: "sample"},
		WaitOptions{PollInterval: time.Millisecond, Timeout: time.Second})
	if err == nil || !isAmbiguousMutationError(err) {
		t.Fatalf("MergeFeaturePullRequest() error = %v", err)
	}
	transport.done()
}

func TestMergeDoesNotReinterpretHTTPFailure(t *testing.T) {
	pull := newFeaturePull()
	steps := exactMergePrefix(t, pull)
	steps = append(steps, requestStep{
		method: http.MethodPut, path: "/repos/example/consumer/pulls/7/merge",
		status: http.StatusUnprocessableEntity, body: `{}`,
	})
	controller, transport := newTestController(t, steps, true)

	_, err := controller.MergeFeaturePullRequest(context.Background(), pull,
		testGateEvidence(pull), MergeSpec{CommitTitle: "sample"},
		WaitOptions{PollInterval: time.Millisecond, Timeout: time.Second})
	if err == nil || isAmbiguousMutationError(err) || !isStatus(err, http.StatusUnprocessableEntity) {
		t.Fatalf("MergeFeaturePullRequest() error = %v", err)
	}
	transport.done()
}

func TestMergeRetriesTransientReconciliationRead(t *testing.T) {
	pull := newFeaturePull()
	steps := exactMergePrefix(t, pull)
	steps = append(steps,
		requestStep{method: http.MethodPut, path: "/repos/example/consumer/pulls/7/merge", err: io.ErrUnexpectedEOF},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/pulls/7", status: http.StatusBadGateway, body: `{}`},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/pulls/7", body: mergedPullJSON(pull, sha4)},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + sha4, body: gitCommitJSON(sha4, sha3, shaA, shaF)},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/stg", body: refJSON("stg", sha4)},
	)
	controller, transport := newTestController(t, steps, true)

	merged, err := controller.MergeFeaturePullRequest(context.Background(), pull,
		testGateEvidence(pull), MergeSpec{CommitTitle: "sample"},
		WaitOptions{PollInterval: time.Millisecond, Timeout: time.Second})
	if err != nil || merged.MergeSHA != sha4 {
		t.Fatalf("MergeFeaturePullRequest() = %+v, %v", merged, err)
	}
	transport.done()
}

func TestMergeRetriesTransientPostSuccessProofReads(t *testing.T) {
	pull := newFeaturePull()
	steps := exactMergePrefix(t, pull)
	steps = append(steps,
		requestStep{method: http.MethodPut, path: "/repos/example/consumer/pulls/7/merge", body: `{"sha":"` + sha4 + `","merged":true,"message":"merged"}`},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + sha4, status: http.StatusBadGateway, body: `{}`},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + sha4, body: gitCommitJSON(sha4, sha3, shaA, shaF)},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/stg", err: io.ErrUnexpectedEOF},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/stg", body: refJSON("stg", sha4)},
	)
	controller, transport := newTestController(t, steps, true)

	merged, err := controller.MergeFeaturePullRequest(context.Background(), pull,
		testGateEvidence(pull), MergeSpec{CommitTitle: "sample"},
		WaitOptions{PollInterval: time.Millisecond, Timeout: time.Second})
	if err != nil || merged.MergeSHA != sha4 {
		t.Fatalf("MergeFeaturePullRequest() = %+v, %v", merged, err)
	}
	transport.done()
}

func TestMergeWaitsWhileBaseRefIsStillAtExactPreMergeSHA(t *testing.T) {
	pull := newFeaturePull()
	steps := exactMergePrefix(t, pull)
	steps = append(steps,
		requestStep{method: http.MethodPut, path: "/repos/example/consumer/pulls/7/merge", body: `{"sha":"` + sha4 + `","merged":true,"message":"merged"}`},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + sha4, body: gitCommitJSON(sha4, sha3, shaA, shaF)},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/stg", body: refJSON("stg", shaA)},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/stg", body: refJSON("stg", sha4)},
	)
	controller, transport := newTestController(t, steps, true)

	merged, err := controller.MergeFeaturePullRequest(context.Background(), pull,
		testGateEvidence(pull), MergeSpec{CommitTitle: "sample"},
		WaitOptions{PollInterval: time.Millisecond, Timeout: time.Second})
	if err != nil || merged.MergeSHA != sha4 || merged.TreeSHA != sha3 {
		t.Fatalf("MergeFeaturePullRequest() = %+v, %v", merged, err)
	}
	transport.done()
}

func TestMergeRecordsReflectionBeforePostSuccessVerification(t *testing.T) {
	pull := newFeaturePull()
	steps := exactMergePrefix(t, pull)
	steps = append(steps,
		requestStep{method: http.MethodPut, path: "/repos/example/consumer/pulls/7/merge", body: `{"sha":"` + sha4 + `","merged":true,"message":"merged"}`},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + sha4, status: http.StatusForbidden, body: `{}`},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/pulls/7", status: http.StatusForbidden, body: `{}`},
	)
	controller, transport := newTestController(t, steps, true)
	var recorded []MergeReflection
	var requestsAtRecord int
	recorder := func(reflection MergeReflection) error {
		recorded = append(recorded, reflection)
		requestsAtRecord = transport.index
		return nil
	}

	merged, err := controller.mergePullRequest(context.Background(), pull,
		testGateEvidence(pull), MergeSpec{CommitTitle: "sample"},
		WaitOptions{PollInterval: time.Millisecond, Timeout: time.Second}, true, recorder)
	if err == nil || merged != (MergeResult{}) || len(recorded) != 1 || recorded[0] != reflectedMerge(pull, sha4) {
		t.Fatalf("mergePullRequest() = %+v, %v; recorded = %+v", merged, err, recorded)
	}
	if requestsAtRecord == 0 || steps[requestsAtRecord-1].method != http.MethodPut {
		t.Fatalf("reflection was recorded after request %d, not immediately after the merge response", requestsAtRecord)
	}
	transport.done()
}

func TestMergeRejectsThirdBaseRefAfterRecordingReflection(t *testing.T) {
	pull := newFeaturePull()
	steps := exactMergePrefix(t, pull)
	steps = append(steps,
		requestStep{method: http.MethodPut, path: "/repos/example/consumer/pulls/7/merge", body: `{"sha":"` + sha4 + `","merged":true,"message":"merged"}`},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + sha4, body: gitCommitJSON(sha4, sha3, shaA, shaF)},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/stg", body: refJSON("stg", shaE)},
	)
	controller, transport := newTestController(t, steps, true)
	var recorded []MergeReflection

	merged, err := controller.mergePullRequest(context.Background(), pull,
		testGateEvidence(pull), MergeSpec{CommitTitle: "sample"},
		WaitOptions{PollInterval: time.Millisecond, Timeout: time.Second}, true,
		func(reflection MergeReflection) error {
			recorded = append(recorded, reflection)
			return nil
		})
	if !IsInvariant(err, "merge_base_advanced") || merged != (MergeResult{}) ||
		len(recorded) != 1 || recorded[0] != reflectedMerge(pull, sha4) {
		t.Fatalf("mergePullRequest() = %+v, %v; recorded = %+v", merged, err, recorded)
	}
	transport.done()
}

func TestMergeReconciliationRecordsExactReflectionBeforeFullVerification(t *testing.T) {
	pull := newFeaturePull()
	steps := exactMergePrefix(t, pull)
	steps = append(steps,
		requestStep{method: http.MethodPut, path: "/repos/example/consumer/pulls/7/merge", err: io.ErrUnexpectedEOF},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/pulls/7", body: mergedPullJSON(pull, sha4)},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + sha4, body: gitCommitJSON(sha4, sha3, shaA, shaF)},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/stg", body: refJSON("stg", shaE)},
	)
	controller, transport := newTestController(t, steps, true)
	var recorded []MergeReflection

	merged, err := controller.mergePullRequest(context.Background(), pull,
		testGateEvidence(pull), MergeSpec{CommitTitle: "sample"},
		WaitOptions{PollInterval: time.Millisecond, Timeout: time.Second}, true,
		func(reflection MergeReflection) error {
			recorded = append(recorded, reflection)
			return nil
		})
	if err == nil || !isAmbiguousMutationError(err) || merged != (MergeResult{}) ||
		len(recorded) != 1 || recorded[0] != reflectedMerge(pull, sha4) {
		t.Fatalf("mergePullRequest() = %+v, %v; recorded = %+v", merged, err, recorded)
	}
	transport.done()
}

func TestMergeStopsBeforePostVerificationWhenReflectionRecorderFails(t *testing.T) {
	pull := newFeaturePull()
	steps := exactMergePrefix(t, pull)
	steps = append(steps, requestStep{
		method: http.MethodPut, path: "/repos/example/consumer/pulls/7/merge",
		body: `{"sha":"` + sha4 + `","merged":true,"message":"merged"}`,
	})
	controller, transport := newTestController(t, steps, true)
	recordErr := invariant("reflection_write_failed")

	merged, err := controller.mergePullRequest(context.Background(), pull,
		testGateEvidence(pull), MergeSpec{CommitTitle: "sample"},
		WaitOptions{PollInterval: time.Millisecond, Timeout: time.Second}, true,
		func(MergeReflection) error { return recordErr })
	if err != recordErr || merged != (MergeResult{}) {
		t.Fatalf("mergePullRequest() = %+v, %v", merged, err)
	}
	transport.done()
}

func TestWaitMergeBaseRefTimesOutWhileStillAtPreMergeSHA(t *testing.T) {
	controller, transport := newTestController(t, []requestStep{{
		method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/stg", body: refJSON("stg", shaA),
	}}, true)
	controller.client.sleep = func(context.Context, time.Duration) error { return context.DeadlineExceeded }

	_, err := controller.waitMergeBaseRef(context.Background(), "stg", shaA, sha4, time.Millisecond)
	if !IsInvariant(err, "merge_base_ref_timeout") {
		t.Fatalf("waitMergeBaseRef() error = %v", err)
	}
	transport.done()
}

func TestMatchMergedPullResponseRequiresExactPullContract(t *testing.T) {
	pull := newFeaturePull()
	exact := exactMergedPullResponse(pull, sha4)
	tests := []struct {
		name   string
		mutate func(*pullResponse)
	}{
		{name: "title", mutate: func(response *pullResponse) { response.Title = "other" }},
		{name: "body", mutate: func(response *pullResponse) { response.Body = "other" }},
		{name: "head ref", mutate: func(response *pullResponse) { response.Head.Ref = "other" }},
		{name: "head sha", mutate: func(response *pullResponse) { response.Head.SHA = shaE }},
		{name: "base ref", mutate: func(response *pullResponse) { response.Base.Ref = "other" }},
		{name: "base sha", mutate: func(response *pullResponse) { response.Base.SHA = shaE }},
		{name: "repository", mutate: func(response *pullResponse) { response.Head.Repo.FullName = "other/repo" }},
		{name: "html url", mutate: func(response *pullResponse) { response.HTMLURL = "https://example.invalid/pr/7" }},
		{name: "state", mutate: func(response *pullResponse) { response.State = "open" }},
		{name: "merged flag", mutate: func(response *pullResponse) { response.Merged = false }},
		{name: "merge sha", mutate: func(response *pullResponse) { response.MergeCommitSHA = "" }},
	}
	controller, transport := newTestController(t, nil, true)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := exact
			test.mutate(&response)
			if _, err := controller.matchMergedPullResponse(response, pull); err == nil {
				t.Fatal("matchMergedPullResponse() accepted inexact response")
			}
		})
	}
	transport.done()
}

func exactMergePrefix(t *testing.T, pull PullRequest) []requestStep {
	t.Helper()
	steps := mergeRequestSteps(t, pull, []string{pull.BaseSHA, pull.HeadSHA})
	return steps[:len(steps)-3]
}

func mergedPullJSON(pull PullRequest, mergeSHA string) string {
	encoded := pullJSON(pull.Number, "closed", pull.HeadRef, pull.HeadSHA, pull.BaseRef, pull.BaseSHA, nil)
	return strings.TrimSuffix(encoded, "}") + `,"merged":true,"merge_commit_sha":"` + mergeSHA + `"}`
}

func exactMergedPullResponse(pull PullRequest, mergeSHA string) pullResponse {
	response := pullResponse{
		Number: pull.Number, HTMLURL: pull.HTMLURL, Title: pull.Title, Body: pull.Body,
		CreatedAt: pull.CreatedAt.Format(time.RFC3339), State: "closed", Merged: true, MergeCommitSHA: mergeSHA,
	}
	response.Head.Ref = pull.HeadRef
	response.Head.SHA = pull.HeadSHA
	response.Head.Repo.FullName = "example/consumer"
	response.Base.Ref = pull.BaseRef
	response.Base.SHA = pull.BaseSHA
	return response
}
