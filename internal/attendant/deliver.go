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

// deliverCardKeyAt names the card of one attempt at a phase. The first
// attempt keeps the original key, so a delivery already in flight when this
// engine started still finds its cards; later attempts carry their number,
// because the board keeps archived cards in its listing and a key the
// ladder reused would find the retired card first.
func deliverCardKeyAt(deliveryID, stage string, attempt int) string {
	if attempt <= 1 {
		return deliverCardKey(deliveryID, stage)
	}
	return fmt.Sprintf("%s:deliver:%s:a%d", deliveryID, stage, attempt)
}

// deliverObservable is the tail gate: a delivery that ended in success,
// fully configured, claimed after the operator's cut-off. Fails closed on
// anything unparsable — enabling the feature must never reach back through
// the ledger's past successes.
func deliverObservable(chain runtime.ChainConfig, run state.RunOverview) bool {
	return run.TerminalCode == string(hook.TerminalSuccess) && deliverConfigured(chain, run)
}

// syncDeliver is what is left to do for a delivery that has already been
// reported.
//
// Everything that moves the change — the CI wait, the merge, the staging
// observation, the promotion — happens while the run is still claimed now
// (deliver_depth.go), because a delivery cannot be called a success before
// it has been carried out. What remains after the report is what only a
// person's later answer can settle: an outcome that asked an operator to
// look waits for their 「確認済み」, a Go that arrived after the wait had
// expired is answered, and the cards are retired.
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
	cards := deliverCards(runDir, run.DeliveryID, tasks)
	// The ticket's board phase follows the run's end, whichever tick
	// posted it (board_phase.go).
	projectDeliveryEnd(ctx, config, services, run, tasks, logger)

	releasePosted, err := services.Tick.ReleaseReportPosted(ctx, run.RunID)
	if err != nil {
		return err
	}
	if releasePosted {
		// A release report that asked an operator to look waits here for
		// the operator's 「確認済み」; everything else is already over.
		if verdict, since, waits := releaseAttentionVerdict(runDir); waits {
			resolveAttention(ctx, config.Tracker, services.Backlog, run, runDir, "release", verdict, since, logger)
		}
		// A Go after the wait expired is answered, not acted on.
		noticeLateGo(ctx, config, services.Backlog, run, runDir, logger)
	}
	if report, err := readDeliverReport(runDir, runner.DeliverStagingReportFile); err == nil && attentionVerdict(report.Verdict) {
		resolveAttention(ctx, config.Tracker, services.Backlog, run, runDir, "staging", report.Verdict, report.ObservedAt, logger)
	}
	return sweepDeliverCards(ctx, hermes, cards)
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
func issueChecksCard(ctx context.Context, config runtime.Config, services *runtime.Services, hermes *runtime.Hermes, run state.RunOverview, runDir string, logger Logger) (deliverProgress, error) {
	stopped, err := stopRequested(ctx, services.Backlog, config.Tracker.AllowedCreatorID, run.IssueID)
	if err != nil {
		return deliverWorking, nil // fail closed: try again next tick
	}
	if stopped {
		content := hook.DeliverStagingContent(run.RunID, hook.DeliverStagingReport{Verdict: "stopped"})
		if services.Tick.PostStagingReport(ctx, run.RunID, run.DeliveryID, content, nil) {
			sealBoardOutcome(runDir, "staging", "stopped", "")
		}
		return deliverStopped, nil
	}
	_, err = hermes.CreateTask(ctx, runtime.CardSpec{
		Title:             fmt.Sprintf("%s deliver: CI 完了待ち", run.RunID),
		Body:              fmt.Sprintf("納品 PR の自動検査 (CI) の完了を待ちます。\nDelivery: %s\nTicket: %s", run.DeliveryID, run.RunID),
		Assignee:          config.Chain.Deliver.ChecksProfile,
		IdempotencyKey:    deliverCardKeyAt(run.DeliveryID, "checks", deliverAttempt(runDir, "checks")),
		Workspace:         "dir:" + runDir,
		MaxRuntimeSeconds: config.Chain.Deliver.ChecksWallSeconds(),
		CreatedBy:         "lassdas-attendant",
	})
	if err == nil {
		logger.Info("deliver checks card created", "run", run.RunID)
	}
	return deliverWorking, err
}

// issueIntegrateCard is the merge decision point: the LAST stop recheck
// before the change reaches staging.
func issueIntegrateCard(ctx context.Context, config runtime.Config, services *runtime.Services, hermes *runtime.Hermes, run state.RunOverview, runDir string, cards map[string]*runtime.BoardTask, logger Logger) (deliverProgress, error) {
	stopped, err := stopRequested(ctx, services.Backlog, config.Tracker.AllowedCreatorID, run.IssueID)
	if err != nil {
		return deliverWorking, nil // fail closed: the merge waits for a readable answer
	}
	if stopped {
		content := hook.DeliverStagingContent(run.RunID, hook.DeliverStagingReport{Verdict: "stopped"})
		if !services.Tick.PostStagingReport(ctx, run.RunID, run.DeliveryID, content, nil) {
			return deliverWorking, nil
		}
		sealBoardOutcome(runDir, "staging", "stopped", "")
		return deliverStopped, sweepDeliverCards(ctx, hermes, cards)
	}
	_, err = hermes.CreateTask(ctx, runtime.CardSpec{
		Title:             fmt.Sprintf("%s deliver: ステージング反映+確認", run.RunID),
		Body:              fmt.Sprintf("ステージングへの自動マージ → デプロイ完了待ち → 画面の封印付き確認、の順で進めます。\nDelivery: %s\nTicket: %s", run.DeliveryID, run.RunID),
		Assignee:          config.Chain.Deliver.IntegrateProfile,
		IdempotencyKey:    deliverCardKeyAt(run.DeliveryID, "integrate", deliverAttempt(runDir, "integrate")),
		Workspace:         "dir:" + runDir,
		MaxRuntimeSeconds: config.Chain.Deliver.IntegrateWallSeconds(),
		CreatedBy:         "lassdas-attendant",
	})
	if err == nil {
		logger.Info("deliver integrate card created", "run", run.RunID)
	}
	return deliverWorking, err
}

// promotableStagingVerdict is the one staging outcome a promotion may rest
// on. It is a named answer rather than a comparison inside the gate so that
// widening it is a change to something a test holds: every other verdict —
// a deployment that failed, one that never started, a screen that could not
// be judged — means nothing verified the change.
func promotableStagingVerdict(verdict string) bool { return verdict == "pass" }

// advanceTowardsPromotion runs while the staging observation has passed and
// no promote card exists: read the stop, apply the Go gate when the
// operator asked for one, and issue the promote card.
//
// The gate is off by default. A delivery thrown at eleven at night is meant
// to be finished by the morning, and a wait for someone to read a comment
// is the one thing that cannot be; a staging observation that passed is the
// evidence the promotion was ever going to rest on. An operator who wants
// to look before production moves sets go_gate: required, and gets back
// exactly what this did before — the deadline, the reminders, the Go.
func advanceTowardsPromotion(ctx context.Context, config runtime.Config, services *runtime.Services, hermes *runtime.Hermes, run state.RunOverview, runDir string, logger Logger) (deliverProgress, error) {
	report, err := readDeliverReport(runDir, runner.DeliverStagingReportFile)
	if err != nil || !promotableStagingVerdict(report.Verdict) || report.PromotionHold != "" {
		// Nothing here can promote: the caller has already decided this is
		// as deep as the delivery goes.
		return deliverReached, nil
	}
	comments, err := services.Backlog.ListComments(ctx, run.IssueID, 0)
	if err != nil {
		return deliverWorking, nil // fail closed: the promotion waits for a readable answer
	}
	// A stop outranks BOTH the Go and the deadline: the requester holds
	// the veto until the very moment the promote card exists, and a stop
	// that raced the deadline must still read back as "stopped" — that is
	// the requester's actual intent. Before this check nothing consumed a
	// stop during the Go wait — the ticket showed "stopped" nowhere and
	// the rail kept waiting, which is the lie the creed forbids.
	if containsStopComment(comments, config.Tracker.AllowedCreatorID) {
		content := hook.DeliverReleaseContent(run.RunID, hook.DeliverReleaseReport{Verdict: "stopped"})
		if services.Tick.PostReleaseReport(ctx, run.RunID, run.DeliveryID, content, nil) {
			sealBoardOutcome(runDir, "release", "stopped", "")
		}
		return deliverStopped, nil
	}
	if config.Chain.Deliver.GoGateRequired() {
		if time.Now().After(report.ObservedAt.Add(config.Chain.Deliver.GoWait())) {
			content := hook.DeliverReleaseContent(run.RunID, hook.DeliverReleaseReport{Verdict: "expired"})
			if services.Tick.PostReleaseReport(ctx, run.RunID, run.DeliveryID, content, nil) {
				sealBoardOutcome(runDir, "release", "expired", "")
			}
			return deliverReached, nil
		}
		marker := hook.CommentMarker(string(hook.RunCommentStagingReport), run.RunID)
		reportCommentID, found := commentIDWithMarker(comments, marker)
		if !found {
			return deliverWorking, nil // the report is not visible yet; fail closed
		}
		if !containsGoComment(comments, config.Tracker.AllowedCreatorID, reportCommentID) {
			// Still waiting: remind on the questions' weekday rhythm, cut at
			// the deadline the expiry above enforces.
			remindGo(ctx, services.Backlog, run, report.ObservedAt, report.ObservedAt.Add(config.Chain.Deliver.GoWait()), comments, logger)
			return deliverWorking, nil
		}
	}
	_, err = hermes.CreateTask(ctx, runtime.CardSpec{
		Title:             fmt.Sprintf("%s deliver: 本番反映", run.RunID),
		Body:              fmt.Sprintf("Go を受けて本番反映します: 昇格 PR 作成 → マージ → 本番デプロイ完了待ち → 本番画面の封印付き確認。\nDelivery: %s\nTicket: %s", run.DeliveryID, run.RunID),
		Assignee:          config.Chain.Deliver.PromoteProfile,
		IdempotencyKey:    deliverCardKeyAt(run.DeliveryID, "promote", deliverAttempt(runDir, "promote")),
		Workspace:         "dir:" + runDir,
		MaxRuntimeSeconds: config.Chain.Deliver.PromoteWallSeconds(),
		CreatedBy:         "lassdas-attendant",
	})
	if err == nil {
		logger.Info("deliver promote card created", "run", run.RunID, "go_gate", config.Chain.Deliver.GoGate)
	}
	return deliverWorking, err
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
		Verdict: report.Verdict, Block: report.Block, TargetURL: report.TargetURL,
		ExpectedText: report.ExpectedText, AbsentText: report.AbsentText,
		Detail:         report.Detail,
		Preview:        deliverPreview(report.Delta),
		GoDeadlineDays: int(config.Chain.Deliver.GoWait().Hours() / 24),
		PromotionHold:  report.PromotionHold,
		ScreenChecked:  report.ScreenChecked,
		Measurement:    measurementLine(report.Measurement),
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
		Verdict: report.Verdict, Block: report.Block, TargetURL: report.TargetURL,
		PullRequestURL: report.PullRequestURL, Detail: report.Detail,
		Measurement: measurementLine(report.Measurement),
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

// A delivery card that stops without sealing anything is not reported any
// more. It used to be: the integrate card's death was told to the ticket as
// "the staging step left no result", and the promote card's as one of three
// honest guesses about whether production had moved. Both were the end of
// the delivery, and neither was a decision about the request — a pod was
// replaced, or a card ran out of wall clock. The ladder takes them instead,
// and the card is dispatched again; the verb resumes past every step whose
// record already exists, so a promotion whose merge landed is observed
// rather than attempted a second time (deliver_depth.go).

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

// measurementLine turns the sealed measurement check into the requester's
// line, or nil when the design made no measurement promise.
func measurementLine(check *runner.MeasurementCheck) *hook.MeasurementLine {
	if check == nil {
		return nil
	}
	return &hook.MeasurementLine{Probe: check.Probe, Metric: check.Metric, Threshold: check.Threshold, Value: check.Value, Pass: check.Pass, Detail: check.Detail}
}
