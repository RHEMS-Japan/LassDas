package hook

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func terminalTestConfig() ReportRouteConfig {
	pull := functionURLTestPullConfig()
	return ReportRouteConfig{
		HMACKey:           append([]byte(nil), pull.HMACKey...),
		RepositoryID:      pull.RepositoryID,
		RepositorySHA256:  pull.RepositorySHA256,
		WorkflowRefSHA256: pull.WorkflowRefSHA256,
		ExpectedRunID:     "run_20260802_alpha",
		Destinations: []ReportDestination{{
			Repository: "example/target", Delivery: DeliverProduction,
			StagingOrigin: "https://staging.example.com", ProductionOrigin: "https://www.example.com",
		}},
		ClockSkew:           5 * time.Minute,
		LeaseDuration:       2 * time.Minute,
		SpaceKey:            pull.SpaceKey,
		ProjectID:           pull.ProjectID,
		ProjectKey:          pull.ProjectKey,
		AllowedCreatorID:    pull.AllowedCreatorID,
		AllowedActivityType: pull.AllowedActivityType,
		Target:              pull.Target,
	}
}

func terminalTestRequest(code TerminalCode) TerminalReportRequest {
	config := terminalTestConfig()
	request := TerminalReportRequest{
		Protocol:          TerminalReportProtocolVersion,
		DeliveryID:        "delivery_0123456789abcdef0123456789abcdef",
		InputSHA256:       strings.Repeat("1", 64),
		RepositoryID:      config.RepositoryID,
		RepositorySHA256:  config.RepositorySHA256,
		WorkflowRefSHA256: config.WorkflowRefSHA256,
		WorkflowSHA:       strings.Repeat("2", 40),
		WorkflowRunID:     123456789,
		RunAttempt:        1,
		AutomationRunID:   config.ExpectedRunID,
		Code:              code,
		Repository:        "example/target",
		RunURL:            "https://github.com/example/automation-receiver/actions/runs/123456789/attempts/1",
		IssuedAt:          functionURLTestNow,
	}
	if code == TerminalSuccess || code == TerminalProductionDeploymentUnverified || code == TerminalProductionVerificationFailed {
		request.PullRequestURL = "https://github.com/example/target/pull/42"
		request.CommitSHA = strings.Repeat("3", 40)
		request.CommitURL = "https://github.com/example/target/commit/" + request.CommitSHA
		request.StagingEvidenceURL = "https://staging.example.com/health/ready"
	}
	if code == TerminalSuccess {
		request.ProductionEvidenceURL = "https://www.example.com/health/ready"
	}
	return request
}

func TestTerminalReportRoundTripAndSignatures(t *testing.T) {
	config := terminalTestConfig()
	request := terminalTestRequest(TerminalSuccess)
	body, err := MarshalTerminalReportRequest(request)
	if err != nil {
		t.Fatalf("MarshalTerminalReportRequest() error = %v", err)
	}
	decoded, err := DecodeTerminalReportRequest(body)
	if err != nil {
		t.Fatalf("DecodeTerminalReportRequest() error = %v", err)
	}
	if decoded != request || decoded.ValidateRoute(config) != nil {
		t.Fatalf("decoded report is not route-valid: %+v", decoded)
	}
	signature := SignTerminalReportRequest(config.HMACKey, body)
	if !VerifyTerminalReportRequestSignature(config.HMACKey, body, signature) ||
		VerifyTerminalReportRequestSignature(config.HMACKey, append(body, ' '), signature) {
		t.Fatal("terminal request signature binding failed")
	}
	response := []byte(`{"decision":"accepted","code":"terminal_report_recorded"}`)
	responseSignature := SignTerminalReportResponse(config.HMACKey, 200, body, response)
	if !VerifyTerminalReportResponseSignature(config.HMACKey, 200, body, response, responseSignature) ||
		VerifyTerminalReportResponseSignature(config.HMACKey, 500, body, response, responseSignature) {
		t.Fatal("terminal response signature binding failed")
	}
}

func TestTerminalReportRecordIsStableAcrossAuthenticatedRetries(t *testing.T) {
	first := terminalTestRequest(TerminalReleaseFailed)
	second := first
	second.IssuedAt = first.IssuedAt.Add(time.Minute)
	firstRecord, err := MarshalTerminalReportRecord(first)
	if err != nil {
		t.Fatal(err)
	}
	secondRecord, err := MarshalTerminalReportRecord(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstRecord) != string(secondRecord) || TerminalReportDigest(firstRecord) != TerminalReportDigest(secondRecord) {
		t.Fatal("retry timestamp changed the immutable terminal record")
	}
	firstBody, _ := MarshalTerminalReportRequest(first)
	secondBody, _ := MarshalTerminalReportRequest(second)
	if string(firstBody) == string(secondBody) {
		t.Fatal("authenticated HTTP attempts unexpectedly had identical timestamped bodies")
	}
}

func TestTerminalReportAllowsOnlyFiniteCodes(t *testing.T) {
	for _, code := range []TerminalCode{
		TerminalSuccess, TerminalInputRejected, TerminalReadinessRejected, TerminalClarificationRequired,
		TerminalReadinessUnresolved, TerminalClarificationExpired, TerminalCancelled,
		TerminalModelFailed, TerminalNonconverged,
		TerminalValidationFailed, TerminalReleaseFailed, TerminalProductionDeploymentUnverified,
		TerminalProductionVerificationFailed, TerminalInternalFailed,
	} {
		request := terminalTestRequest(code)
		if err := request.ValidateRoute(terminalTestConfig()); err != nil {
			t.Fatalf("code %q rejected: %v", code, err)
		}
	}
	request := terminalTestRequest(TerminalCode("ticket_text_controls_comment"))
	if err := request.ValidateRoute(terminalTestConfig()); err == nil {
		t.Fatal("unknown terminal code was accepted")
	}
}

func TestProductionVerificationFailureRequiresDeployedEvidenceWithoutProductionClaim(t *testing.T) {
	request := terminalTestRequest(TerminalProductionVerificationFailed)
	if err := request.ValidateRoute(terminalTestConfig()); err != nil {
		t.Fatalf("valid production verification failure rejected: %v", err)
	}
	missing := request
	missing.CommitSHA = ""
	missing.CommitURL = ""
	if err := missing.ValidateRoute(terminalTestConfig()); err == nil {
		t.Fatal("production verification failure accepted without deployed commit")
	}
	claimed := request
	claimed.ProductionEvidenceURL = "https://www.example.com/health/ready"
	if err := claimed.ValidateRoute(terminalTestConfig()); err == nil {
		t.Fatal("production verification failure claimed successful production evidence")
	}
}

func TestProductionDeploymentUnverifiedRequiresPromotionEvidenceWithoutProductionClaim(t *testing.T) {
	request := terminalTestRequest(TerminalProductionDeploymentUnverified)
	if err := request.ValidateRoute(terminalTestConfig()); err != nil {
		t.Fatalf("valid production deployment uncertainty rejected: %v", err)
	}
	missing := request
	missing.CommitSHA = ""
	missing.CommitURL = ""
	if err := missing.ValidateRoute(terminalTestConfig()); err == nil {
		t.Fatal("production deployment uncertainty accepted without promotion commit")
	}
	claimed := request
	claimed.ProductionEvidenceURL = "https://www.example.com/health/ready"
	if err := claimed.ValidateRoute(terminalTestConfig()); err == nil {
		t.Fatal("production deployment uncertainty claimed successful production evidence")
	}
}

func TestTerminalReportRejectsURLAndIdentitySubstitution(t *testing.T) {
	tests := map[string]func(*TerminalReportRequest){
		"source repository": func(r *TerminalReportRequest) {
			r.RunURL = "https://github.com/attacker/repo/actions/runs/123456789/attempts/1"
		},
		"run id": func(r *TerminalReportRequest) {
			r.RunURL = "https://github.com/example/automation-receiver/actions/runs/9/attempts/1"
		},
		"pull host":       func(r *TerminalReportRequest) { r.PullRequestURL = "https://attacker.invalid/example/target/pull/42" },
		"pull repository": func(r *TerminalReportRequest) { r.PullRequestURL = "https://github.com/attacker/target/pull/42" },
		"commit mismatch": func(r *TerminalReportRequest) {
			r.CommitURL = "https://github.com/example/target/commit/" + strings.Repeat("4", 40)
		},
		"staging host":  func(r *TerminalReportRequest) { r.StagingEvidenceURL = "https://attacker.invalid/health/ready" },
		"staging query": func(r *TerminalReportRequest) { r.StagingEvidenceURL += "?secret=value" },
		"staging traversal": func(r *TerminalReportRequest) {
			r.StagingEvidenceURL = "https://staging.example.com/health/../admin"
		},
		"staging encoded path": func(r *TerminalReportRequest) {
			r.StagingEvidenceURL = "https://staging.example.com/%68ealth/ready"
		},
		"production host": func(r *TerminalReportRequest) { r.ProductionEvidenceURL = "https://staging.example.com/health/ready" },
		"repository id":   func(r *TerminalReportRequest) { r.RepositoryID++ },
		"workflow digest": func(r *TerminalReportRequest) { r.WorkflowRefSHA256 = strings.Repeat("5", 64) },
		"run binding":     func(r *TerminalReportRequest) { r.AutomationRunID = "run_20260802_other_route" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			request := terminalTestRequest(TerminalSuccess)
			mutate(&request)
			if err := request.ValidateRoute(terminalTestConfig()); err == nil {
				t.Fatal("unsafe terminal report was accepted")
			}
		})
	}
}

func TestTerminalReportCanonicalJSONRejectsUnknownAndFormatting(t *testing.T) {
	request := terminalTestRequest(TerminalValidationFailed)
	body, err := MarshalTerminalReportRequest(request)
	if err != nil {
		t.Fatalf("MarshalTerminalReportRequest() error = %v", err)
	}
	for name, mutated := range map[string][]byte{
		"whitespace": append(append([]byte(nil), body...), '\n'),
		"unknown": func() []byte {
			var value map[string]any
			if err := json.Unmarshal(body, &value); err != nil {
				t.Fatal(err)
			}
			value["comment"] = "UNTRUSTED-TICKET-CONTENT"
			encoded, _ := json.Marshal(value)
			return encoded
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeTerminalReportRequest(mutated); err == nil {
				t.Fatal("non-canonical report was accepted")
			}
		})
	}
}

func TestSuccessfulTerminalReportRequiresAllEvidence(t *testing.T) {
	tests := map[string]func(*TerminalReportRequest){
		"pull request": func(r *TerminalReportRequest) { r.PullRequestURL = "" },
		"commit": func(r *TerminalReportRequest) {
			r.CommitSHA = ""
			r.CommitURL = ""
		},
		"staging":    func(r *TerminalReportRequest) { r.StagingEvidenceURL = "" },
		"production": func(r *TerminalReportRequest) { r.ProductionEvidenceURL = "" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			request := terminalTestRequest(TerminalSuccess)
			mutate(&request)
			if err := request.ValidateRoute(terminalTestConfig()); err == nil {
				t.Fatal("success without required evidence was accepted")
			}
		})
	}
}
