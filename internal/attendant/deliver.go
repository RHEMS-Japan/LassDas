package attendant

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
)

// The v2 delivery, attendant side. Three cards carry a published feature to
// production — checks (CI green), integrate (staging merge + sealed
// observation) and promote (Go-driven promotion) — and this file owns
// everything that touches the ledger or the ticket: card issuance, the stop
// recheck right before the merge, the Go detection, both summary comments.
// Cards write artifacts only. Card keys live outside the chain namespace.

func deliverCardKey(deliveryID, stage string) string {
	return deliveryID + ":deliver:" + stage
}

// deliverObservable is the v2 gate: fully configured, a successful run,
// claimed after the operator's cut-off. Fails closed on anything
// unparsable — enabling the feature must never reach back through the
// ledger's past successes.
func deliverObservable(chain runtime.ChainConfig, run state.RunOverview) bool {
	if !chain.Deliver.Enabled() || run.TerminalCode != string(hook.TerminalSuccess) {
		return false
	}
	enabledAfter, err := chain.Deliver.EnabledAfterTime()
	if err != nil || run.ClaimedAt <= 0 || run.ClaimedAt < enabledAfter.UnixMilli() {
		return false
	}
	return true
}

// syncDeliver advances one terminal run's delivery by exactly one step per
// tick.
func syncDeliver(
	ctx context.Context,
	config runtime.Config,
	services *runtime.Services,
	hermes *runtime.Hermes,
	run state.RunOverview,
	tasks []runtime.BoardTask,
	logger Logger,
) error {
	if !deliverObservable(config.Chain, run) {
		return nil
	}
	runDir := runDirectory(config, run.DeliveryID)
	if !deliverFileExists(runDir, "feature-pr.json") {
		return nil
	}
	cards := map[string]*runtime.BoardTask{}
	for _, stage := range []string{"checks", "integrate", "promote"} {
		key := deliverCardKey(run.DeliveryID, stage)
		for index := range tasks {
			if tasks[index].IdempotencyKey == key {
				cards[stage] = &tasks[index]
				break
			}
		}
	}

	releasePosted, err := services.Tick.ReleaseReportPosted(ctx, run.RunID)
	if err != nil {
		return err
	}
	if releasePosted {
		return sweepDeliverCards(ctx, hermes, cards)
	}
	if deliverFileExists(runDir, runner.DeliverProductionReportFile) {
		return reportDeliverRelease(ctx, services, hermes, run, runDir, cards, logger)
	}
	if card := cards["promote"]; card != nil {
		if deliverCardStopped(card) {
			return reportDeadPromote(ctx, services, hermes, run, runDir, cards, logger)
		}
		return nil
	}

	stagingPosted, err := services.Tick.StagingReportPosted(ctx, run.RunID)
	if err != nil {
		return err
	}
	if stagingPosted {
		return advanceTowardsPromotion(ctx, config, services, hermes, run, runDir, logger)
	}
	if deliverFileExists(runDir, runner.DeliverStagingReportFile) {
		return reportDeliverStaging(ctx, config, services, hermes, run, runDir, cards, logger)
	}
	if card := cards["integrate"]; card != nil {
		if deliverCardStopped(card) {
			return reportDeadIntegrate(ctx, services, hermes, run, runDir, cards, logger)
		}
		return nil
	}
	if deliverFileExists(runDir, runner.DeliverChecksFile) {
		return issueIntegrateCard(ctx, config, services, hermes, run, runDir, cards, logger)
	}
	if card := cards["checks"]; card != nil {
		if deliverCardStopped(card) {
			content := hook.DeliverStagingContent(run.RunID, hook.DeliverStagingReport{
				Verdict: "card_failed",
				Detail:  "自動検査 (CI) の完了待ちが結果を残さず終了しました。",
			})
			if !services.Tick.PostStagingReport(ctx, run.RunID, run.DeliveryID, content, nil) {
				return nil
			}
			sealBoardOutcome(runDir, "staging", "card_failed", "")
			return sweepDeliverCards(ctx, hermes, cards)
		}
		return nil
	}
	return issueChecksCard(ctx, config, services, hermes, run, runDir, logger)
}

func deliverCardStopped(card *runtime.BoardTask) bool {
	return card.Status == "blocked" || card.Status == "done" || card.Status == "archived"
}

func deliverFileExists(runDir, name string) bool {
	_, err := os.Stat(filepath.Join(runDir, name))
	return err == nil
}

func sweepDeliverCards(ctx context.Context, hermes *runtime.Hermes, cards map[string]*runtime.BoardTask) error {
	for _, card := range cards {
		if card != nil && card.Status != "archived" {
			if err := hermes.Archive(ctx, card.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// issueChecksCard starts the delivery — after one stop recheck, because
// everything from here on moves without a human.
func issueChecksCard(ctx context.Context, config runtime.Config, services *runtime.Services, hermes *runtime.Hermes, run state.RunOverview, runDir string, logger Logger) error {
	stopped, err := stopRequested(ctx, services.Backlog, config.Tracker.AllowedCreatorID, run.IssueID)
	if err != nil {
		return nil // fail closed: try again next tick
	}
	if stopped {
		content := hook.DeliverStagingContent(run.RunID, hook.DeliverStagingReport{Verdict: "stopped"})
		if services.Tick.PostStagingReport(ctx, run.RunID, run.DeliveryID, content, nil) {
			sealBoardOutcome(runDir, "staging", "stopped", "")
		}
		return nil
	}
	_, err = hermes.CreateTask(ctx, runtime.CardSpec{
		Title:             fmt.Sprintf("%s deliver: CI 完了待ち", run.RunID),
		Body:              fmt.Sprintf("納品 PR の自動検査 (CI) の完了を待ちます。\nDelivery: %s\nTicket: %s", run.DeliveryID, run.RunID),
		Assignee:          config.Chain.Deliver.ChecksProfile,
		IdempotencyKey:    deliverCardKey(run.DeliveryID, "checks"),
		Workspace:         "dir:" + runDir,
		MaxRuntimeSeconds: config.Chain.Deliver.ChecksWallSeconds(),
		CreatedBy:         "lassdas-attendant",
	})
	if err == nil {
		logger.Info("deliver checks card created", "run", run.RunID)
	}
	return err
}

// issueIntegrateCard is the merge decision point: the LAST stop recheck
// before the change reaches staging.
func issueIntegrateCard(ctx context.Context, config runtime.Config, services *runtime.Services, hermes *runtime.Hermes, run state.RunOverview, runDir string, cards map[string]*runtime.BoardTask, logger Logger) error {
	stopped, err := stopRequested(ctx, services.Backlog, config.Tracker.AllowedCreatorID, run.IssueID)
	if err != nil {
		return nil // fail closed: the merge waits for a readable answer
	}
	if stopped {
		content := hook.DeliverStagingContent(run.RunID, hook.DeliverStagingReport{Verdict: "stopped"})
		if !services.Tick.PostStagingReport(ctx, run.RunID, run.DeliveryID, content, nil) {
			return nil
		}
		sealBoardOutcome(runDir, "staging", "stopped", "")
		return sweepDeliverCards(ctx, hermes, cards)
	}
	_, err = hermes.CreateTask(ctx, runtime.CardSpec{
		Title:             fmt.Sprintf("%s deliver: ステージング反映+確認", run.RunID),
		Body:              fmt.Sprintf("ステージングへの自動マージ → デプロイ完了待ち → 画面の封印付き確認、の順で進めます。\nDelivery: %s\nTicket: %s", run.DeliveryID, run.RunID),
		Assignee:          config.Chain.Deliver.IntegrateProfile,
		IdempotencyKey:    deliverCardKey(run.DeliveryID, "integrate"),
		Workspace:         "dir:" + runDir,
		MaxRuntimeSeconds: config.Chain.Deliver.IntegrateWallSeconds(),
		CreatedBy:         "lassdas-attendant",
	})
	if err == nil {
		logger.Info("deliver integrate card created", "run", run.RunID)
	}
	return err
}

// advanceTowardsPromotion runs while the staging report is on the ticket
// and no promote card exists: enforce the Go deadline, detect the Go, and
// issue the promote card.
func advanceTowardsPromotion(ctx context.Context, config runtime.Config, services *runtime.Services, hermes *runtime.Hermes, run state.RunOverview, runDir string, logger Logger) error {
	report, err := readDeliverReport(runDir, runner.DeliverStagingReportFile)
	if err != nil || report.Verdict != "pass" || report.PromotionHold != "" {
		return nil // failed — or promotion-held — staging reports are terminal
	}
	if time.Now().After(report.ObservedAt.Add(config.Chain.Deliver.GoWait())) {
		content := hook.DeliverReleaseContent(run.RunID, hook.DeliverReleaseReport{Verdict: "expired"})
		if services.Tick.PostReleaseReport(ctx, run.RunID, run.DeliveryID, content, nil) {
			sealBoardOutcome(runDir, "release", "expired", "")
		}
		return nil
	}
	comments, err := services.Backlog.ListComments(ctx, run.IssueID, 0)
	if err != nil {
		return nil
	}
	marker := hook.CommentMarker(string(hook.RunCommentStagingReport), run.RunID)
	reportCommentID, found := commentIDWithMarker(comments, marker)
	if !found {
		return nil // the report is not visible yet; fail closed
	}
	if !containsGoComment(comments, config.Tracker.AllowedCreatorID, reportCommentID) {
		return nil
	}
	_, err = hermes.CreateTask(ctx, runtime.CardSpec{
		Title:             fmt.Sprintf("%s deliver: 本番反映", run.RunID),
		Body:              fmt.Sprintf("Go を受けて本番反映します: 昇格 PR 作成 → マージ → 本番デプロイ完了待ち → 本番画面の封印付き確認。\nDelivery: %s\nTicket: %s", run.DeliveryID, run.RunID),
		Assignee:          config.Chain.Deliver.PromoteProfile,
		IdempotencyKey:    deliverCardKey(run.DeliveryID, "promote"),
		Workspace:         "dir:" + runDir,
		MaxRuntimeSeconds: config.Chain.Deliver.PromoteWallSeconds(),
		CreatedBy:         "lassdas-attendant",
	})
	if err == nil {
		logger.Info("deliver promote card created (Go observed)", "run", run.RunID)
	}
	return err
}

// commentIDWithMarker finds the NEWEST comment carrying exactly the given
// marker (the listing is ascending). Taking the newest means a
// marker-shaped line a requester wrote themselves can only RAISE the bar a
// Go must clear, never lower it below the real report.
func commentIDWithMarker(comments []hook.BacklogComment, marker string) (int64, bool) {
	id, found := int64(0), false
	for _, comment := range comments {
		if hook.ExtractCommentMarker(comment.Body) == marker {
			id, found = comment.CommentID, true
		}
	}
	return id, found
}

// containsGoComment mirrors the stop rule: the requester's own comment,
// posted AFTER the staging report, whose first non-blank line is exactly
// "Go". Approving without seeing the evidence is not a thing.
func containsGoComment(comments []hook.BacklogComment, allowedCreatorID, afterCommentID int64) bool {
	for _, comment := range comments {
		if comment.UserID != allowedCreatorID || comment.CommentID <= afterCommentID {
			continue
		}
		for _, line := range strings.Split(comment.Body, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			if trimmed == "Go" {
				return true
			}
			break
		}
	}
	return false
}

func readDeliverReport(runDir, name string) (runner.DeliverReport, error) {
	var report runner.DeliverReport
	raw, err := os.ReadFile(filepath.Join(runDir, name))
	if err != nil || len(raw) > 1<<20 {
		return report, fmt.Errorf("deliver report unreadable")
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		return report, err
	}
	return report, nil
}

// reportDeliverStaging posts the staging summary: screenshot first
// (best-effort), the promotion preview, the Go instructions.
func reportDeliverStaging(ctx context.Context, config runtime.Config, services *runtime.Services, hermes *runtime.Hermes, run state.RunOverview, runDir string, cards map[string]*runtime.BoardTask, logger Logger) error {
	report, err := readDeliverReport(runDir, runner.DeliverStagingReportFile)
	if err != nil {
		return nil
	}
	rendered := hook.DeliverStagingReport{
		Verdict: report.Verdict, TargetURL: report.TargetURL,
		ExpectedText: report.ExpectedText, AbsentText: report.AbsentText,
		Detail:         report.Detail,
		Preview:        deliverPreview(report.Delta),
		GoDeadlineDays: int(config.Chain.Deliver.GoWait().Hours() / 24),
		PromotionHold:  report.PromotionHold,
		ScreenChecked:  report.ScreenChecked,
	}
	attachments := deliverScreenshotAttachment(ctx, services, run, runDir, runner.DeliverStagingShotFile, "stg-"+run.IssueKey+".png", &rendered.ScreenshotAttached, logger)
	content := hook.DeliverStagingContent(run.RunID, rendered)
	if !services.Tick.PostStagingReport(ctx, run.RunID, run.DeliveryID, content, attachments) {
		return nil
	}
	logger.Info("deliver staging report posted", "run", run.RunID, "verdict", report.Verdict)
	sealBoardOutcome(runDir, "staging", report.Verdict, report.PromotionHold)
	// Cards up to integrate are done either way; the promote card does not
	// exist yet.
	return sweepDeliverCards(ctx, hermes, cards)
}

// reportDeliverRelease posts the final production summary.
func reportDeliverRelease(ctx context.Context, services *runtime.Services, hermes *runtime.Hermes, run state.RunOverview, runDir string, cards map[string]*runtime.BoardTask, logger Logger) error {
	report, err := readDeliverReport(runDir, runner.DeliverProductionReportFile)
	if err != nil {
		return nil
	}
	rendered := hook.DeliverReleaseReport{
		Verdict: report.Verdict, TargetURL: report.TargetURL,
		PullRequestURL: report.PullRequestURL, Detail: report.Detail,
	}
	attachments := deliverScreenshotAttachment(ctx, services, run, runDir, runner.DeliverProductionShotFile, "prod-"+run.IssueKey+".png", &rendered.ScreenshotAttached, logger)
	content := hook.DeliverReleaseContent(run.RunID, rendered)
	if !services.Tick.PostReleaseReport(ctx, run.RunID, run.DeliveryID, content, attachments) {
		return nil
	}
	logger.Info("deliver release report posted", "run", run.RunID, "verdict", report.Verdict)
	sealBoardOutcome(runDir, "release", report.Verdict, "")
	return sweepDeliverCards(ctx, hermes, cards)
}

// reportDeadIntegrate handles an integrate card that stopped without a
// report: re-stat first (the card can finish between checks), then report
// honestly — merge state included, because "nothing happened" would be a
// lie once the branch moved.
func reportDeadIntegrate(ctx context.Context, services *runtime.Services, hermes *runtime.Hermes, run state.RunOverview, runDir string, cards map[string]*runtime.BoardTask, logger Logger) error {
	if deliverFileExists(runDir, runner.DeliverStagingReportFile) {
		return nil // picked up next tick via the normal path
	}
	detail := "ステージング反映の工程カードが結果を残さず終了しました。"
	if deliverFileExists(runDir, runner.DeliverMergeFile) {
		detail = "ステージングへのマージは完了していますが、その後の工程カードが結果を残さず終了しました。"
	}
	content := hook.DeliverStagingContent(run.RunID, hook.DeliverStagingReport{Verdict: "card_failed", Detail: detail})
	if !services.Tick.PostStagingReport(ctx, run.RunID, run.DeliveryID, content, nil) {
		return nil
	}
	sealBoardOutcome(runDir, "staging", "card_failed", detail)
	return sweepDeliverCards(ctx, hermes, cards)
}

// reportDeadPromote handles a promote card that stopped without a report.
// The surviving artifacts decide what the ticket is told, in three honest
// states: the merge landed (reflection or merge artifact exists), the
// promotion PR never got made ("unchanged" is provable), or in between —
// where only "unknown" is true.
func reportDeadPromote(ctx context.Context, services *runtime.Services, hermes *runtime.Hermes, run state.RunOverview, runDir string, cards map[string]*runtime.BoardTask, logger Logger) error {
	if deliverFileExists(runDir, runner.DeliverProductionReportFile) {
		return nil
	}
	var report hook.DeliverReleaseReport
	switch {
	case deliverFileExists(runDir, runner.DeliverReflectionFile) || deliverFileExists(runDir, runner.DeliverPromotionMergeFile):
		report = hook.DeliverReleaseReport{Verdict: "deploy_failed",
			Detail: "本番ブランチへの反映後、工程カードが結果を残さず終了しました。デプロイと画面の状態は手動確認が必要です。"}
	case deliverFileExists(runDir, runner.DeliverPromotionFile):
		report = hook.DeliverReleaseReport{Verdict: "merge_unverified",
			Detail: "本番反映の途中で工程カードが結果を残さず終了しました。本番に反映されたかどうかは確認できていません。"}
	default:
		report = hook.DeliverReleaseReport{Verdict: "card_failed",
			Detail: "本番反映の工程カードが、反映を開始する前に結果を残さず終了しました。本番ブランチは未変更です。"}
	}
	content := hook.DeliverReleaseContent(run.RunID, report)
	if !services.Tick.PostReleaseReport(ctx, run.RunID, run.DeliveryID, content, nil) {
		return nil
	}
	sealBoardOutcome(runDir, "release", report.Verdict, report.Detail)
	return sweepDeliverCards(ctx, hermes, cards)
}

// deliverScreenshotAttachment uploads the phase screenshot (best-effort).
func deliverScreenshotAttachment(ctx context.Context, services *runtime.Services, run state.RunOverview, runDir, shotFile, uploadName string, attached *bool, logger Logger) []int64 {
	png, err := os.ReadFile(filepath.Join(runDir, shotFile))
	if err != nil {
		return nil
	}
	id, err := services.Backlog.UploadAttachment(ctx, uploadName, png)
	if err != nil {
		logger.Error("deliver screenshot upload failed; reporting without it", "run", run.RunID, "error", err.Error())
		return nil
	}
	*attached = true
	return []int64{id}
}

// deliverPreview renders the promotion delta for the report.
func deliverPreview(raw json.RawMessage) hook.PromotionPreview {
	if len(raw) == 0 {
		return hook.PromotionPreview{Unavailable: true}
	}
	var delta struct {
		Status  string `json:"status"`
		AheadBy int    `json:"ahead_by"`
		Commits []struct {
			Title string `json:"title"`
		} `json:"commits"`
		CommitsTruncated bool `json:"commits_truncated"`
	}
	if err := json.Unmarshal(raw, &delta); err != nil || delta.Status == "unavailable" || delta.Status == "" {
		return hook.PromotionPreview{Unavailable: true}
	}
	preview := hook.PromotionPreview{
		AheadBy: delta.AheadBy, Truncated: delta.CommitsTruncated,
		Behind: delta.Status == "behind" || delta.Status == "diverged",
	}
	const maxTitles = 20
	for index, commit := range delta.Commits {
		if index >= maxTitles {
			preview.Truncated = true
			break
		}
		preview.Titles = append(preview.Titles, commit.Title)
	}
	return preview
}
