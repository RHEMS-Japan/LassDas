package hook

import (
	"strings"
	"testing"
)

// A success may say it stopped short of what its destination asked for, and
// then it is judged on where it stopped. It may never say it went further,
// and it may never carry evidence of a place it did not reach.
func TestASuccessMayStopShortButNeverClaimMore(t *testing.T) {
	config := terminalTestConfig()
	cases := map[string]struct {
		mutate  func(*TerminalReportRequest)
		wantErr bool
	}{
		"silent, as every report before the depth moved into the run": {
			func(r *TerminalReportRequest) {}, false,
		},
		"naming the depth it was configured for": {
			func(r *TerminalReportRequest) { r.ReachedDelivery = DeliverProduction }, false,
		},
		"stopping at staging, with staging's evidence": {
			func(r *TerminalReportRequest) {
				r.ReachedDelivery = DeliverIntegration
				r.ProductionEvidenceURL = ""
			}, false,
		},
		"stopping at the proposal, with the proposal's evidence": {
			func(r *TerminalReportRequest) {
				r.ReachedDelivery = DeliverPullRequest
				r.CommitSHA, r.CommitURL, r.StagingEvidenceURL, r.ProductionEvidenceURL = "", "", "", ""
				r.DeliveryShortfall = "この納品先は production まで届ける設定ですが、取り込み用の Pull Request の作成までで止めています。"
			}, false,
		},
		"stopping at staging but still claiming production": {
			func(r *TerminalReportRequest) { r.ReachedDelivery = DeliverIntegration }, true,
		},
		"stopping at the proposal but still claiming a deployment": {
			func(r *TerminalReportRequest) { r.ReachedDelivery = DeliverPullRequest }, true,
		},
		"naming a depth that is not one of the three": {
			func(r *TerminalReportRequest) { r.ReachedDelivery = "everywhere" }, true,
		},
		"a shortfall with no depth to explain": {
			func(r *TerminalReportRequest) { r.DeliveryShortfall = "足りません" }, true,
		},
		"a shortfall carrying a line break": {
			func(r *TerminalReportRequest) {
				r.ReachedDelivery = DeliverProduction
				r.DeliveryShortfall = "足り\nません"
			}, true,
		},
		"a shortfall longer than the report may carry": {
			func(r *TerminalReportRequest) {
				r.ReachedDelivery = DeliverProduction
				r.DeliveryShortfall = strings.Repeat("x", MaxDeliveryShortfallBytes+1)
			}, true,
		},
	}
	for name, c := range cases {
		request := terminalTestRequest(TerminalSuccess)
		c.mutate(&request)
		err := request.ValidateRoute(config)
		if c.wantErr && err == nil {
			t.Errorf("%s: accepted", name)
		}
		if !c.wantErr && err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}

	// A destination that only ever proposes: a report that says it reached
	// production is refused for saying so, before anything looks at what
	// evidence it brought. Nothing a run writes may widen the setting an
	// operator gave its destination.
	proposalOnly := terminalTestConfig()
	proposalOnly.Destinations[0].Delivery = DeliverPullRequest
	claiming := terminalTestRequest(TerminalSuccess)
	claiming.ReachedDelivery = DeliverProduction
	if err := claiming.ValidateRoute(proposalOnly); err == nil {
		t.Fatal("a proposal-only destination accepted a report claiming production")
	}
	honest := terminalTestRequest(TerminalSuccess)
	honest.ReachedDelivery = DeliverPullRequest
	honest.CommitSHA, honest.CommitURL = "", ""
	honest.StagingEvidenceURL, honest.ProductionEvidenceURL = "", ""
	if err := honest.ValidateRoute(proposalOnly); err != nil {
		t.Fatalf("a proposal-only destination refused its own depth: %v", err)
	}
}

// A depth is something only a success can name: every other ending either
// reached nowhere or reached somewhere it is already forbidden to claim.
func TestOnlyASuccessNamesADepth(t *testing.T) {
	request := terminalTestRequest(TerminalReleaseFailed)
	request.ReachedDelivery = DeliverIntegration
	if err := request.ValidateShape(); err == nil {
		t.Fatal("a failure named a delivery depth")
	}
}

// The sealed record a report is identified by does not carry the depth, so
// no report's digest moves because the depth moved into the run.
//
// That is what lets a report begun by an older engine be sent again by this
// one: a re-submission whose digest differed would be refused as a conflict
// on every tick, for ever, and the delivery would sit pending with nothing
// driving it. The evidence the depth decides — the commit, the two
// environment pages — is in the record already, and it is what tells a
// proposal apart from a production delivery.
func TestTheDepthDoesNotMoveAnyReportDigest(t *testing.T) {
	request := terminalTestRequest(TerminalSuccess)
	silent, err := MarshalTerminalReportRecord(request)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(silent), "reached_delivery") || strings.Contains(string(silent), "delivery_shortfall") {
		t.Fatalf("the sealed record carries the depth: %s", silent)
	}
	named := request
	named.ReachedDelivery = DeliverProduction
	named.DeliveryShortfall = "何も足りていません"
	namedRecord, err := MarshalTerminalReportRecord(named)
	if err != nil {
		t.Fatal(err)
	}
	if TerminalReportDigest(silent) != TerminalReportDigest(namedRecord) {
		t.Fatal("naming the depth changed the digest a pending report is re-sent under")
	}
	// The evidence still separates the depths, which is the binding that
	// matters: a proposal and a production delivery are different reports.
	proposal := request
	proposal.ReachedDelivery = DeliverPullRequest
	proposal.CommitSHA, proposal.CommitURL = "", ""
	proposal.StagingEvidenceURL, proposal.ProductionEvidenceURL = "", ""
	proposalRecord, err := MarshalTerminalReportRecord(proposal)
	if err != nil {
		t.Fatal(err)
	}
	if TerminalReportDigest(proposalRecord) == TerminalReportDigest(silent) {
		t.Fatal("a proposal and a production delivery seal the same record")
	}
}
