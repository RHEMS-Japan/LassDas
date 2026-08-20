package hook

import "testing"

func localSchemeConfig() ReportRouteConfig {
	config := terminalTestConfig()
	config.RunReferenceScheme = "local"
	return config
}

// The single-pod constitution reports its run reference in the local-run
// scheme; the sealing rules stay those of the GitHub form — the reference
// binds the same repository digest, run id and attempt, or it is refused.
func TestTerminalReportAcceptsLocalRunReference(t *testing.T) {
	request := terminalTestRequest(TerminalSuccess)
	request.RunURL = "local-run://example/automation-receiver/123456789/attempts/1"
	if err := request.ValidateRoute(localSchemeConfig()); err != nil {
		t.Fatalf("ValidateRoute() error = %v", err)
	}
}

func TestQuestionRecordAcceptsLocalRunReference(t *testing.T) {
	record := questionTestRecord()
	record.RunURL = "local-run://example/automation-receiver/123456789/attempts/1"
	if err := record.ValidateRoute(localSchemeConfig()); err != nil {
		t.Fatalf("ValidateRoute() error = %v", err)
	}
}

// Each deployment seals exactly one reference form: a workflow deployment
// must not seal an unclickable local URI, and a pod must not seal a
// fabricated workflow link pointing at a page that does not exist.
func TestRunReferenceSchemeIsDeploymentBound(t *testing.T) {
	local := "local-run://example/automation-receiver/123456789/attempts/1"
	github := "https://github.com/example/automation-receiver/actions/runs/123456789/attempts/1"

	request := terminalTestRequest(TerminalSuccess)
	request.RunURL = local
	if err := request.ValidateRoute(terminalTestConfig()); err == nil {
		t.Fatal("a workflow deployment sealed a local run reference")
	}
	request.RunURL = github
	if err := request.ValidateRoute(localSchemeConfig()); err == nil {
		t.Fatal("a pod deployment sealed a workflow run link")
	}
	record := questionTestRecord()
	record.RunURL = local
	if err := record.ValidateRoute(terminalTestConfig()); err == nil {
		t.Fatal("a workflow deployment sealed a local question reference")
	}
	record.RunURL = github
	if err := record.ValidateRoute(localSchemeConfig()); err == nil {
		t.Fatal("a pod deployment sealed a workflow question link")
	}
}

func TestLocalRunReferenceBindsAllThreeIdentities(t *testing.T) {
	tests := map[string]string{
		"other repository": "local-run://attacker/repo/123456789/attempts/1",
		"other run id":     "local-run://example/automation-receiver/9/attempts/1",
		"other attempt":    "local-run://example/automation-receiver/123456789/attempts/2",
		"trailing path":    "local-run://example/automation-receiver/123456789/attempts/1/extra",
		"scheme case":      "LOCAL-RUN://example/automation-receiver/123456789/attempts/1",
		"github mixture":   "local-run://example/automation-receiver/actions/runs/123456789/attempts/1",
	}
	for name, url := range tests {
		request := terminalTestRequest(TerminalSuccess)
		request.RunURL = url
		if err := request.ValidateRoute(localSchemeConfig()); err == nil {
			t.Fatalf("%s: a substituted local run reference was accepted", name)
		}
	}
}
