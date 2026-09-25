package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/cardsecret"
	"automation.internal/ticket-ingress/internal/hook"
)

// writeTerminalTrail puts a run record in the workspace, which is what the
// closing comment's longest part is made of.
func writeTerminalTrail(t *testing.T, workspace, trail string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(workspace, "m1-trail.txt"), []byte(trail), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The tracker refuses a comment over its limit at the API boundary, which
// loses the whole comment rather than the part that would not fit. The kept
// copy is held to the same limit as the posted one — it is the same text —
// and the marker its re-send is recognised by is still on its last line.
func TestTheKeptClosingCommentIsHeldToTheTrackerLimit(t *testing.T) {
	terminal, _, comments := claimedTerminalFixture(t)
	// A record far longer than a comment can carry, in the shape a record
	// really has: many lines, each meaningful on its own.
	var builder strings.Builder
	for builder.Len() < hook.MaxTrailRecordBytes-200 {
		builder.WriteString("実装役の報告: 設定画面の見出しを書き換え、検証を通しました。\n")
	}
	writeTerminalTrail(t, terminal.workspace, builder.String())
	if err := terminal.Report(context.Background(), hook.TerminalModelFailed,
		Outcome{Code: hook.TerminalModelFailed}, ""); err != nil {
		t.Fatal(err)
	}
	kept, ok := ReadTerminalComment(terminal.workspace)
	if !ok {
		t.Fatal("the ending wrote down no closing comment")
	}
	if len(kept.Body) > hook.MaxTrackerCommentBytes {
		t.Fatalf("the kept comment is %d bytes, over the tracker's %d", len(kept.Body), hook.MaxTrackerCommentBytes)
	}
	if hook.ExtractCommentMarker(kept.Body) != kept.Marker || kept.Marker == "" {
		t.Fatalf("the kept comment lost the marker its re-send is found by: %q", kept.Marker)
	}
	if len(comments.reports) != 1 || comments.reports[0] != kept.Body {
		t.Fatalf("the kept comment is not the posted one:\nkept: %d bytes\nposted: %q", len(kept.Body), comments.reports)
	}
	if !strings.Contains(kept.Body, "ここまでを掲示しています") {
		t.Fatalf("the comment was cut without saying so: %q", kept.Body)
	}
}

// A credential is taken out of the text on its way to the ticket. The kept
// copy is rendered from the same report as the posted one, so whatever the
// masking removes from one is gone from the other; a file on disk holding
// the value the comment does not would be a new way to publish it.
func TestACredentialIsMaskedOutOfTheKeptClosingCommentAsOutOfThePostedOne(t *testing.T) {
	cardsecret.Forget()
	t.Cleanup(cardsecret.Forget)
	secret := "postgres://warehouse.invalid/orders?password=hunter2hunter2"
	cardsecret.Register([]cardsecret.Entry{{Name: "DATABASE_URL", Secret: secret}})

	terminal, _, comments := claimedTerminalFixture(t)
	writeTerminalTrail(t, terminal.workspace,
		"検証の記録\npsql: "+secret+" に接続できませんでした。\n設定を確認してください。\n")
	if err := terminal.Report(context.Background(), hook.TerminalModelFailed,
		Outcome{Code: hook.TerminalModelFailed}, ""); err != nil {
		t.Fatal(err)
	}
	kept, ok := ReadTerminalComment(terminal.workspace)
	if !ok {
		t.Fatal("the ending wrote down no closing comment")
	}
	raw, err := os.ReadFile(filepath.Join(terminal.workspace, TerminalCommentFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "hunter2hunter2") {
		t.Fatalf("the credential was written to the run directory:\n%s", raw)
	}
	if len(comments.reports) != 1 || strings.Contains(comments.reports[0], "hunter2hunter2") {
		t.Fatalf("the credential reached the ticket: %q", comments.reports)
	}
	if comments.reports[0] != kept.Body {
		t.Fatal("the kept comment and the posted one are not the same text")
	}
	if !strings.Contains(kept.Body, cardsecret.Redacted) {
		t.Fatalf("the line was dropped rather than masked: %q", kept.Body)
	}
}

// The digest is the binding between the kept words and the ending the
// ledger sealed. A file whose report no longer hashes to the digest beside
// it is refused outright, because posting it would report one ending under
// another's marker.
func TestAKeptCommentWhoseDigestIsNotItsReportsIsRefused(t *testing.T) {
	terminal, _, _ := claimedTerminalFixture(t)
	if err := terminal.Report(context.Background(), hook.TerminalModelFailed,
		Outcome{Code: hook.TerminalModelFailed}, ""); err != nil {
		t.Fatal(err)
	}
	kept, ok := ReadTerminalComment(terminal.workspace)
	if !ok {
		t.Fatal("the ending wrote down no closing comment")
	}
	for name, damage := range map[string]func(*TerminalCommentRecord){
		"another digest":      func(r *TerminalCommentRecord) { r.ReportSHA256 = strings.Repeat("0", 64) },
		"another ending":      func(r *TerminalCommentRecord) { r.Code = string(hook.TerminalSuccess) },
		"a body without it":   func(r *TerminalCommentRecord) { r.Body = "自動処理の最終結果: model_failed\n" },
		"a report moved on":   func(r *TerminalCommentRecord) { r.Report.RunAttempt = 9 },
		"a marker of its own": func(r *TerminalCommentRecord) { r.Marker = hook.CommentMarker("terminal", "TICKET-3") },
	} {
		damaged := kept
		damage(&damaged)
		if err := damaged.validate(); err == nil {
			t.Fatalf("%s was accepted", name)
		}
		if _, err := damaged.Request(); err == nil {
			t.Fatalf("%s produced a report to send", name)
		}
	}
	if err := kept.validate(); err != nil {
		t.Fatalf("the undamaged record was refused: %v", err)
	}
}
