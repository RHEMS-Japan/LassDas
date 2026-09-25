package attendant

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
	"automation.internal/ticket-ingress/internal/ticketview"
)

// The status board is a pure observation: every tick the attendant writes
// what it can already see (ledger rows, board cards, sealed artifacts) into
// one snapshot file, and appends one event line whenever a delivery's step
// changed since the previous snapshot. Nothing reads these files to make
// decisions — they exist so a human can watch the pipeline live instead of
// waiting for the next tracker comment.

// BoardSnapshot is the whole board at one instant.
type BoardSnapshot struct {
	SchemaVersion int         `json:"schema_version"`
	GeneratedAt   time.Time   `json:"generated_at"`
	Runs          []RunStatus `json:"runs"`
	// Stages are the rail this installation can actually reach, in order.
	// The board used to draw a fixed nine, so a destination that stops at
	// the pull request showed STG, 確認 and 本番 for every delivery and
	// never lit them: a requester read three grey stages after the last one
	// that moved and had no way to tell "not yet" from "never" (observed
	// 2026-09-18).
	Stages []BoardStage `json:"stages"`
	// Notice is the one-line banner shown while intake is held (the same
	// failure ended the last N deliveries); empty otherwise.
	Notice string `json:"notice,omitempty"`
}

// RunStatus is one delivery's position in the pipeline, in requester terms.
type RunStatus struct {
	Running    *ticketview.RunningStep `json:"running,omitempty"`
	DeliveryID string                  `json:"delivery_id"`
	IssueID    int64                   `json:"issue_id,omitempty"`
	IssueKey   string                  `json:"issue_key,omitempty"`
	Summary    string                  `json:"summary,omitempty"`
	State      string                  `json:"state"`
	// Step is one of the pipeline steps (intake, implement, review, checks,
	// staging, confirm, production) or a resting state (question, done,
	// stopped, failed).
	Step      string `json:"step"`
	StepTitle string `json:"step_title"`
	Detail    string `json:"detail,omitempty"`
	Round     int    `json:"round,omitempty"`
	ClaimedAt int64  `json:"claimed_at_ms,omitempty"`
	Terminal  string `json:"terminal_code,omitempty"`
	// CanGo marks the one state where a Go is guaranteed to be honoured:
	// the staging report comment is CONFIRMED posted (sealed outcome, not
	// merely the local report file) and the promotion is not held. The
	// board shows its Go button only here.
	CanGo bool `json:"can_go,omitempty"`
	// CanResolve exposes the existing posted-report acknowledgement window.
	// It never authorizes a deployment or a direct run-state change.
	CanResolve   bool      `json:"can_resolve,omitempty"`
	NextAction   string    `json:"next_action,omitempty"`
	ActionEffect string    `json:"action_effect,omitempty"`
	PRURL        string    `json:"pr_url,omitempty"`
	ChangedFiles []string  `json:"changed_files,omitempty"`
	ReportAt     time.Time `json:"report_at,omitempty"`
	// Stage names the pipeline step an out-of-line state (attention)
	// belongs to, so the board lights the node where the run stopped
	// instead of an empty rail. Empty for every in-line step.
	Stage string `json:"stage,omitempty"`
}

// StepEvent is one appended line of events.jsonl: a delivery moved.
type StepEvent struct {
	At         time.Time `json:"at"`
	DeliveryID string    `json:"delivery_id"`
	IssueKey   string    `json:"issue_key,omitempty"`
	Step       string    `json:"step"`
	StepTitle  string    `json:"step_title"`
	Detail     string    `json:"detail,omitempty"`
}

// terminalSnapshotLimit keeps the snapshot from growing without bound: the
// resting runs (done/stopped/failed) beyond the most recent N stay in the
// ledger and in events.jsonl, just not in the live board.
const terminalSnapshotLimit = 30

// SnapshotStatus assembles the board from what the attendant already reads
// every tick. Read-only everywhere: ledger scan, board listing, artifact
// stat/reads.
func SnapshotStatus(ctx context.Context, config runtime.Config, services *runtime.Services, hermes *runtime.Hermes) (BoardSnapshot, error) {
	runs, err := services.Store.ScanRuns(ctx)
	if err != nil {
		return BoardSnapshot{}, err
	}
	tasks, err := hermes.ListBoardTasks(ctx)
	if err != nil {
		return BoardSnapshot{}, err
	}
	// Trim BEFORE classifying: classification does per-run file I/O, and
	// the ledger only grows. Terminal runs beyond twice the display limit
	// (newest first) cannot appear on the board — resting ones are capped
	// at the limit, and a terminal run still moving (delivery continuation)
	// is claimed recently. Non-terminal runs always classify.
	sort.SliceStable(runs, func(a, b int) bool { return runs[a].ClaimedAt > runs[b].ClaimedAt })
	terminalSeen := 0
	candidates := runs[:0]
	for _, run := range runs {
		if run.State == "terminal" {
			if terminalSeen++; terminalSeen > 2*terminalSnapshotLimit {
				continue
			}
		}
		candidates = append(candidates, run)
	}
	snapshot := BoardSnapshot{SchemaVersion: 1, GeneratedAt: time.Now().UTC(), Stages: railStages(config)}
	for _, run := range candidates {
		snapshot.Runs = append(snapshot.Runs, classifyRun(config, run, tasks))
	}
	kept := snapshot.Runs[:0]
	resting := 0
	for _, run := range snapshot.Runs {
		if run.Step == "done" || run.Step == "stopped" || run.Step == "failed" {
			resting++
			if resting > terminalSnapshotLimit {
				continue
			}
		}
		kept = append(kept, run)
	}
	snapshot.Runs = kept
	snapshot.Notice = intakeNotice(config)
	return snapshot, nil
}

// intakeNotice is the board's banner while intake is held, which now has
// one cause: the operator's pause. The banner that said several deliveries
// had ended the same way is gone with the hold it described.
func intakeNotice(config runtime.Config) string {
	if since, paused := config.Chain.IntakePaused(); paused {
		return intakePausedNotice(since)
	}
	return ""
}

// intakePausedNotice is the board's banner while the operator's pause is
// in force: what is stopped, since when, and what keeps going.
func intakePausedNotice(since time.Time) string {
	return "受付停止中: 運用者の指示で新しい依頼の開始を止めています（停止: " + since.In(hook.DisplayZone()).Format("2006-01-02 15:04") + "）。実行中の依頼はそのまま進みます。再開は設定の intake_paused_since を外します。"
}

func classifyRun(config runtime.Config, run state.RunOverview, tasks []runtime.BoardTask) RunStatus {
	status := RunStatus{
		DeliveryID: run.DeliveryID, IssueID: run.IssueID, IssueKey: run.IssueKey, Summary: run.Summary,
		State: run.State, ClaimedAt: run.ClaimedAt, Terminal: run.TerminalCode,
	}
	runDir := runDirectory(config, run.DeliveryID)
	switch run.State {
	case "queued":
		// The pause outranks a budget or login hold left in the run
		// directory: while paused nothing rechecks those, so their
		// "resumes automatically" line would be false.
		if since, paused := config.Chain.IntakePaused(); paused {
			status.place("intake", "受付停止中", "運用者の指示で新しい依頼の開始を止めています（停止: "+since.In(hook.DisplayZone()).Format("2006-01-02 15:04")+"）。再開後に開始します")
			break
		}
		if placeIntakeHold(&status, runDir) {
			break
		}
		status.place("intake", "受付待ち", "")
	case "awaiting_answer":
		status.place("question", "質問への回答待ち", "依頼者の返信を待っています")
		status.NextAction = "依頼者がチケットの最新の質問を開き、そこに記載された回答例に沿ってコメントしてください。質問が 1 問だけのときは、その選択肢の記号だけでも受け付けます。"
		// The other way out, which the board never said: a requester who
		// does not want to answer - because the question's premise is wrong,
		// or because they would rather re-file - had nothing to do but wait
		// for the deadline, days away (reported live 2026-09-17).
		//
		// The round is named from the run's own sealed question, never from
		// a literal: only the round being asked about is accepted, so a
		// board naming another one would send the requester to write a
		// comment nobody reads (review of #197).
		status.ActionEffect = "回答を受け取ると、追加の調査・設計または実装へ進みます。回答まで作業は再開しません。" + withdrawSentence(run) + "取り下げると、変更を加えずにこの依頼を終了します。"
	case "claimed":
		if placeIntakeHold(&status, runDir) {
			break
		}
		classifyClaimed(&status, run, tasks)
		if status.Step != "attention" {
			// The cards already supply an authoritative coarse stage; the
			// running step only adds which step of it is under way.
			status.Running = ticketview.ReadRunningStep(runDir)
		}
	case "terminal":
		classifyAfterTerminalInDirectory(&status, config, run, tasks, runDir)
	case "terminal_report_pending", "question_report_pending":
		status.place("reporting", "報告を作成中", "チケットへの報告を準備しています")
	default:
		// Anything the vocabulary gains later; never leak internal state
		// names into the requester-facing detail.
		status.place("intake", "処理中", "")
		status.NextAction = "現在の処理状態をこの画面では判定できません。運用担当者がチケットの報告と実行履歴を確認してください。"
		status.ActionEffect = "状態を確認するまで、この画面からの操作はありません。"
	}
	if status.NextAction == "" {
		switch status.Step {
		case "failed":
			status.NextAction = "運用担当者がチケットの最終報告と終了した工程の実行履歴を確認し、原因と残る対応をチケットに記録してください。"
			status.ActionEffect = "この試行の自動処理は終了しています。原因を修正しても自動では再実行しません。再開方法は運用担当者がチケットで案内します。"
		case "stopped":
			status.NextAction = "この試行への操作は不要です。再度依頼する場合は、停止した理由と既に反映された範囲をチケットで確認してください。"
			status.ActionEffect = "この試行は自動で再開しません。停止の記録によって、既に取り込まれた変更が取り消されることはありません。"
		case "done":
			status.NextAction = "この試行の自動処理は終了しています。チケットの最終報告と、PR がある場合はその変更内容を確認できます。"
		case "intake", "investigate", "design", "implement", "review", "checks", "staging", "production", "reporting", "confirm":
			status.NextAction = "いまは利用者の操作は不要です。進行状況はこの画面で確認できます。"
			status.ActionEffect = "工程の結果はこの画面に表示します。質問や承認が必要になった場合は、チケットで具体的な操作をお知らせします。"
		}
	}
	return status
}

func (s *RunStatus) place(step, title, detail string) {
	s.Step, s.StepTitle, s.Detail = step, title, detail
}

// placeIntakeHold shows the hold that keeps a run off the rail before its
// reception — money first, the observation browser's login second — and
// reports whether one is in force.
func placeIntakeHold(status *RunStatus, runDir string) bool {
	if hold, held := readBudgetHold(runDir); held {
		placeBudgetHold(status, hold)
		status.NextAction = "運用担当者が表示された役のモデル利用枠と残高を確認し、必要な利用枠を確保してください。依頼者の再起票は不要です。"
		status.ActionEffect = "利用枠の確認に通ると、同じ依頼の受付を自動で再開します。"
		return true
	}
	if hold, held := readSessionHold(runDir); held {
		placeSessionHold(status, hold)
		status.NextAction = "運用担当者が表示された納品先の確認用アカウントでログインし直し、既存の手順で確認用のログイン情報を更新してください。依頼者の再起票は不要です。"
		status.ActionEffect = "ログイン状態の検査に通ると、同じ依頼の受付を自動で再開します。"
		return true
	}
	return false
}

// placeAt is place for an out-of-line step, naming the pipeline stage it
// belongs to.
func (s *RunStatus) placeAt(step, stage, title, detail string) {
	s.place(step, title, detail)
	s.Stage = stage
}

func classifyClaimed(status *RunStatus, run state.RunOverview, tasks []runtime.BoardTask) {
	view := chainViewFor(tasks, run.DeliveryID)
	status.Round = view.round
	if placeDesignStage(status, view) {
		return
	}
	if view.round == 0 {
		status.place("intake", "受付処理中", "作業の準備をしています")
		return
	}
	detail := fmt.Sprintf("%d 巡目", view.round)
	var implementLeft, reviewLeft, validateLeft, publishLeft, blocked, humanLane bool
	humanStage := "implement"
	for stage, card := range view.cards {
		if card.Status == "done" {
			continue
		}
		// triage / scheduled are deliberate human lanes the automation
		// never touches — "in progress" would be a lie there.
		if card.Status == "triage" || card.Status == "scheduled" {
			humanLane = true
			if strings.Contains(stage, "review") {
				humanStage = "review"
			}
		}
		if failedCardStatuses[card.Status] {
			blocked = true
		}
		// Future validation/publication cards already exist while a review
		// runs. Their pending status must not put the display back at implement.
		switch stage {
		case runtime.StageReviewA, runtime.StageReviewB:
			reviewLeft = true
		case runtime.StageImplement, runtime.StageApply:
			implementLeft = true
		case runtime.StageValidate:
			validateLeft = true
		case runtime.StagePublish:
			publishLeft = true
		}
	}
	switch {
	case humanLane:
		status.placeAt("attention", humanStage, "人の対応待ち", detail+"・工程カードが人の確認レーンにあります")
		status.NextAction = "運用担当者がチケットの実行履歴から、確認待ちに移された工程と理由を調べ、工程カードの対応方針を記録してください。"
		status.ActionEffect = "この画面から工程を再開する操作はありません。担当者が実行基盤で対応するまで、この工程の自動処理は進みません。"
	case blocked:
		status.place("implement", "工程の復旧処理中", detail)
	case implementLeft:
		status.place("implement", "実装・検証中", detail)
	case reviewLeft:
		status.place("review", "レビュー中", detail)
	case validateLeft:
		status.place("checks", "変更内容の検査中", detail)
	case publishLeft:
		status.place("reporting", "PR の公開処理中", detail)
	default:
		status.place("implement", "次の工程を準備中", detail)
	}
}

// classifyAfterTerminal reads the delivery continuation (cards, sealed
// reports, and the posted-outcome seal) the same way syncDeliver does,
// but only to name the step. The seal outranks files and cards: several
// endings (expired, stopped, dead cards) exist only as posted comments,
// and without the seal the board would keep telling the previous story.
func classifyAfterTerminal(status *RunStatus, config runtime.Config, run state.RunOverview, tasks []runtime.BoardTask) {
	classifyAfterTerminalInDirectory(status, config, run, tasks, runDirectory(config, run.DeliveryID))
}

func classifyAfterTerminalInDirectory(status *RunStatus, config runtime.Config, run state.RunOverview, tasks []runtime.BoardTask, runDir string) {
	readDeliveryEvidence(status, runDir)
	if run.TerminalCode == string(hook.TerminalInvestigated) {
		status.place("done", "調査報告を掲示して完了", "調査のみの依頼のため、コードの変更と Pull Request はありません")
		return
	}
	if run.TerminalCode != string(hook.TerminalSuccess) {
		if run.TerminalCode == string(hook.TerminalCancelled) {
			status.place("stopped", "停止済み", "ご指示により停止しました")
			return
		}
		if run.TerminalCode == string(hook.TerminalImplementationReturned) {
			// Nothing failed: the implementer answered, and the board saying
			// "ended in failure" over a detail line that says otherwise
			// leaves the operator to work out which half to believe.
			status.place("stopped", "実装役の報告で終了",
				"変更を加えずに理由を報告して作業を返しました。報告の全文はチケットのコメントにあります")
			return
		}
		status.place("failed", "失敗で終了", hook.DescribeTerminalCode(run.TerminalCode))
		evidence := runner.RecordedFailedStep(runDir)
		if run.TerminalCode == string(hook.TerminalModelFailed) && evidence["model_failure_reason"] == hook.ModelFailureBudgetExhausted {
			status.place("failed", "AI の利用枠不足で終了", "モデル利用枠の上限超過が報告されました")
			status.NextAction = hook.BudgetFailureAction
			status.ActionEffect = "この試行は終了しています。利用枠が回復しても自動では再実行しません。運用担当者が再開方法をチケットで案内します。"
		}
		if step := evidence["failed_step"]; step != "" {
			status.Detail += "。終了した工程: " + step
		}
		return
	}
	// An operator's sealed 「確認済み」 outranks the report that asked for it:
	// the delivery is closed for the automation, whatever the report said.
	if resolution, ok := readDeliverResolution(runDir); ok {
		placeResolvedOutcome(status, resolution.Phase, resolution.Verdict)
		if resolution.Phase == "release" {
			status.PRURL = ""
			if report, err := readDeliverReport(runDir, runner.DeliverProductionReportFile); err == nil {
				status.PRURL = report.PullRequestURL
			}
		}
		return
	}
	outcome, sealed := readBoardOutcome(runDir)
	if sealed && outcome.Phase == "release" {
		placeReleaseOutcome(status, outcome.Verdict)
		// The feature PR targets staging; do not present it as the production PR.
		status.PRURL = ""
		if report, err := readDeliverReport(runDir, runner.DeliverProductionReportFile); err == nil {
			status.PRURL = report.PullRequestURL
			if outcome.Verdict == "pass" && report.Verdict == "pass" && report.ScreenChecked {
				status.Detail = "デプロイと本番の画面確認まで合格しました。"
			}
		}
		status.ReportAt = outcome.At
		if attentionVerdict(outcome.Verdict) {
			allowResolution(status, outcome.At)
		}
		return
	}
	// The production report file beats a staging-phase seal: once the
	// promote card wrote it, the staging story ("waiting for Go") is over
	// even while the release post is still on its way.
	if report, err := readDeliverReport(runDir, runner.DeliverProductionReportFile); err == nil {
		placeReleaseOutcome(status, report.Verdict)
		status.PRURL = report.PullRequestURL
		if report.Verdict == "pass" && report.ScreenChecked {
			status.Detail = "デプロイと本番の画面確認まで合格しました。"
		}
		status.ReportAt = report.ObservedAt
		if status.Step == "attention" {
			status.ActionEffect = "チケットへの結果報告の投稿を、この画面では確認できていません。報告が投稿済みで必要な対応も完了していれば、依頼者または登録された運用担当者が、チケットの先頭行に「確認済み」と書いてコメントしてください。"
		}
		return
	}
	if card, ok := deliverCard(tasks, run.DeliveryID, "promote"); ok && !card.archivedOrDone() {
		status.place("production", "本番へ反映中", "")
		return
	}
	if sealed && outcome.Phase == "staging" {
		placeStagingOutcome(status, outcome.Verdict, outcome.Note)
		// Only the posted report arms a Go — the detection anchor is the
		// report COMMENT, so a file-only confirm must not show the button.
		status.CanGo = status.Step == "confirm"
		status.ReportAt = outcome.At
		if report, err := readDeliverReport(runDir, runner.DeliverStagingReportFile); err == nil && report.Verdict == outcome.Verdict {
			status.ReportAt = report.ObservedAt
			if attentionVerdict(report.Verdict) {
				allowResolution(status, report.ObservedAt)
			}
		} else if status.Step == "attention" {
			status.ActionEffect = "確認受付に必要な記録を読み取れず、この画面からは閉じられません。運用担当者がチケットの報告と実行履歴を確認してください。"
		}
		return
	}
	if report, err := readDeliverReport(runDir, runner.DeliverStagingReportFile); err == nil {
		placeStagingOutcome(status, report.Verdict, report.PromotionHold)
		status.ReportAt = report.ObservedAt
		if status.Step == "attention" || status.Step == "confirm" {
			status.ActionEffect = "チケットへの結果報告を確認するまで、この画面からは操作できません。報告後に受け付けられる操作が表示されます。"
		}
		return
	}
	if card, ok := deliverCard(tasks, run.DeliveryID, "integrate"); ok && !card.archivedOrDone() {
		status.place("staging", "ステージングへ反映中", "マージとデプロイの完了を待っています")
		return
	}
	if card, ok := deliverCard(tasks, run.DeliveryID, "checks"); ok && !card.archivedOrDone() {
		status.place("checks", "自動検査 (CI) 待ち", "必要な検査の実行と結果が揃うのを待っています")
		return
	}
	if card, ok := e2eCard(tasks, run.DeliveryID); ok && !card.archivedOrDone() {
		status.place("confirm", "ステージング画面を確認中", "")
		return
	}
	if sealed && outcome.Phase == "e2e" {
		switch outcome.Verdict {
		case "pass":
			status.place("done", "ステージング反映・確認済み", "")
		case "unknown":
			status.placeAt("attention", "confirm", "ステージング画面の確認ができませんでした", "手動での確認が必要です")
			status.NextAction = "運用担当者がチケットに記載された確認先と条件を開き、画面の確認結果と残る対応をチケットに記録してください。"
			status.ActionEffect = "この画面から画面検査を再実行する操作はありません。運用担当者が既存の手順で確認します。"
		default:
			status.place("failed", "ステージング画面の確認が不合格", "")
		}
		return
	}
	// A v2-observable run whose delivery cards are not up yet (issued a
	// tick later, or delayed by a fail-closed stop recheck): "delivered"
	// would read as the end of the road when the rail is about to move.
	if deliverObservable(config.Chain, run) && deliverFileExists(runDir, "feature-pr.json") {
		status.place("checks", "納品後の工程を準備中", "")
		return
	}
	// A delivery that stops at the pull request is not finished: a person
	// has to merge it, and nothing reaches the repository until they do.
	// Calling that "納品済み" moved the card out of 進行中 and collapsed it
	// to two words, so a requester read "delivered" over an unmerged pull
	// request and had no reason to look further (live 2026-09-18, measured
	// against a pull request that was still open).
	if merge, merged := readFeatureMerge(runDir); merged {
		status.place("done", "マージ済み", "取り込み用の Pull Request はマージされ、依頼の変更がリポジトリに入りました")
		if merge.MergeCommitSHA != "" {
			status.Detail += " (" + merge.MergeCommitSHA[:min(7, len(merge.MergeCommitSHA))] + ")"
		}
		return
	}
	if run.TerminalCode == string(hook.TerminalSuccess) &&
		deliverFileExists(runDir, "feature-pr.json") && !config.Chain.Deliver.Enabled() {
		status.place("confirm", "マージ待ち", "取り込み用の Pull Request を人が確認してマージします")
		status.NextAction = "チケットに記載された Pull Request を開いて内容を確認し、問題がなければマージしてください。"
		status.ActionEffect = "マージするまで、依頼の変更はリポジトリに入りません。マージすると、次の巡回でこの依頼は「マージ済み」になります。"
		return
	}
	status.place("done", "納品済み", "")
}

func placeReleaseOutcome(status *RunStatus, verdict string) {
	if attentionVerdict(verdict) {
		placeDeliveryAttention(status, "production", verdict)
		return
	}
	switch verdict {
	case "pass":
		status.place("done", "本番反映済み", "本番反映の完了が報告されています。画面検査の合格を示す記録は確認できていません。")
	case "expired":
		status.place("done", "ステージング反映済み", "Go の期限切れで本番反映なし")
		status.NextAction = "本番反映が必要な場合は、運用担当者が既存のリリース手順で対応してください。"
		status.ActionEffect = "この試行の承認受付は終了しています。ここから Go を追加投稿しても自動では本番へ進みません。"
	case "deploy_not_applicable":
		status.place("done", "本番ブランチへ反映済み・配布対象外", "変更は本番配布の対象範囲の外のため、配布の実行を待たず、画面確認は行いません。")
		status.NextAction = "対応不要です。"
		status.ActionEffect = "この試行の自動処理は終了しています。"
	case "stopped":
		status.place("stopped", "停止済み", "ご指示により本番反映を行わず終了")
	case "observe_failed":
		status.place("done", "本番反映済み・画面は要確認", "デプロイは完了しましたが、画面が確認条件を満たしていません。")
		status.NextAction = "運用担当者がチケットの画面確認結果と期待した表示を比較し、必要な修正を判断してください。"
		status.ActionEffect = "この試行では自動修正や自動ロールバックは行いません。確認結果と対応方針をチケットに記録してください。"
	case "measure_failed":
		status.place("done", "本番反映済み・計測は閾値超過", "設計書が約束した計測値を満たしていません")
		status.NextAction = "運用担当者がチケットの計測対象・観測値・閾値を比較し、影響と必要な対応を確認してください。"
		status.ActionEffect = "この試行では自動修正や自動ロールバックは行いません。確認結果と対応方針をチケットに記録してください。"
	default:
		status.place("failed", "本番反映の工程で停止", "")
	}
}

func placeStagingOutcome(status *RunStatus, verdict, hold string) {
	if attentionVerdict(verdict) {
		placeDeliveryAttention(status, "staging", verdict)
		return
	}
	switch {
	case verdict == "checks_failed":
		status.placeAt("failed", "checks", "自動検査 (CI) の成功を確認できず終了",
			"必要な検査の成功を確認できず、ステージングへの自動取り込み前に終了しました。")
		status.NextAction = "運用担当者が PR の CI 実行履歴を確認してください。実行がなければ変更ファイルと CI の対象条件を照合し、不合格なら実行ログから原因を確認してください。"
		status.ActionEffect = "この試行の自動処理は終了しています。原因を修正しても自動では再開しません。運用担当者が確認結果と再開方法をチケットに記録してください。"
	case verdict == "pass" && hold != "":
		status.place("done", "ステージング反映済み", "本番の自動反映を見送りました: "+hold)
		status.NextAction = "運用担当者がチケットの本番反映を見送った理由と PR の変更範囲を確認し、必要なら既存のリリース手順で本番へ反映してください。"
		status.ActionEffect = "この試行の自動処理は終了しています。ここで Go を投稿しても本番反映は始まりません。"
	case verdict == "pass":
		status.place("confirm", "本番反映の承認待ち", "あなたの「Go」を待っています")
		status.NextAction = "依頼者がチケットのステージング結果と PR の変更内容を確認し、本番へ反映してよいか判断してください。"
		status.ActionEffect = "「本番反映を承認」で本番への反映工程が始まります。「反映しない」でステージングの変更を残したまま、この依頼の自動処理を終了します。"
	case verdict == "deploy_not_applicable":
		status.place("done", "ステージングへマージ済み・配布対象外", "変更は配布処理の対象範囲の外のため、配布の実行を待たず、画面確認と本番反映は行いません。")
		status.NextAction = "対応不要です。配布の対象範囲は設定の deploy_paths で確認できます。"
		status.ActionEffect = "この試行の自動処理は終了しています。ここで Go を投稿しても本番反映は始まりません。"
	case verdict == "stopped":
		status.place("stopped", "停止済み", "ご指示により停止しました")
	case verdict == "observe_failed":
		status.place("failed", "ステージング反映済み・画面確認が不合格", "")
	case verdict == "measure_failed":
		status.place("failed", "ステージング反映済み・計測が閾値を超過", "設計書が約束した計測値を満たしていません")
	default:
		status.place("failed", "ステージング反映で停止", "")
	}
}

func placeDeliveryAttention(status *RunStatus, stage, verdict string) {
	name := "ステージング"
	if stage == "production" {
		name = "本番"
	}
	switch verdict {
	case "deploy_absent":
		status.placeAt("attention", stage, name+"へ取り込み済み・デプロイは未実行",
			"変更のマージは完了しました。対応するデプロイ実行が見つからず、サービスへの反映は確認していません。")
		status.NextAction = "運用担当者が PR の変更ファイルとデプロイ設定の対象を照合してください。文書など対象外の変更なら、PR の内容を確認すれば完了です。対象の変更なら、既存の運用手順でデプロイと結果確認が必要です。"
	case "deploy_failed":
		status.placeAt("attention", stage, name+"の反映結果を確認してください",
			"自動処理はデプロイの成功を確認できず終了しました。実行の失敗・対象外・結果を取得できなかった場合の区別は、報告と実行履歴の確認が必要です。")
		status.NextAction = "運用担当者が PR のマージ先と変更内容を開き、同じ変更のデプロイ実行履歴を確認してください。対象外の文書変更なら PR の内容を確認して完了できます。実行失敗なら既存の運用手順で復旧し、結果を確認してください。"
	case "merge_unverified":
		status.placeAt("attention", stage, name+"への取り込み結果が不明です",
			"マージの成否を取得できず、自動処理が止まりました。取り込み済みとも未反映とも判定できていません。")
		status.NextAction = "運用担当者が PR のマージ状態・マージ先・変更内容を確認してください。取り込み済みならデプロイの要否と実行結果も確認し、未完了なら既存の運用手順で対応してください。"
	case "observe_blocked":
		if stage == "staging" {
			stage = "confirm"
		}
		status.placeAt("attention", stage, name+"反映済み・画面確認ができず手動確認待ち",
			"デプロイは完了しました。確認用の画面を開けず、変更が依頼どおりか判定できていません。")
		status.NextAction = "運用担当者がチケットの反映結果に記載された URL と確認条件を開き、ログインや遷移先を確認したうえで、画面が条件を満たすか確認してください。"
	}
	status.ActionEffect = "対応を終えたら「確認を記録して閉じる」で、この依頼を対応待ちから確認済みへ移します。この操作で再デプロイや本番への反映は行いません。未解決なら閉じず、チケットに確認結果と残る対応を記録してください。"
}

func allowResolution(status *RunStatus, since time.Time) {
	status.CanResolve = since.IsZero() || !time.Now().After(since.Add(operatorConfirmationWindow))
	if !status.CanResolve {
		status.ActionEffect = "確認の受付期間（報告から60日）を過ぎているため、この画面からは閉じられません。運用担当者がチケットに確認結果と今後の対応を記録してください。自動処理は再開しません。"
	}
}

// Use only the delivery's existing artifact, never model prose or a guessed URL.
func readDeliveryEvidence(status *RunStatus, runDir string) {
	raw, err := os.ReadFile(filepath.Join(runDir, "feature-pr.json"))
	if err != nil {
		return
	}
	var artifact struct {
		Payload struct {
			Feature     struct{ Paths []string }
			PullRequest struct{ HTMLURL string } `json:"pull_request"`
		} `json:"payload"`
	}
	if json.Unmarshal(raw, &artifact) == nil {
		status.PRURL = artifact.Payload.PullRequest.HTMLURL
		status.ChangedFiles = artifact.Payload.Feature.Paths
	}
}

type cardView struct{ status string }

func (c cardView) archivedOrDone() bool { return c.status == "archived" || c.status == "done" }

func deliverCard(tasks []runtime.BoardTask, deliveryID, stage string) (cardView, bool) {
	return cardWithKey(tasks, deliverCardKey(deliveryID, stage))
}

func e2eCard(tasks []runtime.BoardTask, deliveryID string) (cardView, bool) {
	return cardWithKey(tasks, e2eCardKey(deliveryID))
}

func cardWithKey(tasks []runtime.BoardTask, key string) (cardView, bool) {
	for _, task := range tasks {
		if task.IdempotencyKey == key && task.Status != "archived" {
			return cardView{status: task.Status}, true
		}
	}
	return cardView{}, false
}

// BoardStage is one stage of the rail: the id a run's Step names, and what
// a reader is shown.
type BoardStage struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// railStages is the rail this installation can reach. A destination that
// stops at the pull request never reaches staging or production, and the
// stage a person acts on is where it ends.
func railStages(config runtime.Config) []BoardStage {
	stages := []BoardStage{
		{ID: "intake", Label: "受付"}, {ID: "investigate", Label: "調査"}, {ID: "design", Label: "設計"},
		{ID: "implement", Label: "実装"}, {ID: "review", Label: "審査"}, {ID: "checks", Label: "検査"},
	}
	if config.Chain.Deliver.Enabled() {
		return append(stages, BoardStage{ID: "staging", Label: "STG"},
			BoardStage{ID: "confirm", Label: "確認"}, BoardStage{ID: "production", Label: "本番"})
	}
	return append(stages, BoardStage{ID: "confirm", Label: "マージ待ち"})
}

// WriteBoardStatus persists the snapshot atomically and appends one event
// per delivery whose step or detail moved since the previous snapshot. The
// attendant is the only writer, so read-modify-write needs no locking.
func WriteBoardStatus(dir string, snapshot BoardSnapshot) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	previous := map[string]RunStatus{}
	if raw, err := os.ReadFile(filepath.Join(dir, "board.json")); err == nil {
		var prior BoardSnapshot
		if json.Unmarshal(raw, &prior) == nil {
			for _, run := range prior.Runs {
				previous[run.DeliveryID] = run
			}
		}
	}
	var events []StepEvent
	for _, run := range snapshot.Runs {
		before, seen := previous[run.DeliveryID]
		if seen && before.Step == run.Step && before.Detail == run.Detail {
			continue
		}
		events = append(events, StepEvent{
			At: snapshot.GeneratedAt, DeliveryID: run.DeliveryID, IssueKey: run.IssueKey,
			Step: run.Step, StepTitle: run.StepTitle, Detail: run.Detail,
		})
	}
	if len(events) > 0 {
		file, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		for _, event := range events {
			line, err := json.Marshal(event)
			if err != nil {
				_ = file.Close()
				return err
			}
			if _, err := file.Write(append(line, '\n')); err != nil {
				_ = file.Close()
				return err
			}
		}
		if err := file.Close(); err != nil {
			return err
		}
	}
	encoded, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	temp := filepath.Join(dir, "board.json.tmp")
	if err := os.WriteFile(temp, encoded, 0o600); err != nil {
		return err
	}
	return os.Rename(temp, filepath.Join(dir, "board.json"))
}

// placeResolvedOutcome names the end of a delivery an operator confirmed
// by hand: closed for the automation, with what the report could not
// establish (a deploy, a merge) left to the operator's word rather than
// asserted.
func placeResolvedOutcome(status *RunStatus, phase, verdict string) {
	switch {
	case phase == "release":
		status.place("done", "運用担当者が確認済み", "本番の状態は運用担当者の確認どおりです")
	case verdict == "merge_unverified":
		status.place("done", "運用担当者が確認済み", "ステージングへのマージの成否は運用担当者の確認どおりです。本番反映が別途必要な場合のみ、運用担当者が既存の手順で対応します。")
	default:
		status.place("done", "運用担当者が確認済み", "ステージングの反映状態は運用担当者の確認どおりです。本番反映が別途必要な場合のみ、運用担当者が既存の手順で対応します。")
	}
}

// placeDesignStage shows the investigating designer's stages while any of
// the newest design round's cards is still open: 調査 while the investigate
// card runs, 設計 while its reviews and decision run. A finished design
// hands off only while the first implementation card exists: retirement
// can temporarily leave just a future publication card on the board.
func placeDesignStage(status *RunStatus, view chainView) bool {
	if view.designRound == 0 {
		return false
	}
	detail := fmt.Sprintf("設計 %d 巡目", view.designRound)
	open := false
	investigating := false
	blocked := false
	for stage, card := range view.designCards {
		if card.Status == "done" {
			continue
		}
		open = true
		if failedCardStatuses[card.Status] {
			blocked = true
		}
		if stage == runtime.StageInvestigate {
			investigating = true
		}
	}
	switch {
	case !open:
		_, hasApply := view.cards[runtime.StageApply]
		_, hasImplement := view.cards[runtime.StageImplement]
		if hasApply || hasImplement {
			return false
		}
		status.place("design", "次の工程を準備中", detail)
	case blocked:
		status.place("design", "工程の復旧処理中", detail)
	case investigating:
		status.place("investigate", "調査中（稼働環境とリポジトリを読み取りだけで計っています）", detail)
	default:
		status.place("design", "設計中・設計レビュー中", detail)
	}
	return true
}

// withdrawSentence tells the requester how to drop a ticket that is waiting
// on a question. Only the requester's own comment counts, and only the round
// the run is on, so both are said. A run whose sealed question cannot be
// read points at the question comment's own heading instead of naming a
// round that may be wrong.
func withdrawSentence(run state.RunOverview) string {
	if record, err := hook.DecodeQuestionRecord([]byte(run.QuestionRecordJSON)); err == nil && record.QuestionRevision > 0 {
		return "この依頼を取り下げるときは、依頼者が「中止 " + hook.QuestionRevisionTag(record.QuestionRevision) + "」とチケットにコメントしてください。"
	}
	return "この依頼を取り下げるときは、依頼者が質問コメントの見出しにある番号を使って「中止 (その番号)」とチケットにコメントしてください。"
}
