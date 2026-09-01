package attendant

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
)

// The debug role, attendant side. The observation card itself only writes
// artifacts; everything that touches the ledger or the ticket — creating
// the card, picking the sealed result up, uploading the screenshot,
// posting the comment — happens here, on the single writer's own tick.
// The card's idempotency key lives OUTSIDE the chain namespace
// (ParseChainCardKey refuses it), so the five-stage machinery never sees it.

func e2eCardKey(deliveryID string) string { return deliveryID + ":e2e" }

// e2eObservable is the debug role's gate: the role is on, the run succeeded,
// and the run was claimed after the operator's cut-off. The ledger keeps
// every past success, so the cut-off (validated at load time) is what stops
// enabling the role from reaching back and commenting on long-closed
// tickets. Fails closed on anything unparsable.
func e2eObservable(chain runtime.ChainConfig, run state.RunOverview) bool {
	if chain.E2EProfile == "" || run.TerminalCode != string(hook.TerminalSuccess) {
		return false
	}
	enabledAfter, err := chain.E2EEnabledAfterTime()
	if err != nil || run.ClaimedAt <= 0 || run.ClaimedAt < enabledAfter.UnixMilli() {
		return false
	}
	return true
}

// syncE2E advances one terminal run's observation by exactly one step per
// tick: post the sealed result, or report a died card honestly, or create
// the card — never more than one of those.
func syncE2E(
	ctx context.Context,
	config runtime.Config,
	services *runtime.Services,
	hermes *runtime.Hermes,
	run state.RunOverview,
	tasks []runtime.BoardTask,
	logger Logger,
) error {
	if !e2eObservable(config.Chain, run) {
		return nil
	}
	runDir := runDirectory(config, run.DeliveryID)
	if _, err := os.Stat(filepath.Join(runDir, "feature-pr.json")); err != nil {
		return nil
	}
	posted, err := services.Tick.E2ECommentPosted(ctx, run.RunID)
	if err != nil {
		return err
	}
	var card *runtime.BoardTask
	key := e2eCardKey(run.DeliveryID)
	for index := range tasks {
		if tasks[index].IdempotencyKey == key {
			card = &tasks[index]
			break
		}
	}
	if posted {
		// The observation is on the ticket; only the card remains to sweep.
		if card != nil && card.Status != "archived" {
			return hermes.Archive(ctx, card.ID)
		}
		return nil
	}
	if _, err := os.Stat(filepath.Join(runDir, runner.E2EResultFile)); err == nil {
		return reportE2EResult(ctx, services, hermes, run, runDir, card, logger)
	}
	if card == nil {
		title := fmt.Sprintf("%s e2e", run.RunID)
		if run.Summary != "" {
			title = fmt.Sprintf("%s e2e: %s", run.RunID, run.Summary)
		}
		_, err := hermes.CreateTask(ctx, runtime.CardSpec{
			Title: title,
			Body: fmt.Sprintf(
				"マージ後のステージング確認（デバッグ役）。人のマージ → ステージング反映の成功 → 画面の自動確認、の順で待機します。\nDelivery: %s\nTicket: %s",
				run.DeliveryID, run.RunID,
			),
			Assignee:          config.Chain.E2EProfile,
			IdempotencyKey:    key,
			Workspace:         "dir:" + runDir,
			MaxRuntimeSeconds: config.Chain.E2EWallSeconds(),
			CreatedBy:         "lassdas-attendant",
		})
		if err == nil {
			logger.Info("e2e card created", "run", run.RunID)
		}
		return err
	}
	switch card.Status {
	case "blocked", "done", "archived":
		// The card stopped without sealing a result. Re-stat first — a card
		// can finish between this tick's result check and here — then report
		// the honest unknown and sweep it: silence would read as "still
		// checking" forever.
		if _, err := os.Stat(filepath.Join(runDir, runner.E2EResultFile)); err == nil {
			return reportE2EResult(ctx, services, hermes, run, runDir, card, logger)
		}
		content := hook.E2ECommentContent(run.RunID, hook.E2EReport{
			Verdict: "unknown",
			Detail:  "確認カードが結果を残さず終了しました（実行上限または実行環境の問題）。必要なら手動でご確認ください。",
		})
		if !services.Tick.PostE2EComment(ctx, run.RunID, run.DeliveryID, content, nil) {
			return nil
		}
		sealBoardOutcome(runDir, "e2e", "unknown", "")
		if card.Status != "archived" {
			return hermes.Archive(ctx, card.ID)
		}
	}
	return nil
}

// reportE2EResult turns the sealed observation into the requester comment:
// screenshot first (best-effort), then the exactly-once comment, then the
// card sweep.
func reportE2EResult(
	ctx context.Context,
	services *runtime.Services,
	hermes *runtime.Hermes,
	run state.RunOverview,
	runDir string,
	card *runtime.BoardTask,
	logger Logger,
) error {
	raw, err := os.ReadFile(filepath.Join(runDir, runner.E2EResultFile))
	if err != nil || len(raw) > 1<<20 {
		return nil
	}
	var result runner.E2EResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil
	}
	report := hook.E2EReport{
		Verdict: result.Verdict, TargetURL: result.TargetURL,
		ExpectedText: result.ExpectedText, ExpectedSeen: result.ExpectedSeen,
		AbsentText: result.AbsentText, AbsentGone: result.AbsentGone,
		Detail: result.Detail,
	}
	var attachments []int64
	if result.Screenshot {
		if png, err := os.ReadFile(filepath.Join(runDir, runner.E2EScreenshotFile)); err == nil {
			if id, err := services.Backlog.UploadAttachment(ctx, "e2e-"+run.IssueKey+".png", png); err == nil {
				attachments = append(attachments, id)
				report.ScreenshotAttached = true
			} else {
				logger.Error("e2e screenshot upload failed; reporting without it", "run", run.RunID, "error", err.Error())
			}
		}
	}
	content := hook.E2ECommentContent(run.RunID, report)
	if !services.Tick.PostE2EComment(ctx, run.RunID, run.DeliveryID, content, attachments) {
		return nil
	}
	sealBoardOutcome(runDir, "e2e", result.Verdict, "")
	logger.Info("e2e result reported", "run", run.RunID, "verdict", result.Verdict)
	if card != nil && card.Status != "archived" {
		return hermes.Archive(ctx, card.ID)
	}
	return nil
}
