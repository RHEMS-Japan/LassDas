package hook

import (
	"strings"
	"testing"
)

// TestADeadlineReportMayCarryProductionEvidenceWithTheDepthsBeneathIt holds
// the gate open for the one failed ending that can have reached production.
// Refused there, the report would never be kept and never post, and the run
// would stay open for good — the failure this gate exists to prevent, not
// to cause.
func TestADeadlineReportMayCarryProductionEvidenceWithTheDepthsBeneathIt(t *testing.T) {
	config := terminalTestConfig()
	full := terminalTestRequest(TerminalDeadlineReached)
	full.PullRequestURL = "https://github.com/example/target/pull/42"
	full.CommitSHA = strings.Repeat("3", 40)
	full.CommitURL = "https://github.com/example/target/commit/" + full.CommitSHA
	full.StagingEvidenceURL = "https://staging.example.com/health/ready"
	full.ProductionEvidenceURL = "https://www.example.com/health/ready"
	if err := full.ValidateRoute(config); err != nil {
		t.Fatalf("a deadline report that reached production was refused: %v", err)
	}

	// Production without the depths beneath it is a shape no delivery
	// produces, and the gate still says so.
	bare := terminalTestRequest(TerminalDeadlineReached)
	bare.ProductionEvidenceURL = "https://www.example.com/health/ready"
	if err := bare.ValidateRoute(config); err == nil {
		t.Fatal("a deadline report claimed production with no pull request, commit or staging beneath it")
	}

	// The endings that stop short of production keep refusing it.
	other := terminalTestRequest(TerminalImplementationReturned)
	other.ProductionEvidenceURL = "https://www.example.com/health/ready"
	if err := other.ValidateRoute(config); err == nil {
		t.Fatal("a failed ending other than the deadline was allowed to claim production")
	}

	// A deadline report that stopped at staging, or at the pull request,
	// passes exactly as it did before the deadline could name production.
	staging := terminalTestRequest(TerminalDeadlineReached)
	staging.PullRequestURL = full.PullRequestURL
	staging.CommitSHA = full.CommitSHA
	staging.CommitURL = full.CommitURL
	staging.StagingEvidenceURL = full.StagingEvidenceURL
	if err := staging.ValidateRoute(config); err != nil {
		t.Fatalf("a deadline report that reached staging was refused: %v", err)
	}
	proposal := terminalTestRequest(TerminalDeadlineReached)
	proposal.PullRequestURL = full.PullRequestURL
	if err := proposal.ValidateRoute(config); err != nil {
		t.Fatalf("a deadline report that reached the pull request was refused: %v", err)
	}
}
