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
	write("readiness-ticket.json", `{"request":"再試行の導線を出す","target_files":["client/a.tsx","client/b.json"]}`)
	write("intake.json", `{"rationale":"失敗表示の隣に再試行ボタンを置く。"}`)
	write("history/readiness/assessment-1.json", `{"assumptions":[{"statement":"古い前提"}]}`)
	write("history/readiness/assessment-2.json", `{"assumptions":[{"statement":"一覧のみ取り直す"},{"statement":"  "}]}`)

	facts := loadPlanFacts(runDir)
	if facts.Request != "再試行の導線を出す" {
		t.Fatalf("Request = %q", facts.Request)
	}
	if facts.Rationale != "失敗表示の隣に再試行ボタンを置く。" {
		t.Fatalf("Rationale = %q", facts.Rationale)
	}
	if len(facts.TargetFiles) != 2 || facts.TargetFiles[0] != "client/a.tsx" {
		t.Fatalf("TargetFiles = %v", facts.TargetFiles)
	}
	// The newest assessment wins and blank statements are dropped.
	if len(facts.Assumptions) != 1 || facts.Assumptions[0] != "一覧のみ取り直す" {
		t.Fatalf("Assumptions = %v", facts.Assumptions)
	}
}

func TestLoadPlanFactsFallsBackAndToleratesAbsence(t *testing.T) {
	runDir := t.TempDir()
	// No artifacts at all: the notice renders with empty facts, never fails.
	if facts := loadPlanFacts(runDir); facts.Request != "" || len(facts.TargetFiles) != 0 {
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
		Request:     "再試行の導線を出す",
		Rationale:   strings.Repeat("あ", 700),
		TargetFiles: []string{"client/a.tsx"},
		Assumptions: []string{"一覧のみ取り直す"},
	})
	for _, want := range []string{
		"【実装方針】", "依頼の解釈: 再試行の導線を出す", "client/a.tsx", "一覧のみ取り直す",
		"「停止」とだけ書いたコメント", "…（以下略）",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("plan comment lacks %q:\n%s", want, content)
		}
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
	files := make([]string, 40)
	for index := range files {
		files[index] = strings.Repeat("p", 300)
	}
	content := hook.PlanCommentContent("run-42", hook.PlanFacts{
		Request:     strings.Repeat("あ", 8000),
		Rationale:   strings.Repeat("い", 8000),
		TargetFiles: files,
		Assumptions: assumptions,
	})
	if len(content) > 16*1024 {
		t.Fatalf("plan comment is %d bytes, above the tracker limit", len(content))
	}
	if err := hook.ValidateCommentContract(content, hook.CommentMarker("plan", "run-42")); err != nil {
		t.Fatalf("capped plan comment violates the contract: %v", err)
	}
}
