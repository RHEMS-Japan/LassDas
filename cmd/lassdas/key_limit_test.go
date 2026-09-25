package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/initwizard"
)

// keyAnswering stands in for the provider's key endpoint, answering with
// the limit the test wants and recording what was asked.
func keyAnswering(t *testing.T, body string) (*http.Client, *[]*http.Request) {
	t.Helper()
	var seen []*http.Request
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen = append(seen, r)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	return client, &seen
}

// storedProject writes a project the way `setup secrets` does, with one
// provider key saved.
func storedProject(t *testing.T, home, baseURL string, keys map[string]string) {
	t.Helper()
	dir, err := initwizard.ProjectDir(home, "sample")
	if err != nil {
		t.Fatal(err)
	}
	state, secrets, err := initwizard.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	state.Project, state.BaseURL = "sample", baseURL
	for name, value := range keys {
		secrets[name] = value
	}
	if err := initwizard.Save(dir, state, secrets); err != nil {
		t.Fatal(err)
	}
}

// The engine holds no budget: it goes on changing its approach until the
// request is done, and the only thing that stops a delivery that never
// will be is the provider refusing the key. A key with no limit removes
// that stop, so the check says so while the limit can still be set.
func TestSetupCheckWarnsAboutAKeyWithNoSpendingLimit(t *testing.T) {
	home := t.TempDir()
	storedProject(t, home, "https://openrouter.ai/api/v1", map[string]string{"LASSDAS_IMPLEMENTER_KEY": "sk-or-a-key-value"})
	client, seen := keyAnswering(t, `{"data":{"label":"a key","usage":3.5,"limit":null}}`)
	notice := keyLimitNotice(context.Background(), home, "sample", client)
	if !strings.Contains(notice, "利用上限が設定されていません") {
		t.Fatalf("no warning for a key without a limit: %q", notice)
	}
	if !strings.Contains(notice, "リセット") {
		t.Fatalf("the warning does not say what to set: %q", notice)
	}
	if strings.Contains(notice, "sk-or-a-key-value") {
		t.Fatalf("the key was printed: %q", notice)
	}
	if len(*seen) != 1 || !strings.HasSuffix((*seen)[0].URL.Path, "/key") {
		t.Fatalf("the provider was asked %d times at %v", len(*seen), *seen)
	}
	if (*seen)[0].Header.Get("Authorization") != "Bearer sk-or-a-key-value" {
		t.Fatalf("the key did not travel as the provider expects")
	}
}

// A key with a limit is the state this check exists to reach, so it says
// nothing at all.
func TestSetupCheckIsSilentAboutAKeyWithALimit(t *testing.T) {
	home := t.TempDir()
	storedProject(t, home, "https://openrouter.ai/api/v1", map[string]string{"LASSDAS_IMPLEMENTER_KEY": "sk-or-a-key-value"})
	client, _ := keyAnswering(t, `{"data":{"label":"a key","usage":3.5,"limit":50,"limit_remaining":46.5}}`)
	if notice := keyLimitNotice(context.Background(), home, "sample", client); notice != "" {
		t.Fatalf("a limited key was warned about: %q", notice)
	}
}

// One reading per distinct key, not per variable: a setup that shares one
// key across every role must not ask the provider seven times and say the
// same thing seven times.
func TestTheProviderIsAskedOncePerDistinctKey(t *testing.T) {
	home := t.TempDir()
	storedProject(t, home, "https://openrouter.ai/api/v1", map[string]string{
		"LASSDAS_IMPLEMENTER_KEY":   "one-shared-key",
		"LASSDAS_REVIEW_A_KEY":      "one-shared-key",
		"LASSDAS_REVIEW_B_KEY":      "one-shared-key",
		"LASSDAS_INTAKE_TARGET_KEY": "one-shared-key",
		// Not a provider key, and not billed by the gateway.
		"TARGET_GITHUB_TOKEN": "a-destination-token",
		"BACKLOG_API_KEY":     "a-tracker-key",
	})
	client, seen := keyAnswering(t, `{"data":{"usage":1,"limit":null}}`)
	notice := keyLimitNotice(context.Background(), home, "sample", client)
	if len(*seen) != 1 {
		t.Fatalf("the provider was asked %d times", len(*seen))
	}
	if !strings.Contains(notice, "(1 本)") {
		t.Fatalf("the warning counts the variables rather than the keys: %q", notice)
	}
}

// A provider that cannot be reached is said plainly. Reporting it as "no
// limit" would be a claim nothing here can support, and reporting nothing
// would hide that the check did not happen.
func TestAnUnreachableProviderIsNotReportedAsNoLimit(t *testing.T) {
	home := t.TempDir()
	storedProject(t, home, "https://openrouter.ai/api/v1", map[string]string{"LASSDAS_IMPLEMENTER_KEY": "sk-or-a-key-value"})
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 401, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
	})}
	notice := keyLimitNotice(context.Background(), home, "sample", client)
	if !strings.Contains(notice, "確認できませんでした") || strings.Contains(notice, "設定されていません") {
		t.Fatalf("an unreachable provider read as %q", notice)
	}
}

// A project with no key stored yet — the ordinary state the first time the
// check runs — has nothing to check, and no project named at all is the
// same. Neither invents a warning.
func TestNoStoredKeyIsNoWarning(t *testing.T) {
	home := t.TempDir()
	client, seen := keyAnswering(t, `{"data":{"usage":1,"limit":null}}`)
	if notice := keyLimitNotice(context.Background(), home, "", client); notice != "" {
		t.Fatalf("no project named: %q", notice)
	}
	storedProject(t, home, "https://openrouter.ai/api/v1", nil)
	if notice := keyLimitNotice(context.Background(), home, "sample", client); notice != "" {
		t.Fatalf("no key stored: %q", notice)
	}
	if len(*seen) != 0 {
		t.Fatalf("the provider was asked with no key to ask about")
	}
}

// The whole check prints the warning where an operator reads it, beside
// the other gaps, without ever asking for a key.
func TestSetupCheckPrintsTheWarningBesideTheOtherGaps(t *testing.T) {
	root := gitRepo(t)
	home := t.TempDir()
	storedProject(t, home, "https://openrouter.ai/api/v1", map[string]string{"LASSDAS_IMPLEMENTER_KEY": "sk-or-a-key-value"})
	answers, err := json.Marshal(map[string]any{"answers": map[string]any{"repository": "example/app"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".lassdas"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".lassdas", "setup.json"), answers, 0o644); err != nil {
		t.Fatal(err)
	}
	client, _ := keyAnswering(t, `{"data":{"usage":1,"limit":null}}`)
	var out bytes.Buffer
	_ = setupCheck(context.Background(), root, home, "sample", client, &out)
	if !strings.Contains(out.String(), "利用上限が設定されていません") {
		t.Fatalf("the warning is not printed: %q", out.String())
	}
	if strings.Contains(out.String(), "sk-or-a-key-value") {
		t.Fatalf("the key was printed: %q", out.String())
	}
}
