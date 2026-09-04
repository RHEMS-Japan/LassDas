package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/visiblecheck"
)

func e2ePipeline(t *testing.T) *Pipeline {
	t.Helper()
	return &Pipeline{Workspace: t.TempDir(), Logger: baseAdvanceLogger{}}
}

// sealRounds fabricates n sealed candidate rounds; the observation chains to
// the LAST one's ticket, so each round gets both artifacts.
func sealRounds(t *testing.T, pipeline *Pipeline, n int) {
	t.Helper()
	for round := 1; round <= n; round++ {
		dir := pipeline.path(fmt.Sprintf("history/stage-%d", round))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"candidate.json", "ticket.json"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(`{}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestRunE2ECheckIsIdempotentOnceSealed(t *testing.T) {
	pipeline := e2ePipeline(t)
	if err := os.WriteFile(pipeline.path(E2EResultFile), []byte(`{"verdict":"pass"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A sealed result short-circuits before any binary is needed; a broken
	// controller path proves nothing ran.
	pipeline.Config.ControllerBin = "false"
	if err := pipeline.RunE2ECheck(context.Background()); err != nil {
		t.Fatalf("RunE2ECheck() error = %v", err)
	}
}

func TestRunE2ECheckNeedsTheDeliveredPullRequest(t *testing.T) {
	pipeline := e2ePipeline(t)
	if err := pipeline.RunE2ECheck(context.Background()); err == nil {
		t.Fatal("a run without a delivered pull request was accepted")
	}
}

// A delivered pull request always comes out of a sealed round; artifacts
// without one are corrupt plumbing, not a verdict.
func TestRunE2ECheckNeedsASealedRound(t *testing.T) {
	pipeline := e2ePipeline(t)
	if err := os.WriteFile(pipeline.path("feature-pr.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := pipeline.RunE2ECheck(context.Background())
	if err == nil || !strings.Contains(err.Error(), "candidate round") {
		t.Fatalf("error = %v", err)
	}
}

// The merge wait must chain to the SEALED round's ticket — the one whose
// target files were actually implemented — not the readiness derivation.
func TestRunE2ECheckChainsTheSealedRoundTicket(t *testing.T) {
	pipeline := e2ePipeline(t)
	sealRounds(t, pipeline, 2)
	if err := os.WriteFile(pipeline.path("feature-pr.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), "argv.txt")
	script := filepath.Join(t.TempDir(), "controller.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+record+"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	pipeline.Config.ControllerBin = script
	if err := pipeline.RunE2ECheck(context.Background()); err != nil {
		t.Fatalf("RunE2ECheck() error = %v", err)
	}
	argv, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	arguments := strings.Split(strings.TrimSpace(string(argv)), "\n")
	ticket := ""
	for index, argument := range arguments {
		if argument == "--ticket" && index+1 < len(arguments) {
			ticket = arguments[index+1]
		}
	}
	if ticket != pipeline.path("history/stage-2/ticket.json") {
		t.Fatalf("--ticket = %q, want the round-2 ticket", ticket)
	}
}

// A merge/staging wait that fails is a RESULT — sealed as unknown, exit
// zero — never a blocked card.
func TestRunE2ECheckSealsUnknownWhenTheWaitFails(t *testing.T) {
	pipeline := e2ePipeline(t)
	sealRounds(t, pipeline, 1)
	if err := os.WriteFile(pipeline.path("feature-pr.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pipeline.Config.ControllerBin = "false"
	if err := pipeline.RunE2ECheck(context.Background()); err != nil {
		t.Fatalf("RunE2ECheck() error = %v", err)
	}
	raw, err := os.ReadFile(pipeline.path(E2EResultFile))
	if err != nil {
		t.Fatal(err)
	}
	var result E2EResult
	if json.Unmarshal(raw, &result) != nil || result.Verdict != "unknown" {
		t.Fatalf("sealed result = %s", raw)
	}
}

func TestConsumerStagingOrigin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "consumer.json")
	content := `{"consumers":[
		{"repository":"example/one","staging_origin":"https://one.example.invalid"},
		{"repository":"example/two","staging_origin":""}
	]}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	origin, err := consumerStagingOrigin(path, "example/one")
	if err != nil || origin != "https://one.example.invalid" {
		t.Fatalf("origin = %q, %v", origin, err)
	}
	if _, err := consumerStagingOrigin(path, "example/two"); err == nil {
		t.Fatal("an empty staging origin was accepted")
	}
	if _, err := consumerStagingOrigin(path, "example/absent"); err == nil {
		t.Fatal("an unknown repository was accepted")
	}
}

func TestLoadE2ESessionCookies(t *testing.T) {
	if cookies, detail := loadE2ESessionCookies("", ""); cookies != nil || !strings.Contains(detail, "設定されていません") {
		t.Fatalf("unset path = %v, %q", cookies, detail)
	}
	if cookies, detail := loadE2ESessionCookies(filepath.Join(t.TempDir(), "absent.json"), ""); cookies != nil || !strings.Contains(detail, "読めません") {
		t.Fatalf("absent file = %v, %q", cookies, detail)
	}
	path := filepath.Join(t.TempDir(), "session.json")
	if err := os.WriteFile(path, []byte(`{"cookies":[{"name":"s","value":"v","domain":"example.invalid","path":"/","secure":true,"httpOnly":true}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cookies, detail := loadE2ESessionCookies(path, "")
	if len(cookies) != 1 || cookies[0].Name != "s" || !cookies[0].HTTPOnly || detail != "" {
		t.Fatalf("cookies = %+v, %q", cookies, detail)
	}
	// A renewed copy grown from the mounted seed is the jar in use; an
	// absent one, or one grown from another seed, leaves the seed in charge.
	renewed := filepath.Join(t.TempDir(), "renewed.json")
	if cookies, _ := loadE2ESessionCookies(path, renewed); len(cookies) != 1 || cookies[0].Name != "s" {
		t.Fatalf("absent renewal must fall back to the seed: %+v", cookies)
	}
	if err := os.WriteFile(renewed, []byte(`{"cookies":[{"name":"r","value":"v","domain":"example.invalid","path":"/"}],"seed_sha256":"`+visiblecheck.SeedDigest(path)+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if cookies, _ := loadE2ESessionCookies(path, renewed); len(cookies) != 1 || cookies[0].Name != "r" {
		t.Fatalf("a renewal of the mounted seed must win: %+v", cookies)
	}
	if err := os.WriteFile(renewed, []byte(`{"cookies":[{"name":"old","value":"v","domain":"example.invalid","path":"/"}],"seed_sha256":"0000"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if cookies, _ := loadE2ESessionCookies(path, renewed); len(cookies) != 1 || cookies[0].Name != "s" {
		t.Fatalf("a renewal of another seed must lose to the seed: %+v", cookies)
	}
}

func TestConsumerObservationCarriesTheLoginEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "consumer.json")
	raw := `{"consumers":[
		{"repository":"example/one","staging_origin":"https://one.example.invalid","production_origin":"https://one-prod.example.invalid","staging_login_url":"https://one-api.example.invalid/login?returnTo=/console","observation_language":"ja"},
		{"repository":"example/two","staging_origin":"https://two.example.invalid"}]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	origin, entry, err := consumerObservation(path, "example/one", "staging")
	if err != nil || origin != "https://one.example.invalid" || entry.LoginURL != "https://one-api.example.invalid/login?returnTo=/console" || entry.LandedPrefix != origin || entry.Language != "ja" {
		t.Fatalf("staging = %q %+v %v", origin, entry, err)
	}
	if _, entry, err := consumerObservation(path, "example/one", "production"); err != nil || entry.LoginURL != "" || entry.Language != "ja" {
		t.Fatalf("production without a login entry must sign in nowhere but keep the language: %+v %v", entry, err)
	}
	if _, entry, err := consumerObservation(path, "example/two", "staging"); err != nil || entry.LoginURL != "" {
		t.Fatalf("a consumer without a login entry must sign in nowhere: %+v %v", entry, err)
	}
	// A language the strict loader would refuse never reaches the browser.
	bad := `{"consumers":[{"repository":"example/three","staging_origin":"https://three.example.invalid","observation_language":"ja;q=0.9"}]}`
	if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, entry, err := consumerObservation(path, "example/three", "staging"); err != nil || entry.Language != "" {
		t.Fatalf("an invalid language must be dropped: %+v %v", entry, err)
	}
}

func TestLooksLikeLogin(t *testing.T) {
	if !looksLikeLogin("https://console.example.invalid/login?next=%2F") {
		t.Fatal("a login URL was not recognised")
	}
	if looksLikeLogin("https://console.example.invalid/console/model-policy") {
		t.Fatal("a feature URL was mistaken for a login page")
	}
}

func TestSealE2EResultStampsSchemaAndTime(t *testing.T) {
	pipeline := e2ePipeline(t)
	if err := pipeline.sealE2EResult(E2EResult{Verdict: "fail"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(pipeline.path(E2EResultFile))
	if err != nil {
		t.Fatal(err)
	}
	var result E2EResult
	if json.Unmarshal(raw, &result) != nil || result.SchemaVersion != 1 || result.ObservedAt.IsZero() {
		t.Fatalf("sealed result = %s", raw)
	}
}

// The stand-in controller writes the progress record; the run then fails on
// the missing readiness ticket — proving the wait step is skipped once its
// record exists (resume) and that missing verification inputs are plumbing
// errors, not verdicts.
func TestRunE2ECheckResumesPastACompletedWait(t *testing.T) {
	pipeline := e2ePipeline(t)
	sealRounds(t, pipeline, 1)
	for name, content := range map[string]string{
		"feature-pr.json":    `{}`,
		E2EMergedStagingFile: `{"merged_sha":"` + strings.Repeat("a", 40) + `"}`,
	} {
		if err := os.WriteFile(pipeline.path(name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pipeline.Config.ControllerBin = "false"
	err := pipeline.RunE2ECheck(context.Background())
	if err == nil || !strings.Contains(err.Error(), "readiness") {
		t.Fatalf("error = %v", err)
	}
}

func TestE2EVerificationComposesTheTarget(t *testing.T) {
	pipeline := e2ePipeline(t)
	consumerPath := filepath.Join(t.TempDir(), "consumer.json")
	if err := os.WriteFile(consumerPath, []byte(`{"consumers":[{"repository":"example/one","staging_origin":"https://one.example.invalid"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pipeline.Config.ConsumerConfigPath = consumerPath
	ticket := fmt.Sprintf(`{"repository":"example/one","verification_path":"/console/x","expected_text":"絞り込み","absent_text":""}`)
	if err := os.WriteFile(pipeline.path("readiness-ticket.json"), []byte(ticket), 0o600); err != nil {
		t.Fatal(err)
	}
	target, expected, absent, _, err := pipeline.e2eVerification()
	if err != nil || target != "https://one.example.invalid/console/x" || expected != "絞り込み" || absent != "" {
		t.Fatalf("verification = %q %q %q, %v", target, expected, absent, err)
	}
}
