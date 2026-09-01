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
}

// RunStatus is one delivery's position in the pipeline, in requester terms.
type RunStatus struct {
	DeliveryID string `json:"delivery_id"`
	IssueID    int64  `json:"issue_id,omitempty"`
	IssueKey   string `json:"issue_key,omitempty"`
	Summary    string `json:"summary,omitempty"`
	State      string `json:"state"`
	// Step is one of the pipeline steps (intake, implement, review, checks,
	// staging, confirm, production) or a resting state (question, done,
	// stopped, failed).
	Step      string `json:"step"`
	StepTitle string `json:"step_title"`
	Detail    string `json:"detail,omitempty"`
	Round     int    `json:"round,omitempty"`
	ClaimedAt int64  `json:"claimed_at_ms,omitempty"`
	Terminal  string `json:"terminal_code,omitempty"`
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
	snapshot := BoardSnapshot{SchemaVersion: 1, GeneratedAt: time.Now().UTC()}
	for _, run := range runs {
		snapshot.Runs = append(snapshot.Runs, classifyRun(config, run, tasks))
	}
	sort.SliceStable(snapshot.Runs, func(a, b int) bool {
		return snapshot.Runs[a].ClaimedAt > snapshot.Runs[b].ClaimedAt
	})
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
	return snapshot, nil
}

func classifyRun(config runtime.Config, run state.RunOverview, tasks []runtime.BoardTask) RunStatus {
	status := RunStatus{
		DeliveryID: run.DeliveryID, IssueID: run.IssueID, IssueKey: run.IssueKey, Summary: run.Summary,
		State: run.State, ClaimedAt: run.ClaimedAt, Terminal: run.TerminalCode,
	}
	switch run.State {
	case "queued":
		status.place("intake", "受付待ち", "")
	case "awaiting_answer":
		status.place("question", "質問への回答待ち", "依頼者の返信を待っています")
	case "claimed":
		classifyClaimed(&status, run, tasks)
	case "terminal":
		classifyAfterTerminal(&status, config, run, tasks)
	case "terminal_report_pending", "question_report_pending":
		status.place("reporting", "報告を作成中", "チケットへの報告を準備しています")
	default:
		// Anything the vocabulary gains later; never leak internal state
		// names into the requester-facing detail.
		status.place("intake", "処理中", "")
	}
	return status
}

func (s *RunStatus) place(step, title, detail string) {
	s.Step, s.StepTitle, s.Detail = step, title, detail
}

func classifyClaimed(status *RunStatus, run state.RunOverview, tasks []runtime.BoardTask) {
	view := chainViewFor(tasks, run.DeliveryID)
	status.Round = view.round
	if view.round == 0 {
		status.place("intake", "受付処理中", "作業の準備をしています")
		return
	}
	detail := fmt.Sprintf("%d 巡目", view.round)
	var implementLeft, reviewLeft, blocked, humanLane bool
	for stage, card := range view.cards {
		if card.Status == "done" {
			continue
		}
		// triage / scheduled are deliberate human lanes the automation
		// never touches — "in progress" would be a lie there.
		if card.Status == "triage" || card.Status == "scheduled" {
			humanLane = true
		}
		if failedCardStatuses[card.Status] {
			blocked = true
		}
		if strings.Contains(stage, "review") {
			reviewLeft = true
		} else {
			implementLeft = true
		}
	}
	switch {
	case humanLane:
		status.place("attention", "人の対応待ち", detail+"・工程カードが人の確認レーンにあります")
	case blocked:
		status.place("implement", "工程の復旧処理中", detail)
	case implementLeft:
		status.place("implement", "実装・検証中", detail)
	case reviewLeft:
		status.place("review", "レビュー中", detail)
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
	if run.TerminalCode != string(hook.TerminalSuccess) {
		if run.TerminalCode == string(hook.TerminalCancelled) {
			status.place("stopped", "停止済み", "ご指示により停止しました")
			return
		}
		status.place("failed", "失敗で終了", run.TerminalCode)
		return
	}
	runDir := runDirectory(config, run.DeliveryID)
	outcome, sealed := readBoardOutcome(runDir)
	if sealed && outcome.Phase == "release" {
		placeReleaseOutcome(status, outcome.Verdict)
		return
	}
	// The production report file beats a staging-phase seal: once the
	// promote card wrote it, the staging story ("waiting for Go") is over
	// even while the release post is still on its way.
	if report, err := readDeliverReport(runDir, runner.DeliverProductionReportFile); err == nil {
		placeReleaseOutcome(status, report.Verdict)
		return
	}
	if card, ok := deliverCard(tasks, run.DeliveryID, "promote"); ok && !card.archivedOrDone() {
		status.place("production", "本番へ反映中", "")
		return
	}
	if sealed && outcome.Phase == "staging" {
		placeStagingOutcome(status, outcome.Verdict, outcome.Note)
		return
	}
	if report, err := readDeliverReport(runDir, runner.DeliverStagingReportFile); err == nil {
		placeStagingOutcome(status, report.Verdict, report.PromotionHold)
		return
	}
	if card, ok := deliverCard(tasks, run.DeliveryID, "integrate"); ok && !card.archivedOrDone() {
		status.place("staging", "ステージングへ反映中", "マージとデプロイの完了を待っています")
		return
	}
	if card, ok := deliverCard(tasks, run.DeliveryID, "checks"); ok && !card.archivedOrDone() {
		status.place("checks", "自動検査 (CI) 待ち", "納品した変更の検査が走っています")
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
			status.place("attention", "ステージング画面の確認ができませんでした", "手動での確認が必要です")
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
	status.place("done", "納品済み", "")
}

func placeReleaseOutcome(status *RunStatus, verdict string) {
	switch verdict {
	case "pass":
		status.place("done", "本番反映済み", "本番の画面確認まで合格")
	case "expired":
		status.place("done", "ステージング反映済み", "Go の期限切れで本番反映なし")
	case "observe_failed":
		status.place("done", "本番反映済み・画面は要確認", "")
	case "deploy_failed":
		status.place("attention", "本番反映の完了確認が必要", "運用担当者が状態を確認します")
	case "merge_unverified":
		status.place("attention", "本番反映の成否を確認中", "運用担当者が状態を確認します")
	default:
		status.place("failed", "本番反映の工程で停止", "")
	}
}

func placeStagingOutcome(status *RunStatus, verdict, hold string) {
	switch {
	case verdict == "pass" && hold != "":
		status.place("done", "ステージング反映済み", "本番反映は運用手順で行います")
	case verdict == "pass":
		status.place("confirm", "本番反映の承認待ち", "あなたの「Go」を待っています")
	case verdict == "stopped":
		status.place("stopped", "停止済み", "ご指示により停止しました")
	case verdict == "observe_failed":
		status.place("failed", "ステージング反映済み・画面確認が不合格", "")
	case verdict == "deploy_failed" || verdict == "merge_unverified":
		status.place("attention", "ステージング反映の状態確認が必要", "運用担当者が状態を確認します")
	default:
		status.place("failed", "ステージング反映で停止", "")
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
		file, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
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
	if err := os.WriteFile(temp, encoded, 0o644); err != nil {
		return err
	}
	return os.Rename(temp, filepath.Join(dir, "board.json"))
}
