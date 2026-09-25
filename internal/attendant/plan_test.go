package attendant

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/hook"
)

type fakeCommentLister struct {
	comments []hook.BacklogComment
	err      error
}

func (f fakeCommentLister) ListComments(context.Context, int64, int64) ([]hook.BacklogComment, error) {
	return f.comments, f.err
}

// A listing failure must fail closed: the caller postpones the round instead
// of issuing cards past an unread stop request.
func TestStopRequestedFailsClosed(t *testing.T) {
	_, err := stopRequested(context.Background(), fakeCommentLister{err: errors.New("backlog down")}, 7001, 42)
	if err == nil {
		t.Fatal("listing failure was swallowed")
	}
	stopped, err := stopRequested(context.Background(), fakeCommentLister{comments: []hook.BacklogComment{
		{UserID: 7001, Body: "停止"},
	}}, 7001, 42)
	if err != nil || !stopped {
		t.Fatalf("stopRequested() = (%v, %v), want (true, nil)", stopped, err)
	}
}

func TestContainsStopComment(t *testing.T) {
	const requester = int64(7001)
	cases := []struct {
		name     string
		comments []hook.BacklogComment
		want     bool
	}{
		{"empty", nil, false},
		{"exact stop", []hook.BacklogComment{{UserID: requester, Body: "停止"}}, true},
		{"stop with surrounding whitespace", []hook.BacklogComment{{UserID: requester, Body: "\n  停止  \n"}}, true},
		{"stop after blank lines", []hook.BacklogComment{{UserID: requester, Body: "\n\n停止\nこの方針は違います"}}, true},
		// The word further down an ordinary comment must not stop the run.
		{"stop mentioned mid-comment", []hook.BacklogComment{{UserID: requester, Body: "レビュー後に停止も検討します"}}, false},
		{"stop on a later line only", []hook.BacklogComment{{UserID: requester, Body: "方針は良いです\n停止"}}, false},
		{"stop with suffix", []hook.BacklogComment{{UserID: requester, Body: "停止してください"}}, false},
		// Only the allowed requester can stop the run.
		{"stop by another user", []hook.BacklogComment{{UserID: 9999, Body: "停止"}}, false},
		{"stop among many comments", []hook.BacklogComment{
			{UserID: requester, Body: "起票します"},
			{UserID: 9999, Body: "停止"},
			{UserID: requester, Body: "停止"},
		}, true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := containsStopComment(tt.comments, requester); got != tt.want {
				t.Fatalf("containsStopComment() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLoadPlanFactsReadsTheSealedArtifacts(t *testing.T) {
	runDir := t.TempDir()
	write := func(path, content string) {
		t.Helper()
		full := filepath.Join(runDir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("readiness-ticket.json", `{"request":"再試行の導線を出す"}`)
	write("intake.json", `{"rationale":"失敗表示の隣に再試行ボタンを置く。"}`)
	write("history/readiness/assessment-1.json", `{"assumptions":[{"statement":"古い前提"}]}`)
	write("history/readiness/assessment-2.json", `{"assumptions":[{"statement":"一覧のみ取り直す"},{"statement":"  "}]}`)
	write("history/readiness/decision.json", `{"outcome":"ready","needs_design":true,"design_reason":"trigger_word"}`)

	facts := loadPlanFacts(runDir)
	if facts.Request != "再試行の導線を出す" {
		t.Fatalf("Request = %q", facts.Request)
	}
	if facts.Rationale != "失敗表示の隣に再試行ボタンを置く。" {
		t.Fatalf("Rationale = %q", facts.Rationale)
	}
	// The newest assessment wins and blank statements are dropped.
	if len(facts.Assumptions) != 1 || facts.Assumptions[0] != "一覧のみ取り直す" {
		t.Fatalf("Assumptions = %v", facts.Assumptions)
	}
	// The design decision is read from the sealed readiness decision.
	if !facts.NeedsDesign || facts.DesignReason != "trigger_word" {
		t.Fatalf("design decision = (%v, %q)", facts.NeedsDesign, facts.DesignReason)
	}
	// A decision sealed before the design stage existed says nothing about it.
	write("history/readiness/decision.json", `{"outcome":"ready"}`)
	if facts := loadPlanFacts(runDir); facts.NeedsDesign || facts.DesignReason != "" {
		t.Fatalf("an old decision produced a design decision: (%v, %q)", facts.NeedsDesign, facts.DesignReason)
	}
}

func TestLoadPlanFactsFallsBackAndToleratesAbsence(t *testing.T) {
	runDir := t.TempDir()
	// No artifacts at all: the notice renders with empty facts, never fails.
	if facts := loadPlanFacts(runDir); facts.Request != "" {
		t.Fatalf("empty run dir produced %+v", facts)
	}
	// Without the readiness ticket the draft's request still fills in.
	if err := os.WriteFile(filepath.Join(runDir, "ticket-draft.json"), []byte(`{"request":"下書きの依頼"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if facts := loadPlanFacts(runDir); facts.Request != "下書きの依頼" {
		t.Fatalf("draft fallback produced %+v", facts)
	}
	// A corrupt artifact is skipped, not fatal.
	if err := os.WriteFile(filepath.Join(runDir, "intake.json"), []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if facts := loadPlanFacts(runDir); facts.Rationale != "" {
		t.Fatalf("corrupt intake produced %+v", facts)
	}
}

func TestPlanCommentContentRendersTheFacts(t *testing.T) {
	content := hook.PlanCommentContent("run-42", hook.PlanFacts{
		Request:      "再試行の導線を出す",
		Rationale:    strings.Repeat("あ", 700),
		Assumptions:  []string{"一覧のみ取り直す"},
		DesignReason: "approach_in_ticket",
	})
	for _, want := range []string{
		"【実装方針】", "依頼の解釈: 再試行の導線を出す", "一覧のみ取り直す",
		"設計なし: 方針が本文にあるため設計を省略",
		"「停止」とだけ書いたコメント", "…（以下略）",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("plan comment lacks %q:\n%s", want, content)
		}
	}
	// The notice never promises which files the run will touch: nothing
	// decides that until the change is made.
	if strings.Contains(content, "触る予定の範囲") || strings.Contains(content, "見当をつけた範囲") {
		t.Fatalf("the plan comment still promises a file scope:\n%s", content)
	}
	if err := hook.ValidateCommentContract(content, hook.CommentMarker("plan", "run-42")); err != nil {
		t.Fatalf("plan comment violates the contract: %v", err)
	}
	// Empty facts still render a contract-complete notice: the plan is a
	// courtesy, never a reason to fail the run that produced no artifacts.
	empty := hook.PlanCommentContent("run-42", hook.PlanFacts{})
	if err := hook.ValidateCommentContract(empty, hook.CommentMarker("plan", "run-42")); err != nil {
		t.Fatalf("empty plan comment violates the contract: %v", err)
	}
}

// Schema-maximal facts must stay below the tracker's 16 KiB comment limit
// with the footer — and its machine marker — intact at the very end.
func TestPlanCommentContentStaysWithinTheTrackerLimit(t *testing.T) {
	assumptions := make([]string, 16)
	for index := range assumptions {
		assumptions[index] = strings.Repeat("前", 600)
	}
	content := hook.PlanCommentContent("run-42", hook.PlanFacts{
		Request:     strings.Repeat("あ", 8000),
		Rationale:   strings.Repeat("い", 8000),
		Assumptions: assumptions,
	})
	if len(content) > hook.MaxTrackerCommentBytes {
		t.Fatalf("plan comment is %d bytes, above the tracker limit", len(content))
	}
	if err := hook.ValidateCommentContract(content, hook.CommentMarker("plan", "run-42")); err != nil {
		t.Fatalf("capped plan comment violates the contract: %v", err)
	}
}

// A reception that asks as little as it can decides the rest, and what it
// decided is the part of the plan notice a requester is most likely to want
// back. The decisions are told apart from the conventions by the kind the
// assessment sealed them under, and they get their own heading with the one
// action that changes them.
func TestTheReceptionsOwnDecisionsReachThePlanNotice(t *testing.T) {
	runDir := t.TempDir()
	path := filepath.Join(runDir, "history", "readiness", "assessment-1.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	sealed := `{"assumptions":[` +
		`{"kind":"repository_convention","statement":"命名は既存のファイルに合わせる"},` +
		`{"kind":"defensible_default","statement":"並び順は指定が無いため新着順にする"},` +
		`{"kind":"non_user_visible_implementation","statement":"取得はいまの関数を使う"},` +
		`{"kind":"defensible_default","statement":"空のときは件数 0 と出す"}]}`
	if err := os.WriteFile(path, []byte(sealed), 0o644); err != nil {
		t.Fatal(err)
	}
	facts := loadPlanFacts(runDir)
	if len(facts.Decided) != 2 || facts.Decided[0] != "並び順は指定が無いため新着順にする" {
		t.Fatalf("Decided = %v, want the two points the reception settled itself", facts.Decided)
	}
	if len(facts.Assumptions) != 2 || facts.Assumptions[0] != "命名は既存のファイルに合わせる" {
		t.Fatalf("Assumptions = %v, want the two it took from the repository", facts.Assumptions)
	}
	facts.Request = "一覧に絞り込みを足す"
	content := hook.PlanCommentContent("run-42", facts)
	for _, want := range []string{
		"確認せずにこちらで決めた点（違う場合は停止してください）",
		"- 並び順は指定が無いため新着順にする",
		"- 空のときは件数 0 と出す",
		"前提とした解釈（曖昧だった点はこう進めます）",
		"- 命名は既存のファイルに合わせる",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("the plan notice lacks %q:\n%s", want, content)
		}
	}
	if strings.Index(content, "確認せずにこちらで決めた点") > strings.Index(content, "前提とした解釈") {
		t.Fatal("the decisions are printed after the conventions; the requester reads them first")
	}
	// An assessment sealed before the kind existed keeps reading as what it
	// was: an assumption, not a decision taken instead of asking.
	old := `{"assumptions":[{"statement":"古い前提"}]}`
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	if facts := loadPlanFacts(runDir); len(facts.Decided) != 0 || len(facts.Assumptions) != 1 {
		t.Fatalf("an older assessment produced %d decisions and %d assumptions", len(facts.Decided), len(facts.Assumptions))
	}
}

// However long the record of what was decided grows, the notice still says
// how to stop the run. That instruction is the only move a requester has
// left once the reception has asked its one round, so the lists are what
// gives way when the body runs out of room, never the instruction.
func TestTheStopInstructionSurvivesALongRecord(t *testing.T) {
	facts := hook.PlanFacts{
		Request:   strings.Repeat("依", 600),
		Rationale: strings.Repeat("方", 600),
	}
	for index := 0; index < 12; index++ {
		facts.Decided = append(facts.Decided, strings.Repeat("決", 200))
		facts.Assumptions = append(facts.Assumptions, strings.Repeat("前", 200))
	}
	content := hook.PlanCommentContent("run-42", facts)
	if len(content) > 16*1024 {
		t.Fatalf("the notice is %d bytes, over the tracker's limit", len(content))
	}
	for _, want := range []string{"「停止」とだけ書いたコメント", "確認せずにこちらで決めた点", "…（長いため以下略）"} {
		if !strings.Contains(content, want) {
			t.Fatalf("the notice lacks %q at %d bytes", want, len(content))
		}
	}
	if err := hook.ValidateCommentContract(content, hook.CommentMarker("plan", "run-42")); err != nil {
		t.Fatalf("the notice broke its contract: %v", err)
	}
}

// Between finishing the reception and acting on what it produced, the stop
// is read again. The reception takes minutes; a requester who writes 「停止」
// while it runs has stopped the run, and a question posted after that is a
// question they already said no to (live 2026-09-25: the stop was written
// eight seconds before the question went out).
func TestAStopWrittenWhileTheReceptionRanIsHeardBeforeItActs(t *testing.T) {
	const requester = int64(7001)
	logger := &recordingLogger{}
	if !stoppedWhileWorking(context.Background(),
		fakeCommentLister{comments: []hook.BacklogComment{{UserID: requester, Body: "停止"}}},
		requester, 42, "run-42", "asking anyway", logger) {
		t.Fatal("a stop on the ticket was not heard")
	}
	if stoppedWhileWorking(context.Background(),
		fakeCommentLister{comments: []hook.BacklogComment{{UserID: requester, Body: "よろしくお願いします"}}},
		requester, 42, "run-42", "asking anyway", logger) {
		t.Fatal("an ordinary comment stopped the run")
	}
	if len(logger.lines) != 0 {
		t.Fatalf("a readable listing logged something: %v", logger.lines)
	}
	// Unlike the fail-closed read before the claim, an unreadable listing
	// here proceeds — and says what proceeding means, because the whole
	// reception would otherwise be re-run on every retry.
	if stoppedWhileWorking(context.Background(), fakeCommentLister{err: errors.New("tracker down")},
		requester, 42, "run-42", "asking anyway", logger) {
		t.Fatal("an unreadable listing was treated as a stop")
	}
	if len(logger.lines) != 1 || !strings.Contains(logger.lines[0], "asking anyway") {
		t.Fatalf("the log does not say what proceeding means here: %v", logger.lines)
	}
}

// A gate that settled its own questions writes them where the plan notice
// reads them, and the notice says so. Without the line the points read as
// things nobody thought worth asking about, when they are the opposite: the
// points this run had written down to ask, and stopping is the only way the
// requester gets to answer one.
func TestThePlanNoticeSaysWhenTheReceptionSettledItsOwnQuestions(t *testing.T) {
	runDir := t.TempDir()
	write := func(path, content string) {
		t.Helper()
		full := filepath.Join(runDir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("readiness-ticket.json", `{"request":"ラベルを差し替える"}`)
	write("history/readiness/assessment-1.json",
		`{"assumptions":[{"kind":"repository_convention","statement":"命名は既存のファイルに合わせる"}]}`)
	write("history/readiness/decision.json", `{"outcome":"ready","needs_design":false,"design_reason":"approach_in_ticket",`+
		`"assumptions":[{"kind":"defensible_default","statement":"両方の言語画面を直す"},`+
		`{"kind":"non_user_visible_implementation","statement":"読まれない種類は出さない"}],`+
		`"reception_judgment":{"model":"typesafe/jev-1.13","answer":"yes","confidence":0.93,"threshold":0.85}}`)

	facts := loadPlanFacts(runDir)
	if len(facts.Decided) != 1 || facts.Decided[0] != "両方の言語画面を直す" {
		t.Fatalf("Decided = %v, want the point the gate settled instead of asking", facts.Decided)
	}
	if facts.SettledConfidence != 0.93 {
		t.Fatalf("SettledConfidence = %v, want 0.93", facts.SettledConfidence)
	}
	content := hook.PlanCommentContent("run-42", facts)
	for _, want := range []string{
		"確認せずにこちらで決めた点（違う場合は停止してください）",
		"- 両方の言語画面を直す",
		"確信度 0.93",
		"お伺いせずに受付が決めました",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("the plan notice lacks %q:\n%s", want, content)
		}
	}
	// The sentence sits with the list it is about, so an overflowing body
	// cuts the two together rather than leaving a sentence pointing at
	// something that is no longer there.
	if strings.Index(content, "- 両方の言語画面を直す") > strings.Index(content, "確信度 0.93") {
		t.Fatal("the sentence is printed before the points it explains")
	}

	// A run where nothing settled its questions says nothing about it.
	write("history/readiness/decision.json", `{"outcome":"ready","needs_design":false,"design_reason":"approach_in_ticket"}`)
	quiet := loadPlanFacts(runDir)
	if quiet.SettledConfidence != 0 || len(quiet.Decided) != 0 {
		t.Fatalf("a decision that settled nothing produced %v at confidence %v", quiet.Decided, quiet.SettledConfidence)
	}
	if body := hook.PlanCommentContent("run-42", quiet); strings.Contains(body, "確信度") {
		t.Fatalf("the notice explains a judgment nobody made:\n%s", body)
	}
}
