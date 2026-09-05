package worker

import (
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/probe"
)

func TestDesignerSharesAtMostOneReviewerEndpoint(t *testing.T) {
	config := validTestConfig()
	if err := config.Validate(); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	designer := config.Models.Implementer
	designer.ID = "designer"
	config.Models.Designer = &designer
	shared := 0
	for _, reviewer := range config.Models.Reviewers {
		if strings.EqualFold(reviewer.BaseURL, designer.BaseURL) && strings.EqualFold(reviewer.Model, designer.Model) {
			shared++
		}
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("designer on the implementer's endpoint (%d reviewers share it): %v", shared, err)
	}
	// Point every reviewer at the designer's endpoint and model: no reviewer
	// is left that the designer's model family cannot influence.
	for index := range config.Models.Reviewers {
		config.Models.Reviewers[index].BaseURL = designer.BaseURL
		config.Models.Reviewers[index].Model = designer.Model
	}
	err := config.Validate()
	if err == nil || (!strings.Contains(err.Error(), "designer") && !strings.Contains(err.Error(), "implementer") && !strings.Contains(err.Error(), "duplicates")) {
		t.Errorf("two reviewers sharing the designer's endpoint accepted: %v", err)
	}
	config = validTestConfig()
	designer = config.Models.Implementer
	designer.ID = config.Models.Reviewers[0].ID
	config.Models.Designer = &designer
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "duplicates") {
		t.Errorf("designer reusing a reviewer id accepted: %v", err)
	}
	config = validTestConfig()
	config.Models.Designer = &ModelEndpoint{ID: "designer"}
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "designer") {
		t.Errorf("incomplete designer endpoint accepted: %v", err)
	}
}

func TestProbesKeepTheLoginHostOut(t *testing.T) {
	config := validTestConfig()
	config.Consumers[0].StagingLoginURL = "https://login-stg.example.invalid/auth/login?returnTo=/console"
	config.Probes = []probe.Spec{{ID: "http.timing", Kind: probe.KindHTTP, Hosts: []string{"login-stg.example.invalid"}, Methods: []string{"GET"}, Args: map[string]string{"path": `/[a-z/]{0,40}`}}}
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "forbidden host") {
		t.Errorf("probe addressing the login host accepted: %v", err)
	}
	config.Probes[0].Hosts = []string{"app-stg.example.invalid"}
	if err := config.Validate(); err != nil {
		t.Errorf("probe on another host refused: %v", err)
	}
	catalog, err := config.ProbeCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := catalog.Lookup("http.timing"); !ok {
		t.Error("catalogue lost the declared probe")
	}
	if _, ok := catalog.Lookup("repo.read"); !ok {
		t.Error("catalogue lacks the built-in repo probes")
	}
	config.Probes = []probe.Spec{{ID: "bad id", Kind: probe.KindExec, Argv: []string{"kubectl"}}}
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "probes") {
		t.Errorf("invalid catalogue accepted: %v", err)
	}
}
