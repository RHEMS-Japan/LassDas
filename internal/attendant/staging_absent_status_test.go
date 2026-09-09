package attendant

import "testing"

// A merge that deployed nothing waits for an operator, exactly as the other
// staging outcomes an operator has to look at do. Left out of that set it
// would show as a plain failure and the delivery would end unattended.
func TestADeployThatNeverStartedWaitsForAnOperator(t *testing.T) {
	if !attentionVerdict("deploy_absent") {
		t.Fatal("deploy_absent does not wait for an operator")
	}
	var status RunStatus
	placeStagingOutcome(&status, "deploy_absent", "")
	if status.Step != "attention" || status.Stage != "staging" {
		t.Fatalf("step = %q stage = %q, want attention at staging", status.Step, status.Stage)
	}
	if status.StepTitle == "" || status.StepTitle == "ステージング反映で停止" {
		t.Fatalf("step title = %q: the generic failure title says the wrong thing", status.StepTitle)
	}
}

// A merge that deployed nothing must never open the promotion. The gate
// reads only the verdict, so widening it by one word would let a delivery
// nothing verified reach production — and the whole suite stayed green
// when that was tried (review of #134).
func TestADeployThatNeverStartedCannotOpenThePromotion(t *testing.T) {
	for _, verdict := range []string{"deploy_absent", "deploy_failed", "merge_unverified", "observe_blocked", "observe_failed", "checks_failed"} {
		if promotableStagingVerdict(verdict) {
			t.Errorf("verdict %q opens the promotion", verdict)
		}
	}
	if !promotableStagingVerdict("pass") {
		t.Error("a passing staging report no longer opens the promotion")
	}
}
