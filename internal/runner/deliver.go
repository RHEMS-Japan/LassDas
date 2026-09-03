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

// The v2 delivery continuation: after publish sealed the feature PR, three
// cards carry it the rest of the way — checks (CI green), integrate (merge
// to staging, deploy wait, sealed observation) and promote (promotion PR,
// production deploy, sealed production observation). One state-driven verb
// serves all three; the --until milestone is what keeps the attendant's
// stop-recheck between CI-green and the merge. Cards write artifacts only —
// the attendant owns every ledger row and every requester comment.

const (
	DeliverChecksFile            = "feature-checks.json"
	DeliverMergeFile             = "feature-merge.json"
	DeliverStagingProofFile      = "staging-proof.json"
	DeliverStagingPlainProofFile = "staging-proof.plain.json"
	DeliverStagingVisibleFile    = "staging-visible.json"
	DeliverStagingShotFile       = "staging-shot.png"
	DeliverDeltaFile             = "promotion-delta.json"
	DeliverStagingReportFile     = "deliver-staging-report.json"
	DeliverPromotionFile         = "promotion.json"
	DeliverPromotionMergeFile    = "promotion-merge.json"
	DeliverReflectionFile        = "promotion-reflection.json"
	DeliverProductionFile        = "production.json"
	DeliverProductionPlainFile   = "production-proof.plain.json"
	DeliverProductionVisibleFile = "production-visible.json"
	DeliverProductionShotFile    = "production-shot.png"
	DeliverProductionReportFile  = "deliver-production-report.json"
)

// Milestones for --until.
const (
	DeliverUntilChecks     = "checks"
	DeliverUntilStaging    = "staging-observed"
	DeliverUntilProduction = "production-observed"
)

// DeliverReport is the attendant-facing summary one delivery phase seals.
// Failed steps are RESULTS (sealed, exit zero); only broken plumbing exits
// non-zero and blocks the card.
type DeliverReport struct {
	SchemaVersion int    `json:"schema_version"`
	Phase         string `json:"phase"`   // staging | production
	Verdict       string `json:"verdict"` // pass | checks_failed | merge_failed | merge_unverified | deploy_failed | observe_failed | observe_blocked | promotion_failed
	// Block, with an observe_blocked verdict, says why the page could not
	// be judged at all: "sign_in" (the consumer's login did not land — the
	// session jar is no longer accepted) or "redirect" (the target sent the
	// browser to another page). Neither says anything about the change.
	Block          string          `json:"block,omitempty"`
	Detail         string          `json:"detail,omitempty"`
	TargetURL      string          `json:"target_url,omitempty"`
	ExpectedText   string          `json:"expected_text,omitempty"`
	AbsentText     string          `json:"absent_text,omitempty"`
	FinalURL       string          `json:"final_url,omitempty"`
	StatusCode     int             `json:"status_code,omitempty"`
	Screenshot     bool            `json:"screenshot"`
	MergedSHA      string          `json:"merged_sha,omitempty"`
	PullRequestURL string          `json:"pull_request_url,omitempty"`
	Delta          json.RawMessage `json:"delta,omitempty"`
	// PromotionHold, when set, is the human-readable reason a promotion
	// CANNOT pass the gate right now (the promotion moves exactly one
	// delivery: foreign changes on staging, divergence, or an unreadable
	// delta all block it). The attendant then reports honestly instead of
	// asking for a Go that could only fail.
	PromotionHold string `json:"promotion_hold,omitempty"`
	// ScreenChecked marks a pass whose screen WAS machine-verified. The
	// reference path (no visible-wording promise on the ticket) passes
	// without checking any screen, and the report headline must never
	// claim otherwise.
	ScreenChecked bool      `json:"screen_checked,omitempty"`
	ObservedAt    time.Time `json:"observed_at"`
}

// RunDeliver advances the delivery to the requested milestone, resuming
// past every step whose artifact already exists.
func (p *Pipeline) RunDeliver(ctx context.Context, until string) error {
	if until != DeliverUntilChecks && until != DeliverUntilStaging && until != DeliverUntilProduction {
		return errors.New("deliver milestone is invalid")
	}
	if err := p.verifyToolPins(); err != nil {
		return err
	}
	if !p.exists("feature-pr.json") {
		return errors.New("deliver needs the delivered pull request artifact")
	}
	round := p.latestCandidateRound()
	if round < 1 {
		return errors.New("deliver needs a sealed candidate round")
	}
	stageDir := fmt.Sprintf("history/stage-%d", round)
	reviewers, err := chainReviewers(p.Config.ConsumerConfigPath)
	if err != nil {
		return err
	}
	reviews := chainReviewFiles(reviewers)

	if until == DeliverUntilProduction {
		if p.exists(DeliverProductionReportFile) {
			return nil
		}
		return p.deliverProduction(ctx, stageDir, reviews)
	}
	if p.exists(DeliverStagingReportFile) {
		return nil
	}
	if done, err := p.deliverChecks(ctx, stageDir); err != nil || done {
		return err
	}
	if until == DeliverUntilChecks {
		return nil
	}
	return p.deliverStaging(ctx, stageDir, reviews)
}

// deliverChecks waits for the feature CI. A red gate is a sealed result
// (done=true): the run stops before staging and the attendant reports it.
func (p *Pipeline) deliverChecks(ctx context.Context, stageDir string) (bool, error) {
	if p.exists(DeliverChecksFile) {
		return false, nil
	}
	code, err := p.controller(ctx, "wait-feature", append([]string{"wait-feature"},
		p.deliverCommon(stageDir, "--feature-pr", p.path("feature-pr.json"), "--out", p.path(DeliverChecksFile))...))
	if err != nil {
		return false, err
	}
	if code != 0 {
		return true, p.sealDeliverReport(DeliverReport{
			Phase: "staging", Verdict: "checks_failed",
			Detail: "納品 PR の自動検査 (CI) が期限内に全部緑になりませんでした。ステージングへの反映は行っていません。",
		})
	}
	return false, nil
}

// deliverStaging merges to the integration branch, waits for the staging
// deployment, seals the browser observation and the promotion delta, and
// writes the staging report.
func (p *Pipeline) deliverStaging(ctx context.Context, stageDir string, reviews []string) error {
	if !p.exists(DeliverMergeFile) {
		code, err := p.controller(ctx, "merge-feature", append([]string{"merge-feature"},
			p.deliverCommon(stageDir,
				"--feature-pr", p.path("feature-pr.json"),
				"--checks", p.path(DeliverChecksFile),
				"--out", p.path(DeliverMergeFile))...))
		if err != nil {
			return err
		}
		if code != 0 {
			// The merge verb can fail AFTER the merge landed (its
			// post-merge verification is fallible). Read the pull request
			// back so the report never claims "not merged" about a branch
			// that moved.
			switch p.probeMerged(ctx, stageDir, "feature-pr.json", "payload", "pull_request", "Number") {
			case "merged":
				return p.sealDeliverReport(DeliverReport{
					Phase: "staging", Verdict: "deploy_failed",
					Detail: "ステージングへのマージ自体は成立しましたが、その後の確認を続行できませんでした。変更はステージングに入っています。",
				})
			case "unmerged":
				return p.sealDeliverReport(DeliverReport{
					Phase: "staging", Verdict: "merge_failed",
					Detail: "ステージングへの自動マージが完了しませんでした（統合先が動いた・保護設定と競合した等）。",
				})
			default:
				return p.sealDeliverReport(DeliverReport{
					Phase: "staging", Verdict: "merge_unverified",
					Detail: "自動マージの結果を確認できませんでした。ステージングに反映された可能性があります。",
				})
			}
		}
	}
	if !p.exists(DeliverStagingProofFile) {
		code, err := p.controller(ctx, "await-staging", append([]string{"await-staging"},
			p.deliverGate(stageDir, reviews,
				"--feature-merge", p.path(DeliverMergeFile),
				"--out", p.path(DeliverStagingProofFile))...))
		if err != nil {
			return err
		}
		if code != 0 {
			return p.sealDeliverReport(DeliverReport{
				Phase: "staging", Verdict: "deploy_failed",
				Detail: "ステージングの自動デプロイの完了を確認できませんでした。",
			})
		}
	}
	if err := extractArtifactPayload[json.RawMessage](p.path(DeliverStagingProofFile), p.path(DeliverStagingPlainProofFile)); err != nil {
		return err
	}
	// A ticket that promises nothing about visible wording (the intake
	// contract allows that) has nothing a machine can pass or fail — and
	// the promotion gates require the sealed pass evidence this path
	// cannot produce. Verifying nothing would be theater; failing would be
	// a lie. So: observe for reference (photo, no verdict), report the
	// deploy-verified pass, and hold the promotion honestly. Only the
	// explicit no-promise shape takes this path — an unreadable ticket or
	// a broken consumer config keeps failing loudly downstream.
	if verificationPath, err := p.readJSONField(stageDir+"/ticket.json", "verification_path"); err == nil && verificationPath == "" {
		return p.sealReferenceStagingReport(ctx, stageDir)
	}
	if !p.exists(DeliverStagingVisibleFile) {
		code, err := p.browsercheck(ctx, stageDir, reviews, "staging",
			"--staging-proof", p.path(DeliverStagingPlainProofFile),
			"--evidence-out", p.path(DeliverStagingVisibleFile),
			"--screenshot-out", p.path(DeliverStagingShotFile))
		if err != nil {
			return err
		}
		if code != 0 {
			// The sealed observation refuses anything short of a full pass.
			// Take the courtesy observation so the report still carries a
			// screenshot of what WAS shown.
			return p.sealCourtesyObservation(ctx, stageDir, "staging",
				"ステージング画面の機械確認が合格しませんでした。本番反映は行えません（合格の証拠が封印された場合のみ昇格できます）。")
		}
	}
	if !p.exists(DeliverDeltaFile) {
		if code, err := p.controller(ctx, "promotion-delta", append([]string{"promotion-delta"},
			p.deliverCommon(stageDir, "--out", p.path(DeliverDeltaFile))...)); err != nil {
			return err
		} else if code != 0 {
			_ = os.WriteFile(p.path(DeliverDeltaFile), []byte(`{"status":"unavailable"}`), 0o600)
		}
	}
	report := DeliverReport{Phase: "staging", Verdict: "pass", ScreenChecked: true}
	p.fillObservation(&report, stageDir, "staging", DeliverStagingVisibleFile, DeliverStagingShotFile)
	p.fillDelta(&report)
	report.PromotionHold = p.promotionHold()
	if sha, err := p.readJSONField(DeliverMergeFile, "payload", "merge", "MergeSHA"); err == nil {
		report.MergedSHA = sha
	}
	return p.sealDeliverReport(report)
}

// promotionHold answers "could a promotion pass the gate right now?" — the
// gate moves exactly one delivery (its product paths plus the CI digest
// files, verified file-by-file), so anything else on the branch makes a Go
// unfulfillable and must be said BEFORE asking for one. Empty means a
// promotion can proceed.
func (p *Pipeline) promotionHold() string {
	const consultOperator = "本番へ反映する場合は、運用の昇格手順で反映してください。"
	var delta struct {
		Status         string   `json:"status"`
		Files          []string `json:"files"`
		FilesTruncated bool     `json:"files_truncated"`
	}
	raw, err := readWorkspaceFile(p.path(DeliverDeltaFile), maxWorkspaceReadBytes)
	if err != nil || json.Unmarshal(raw, &delta) != nil || delta.Status == "" || delta.Status == "unavailable" {
		return "本番との差分を確認できなかったため、自動の本番反映は行いません。" + consultOperator
	}
	switch delta.Status {
	case "behind", "diverged":
		return "本番にはステージングに無い変更が入っています（分岐状態）。自動の本番反映は行いません。" + consultOperator
	case "identical":
		return "ステージングと本番は既に同じ内容のため、反映するものがありません。"
	}
	if delta.FilesTruncated {
		return "本番との差分が大きすぎて全件を確認できないため、自動の本番反映は行いません。" + consultOperator
	}
	allowed := map[string]bool{}
	var product []string
	if raw, err := readWorkspaceFile(p.path("feature-pr.json"), maxWorkspaceReadBytes); err == nil {
		var wrapper struct {
			Binding struct {
				ProductPaths []string `json:"product_paths"`
			} `json:"binding"`
		}
		if json.Unmarshal(raw, &wrapper) == nil {
			product = wrapper.Binding.ProductPaths
		}
	}
	if len(product) == 0 {
		return "今回の納品の対象ファイル一覧を読めなかったため、自動の本番反映は行いません。" + consultOperator
	}
	for _, path := range product {
		allowed[path] = true
	}
	for _, path := range p.consumerDigestPaths() {
		allowed[path] = true
	}
	for _, path := range delta.Files {
		if !allowed[path] {
			return "ステージングには今回の納品以外の変更が滞留しています。本番反映は 1 納品ずつのため、自動の本番反映は行いません。" + consultOperator
		}
	}
	return ""
}

// consumerDigestPaths reads the CI digest-commit paths for this delivery's
// repository — those files legitimately ride every promotion.
func (p *Pipeline) consumerDigestPaths() []string {
	raw, err := os.ReadFile(p.Config.ConsumerConfigPath)
	if err != nil {
		return nil
	}
	repository, err := p.readJSONField("feature-pr.json", "binding", "repository")
	if err != nil {
		return nil
	}
	var parsed struct {
		Consumers []struct {
			Repository          string `json:"repository"`
			StagingDigestCommit struct {
				ExactPaths []string `json:"exact_paths"`
			} `json:"staging_digest_commit"`
		} `json:"consumers"`
	}
	if json.Unmarshal(raw, &parsed) != nil {
		return nil
	}
	for _, consumer := range parsed.Consumers {
		if consumer.Repository == repository {
			return consumer.StagingDigestCommit.ExactPaths
		}
	}
	return nil
}

// deliverProduction runs only after the attendant saw the requester's Go:
// promotion PR, promotion merge, production deployment, sealed production
// observation, final report.
func (p *Pipeline) deliverProduction(ctx context.Context, stageDir string, reviews []string) error {
	if !p.exists(DeliverStagingReportFile) || !p.exists(DeliverStagingVisibleFile) {
		return errors.New("deliver production needs the sealed staging observation")
	}
	if !p.exists(DeliverPromotionFile) {
		code, err := p.controller(ctx, "create-promotion-pr", append([]string{"create-promotion-pr"},
			p.deliverGate(stageDir, reviews,
				"--staging", p.path(DeliverStagingProofFile),
				"--visible", p.path(DeliverStagingVisibleFile),
				"--screenshot", p.path(DeliverStagingShotFile),
				"--out", p.path(DeliverPromotionFile))...))
		if err != nil {
			return err
		}
		if code != 0 {
			return p.sealDeliverReport(DeliverReport{
				Phase: "production", Verdict: "promotion_failed",
				Detail: "本番反映の準備 (昇格 PR の作成) が検査で止まりました（確認後にステージングが変わった・ステージングに他の変更が滞留している等）。本番は未変更です。",
			})
		}
	}
	if !p.exists(DeliverPromotionMergeFile) {
		code, err := p.controller(ctx, "merge-promotion", append([]string{"merge-promotion"},
			p.deliverGate(stageDir, reviews,
				"--promotion", p.path(DeliverPromotionFile),
				"--reflection-out", p.path(DeliverReflectionFile),
				"--out", p.path(DeliverPromotionMergeFile))...))
		if err != nil {
			return err
		}
		if code != 0 {
			// Same honesty rule as the staging merge: the release branch
			// can move even when the verb fails. The reflection artifact is
			// written the moment the merge lands; absent that, ask GitHub.
			if p.exists(DeliverReflectionFile) {
				return p.sealDeliverReport(DeliverReport{
					Phase: "production", Verdict: "deploy_failed",
					Detail: "本番ブランチへの反映自体は成立しましたが、その後の確認を続行できませんでした。",
				})
			}
			switch p.probeMerged(ctx, stageDir, DeliverPromotionFile, "payload", "pull_request", "Number") {
			case "merged":
				return p.sealDeliverReport(DeliverReport{
					Phase: "production", Verdict: "deploy_failed",
					Detail: "本番ブランチへの反映自体は成立しましたが、その後の確認を続行できませんでした。",
				})
			case "unmerged":
				return p.sealDeliverReport(DeliverReport{
					Phase: "production", Verdict: "promotion_failed",
					Detail: "昇格 PR の自動マージが完了しませんでした。本番ブランチは未変更です。",
				})
			default:
				return p.sealDeliverReport(DeliverReport{
					Phase: "production", Verdict: "merge_unverified",
					Detail: "昇格マージの結果を確認できませんでした。本番に反映された可能性があります。",
				})
			}
		}
	}
	if !p.exists(DeliverProductionFile) {
		code, err := p.controller(ctx, "await-production", append([]string{"await-production"},
			p.deliverGate(stageDir, reviews,
				"--promotion-merge", p.path(DeliverPromotionMergeFile),
				"--out", p.path(DeliverProductionFile))...))
		if err != nil {
			return err
		}
		if code != 0 {
			return p.sealDeliverReport(DeliverReport{
				Phase: "production", Verdict: "deploy_failed",
				Detail: "本番の自動デプロイの完了を確認できませんでした（コードは prod ブランチに入っています）。",
			})
		}
	}
	if err := extractProductionProof(p.path(DeliverProductionFile), p.path(DeliverProductionPlainFile)); err != nil {
		return err
	}
	if !p.exists(DeliverProductionVisibleFile) {
		code, err := p.browsercheck(ctx, stageDir, reviews, "production",
			"--staging-proof", p.path(DeliverStagingPlainProofFile),
			"--production-proof", p.path(DeliverProductionPlainFile),
			"--prior-evidence", p.path(DeliverStagingVisibleFile),
			"--prior-screenshot", p.path(DeliverStagingShotFile),
			"--evidence-out", p.path(DeliverProductionVisibleFile),
			"--screenshot-out", p.path(DeliverProductionShotFile))
		if err != nil {
			return err
		}
		if code != 0 {
			return p.sealCourtesyObservation(ctx, stageDir, "production",
				"本番反映は完了しましたが、本番画面の機械確認が合格しませんでした。手動でご確認ください。")
		}
	}
	report := DeliverReport{Phase: "production", Verdict: "pass", ScreenChecked: true}
	p.fillObservation(&report, stageDir, "production", DeliverProductionVisibleFile, DeliverProductionShotFile)
	if url, err := p.readJSONField(DeliverPromotionFile, "payload", "pull_request", "HTMLURL"); err == nil {
		report.PullRequestURL = url
	}
	if sha, err := p.readJSONField(DeliverPromotionMergeFile, "payload", "merge", "MergeSHA"); err == nil {
		report.MergedSHA = sha
	}
	return p.sealDeliverReport(report)
}

// probeMerged asks GitHub whether the pull request named in the artifact
// actually merged. Used ONLY after a merge verb failed; every failure to
// answer is an honest "unknown", never a guess.
func (p *Pipeline) probeMerged(ctx context.Context, stageDir, artifact string, keys ...string) string {
	number, err := p.readJSONField(artifact, keys...)
	if err != nil || number == "" {
		return "unknown"
	}
	if err := os.Remove(p.path("merge-probe.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "unknown"
	}
	code, err := p.controller(ctx, "read-merged", append([]string{"read-merged"},
		p.deliverCommon(stageDir, "--number", number, "--out", p.path("merge-probe.json"))...))
	if err != nil || code != 0 {
		return "unknown"
	}
	merged, err := p.readJSONField("merge-probe.json", "merged")
	if err != nil {
		return "unknown"
	}
	switch merged {
	case "true":
		return "merged"
	case "false":
		return "unmerged"
	default:
		// A missing or unexpected field is NOT "not merged" — the whole
		// point of this probe is never to guess.
		return "unknown"
	}
}

// deliverCommon renders the arguments every controller verb shares.
func (p *Pipeline) deliverCommon(stageDir string, extra ...string) []string {
	arguments := []string{
		"--config", p.Config.ConsumerConfigPath,
		"--ticket", p.path(stageDir + "/ticket.json"),
	}
	return append(arguments, extra...)
}

// deliverGate adds the full publication-gate artifact set the promotion
// verbs re-verify, on top of the common arguments.
func (p *Pipeline) deliverGate(stageDir string, reviews []string, extra ...string) []string {
	arguments := p.deliverCommon(stageDir,
		"--source", p.path(stageDir+"/source.json"),
		"--candidate", p.path(stageDir+"/candidate.json"),
		"--decision", p.path(stageDir+"/decision.json"),
		"--validation", p.path("validation.json"),
		"--baseline", p.deliverBaseline(),
	)
	for _, name := range reviews {
		arguments = append(arguments, "--review", p.path(stageDir+"/"+name))
	}
	return append(arguments, extra...)
}

// deliverBaseline picks the baseline publish actually used: the advanced
// snapshot when the integration branch moved mid-run, the original
// otherwise.
func (p *Pipeline) deliverBaseline() string {
	if p.exists(advancedBaselineFile) {
		return p.path(advancedBaselineFile)
	}
	return p.path("baseline.json")
}

// browsercheck runs the sealed observation binary with the gate artifacts.
func (p *Pipeline) browsercheck(ctx context.Context, stageDir string, reviews []string, environment string, extra ...string) (int, error) {
	if p.Config.BrowserCheckBin == "" {
		return 0, errors.New("browsercheck binary is not configured")
	}
	toolSHA, err := p.readJSONField(stageDir+"/ticket.json", "tool_sha")
	if err != nil || toolSHA == "" {
		return 0, errors.New("ticket tool sha unreadable")
	}
	argv := []string{p.Config.BrowserCheckBin,
		"--config", p.Config.ConsumerConfigPath,
		"--ticket", p.path(stageDir + "/ticket.json"),
		"--source", p.path(stageDir + "/source.json"),
		"--candidate", p.path(stageDir + "/candidate.json"),
		"--decision", p.path(stageDir + "/decision.json"),
		"--validation", p.path("validation.json"),
		"--environment", environment,
		"--tool-sha", toolSHA,
	}
	for _, name := range reviews {
		argv = append(argv, "--review", p.path(stageDir+"/"+name))
	}
	argv = append(argv, extra...)
	return p.step(ctx, "browsercheck-"+environment, argv)
}

// sealReferenceStagingReport reports a staging delivery whose ticket makes
// no promise about visible wording: the deploy itself is verified (the
// sealed proof chain up to here), a reference photo is attached when the
// browser can take one, and the promotion is held — the gates require
// pass evidence this path cannot honestly produce.
func (p *Pipeline) sealReferenceStagingReport(ctx context.Context, stageDir string) error {
	if !p.exists(DeliverDeltaFile) {
		if code, err := p.controller(ctx, "promotion-delta", append([]string{"promotion-delta"},
			p.deliverCommon(stageDir, "--out", p.path(DeliverDeltaFile))...)); err != nil {
			return err
		} else if code != 0 {
			_ = os.WriteFile(p.path(DeliverDeltaFile), []byte(`{"status":"unavailable"}`), 0o600)
		}
	}
	report := DeliverReport{
		Phase: "staging", Verdict: "pass",
		PromotionHold: "このチケットには画面に表示される内容の約束が無く、本番反映に必要な画面確認の合格証拠を作れないため、自動の本番反映は行いません。" +
			"ステージングでの動作確認は人の目で行ってください。本番へ反映する場合は、運用の昇格手順で反映してください。",
	}
	var cookieNote string
	if repository, err := p.readJSONField(stageDir+"/ticket.json", "repository"); err == nil && repository != "" {
		if origin, entry, err := consumerObservation(p.Config.ConsumerConfigPath, repository, "staging"); err == nil {
			report.TargetURL = strings.TrimRight(origin, "/") + "/"
			var cookies []visiblecheck.E2ECookie
			cookies, cookieNote = e2eSessionCookies()
			if observed, err := visiblecheck.ObserveForReference(ctx, report.TargetURL, cookies, entry); err == nil {
				report.FinalURL, report.StatusCode = observed.FinalURL, observed.StatusCode
				if note := referenceBlockNote(observed.Block); note != "" {
					cookieNote = strings.TrimSpace(cookieNote + " " + note)
				}
				if len(observed.ScreenshotPNG) > 0 {
					if err := os.WriteFile(p.path(DeliverStagingShotFile), observed.ScreenshotPNG, 0o600); err == nil {
						report.Screenshot = true
					}
				}
			}
		}
	}
	// The detail is composed AFTER the observation so it never promises a
	// photo the best-effort capture did not produce.
	report.Detail = "デプロイの完了は照合済みです。チケットに画面へ表示される内容の約束が書かれていないため、画面の合否確認は行っていません。"
	if report.Screenshot {
		report.Detail += " 参考として反映後の画面写真を添付します。"
	}
	if cookieNote != "" {
		report.Detail = strings.TrimSpace(report.Detail + " " + cookieNote)
	}
	p.fillDelta(&report)
	if sha, err := p.readJSONField(DeliverMergeFile, "payload", "merge", "MergeSHA"); err == nil {
		report.MergedSHA = sha
	}
	return p.sealDeliverReport(report)
}

// sealCourtesyObservation is the two-tier fallback: the sealed observation
// refused (no evidence, no screenshot), so take the debug role's courtesy
// observation for the report — a picture of what WAS shown, honestly short
// of promotion-grade proof.
func (p *Pipeline) sealCourtesyObservation(ctx context.Context, stageDir, phase, detail string) error {
	report := DeliverReport{Phase: phase, Verdict: "observe_failed", Detail: detail}
	target, expected, absent, entry, err := p.deliverVerification(stageDir, phase)
	if err == nil {
		report.TargetURL, report.ExpectedText, report.AbsentText = target, expected, absent
		cookies, cookieDetail := e2eSessionCookies()
		if cookieDetail != "" {
			report.Detail = strings.TrimSpace(report.Detail + " " + cookieDetail)
		}
		if observed, observeErr := visiblecheck.ObserveForE2E(ctx, target, expected, absent, cookies, entry); observeErr == nil {
			report.FinalURL, report.StatusCode = observed.FinalURL, observed.StatusCode
			if verdict, block, blocked := courtesyVerdict(phase, observed); verdict != "" {
				report.Verdict, report.Block = verdict, block
				report.Detail = strings.TrimSpace(blocked + " " + cookieDetail)
			}
			shot := DeliverStagingShotFile
			if phase == "production" {
				shot = DeliverProductionShotFile
			}
			if len(observed.ScreenshotPNG) > 0 {
				if err := os.WriteFile(p.path(shot), observed.ScreenshotPNG, 0o600); err == nil {
					report.Screenshot = true
				}
			}
		}
	}
	p.fillDelta(&report)
	return p.sealDeliverReport(report)
}

// courtesyVerdict turns a blocked courtesy observation into the report's
// honest verdict. A page the browser never reached says nothing about the
// change, so it is neither a pass nor a fail: observe_blocked, with the
// reason spelled out for the person who has to act. An observation that
// did open the target keeps the sealed path's failure verdict (empty).
func courtesyVerdict(phase string, observed visiblecheck.E2EObservation) (verdict, block, detail string) {
	environment := "ステージング"
	if phase == "production" {
		environment = "本番"
	}
	switch observed.Block {
	case visiblecheck.BlockSignIn:
		detail := environment + "の確認用の画面にログインできなかったため、画面の合否を判定できませんでした。確認用のログイン状態が切れているか、取り消されています。"
		if observed.FinalURL != "" {
			detail += fmt.Sprintf("（ブラウザの停止先: %s）", observed.FinalURL)
		}
		return "observe_blocked", string(observed.Block),
			detail + "運用担当者が確認用のログインをやり直すと、以後の依頼で自動確認が復旧します。この依頼の変更が正しいかどうかは、この結果からは分かりません。"
	case visiblecheck.BlockRedirect:
		return "observe_blocked", string(observed.Block),
			fmt.Sprintf("確認先の画面が同じサイトの別の画面へ転送されたため、画面の合否を判定できませんでした（転送先: %s）。", observed.FinalURL) +
				"組織の状態などで転送される画面は自動確認に向きません。転送されない画面を確認先に指定して起票し直してください。"
	}
	return "", "", ""
}

// referenceBlockNote says what the reference photo actually shows when
// the browser was not let in. A redirect within the destination (its
// root sending the browser to a default page) is the ordinary case for a
// reference photo and needs no note.
func referenceBlockNote(block visiblecheck.Block) string {
	if block == visiblecheck.BlockSignIn {
		return "確認用の画面にログインできなかったため、写真はログイン前の画面です。運用担当者が確認用のログインをやり直す必要があります。"
	}
	return ""
}

// deliverVerification reads the observation target from the SEALED round's
// ticket — the same one every gate verb re-verifies — against the origin of
// the phase's OWN environment, with that environment's login entry.
func (p *Pipeline) deliverVerification(stageDir, environment string) (string, string, string, visiblecheck.SignIn, error) {
	ticket := stageDir + "/ticket.json"
	repository, err := p.readJSONField(ticket, "repository")
	if err != nil || repository == "" {
		return "", "", "", visiblecheck.SignIn{}, errors.New("deliver needs the sealed ticket")
	}
	verificationPath, err := p.readJSONField(ticket, "verification_path")
	if err != nil || verificationPath == "" {
		return "", "", "", visiblecheck.SignIn{}, errors.New("deliver needs the verification path")
	}
	expected, err := p.readJSONField(ticket, "expected_text")
	if err != nil || expected == "" {
		return "", "", "", visiblecheck.SignIn{}, errors.New("deliver needs the expected text")
	}
	absent, _ := p.readJSONField(ticket, "absent_text")
	origin, entry, err := consumerObservation(p.Config.ConsumerConfigPath, repository, environment)
	if err != nil {
		return "", "", "", visiblecheck.SignIn{}, err
	}
	return origin + verificationPath, expected, absent, entry, nil
}

func (p *Pipeline) fillObservation(report *DeliverReport, stageDir, environment, visibleFile, shotFile string) {
	if target, expected, absent, _, err := p.deliverVerification(stageDir, environment); err == nil {
		report.TargetURL, report.ExpectedText, report.AbsentText = target, expected, absent
	}
	if finalURL, err := p.readJSONField(visibleFile, "final_url"); err == nil {
		report.FinalURL = finalURL
	}
	report.Screenshot = p.exists(shotFile)
}

func (p *Pipeline) fillDelta(report *DeliverReport) {
	raw, err := readWorkspaceFile(p.path(DeliverDeltaFile), maxWorkspaceReadBytes)
	if err != nil || !json.Valid(raw) {
		return
	}
	report.Delta = json.RawMessage(raw)
}

func (p *Pipeline) sealDeliverReport(report DeliverReport) error {
	report.SchemaVersion = 1
	if report.ObservedAt.IsZero() {
		report.ObservedAt = time.Now().UTC()
	}
	name := DeliverStagingReportFile
	if report.Phase == "production" {
		name = DeliverProductionReportFile
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		return err
	}
	return os.WriteFile(p.path(name), encoded, 0o600)
}

// extractArtifactPayload writes the bare payload of a controller delivery
// artifact to its own file — the browser binary reads proofs unwrapped.
func extractArtifactPayload[T any](wrapped, plain string) error {
	raw, err := readWorkspaceFile(wrapped, maxWorkspaceReadBytes)
	if err != nil {
		return err
	}
	var wrapper struct {
		Payload T `json:"payload"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return errors.New("delivery artifact payload invalid")
	}
	encoded, err := json.Marshal(wrapper.Payload)
	if err != nil {
		return err
	}
	return os.WriteFile(plain, encoded, 0o600)
}

// extractProductionProof pulls payload.production out of the production
// artifact.
func extractProductionProof(wrapped, plain string) error {
	type productionOnly struct {
		Production json.RawMessage `json:"production"`
	}
	raw, err := readWorkspaceFile(wrapped, maxWorkspaceReadBytes)
	if err != nil {
		return err
	}
	var wrapper struct {
		Payload productionOnly `json:"payload"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil || len(wrapper.Payload.Production) == 0 {
		return errors.New("production artifact payload invalid")
	}
	return os.WriteFile(plain, wrapper.Payload.Production, 0o600)
}
