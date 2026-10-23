package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ticket-runner/internal/chain"
)

func approvalDisplaySettings(t *testing.T, a *chain.Approval, timezone, summary string) {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"timezone": timezone, "summary": summary})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, a); err != nil {
		t.Fatal(err)
	}
}

func TestApprovalCheck11Timezone(t *testing.T) {
	for _, zone := range []string{"", "UTC", "Asia/Tokyo", "America/New_York", "Invalid/Timezone", "Local"} {
		t.Run(zone, func(t *testing.T) {
			cfg := approvalConfiguration()
			approvalDisplaySettings(t, cfg.Workflow.Stages[2].Approval, zone, "")
			err := validateApprovalStages(cfg)
			valid := zone != "Invalid/Timezone" && zone != "Local"
			if (err == nil) != valid {
				t.Fatalf("valid=%t, err=%v", valid, err)
			}
			if err != nil && !strings.Contains(err.Error(), "check 11") {
				t.Fatal(err)
			}
		})
	}
}

func TestApprovalCheck12Summary(t *testing.T) {
	for _, path := range []string{"", "ops/bundle/summary.txt", "ops/bundle/nested/summary.txt", "ops/summary.txt", "ops/bundle-other/summary.txt", "ops/bundle/../summary.txt", "/ops/bundle/summary.txt", "ops/bundle"} {
		t.Run(path, func(t *testing.T) {
			cfg := approvalConfiguration()
			approvalDisplaySettings(t, cfg.Workflow.Stages[2].Approval, "", path)
			err := validateApprovalStages(cfg)
			valid := path == "" || path == "ops/bundle/summary.txt" || path == "ops/bundle/nested/summary.txt"
			if (err == nil) != valid {
				t.Fatalf("valid=%t, err=%v", valid, err)
			}
			if err != nil && !strings.Contains(err.Error(), "check 12") {
				t.Fatal(err)
			}
		})
	}
}

func TestApprovalPlainPostExactText(t *testing.T) {
	cfg := approvalConfiguration()
	stage := cfg.Workflow.Stages[2]
	approvalDisplaySettings(t, stage.Approval, "Asia/Tokyo", "ops/bundle/summary.txt")
	bundle := chain.ApprovalBundle{Files: []chain.ApprovalFile{{Path: "dry-run.txt"}, {Path: "plan.txt"}, {Path: "summary.txt"}}, Contents: map[string][]byte{"dry-run.txt": []byte("checked\n"), "plan.txt": []byte("proposed\n"), "summary.txt": []byte("確認済みの設定を本番に反映します。")}}
	post := chain.ApprovalPost{Digest: strings.Repeat("a", 64), DigestPrefix: "aaaaaaaa", Deadline: time.Date(2026, 10, 25, 13, 6, 3, 0, time.UTC)}
	got, err := approvalPostText(stage, bundle, post, chain.State{}, nil)
	want := "本番の操作の確認をお願いします。\n\nやること:\n確認済みの設定を本番に反映します。\n\n実行してよければ、次の 1 行だけを新しいコメントに書いてください:\nGO aaaaaaaa\n\n期限: 2026-10-25 22:06 (JST) まで。過ぎたら計画を作り直して、もう一度確認します。\n前回の実行: まだ実行していません。\n止めたいときは「停止」とだけ書いてください。GO を消したり書き直したりしても取り消しにはなりません。\n安全のため、確認待ちの間に他の人がコメントしたり、コメントが編集・削除されたり、承認者が GO・停止以外のコメントを書いたりすると、実行せずに止まります。\n\n## 試し実行の結果\n\n```\ndry-run.txt\nchecked\n```\n\n## AI が書いた計画\n\n```\nplan.txt\nproposed\n```\n"
	if err != nil || got != want {
		t.Fatalf("err=%v\ngot: %s\nwant: %s", err, got, want)
	}
}

func TestApprovalSummaryPostAndSavedCopy(t *testing.T) {
	a, f, state := approvalFixture(t)
	approvalDisplaySettings(t, state.Workflow.Stages[2].Approval, "Asia/Tokyo", "ops/bundle/nested/summary.txt")
	summary := "  設定を更新します。\n結果を確認します。\n"
	path := filepath.Join(a.cfg.Roles[0].Processes[0].Directory, "ops/bundle/nested/summary.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(summary), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := a.before(ctx, &state, chain.Assignment{Role: "c"}); !errors.Is(err, chain.ErrAwaitingApproval) {
		t.Fatal(err)
	}
	text := f.all()[0]
	if !strings.Contains(text, "やること:\n"+summary+"\n実行してよければ") || strings.Contains(text, "summary.txt") || !strings.Contains(text, "(JST)") {
		t.Fatal(text)
	}
	posts, err := chain.ReadApprovalRecords[chain.ApprovalPost](a.approvalDirectory(), "post")
	if err != nil || len(posts) != 1 {
		t.Fatalf("posts=%v err=%v", posts, err)
	}
	p := posts[0]
	bundle, err := chain.ReadApprovalBundle(p.BundlePath(a.approvalDirectory()))
	if err != nil || string(bundle.Contents["nested/summary.txt"]) != summary || bundle.Digest != p.Digest || p.SentTextSHA256 != chain.ApprovalSHA256([]byte(text)) {
		t.Fatal("summary or digest not preserved", err)
	}
	raw, err := os.ReadFile(filepath.Join(a.approvalDirectory(), "post-1.json"))
	if err != nil || !strings.Contains(string(raw), `"deadline":"`+p.Deadline.UTC().Format(time.RFC3339)+`"`) {
		t.Fatalf("deadline not UTC: %s %v", raw, err)
	}
}

func TestApprovalSummaryUnset(t *testing.T) {
	_, f, _, p := approvalPostFixture(t)
	text := f.all()[0]
	if strings.Contains(text, "やること:") || !strings.Contains(text, p.Deadline.Format("2006-01-02 15:04 (MST)")) {
		t.Fatal(text)
	}
	for _, internal := range []string{p.Digest, "アカウント id", "コメント id", "本体の時計", "apply", "dry_run"} {
		if strings.Contains(text, internal) {
			t.Fatalf("internal text: %s", internal)
		}
	}
}

func TestApprovalSummaryUnreadable(t *testing.T) {
	for _, tc := range []struct{ name, content string }{
		{"missing", ""}, {"empty", " \n\t"}, {"long", strings.Repeat("日", 2001)},
		{"fence", "説明\n```text\n内容\n```"}, {"heading", "説明\n# 見出し"},
		{"utf8", string([]byte{0xff})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, f, s := approvalFixture(t)
			approvalDisplaySettings(t, s.Workflow.Stages[2].Approval, "", "ops/bundle/summary.txt")
			if tc.name != "missing" {
				if err := os.WriteFile(filepath.Join(a.cfg.Roles[0].Processes[0].Directory, "ops/bundle/summary.txt"), []byte(tc.content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := a.before(ctx, &s, chain.Assignment{Role: "c"})
			var failure chain.ApprovalFailure
			if !errors.As(err, &failure) || failure != "summary_unreadable" || s.AwaitingApproval != nil || s.Pending != nil || len(f.all()) != 0 {
				t.Fatalf("failure=%v state=%+v posts=%v", err, s, f.all())
			}
			record := chain.ApprovalFailureRecord("c", string(failure))
			if chain.ApprovalFailureReason(record) != "summary_unreadable" {
				t.Fatal("unrecognized failure")
			}
			want := "確認の途中で止まりました。理由: やることの説明文が無いか、読めません。"
			if got := endingText(a.cfg, chain.Ending{ApprovalFailure: string(failure), Error: record.Error}); got != want {
				t.Fatalf("got=%s", got)
			}
		})
	}
}

func TestApprovalPreviousExecutionWording(t *testing.T) {
	stage := approvalConfiguration().Workflow.Stages[2]
	for _, tc := range []struct {
		name string
		rows []chain.Result
		want string
	}{
		{"none", nil, "まだ実行していません。"},
		{"interrupted", []chain.Result{{Role: "c", Speaker: "process", Interrupted: true}, {Role: "c", Speaker: "runtime"}}, "途中で止まり、結果が分かりません。同じ計画で実行してよいか、もう一度確認します。"},
		{"failed", []chain.Result{{Role: "c", Speaker: "process", Error: "exit status 1"}, {Role: "c", Speaker: "runtime"}}, "失敗しました。同じ計画で実行してよいか、もう一度確認します。"},
		{"success", []chain.Result{{Role: "c", Speaker: "process"}, {Role: "c", Speaker: "runtime"}}, "正常に終わりました。同じ計画でもう一度実行してよいか確認します。"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A later approval refusal must not replace the actual execution outcome.
			rows := append(tc.rows, chain.ApprovalFailureRecord("c", "rejected"))
			got, err := approvalPostText(stage, chain.ApprovalBundle{}, chain.ApprovalPost{}, chain.State{History: rows}, nil)
			if err != nil || !strings.Contains(got, "前回の実行: "+tc.want+"\n") {
				t.Fatalf("%s %v", got, err)
			}
		})
	}
}

func TestApprovalSummaryMaskedText(t *testing.T) {
	stage := approvalConfiguration().Workflow.Stages[2]
	stage.Approval.Summary = "ops/bundle/summary.txt"
	for _, text := range []string{"synthetic/value", "synthetic%2Fvalue"} {
		bundle := chain.ApprovalBundle{Contents: map[string][]byte{"summary.txt": []byte(text)}}
		_, err := approvalPostText(stage, bundle, chain.ApprovalPost{}, chain.State{}, []string{"synthetic/value"})
		var failure chain.ApprovalFailure
		if !errors.As(err, &failure) || failure != "post_masked" {
			t.Fatalf("got %v", err)
		}
	}
}

func TestApprovalPauseNoticeLanguage(t *testing.T) {
	for _, reason := range []string{"approval_expired", "approval_post_mismatch", "approval_tampered", "approval_no_approver", "approval_config_mismatch"} {
		text := pausedWorkText(pauseEpisode{Reason: reason}, nil, nil)
		if !strings.HasPrefix(text, "確認の途中で止まりました。理由: ") || !strings.Contains(text, "停止") {
			t.Fatal(text)
		}
		if reason == "approval_expired" && !strings.Contains(text, "再開") {
			t.Fatal("missing resume instruction")
		}
	}
}
