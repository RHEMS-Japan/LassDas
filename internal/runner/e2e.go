package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/visiblecheck"
)

// The debug role's observation card: wait for the HUMAN merge and the
// staging deployment, then look at the deployed page the readiness gate
// named and record what was actually visible. The card writes artifacts
// only — the attendant, sole owner of the ledger and of every requester
// comment, picks the result up on its own tick and posts it.

const (
	// E2EMergedStagingFile is the controller's progress record: the merge
	// commit and the staging run that deployed it.
	E2EMergedStagingFile = "e2e-merged-staging.json"
	// E2EResultFile is the sealed observation verdict the attendant reports.
	E2EResultFile = "e2e-result.json"
	// E2EScreenshotFile is the full-page capture taken with the verdict.
	E2EScreenshotFile = "e2e-screenshot.png"
	// E2ESessionFileEnvironment names the optional session-cookie file
	// (Playwright storageState JSON) the staging console needs; without it
	// a login page turns every observation into an honest "unknown".
	E2ESessionFileEnvironment = "LASSDAS_E2E_SESSION_FILE"
)

// E2EResult is the observation verdict, sealed in the run directory.
type E2EResult struct {
	SchemaVersion int       `json:"schema_version"`
	Verdict       string    `json:"verdict"` // pass | fail | unknown
	Detail        string    `json:"detail,omitempty"`
	TargetURL     string    `json:"target_url,omitempty"`
	ExpectedText  string    `json:"expected_text,omitempty"`
	ExpectedSeen  bool      `json:"expected_seen"`
	AbsentText    string    `json:"absent_text,omitempty"`
	AbsentGone    bool      `json:"absent_gone"`
	FinalURL      string    `json:"final_url,omitempty"`
	StatusCode    int       `json:"status_code,omitempty"`
	Screenshot    bool      `json:"screenshot"`
	ObservedAt    time.Time `json:"observed_at"`
}

// RunE2ECheck drives the whole observation. Verdicts — including "the pull
// request was closed unmerged" and "the session is not configured" — are
// results, sealed and exited zero; only broken plumbing (unreadable
// artifacts, unwritable results) exits non-zero and blocks the card.
func (p *Pipeline) RunE2ECheck(ctx context.Context) error {
	if p.exists(E2EResultFile) {
		return nil
	}
	if !p.exists("feature-pr.json") {
		return errors.New("e2e-check needs the delivered pull request artifact")
	}
	// The feature-PR artifact chains to the SEALED round's ticket — whose
	// target files are what actually changed — not to the pre-implementation
	// readiness derivation, which the implementer is not bound to.
	round := p.latestCandidateRound()
	if round < 1 {
		return errors.New("e2e-check needs a sealed candidate round")
	}
	roundTicket := p.path(fmt.Sprintf("history/stage-%d/ticket.json", round))
	if !p.exists(E2EMergedStagingFile) {
		code, err := p.controller(ctx, "await-merged-staging", []string{
			"await-merged-staging", "--config", p.Config.ConsumerConfigPath,
			"--ticket", roundTicket,
			"--feature-pr", p.path("feature-pr.json"),
			"--out", p.path(E2EMergedStagingFile),
		})
		if err != nil || code != 0 {
			return p.sealE2EResult(E2EResult{
				Verdict: "unknown",
				Detail:  "マージまたはステージング反映の完了を確認できませんでした（マージされずクローズされた場合・期限内にマージされなかった場合を含みます）。",
			})
		}
	}
	target, expected, absent, err := p.e2eVerification()
	if err != nil {
		return err
	}
	cookies, cookieDetail := loadE2ESessionCookies(os.Getenv(E2ESessionFileEnvironment))
	observation, err := visiblecheck.ObserveForE2E(ctx, target, expected, absent, cookies)
	if err != nil {
		return p.sealE2EResult(E2EResult{
			Verdict: "unknown", TargetURL: target, ExpectedText: expected, AbsentText: absent,
			Detail: strings.TrimSpace("画面の自動確認を実行できませんでした。 " + cookieDetail),
		})
	}
	result := E2EResult{
		TargetURL: target, ExpectedText: expected, ExpectedSeen: observation.ExpectedSeen,
		AbsentText: absent, AbsentGone: observation.AbsentGone,
		FinalURL: observation.FinalURL, StatusCode: observation.StatusCode,
		ObservedAt: observation.ObservedAt,
	}
	switch {
	case observation.ExpectedSeen && observation.AbsentGone:
		result.Verdict = "pass"
	case looksLikeLogin(observation.FinalURL):
		result.Verdict = "unknown"
		result.Detail = strings.TrimSpace("画面がログインページのままでした。確認用セッションの設定または再取得が必要です。 " + cookieDetail)
	default:
		result.Verdict = "fail"
	}
	if len(observation.ScreenshotPNG) > 0 {
		if err := os.WriteFile(p.path(E2EScreenshotFile), observation.ScreenshotPNG, 0o600); err == nil {
			result.Screenshot = true
		}
	}
	return p.sealE2EResult(result)
}

func (p *Pipeline) sealE2EResult(result E2EResult) error {
	result.SchemaVersion = 1
	if result.ObservedAt.IsZero() {
		result.ObservedAt = time.Now().UTC()
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return os.WriteFile(p.path(E2EResultFile), encoded, 0o600)
}

// e2eVerification reads the observation target the readiness gate derived:
// the page path, the text that must appear, the text that must be gone.
func (p *Pipeline) e2eVerification() (string, string, string, error) {
	repository, err := p.readJSONField("readiness-ticket.json", "repository")
	if err != nil || repository == "" {
		return "", "", "", errors.New("e2e-check needs the readiness ticket")
	}
	verificationPath, err := p.readJSONField("readiness-ticket.json", "verification_path")
	if err != nil || verificationPath == "" {
		return "", "", "", errors.New("e2e-check needs the verification path")
	}
	expected, err := p.readJSONField("readiness-ticket.json", "expected_text")
	if err != nil || expected == "" {
		return "", "", "", errors.New("e2e-check needs the expected text")
	}
	absent, _ := p.readJSONField("readiness-ticket.json", "absent_text")
	origin, err := consumerStagingOrigin(p.Config.ConsumerConfigPath, repository)
	if err != nil {
		return "", "", "", err
	}
	return origin + verificationPath, expected, absent, nil
}

// consumerStagingOrigin resolves the destination's staging origin from the
// host-side consumer configuration, the same way chainReviewers resolves
// the reviewer identities.
func consumerStagingOrigin(consumerConfigPath, repository string) (string, error) {
	raw, err := os.ReadFile(consumerConfigPath)
	if err != nil {
		return "", errors.New("consumer config unreadable")
	}
	var parsed struct {
		Consumers []struct {
			Repository    string `json:"repository"`
			StagingOrigin string `json:"staging_origin"`
		} `json:"consumers"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", errors.New("consumer config invalid")
	}
	for _, consumer := range parsed.Consumers {
		if consumer.Repository == repository && consumer.StagingOrigin != "" {
			return consumer.StagingOrigin, nil
		}
	}
	return "", errors.New("consumer staging origin missing")
}

// loadE2ESessionCookies reads the operator-provisioned session file
// (Playwright storageState JSON). Every failure degrades to "no cookies"
// with a human-readable note — the observation itself then reports the
// login page honestly instead of failing the card.
func loadE2ESessionCookies(path string) ([]visiblecheck.E2ECookie, string) {
	if path == "" {
		return nil, "確認用セッションが設定されていません。"
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > 1<<20 {
		return nil, "確認用セッションのファイルを読めませんでした。"
	}
	var state struct {
		Cookies []visiblecheck.E2ECookie `json:"cookies"`
	}
	if err := json.Unmarshal(raw, &state); err != nil || len(state.Cookies) == 0 {
		return nil, "確認用セッションのファイルの形式が想定と異なります。"
	}
	return state.Cookies, ""
}

func looksLikeLogin(finalURL string) bool {
	return strings.Contains(finalURL, "/login")
}
