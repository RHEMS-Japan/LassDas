package attendant

import "testing"

// A change outside the declared deploy scope is done, not waiting: it must
// neither ask an operator to look nor open the promotion, on either phase.
func TestAChangeOutsideTheDeployScopeIsDoneNotWaiting(t *testing.T) {
	if attentionVerdict("deploy_not_applicable") {
		t.Fatal("deploy_not_applicable waits for an operator")
	}
	if promotableStagingVerdict("deploy_not_applicable") {
		t.Fatal("deploy_not_applicable opens the promotion")
	}
	var staging RunStatus
	placeStagingOutcome(&staging, "deploy_not_applicable", "")
	if staging.Step != "done" || staging.StepTitle == "" || staging.StepTitle == "ステージング反映で停止" {
		t.Fatalf("staging step = %q title = %q, want done with its own title", staging.Step, staging.StepTitle)
	}
	var release RunStatus
	placeReleaseOutcome(&release, "deploy_not_applicable")
	if release.Step != "done" || release.StepTitle == "" || release.StepTitle == "本番反映の工程で停止" {
		t.Fatalf("release step = %q title = %q, want done with its own title", release.Step, release.StepTitle)
	}
}
