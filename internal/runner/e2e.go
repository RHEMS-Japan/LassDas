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
	"automation.internal/ticket-ingress/internal/worker"
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
	// E2ESessionStateFileEnvironment names the engine's renewed copy of it.
	E2ESessionFileEnvironment      = visiblecheck.SessionFileEnvironment
	E2ESessionStateFileEnvironment = visiblecheck.SessionStateFileEnvironment
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
	target, expected, absent, entry, err := p.e2eVerification()
	if err != nil {
		return err
	}
	cookies, cookieDetail := e2eSessionCookies()
	observation, err := visiblecheck.ObserveForE2E(ctx, target, expected, absent, cookies, entry)
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
	case observation.Block == visiblecheck.BlockSignIn:
		// The login did not land: the page was never reached, so this says
		// nothing about the change.
		result.Verdict = "unknown"
		result.Detail = strings.TrimSpace("確認用の画面にログインできませんでした。確認用のログイン状態が切れているか、取り消されています。運用担当者が確認用のログインをやり直す必要があります。 " + cookieDetail)
	case observation.Block == visiblecheck.BlockRedirect:
		result.Verdict = "unknown"
		result.Detail = strings.TrimSpace(fmt.Sprintf("確認先の画面が同じサイトの別の画面へ転送されました（転送先: %s）。組織の状態などで転送される画面は自動確認に向きません。 ", observation.FinalURL) + cookieDetail)
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
func (p *Pipeline) e2eVerification() (string, string, string, visiblecheck.SignIn, error) {
	repository, err := p.readJSONField("readiness-ticket.json", "repository")
	if err != nil || repository == "" {
		return "", "", "", visiblecheck.SignIn{}, errors.New("e2e-check needs the readiness ticket")
	}
	verificationPath, err := p.readJSONField("readiness-ticket.json", "verification_path")
	if err != nil || verificationPath == "" {
		return "", "", "", visiblecheck.SignIn{}, errors.New("e2e-check needs the verification path")
	}
	expected, err := p.readJSONField("readiness-ticket.json", "expected_text")
	if err != nil || expected == "" {
		return "", "", "", visiblecheck.SignIn{}, errors.New("e2e-check needs the expected text")
	}
	absent, _ := p.readJSONField("readiness-ticket.json", "absent_text")
	origin, entry, err := consumerObservation(p.Config.ConsumerConfigPath, repository, "staging")
	if err != nil {
		return "", "", "", visiblecheck.SignIn{}, err
	}
	return origin + verificationPath, expected, absent, entry, nil
}

// consumerStagingOrigin resolves the destination's staging origin from the
// host-side consumer configuration.
func consumerStagingOrigin(consumerConfigPath, repository string) (string, error) {
	return consumerOrigin(consumerConfigPath, repository, "staging")
}

// consumerObservation resolves the observation origin and the login entry
// for one environment — the staging report must open the staging console
// and the production report the production console, never each other's.
// The landing of a login is the environment's own origin.
func consumerObservation(consumerConfigPath, repository, environment string) (string, visiblecheck.SignIn, error) {
	raw, err := os.ReadFile(consumerConfigPath)
	if err != nil {
		return "", visiblecheck.SignIn{}, errors.New("consumer config unreadable")
	}
	var parsed struct {
		Consumers []struct {
			Repository          string `json:"repository"`
			StagingOrigin       string `json:"staging_origin"`
			ProductionOrigin    string `json:"production_origin"`
			StagingLoginURL     string `json:"staging_login_url"`
			ProductionLoginURL  string `json:"production_login_url"`
			ObservationLanguage string `json:"observation_language"`
		} `json:"consumers"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", visiblecheck.SignIn{}, errors.New("consumer config invalid")
	}
	for _, consumer := range parsed.Consumers {
		if consumer.Repository != repository {
			continue
		}
		origin, login := consumer.StagingOrigin, consumer.StagingLoginURL
		if environment == "production" {
			origin, login = consumer.ProductionOrigin, consumer.ProductionLoginURL
		}
		if origin != "" {
			entry := visiblecheck.SignIn{}
			if worker.ValidLanguageTag(consumer.ObservationLanguage) {
				entry.Language = consumer.ObservationLanguage
			}
			if login != "" {
				entry.LoginURL, entry.LandedPrefix = login, origin
				entry.SeedPath, entry.KeepJarAt = os.Getenv(E2ESessionFileEnvironment), os.Getenv(E2ESessionStateFileEnvironment)
			}
			return origin, entry, nil
		}
	}
	return "", visiblecheck.SignIn{}, errors.New("consumer observation origin missing")
}

// consumerOrigin is the origin half of consumerObservation.
func consumerOrigin(consumerConfigPath, repository, environment string) (string, error) {
	origin, _, err := consumerObservation(consumerConfigPath, repository, environment)
	return origin, err
}

// loadE2ESessionCookies reads the session jar — the operator's seed and
// the engine's renewed copy; the implementation lives with the browser
// code so the sealed observations use the identical loader.
func loadE2ESessionCookies(seedPath, statePath string) ([]visiblecheck.E2ECookie, string) {
	return visiblecheck.LoadSessionCookies(seedPath, statePath)
}

// e2eSessionCookies reads the jar the pod environment names.
func e2eSessionCookies() ([]visiblecheck.E2ECookie, string) {
	return loadE2ESessionCookies(os.Getenv(E2ESessionFileEnvironment), os.Getenv(E2ESessionStateFileEnvironment))
}

func looksLikeLogin(finalURL string) bool {
	return strings.Contains(finalURL, "/login")
}
