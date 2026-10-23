package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"ticket-runner/internal/chain"
	"ticket-runner/internal/tracker"
)

func approvalFixture(t *testing.T) (approvalChild, *noticeTracker, chain.State) {
	t.Helper()
	cfg := approvalConfiguration()
	cfg.Backlog = watchConfiguration(t).Backlog
	if err := validateApprovalStages(cfg); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	for i := range cfg.Roles {
		cfg.Roles[i].Processes[0].Directory = workspace
		cfg.Roles[i].Processes[0].Env = map[string]string{"TASK_WORKSPACE": workspace}
	}
	state := chain.State{Request: noticeRequest, Workflow: cfg.Workflow, Step: "b", History: []chain.Result{{Role: "b", Speaker: "runtime", FinishedAt: time.Now().UTC().Add(-time.Hour)}}}
	_, directory := noticeJob(t, state)
	cfg.recordComments = true
	cfg.commentJobs = filepath.Dir(directory)
	native := watchedIssue(51, "Original conditions", "2026-01-03T00:00:00Z")
	native["updated"] = "2026-01-03T00:00:00Z"
	raw, _ := json.Marshal(native)
	if err := writeRuntimeFile(filepath.Join(directory, "issue.json"), raw); err != nil {
		t.Fatal(err)
	}
	source := cfg.source()
	issue, err := source.ReadIssue(raw)
	if err != nil {
		t.Fatal(err)
	}
	fake := &noticeTracker{approvalIssue: native, approvalRule: "markdown", rows: []json.RawMessage{seenTestRaw(800, 55, "initial", 0)}}
	fake.install(t, func(r *http.Request) (*http.Response, error) { return nil, fmt.Errorf("unexpected test request") })
	child := approvalChild{cfg: cfg, source: source, issue: issue, directory: directory, retry: time.Millisecond}
	if _, err := source.Comments(context.Background(), issue); err != nil {
		t.Fatal(err)
	}
	if err = child.notice().save(noticeLog{}); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"dry-run.txt": "checked\n", "plan.txt": "proposed\n"} {
		path := filepath.Join(workspace, "ops/bundle", name)
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return child, fake, state
}
func approvalPostFixture(t *testing.T) (approvalChild, *noticeTracker, chain.State, chain.ApprovalPost) {
	t.Helper()
	a, f, s := approvalFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := a.before(ctx, &s, chain.Assignment{Role: "c"}); !errors.Is(err, chain.ErrAwaitingApproval) {
		t.Fatal(err)
	}
	posts, err := chain.ReadApprovalRecords[chain.ApprovalPost](a.approvalDirectory(), "post")
	if err != nil || len(posts) != 1 {
		t.Fatalf("posts=%v err=%v", posts, err)
	}
	return a, f, s, posts[0]
}
func approvalEditRow(t *testing.T, f *noticeTracker, id int64, change func(map[string]any)) {
	t.Helper()
	f.set(func(f *noticeTracker) {
		for i, raw := range f.rows {
			var row map[string]any
			if err := json.Unmarshal(raw, &row); err != nil {
				t.Fatal(err)
			}
			if int64(row["id"].(float64)) == id {
				change(row)
				f.rows[i], _ = json.Marshal(row)
				return
			}
		}
		t.Fatalf("missing row %d", id)
	})
}
func approvalAdd(t *testing.T, f *noticeTracker, id, author int64, body string, change func(map[string]any)) {
	t.Helper()
	row := map[string]any{"id": id, "issueId": 51, "projectId": 17, "createdUser": map[string]any{"id": author}, "content": body, "created": time.Now().UTC().Format(time.RFC3339Nano), "updated": time.Now().UTC().Format(time.RFC3339Nano)}
	row["updated"] = row["created"]
	if change != nil {
		change(row)
	}
	raw, _ := json.Marshal(row)
	f.set(func(f *noticeTracker) { f.rows = append(f.rows, raw) })
}
func TestApprovalChildPostFullTextAndRestart(t *testing.T) {
	a, f, s, p := approvalPostFixture(t)
	text := f.all()[0]
	for _, want := range []string{"## 試し実行の結果\n\n```\ndry-run.txt\nchecked\n```", "## AI が書いた計画\n\n```\nplan.txt\nproposed\n```", "GO " + p.DigestPrefix, p.Deadline.Format("2006-01-02 15:04 (MST)"), "前回の実行: まだ実行していません。", "GO を消したり書き直したり", "確認待ちの間に承認者が GO・停止・再開以外", "最初の確認の投稿より後は、確認待ちかどうかに関わらず"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q", want)
		}
	}
	if s.AwaitingApproval == nil || s.Waiting || p.SentTextSHA256 != chain.ApprovalSHA256([]byte(text)) {
		t.Fatal("bad checkpoint")
	}
	// Re-entering after a crash between post confirmation and state checkpoint
	// reuses the existing bundle and post. Editing the workspace cannot replace it.
	s.AwaitingApproval = nil
	if err := a.before(context.Background(), &s, chain.Assignment{Role: "c"}); !errors.Is(err, chain.ErrAwaitingApproval) {
		t.Fatal(err)
	}
	if len(f.all()) != 1 {
		t.Fatal("duplicate post")
	}
	// The next attempt has its own event even within the normal notice interval.
	s.History = append(s.History, chain.ApprovalFailureRecord("c", "bundle_limit"))
	s.AwaitingApproval = nil
	if err := a.before(context.Background(), &s, chain.Assignment{Role: "c"}); !errors.Is(err, chain.ErrAwaitingApproval) {
		t.Fatal(err)
	}
	if len(f.all()) != 2 || s.AwaitingApproval.PostN != 2 {
		t.Fatal("second post suppressed")
	}
}
func TestApprovalChildPostFailures(t *testing.T) {
	for _, variant := range []string{"body", "author", "updated", "400", "403", "413", "429"} {
		t.Run(variant, func(t *testing.T) {
			a, f, s := approvalFixture(t)
			switch variant {
			case "body":
				f.approvalPostChange = func(r map[string]any) { r["content"] = "changed" }
			case "author":
				f.approvalPostChange = func(r map[string]any) { r["createdUser"] = map[string]any{"id": 7} }
			case "updated":
				f.approvalPostChange = func(r map[string]any) { r["updated"] = "2026-01-01T00:00:00Z" }
			case "400":
				f.approvalPostStatuses = []int{400}
			case "403":
				f.approvalPostStatuses = []int{403}
			case "413":
				f.approvalPostStatuses = []int{413}
			case "429":
				f.approvalPostStatuses = []int{429, 201}
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := a.before(ctx, &s, chain.Assignment{Role: "c"})
			if variant == "429" {
				if !errors.Is(err, chain.ErrAwaitingApproval) || f.attempts() != 2 {
					t.Fatalf("%v attempts=%d", err, f.attempts())
				}
				return
			}
			if variant == "400" || variant == "403" || variant == "413" {
				if err == nil || !strings.Contains(err.Error(), "post_size") {
					t.Fatal(err)
				}
				log, _ := a.notice().load()
				if len(log.Notices) != 1 || !log.Notices[0].Obsolete || pendingNotice(log) >= 0 {
					t.Fatal("refusal blocks notices")
				}
			} else if !errors.Is(err, chain.ErrApprovalPause) {
				t.Fatal(err)
			}
			posts, _ := chain.ReadApprovalRecords[chain.ApprovalPost](a.approvalDirectory(), "post")
			if len(posts) != 0 || s.AwaitingApproval != nil {
				t.Fatal("unverified post usable")
			}
		})
	}
}
func TestApprovalChildComparisonConditions(t *testing.T) {
	tests := []struct {
		name   string
		change func(*testing.T, approvalChild, *noticeTracker, *chain.State, chain.ApprovalPost)
	}{
		{"notices_missing", func(t *testing.T, a approvalChild, f *noticeTracker, s *chain.State, p chain.ApprovalPost) {
			if err := os.Remove(a.notice().path()); err != nil {
				t.Fatal(err)
			}
		}},
		{"deleted", func(t *testing.T, a approvalChild, f *noticeTracker, s *chain.State, p chain.ApprovalPost) {
			f.set(func(f *noticeTracker) { f.rows = f.rows[1:] })
		}},
		{"body", func(t *testing.T, a approvalChild, f *noticeTracker, s *chain.State, p chain.ApprovalPost) {
			approvalEditRow(t, f, 800, func(r map[string]any) { r["content"] = "edited" })
		}},
		{"updated", func(t *testing.T, a approvalChild, f *noticeTracker, s *chain.State, p chain.ApprovalPost) {
			approvalEditRow(t, f, 800, func(r map[string]any) { r["updated"] = "2026-02-01T00:00:00Z" })
		}},
		{"author", func(t *testing.T, a approvalChild, f *noticeTracker, s *chain.State, p chain.ApprovalPost) {
			approvalEditRow(t, f, 800, func(r map[string]any) { r["createdUser"] = map[string]any{"id": 8} })
		}},
		{"sent_body", func(t *testing.T, a approvalChild, f *noticeTracker, s *chain.State, p chain.ApprovalPost) {
			approvalEditRow(t, f, p.CommentID, func(r map[string]any) { r["content"] = "edited" })
		}},
		{"sent_updated", func(t *testing.T, a approvalChild, f *noticeTracker, s *chain.State, p chain.ApprovalPost) {
			approvalEditRow(t, f, p.CommentID, func(r map[string]any) { r["updated"] = "2026-02-01T00:00:00Z" })
		}},
		{"request", func(t *testing.T, a approvalChild, f *noticeTracker, s *chain.State, p chain.ApprovalPost) {
			f.approvalIssue["description"] = "changed"
		}},
		{"title", func(t *testing.T, a approvalChild, f *noticeTracker, s *chain.State, p chain.ApprovalPost) {
			f.approvalIssue["summary"] = "changed"
		}},
		{"title_history", func(t *testing.T, a approvalChild, f *noticeTracker, s *chain.State, p chain.ApprovalPost) {
			approvalAdd(t, f, 950, 99, "", func(r map[string]any) {
				r["changeLog"] = []any{map[string]any{"field": "summary", "originalValue": "old", "newValue": "new"}}
			})
		}},
		{"description_history_with_body", func(t *testing.T, a approvalChild, f *noticeTracker, s *chain.State, p chain.ApprovalPost) {
			approvalAdd(t, f, 950, 7, "GO invalid", func(r map[string]any) {
				r["changeLog"] = []any{map[string]any{"field": "description", "originalValue": "old", "newValue": "new"}}
			})
		}},
		{"own_unrecorded", func(t *testing.T, a approvalChild, f *noticeTracker, s *chain.State, p chain.ApprovalPost) {
			approvalAdd(t, f, 950, 99, "correction", nil)
		}},
		{"third_party", func(t *testing.T, a approvalChild, f *noticeTracker, s *chain.State, p chain.ApprovalPost) {
			approvalAdd(t, f, 950, 88, "correction", nil)
		}},
		{"attachment", func(t *testing.T, a approvalChild, f *noticeTracker, s *chain.State, p chain.ApprovalPost) {
			approvalAdd(t, f, 950, 88, "", func(r map[string]any) { r["changeLog"] = []any{map[string]any{"field": "attachment"}} })
		}},
		{"format", func(t *testing.T, a approvalChild, f *noticeTracker, s *chain.State, p chain.ApprovalPost) {
			f.approvalRule = "backlog"
		}},
		{"seen_missing", func(t *testing.T, a approvalChild, f *noticeTracker, s *chain.State, p chain.ApprovalPost) {
			os.Remove(filepath.Join(a.directory, "comments-seen.json"))
			f.set(func(f *noticeTracker) { f.rows = nil })
		}},
		{"seen_unreadable", func(t *testing.T, a approvalChild, f *noticeTracker, s *chain.State, p chain.ApprovalPost) {
			os.WriteFile(filepath.Join(a.directory, "comments-seen.json"), []byte("bad"), 0600)
		}},
		{"post_record_unreadable", func(t *testing.T, a approvalChild, f *noticeTracker, s *chain.State, p chain.ApprovalPost) {
			os.WriteFile(filepath.Join(a.approvalDirectory(), "post-1.json"), []byte("bad"), 0600)
		}},
		{"timestamps_missing", func(t *testing.T, a approvalChild, f *noticeTracker, s *chain.State, p chain.ApprovalPost) {
			approvalEditRow(t, f, 800, func(r map[string]any) { delete(r, "created"); delete(r, "updated") })
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, f, s, p := approvalPostFixture(t)
			tt.change(t, a, f, &s, p)
			_, err := a.read(context.Background(), s, "c")
			if !errors.Is(err, errApprovalTampered) {
				t.Fatalf("want comparison mismatch, got %v", err)
			}
			if ready, err := a.parentTick(context.Background(), &s); err != nil || ready || s.ApprovalPaused() != "approval_tampered" {
				t.Fatalf("parent comparison: ready=%t pause=%s err=%v", ready, s.ApprovalPaused(), err)
			}
		})
	}
}
func TestApprovalChildStopAndPendingBeforeComparison(t *testing.T) {
	a, f, s, _ := approvalPostFixture(t)
	approvalEditRow(t, f, 800, func(r map[string]any) { r["content"] = "changed" })
	log, _ := a.notice().load()
	log.Notices = append(log.Notices, noticeRecord{Kind: startedNotice, Text: "pending"})
	if err := a.notice().save(log); err != nil {
		t.Fatal(err)
	}
	if _, err := a.read(context.Background(), s, "c"); !errors.Is(err, errApprovalRetry) {
		t.Fatal(err)
	}
	approvalAdd(t, f, 999, 7, "停止\nreason", nil)
	if _, err := a.read(context.Background(), s, "c"); !errors.Is(err, errApprovalStopped) {
		t.Fatal(err)
	}
}
func TestApprovalChildNoticeLockAfterCommentRead(t *testing.T) {
	a, f, s, _ := approvalPostFixture(t)
	once := false
	f.approvalReadHook = func() {
		if once {
			return
		}
		once = true
		n := a.notice()
		release := n.hold()
		defer release()
		log, _ := n.load()
		log.Notices = append(log.Notices, noticeRecord{Kind: startedNotice, Text: "watcher just recorded this"})
		if err := n.save(log); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.read(context.Background(), s, "c"); !errors.Is(err, errApprovalRetry) {
		t.Fatal(err)
	}
}
func TestApprovalChildApproverExclusions(t *testing.T) {
	for _, variant := range []string{"ordinary_before", "go_before", "creator", "runtime", "removed", "control_before", "ordinary_after", "same_second_change", "empty_runtime", "duplicate"} {
		t.Run(variant, func(t *testing.T) {
			a, f, s := approvalFixture(t)
			switch variant {
			case "ordinary_before":
				approvalAdd(t, f, 850, 7, "ordinary", nil)
			case "go_before":
				approvalAdd(t, f, 850, 7, "GO 12345678", nil)
			case "creator":
				a.issue.Creator.ID = 7
			case "runtime":
				f.me = 7
			case "removed":
				a.cfg.Workflow.Stages[2].Approval = nil
			case "control_before":
				approvalAdd(t, f, 850, 7, "再開\nreason", nil)
			case "same_second_change":
				approvalAdd(t, f, 850, 7, "再開", func(r map[string]any) {
					r["created"] = "2026-01-03T00:00:00Z"
					r["updated"] = r["created"]
					r["changeLog"] = []any{map[string]any{"field": "description"}}
				})
			}
			_, err := a.read(context.Background(), s, "c")
			if variant == "ordinary_before" || variant == "go_before" || variant == "creator" || variant == "runtime" || variant == "removed" {
				if !errors.Is(err, errApprovalNoApprover) {
					t.Fatal(err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = a.before(context.Background(), &s, chain.Assignment{Role: "c"}); !errors.Is(err, chain.ErrAwaitingApproval) {
				t.Fatal(err)
			}
			first := s.AwaitingApproval.PostCommentID
			switch variant {
			case "ordinary_after":
				approvalAdd(t, f, 999, 7, "please change", nil)
			case "empty_runtime":
				approvalAdd(t, f, 999, 99, "", nil)
			case "duplicate":
				approvalAdd(t, f, 999, 99, f.all()[0], nil)
			}
			v, err := a.read(context.Background(), s, "c")
			if err != nil {
				t.Fatal(err)
			}
			if v.boundary != first {
				t.Fatal("duplicate moved boundary")
			}
		})
	}
}
func approvalAcceptedGO(t *testing.T, a approvalChild, f *noticeTracker, s chain.State, p chain.ApprovalPost) chain.ApprovalGO {
	t.Helper()
	now := time.Now().UTC()
	g := chain.ApprovalGO{Result: "go", PostN: p.N, PostCommentID: p.CommentID, GOCommentID: 950, AuthorID: 7, Body: "GO " + p.DigestPrefix, AcceptedAt: now, Digest: p.Digest, LaunchHistoryLen: len(s.History) + 1, PendingSince: now}
	approvalAdd(t, f, g.GOCommentID, g.AuthorID, g.Body, nil)
	if err := g.Save(a.approvalDirectory()); err != nil {
		t.Fatal(err)
	}
	return g
}
func TestApprovalChildConsumptionConditions(t *testing.T) {
	for _, variant := range []string{"valid", "old_post", "digest", "deadline", "position", "zero_time", "ordinary", "resume", "pause", "approver_removed", "copy_changed", "comparison", "go_deleted", "go_edited", "acceptance_row"} {
		t.Run(variant, func(t *testing.T) {
			a, f, s, p := approvalPostFixture(t)
			g := approvalAcceptedGO(t, a, f, s, p)
			switch variant {
			case "acceptance_row":
				s.History = append(s.History, chain.Result{Role: "router", Speaker: "runtime", Output: "承認の GO を受理した", FinishedAt: g.AcceptedAt.Add(time.Millisecond)})
				g.LaunchHistoryLen = len(s.History) + 1
			case "old_post":
				g.PostCommentID--
			case "digest":
				g.Digest = strings.Repeat("0", 64)
			case "deadline":
				a.now = func() time.Time { return p.Deadline }
			case "position":
				g.LaunchHistoryLen++
			case "zero_time":
				g.PendingSince = time.Time{}
			case "ordinary":
				approvalAdd(t, f, 999, 7, "change it", nil)
			case "resume":
				approvalAdd(t, f, 999, 7, "再開", nil)
			case "pause":
				s.History = append(s.History, chain.Result{Role: "router", Speaker: "runtime", Output: "paused", FinishedAt: time.Now().UTC()})
			case "approver_removed":
				a.cfg.Workflow.Stages[2].Approval.ApproverUserIDs = []int64{8}
			case "copy_changed":
				os.WriteFile(filepath.Join(p.BundlePath(a.approvalDirectory()), "plan.txt"), []byte("changed"), 0600)
			case "comparison":
				approvalEditRow(t, f, 800, func(r map[string]any) { r["content"] = "changed" })
			case "go_deleted":
				f.set(func(f *noticeTracker) { f.rows = f.rows[:len(f.rows)-1] })
			case "go_edited":
				approvalEditRow(t, f, g.GOCommentID, func(r map[string]any) { r["content"] = "changed"; r["updated"] = "2026-02-01T00:00:00Z" })
			}
			if err := g.Save(a.approvalDirectory()); err != nil {
				t.Fatal(err)
			}
			s.AwaitingApproval = nil
			err := a.before(context.Background(), &s, chain.Assignment{Role: "c"})
			gos, readErr := chain.ReadApprovalRecords[chain.ApprovalGO](a.approvalDirectory(), "go")
			if readErr != nil || len(gos) != 1 {
				t.Fatal(readErr)
			}
			valid := variant == "valid" || variant == "go_deleted" || variant == "go_edited" || variant == "acceptance_row"
			if valid {
				if err != nil || gos[0].ConsumedAt.IsZero() || s.PendingSince != g.PendingSince || len(s.History) != g.LaunchHistoryLen {
					t.Fatalf("err=%v GO=%+v state=%+v", err, gos[0], s)
				}
				if ok, err := a.consume(&s, p, approvalView{gos: gos}, nil); err != nil || ok {
					t.Fatal("GO reused")
				}
			} else if err == nil || gos[0].ExpiredAt.IsZero() || !gos[0].ConsumedAt.IsZero() {
				t.Fatalf("err=%v GO=%+v", err, gos[0])
			}
		})
	}
}
func TestApprovalFailureNoticeAndQuestionIsolation(t *testing.T) {
	cfg := approvalConfiguration()
	text := endingText(cfg, chain.Ending{Role: "c", ApprovalFailure: "post_fence", Error: chain.ApprovalFailureText("post_fence")})
	if strings.Contains(text, "通りません") || strings.Contains(text, "post_fence") || !strings.Contains(text, "確認のコメントに使えない文字や書式") {
		t.Fatal(text)
	}
	state := chain.State{Waiting: true, AwaitingApproval: &chain.AwaitingApproval{Stage: "c"}}
	if resume, answered, err := resumeWaitingRequest(context.Background(), cfg, sourceIssue{}, t.TempDir(), "", state, time.Millisecond); err != nil || resume || answered {
		t.Fatal("question released approval")
	}
}

func TestApprovalFailureEndingTextPreservesMeaning(t *testing.T) {
	for _, tc := range []struct{ name, text, posted string }{
		{"receipt_credential", "起動した・結果は読めない (効果不明): bundle_credential", "実行を開始しましたが、結果が分かりません。理由: 確認用のファイルに認証情報が含まれていました"},
		{"bundle_credential", "承認条件が成立しなかった: bundle_credential", "確認の途中で止まりました。理由: 確認用のファイルに認証情報が含まれていました"},
		{"receipt_unreadable", "起動した・結果は読めない (効果不明): receipt_unreadable", "実行を開始しましたが、結果が分かりません。理由: 実行結果を読み取れませんでした"},
		{"post_fence", "承認条件が成立しなかった: post_fence", "確認の途中で止まりました。理由: 確認のコメントに使えない文字や書式が含まれています"},
		{"rejected", "承認条件が成立しなかった: rejected", "確認の途中で止まりました。理由: 承認者から修正を求められました"},
		{"expired", "承認条件が成立しなかった: expired", "確認の途中で止まりました。理由: 確認の期限が切れました"},
		{"bundle_name", "承認条件が成立しなかった: bundle_name", "確認の途中で止まりました。理由: 確認用のファイルが決まりに合いません (名前)"},
		{"bundle_kind", "承認条件が成立しなかった: bundle_kind", "確認の途中で止まりました。理由: 確認用のファイルが決まりに合いません (種類)"},
		{"bundle_limit", "承認条件が成立しなかった: bundle_limit", "確認の途中で止まりました。理由: 確認用のファイルが決まりに合いません (量や形式)"},
		{"show_missing", "承認条件が成立しなかった: show_missing", "確認の途中で止まりました。理由: 試し実行の結果のファイルがありません"},
		{"post_size", "承認条件が成立しなかった: post_size", "確認の途中で止まりました。理由: 確認のコメントが長すぎるか、投稿できませんでした"},
		{"post_masked", "承認条件が成立しなかった: post_masked", "確認の途中で止まりました。理由: 確認のコメントに認証情報が含まれています"},
		{"digest_collision", "承認条件が成立しなかった: digest_collision", "確認の途中で止まりました。理由: 確認の対象を区別できませんでした"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := approvalConfiguration()
			state := chain.State{Workflow: cfg.Workflow, History: []chain.Result{{Role: "c", Speaker: "p"}, {Role: "c", Speaker: "runtime"}, {Role: "c", Speaker: "runtime", Error: tc.text}}}
			ending, ok := state.LastEnding()
			if !ok {
				t.Fatal("missing ending")
			}
			want := tc.posted + "。"
			if text := endingText(cfg, ending); text != want {
				t.Fatalf("ending = %q, want %q", text, want)
			}
			text, _ := rerunNoticeText(cfg, t.TempDir(), "a", state.History, "test-model")
			if text != want+"作業をやり直します。（モデル: test-model）" {
				t.Fatalf("rerun notice = %q, want prefix %q", text, want)
			}
			stage := cfg.Workflow.Stages[2]
			posted, err := approvalPostText(stage, chain.ApprovalBundle{}, chain.ApprovalPost{}, state, nil, nil)
			previous := "正常に終わりました。もう一度実行してよいか確認します。"
			if strings.HasPrefix(tc.text, "起動した・結果は読めない") {
				previous = "途中で止まり、結果が分かりません。もう一度実行してよいか確認します。"
			}
			if err != nil || !strings.Contains(posted, "前回の実行: "+previous+"\n") || strings.Contains(posted, tc.text) {
				t.Fatalf("post = %q, err = %v", posted, err)
			}
			if state.History[len(state.History)-1].Error != tc.text {
				t.Fatal("posted wording changed the saved failure")
			}
		})
	}
}

func TestApprovalAwaitingWatcherReturnsSlotWithoutUnfinished(t *testing.T) {
	cfg := watchConfiguration(t)
	state := chain.State{AwaitingApproval: &chain.AwaitingApproval{Stage: "c", PostN: 1, PostCommentID: 901, Deadline: time.Now().Add(time.Hour)}}
	_, directory := noticeJob(t, state)
	if err := os.MkdirAll(filepath.Join(directory, "workspace"), 0700); err != nil {
		t.Fatal(err)
	}
	bound, err := bindRequestConfig(cfg, directory, "EXAMPLE-51")
	if err != nil {
		t.Fatal(err)
	}
	configPath, requestPath := filepath.Join(directory, "engine.json"), filepath.Join(directory, "request.txt")
	if err = writeRuntimeFile(configPath, configurationJSON(t, bound)); err != nil {
		t.Fatal(err)
	}
	if err = writeRuntimeFile(requestPath, []byte(noticeRequest)); err != nil {
		t.Fatal(err)
	}
	fixture := &noticeTracker{}
	fixture.install(t, alwaysChoose("implement"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var log bytes.Buffer
	turns := newTurnstile(1)
	issue := announcedIssue()
	issue.Creator.ID = 55
	err = runWatchedRequest(ctx, cfg, issue, directory, configPath, requestPath, 20*time.Millisecond, turns, &log)
	if err != nil || strings.Contains(log.String(), "remains unfinished") {
		t.Fatalf("err=%v log=%s", err, &log)
	}
	if !turns.try(52) {
		t.Fatal("execution slot retained")
	}
	turns.release()
	if state := loadJobState(t, directory); state.AwaitingApproval == nil || state.Done {
		t.Fatal("wait was cleared")
	}
}
func TestApprovalChildReadbackPrecedesGO(t *testing.T) {
	a, f, s := approvalFixture(t)
	f.approvalPostChange = func(row map[string]any) { row["content"] = "changed" }
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := a.before(ctx, &s, chain.Assignment{Role: "c"})
	if !errors.Is(err, chain.ErrApprovalPause) {
		t.Fatal(err)
	}
	if s.Pending != nil || s.AwaitingApproval != nil {
		t.Fatal("readback failure authorized progress")
	}
}
func TestApprovalChildPostSizeAndMaskedText(t *testing.T) {
	a, _, s := approvalFixture(t)
	stage, _, _ := s.ApprovalStage("c")
	post := chain.ApprovalPost{Digest: strings.Repeat("a", 64), DigestPrefix: "aaaaaaaa", Deadline: time.Now().Add(time.Hour)}
	for _, variant := range []string{"size", "masked"} {
		t.Run(variant, func(t *testing.T) {
			contents := "safe\n"
			credentials := []string(nil)
			if variant == "size" {
				contents = strings.Repeat("x\n", chain.ApprovalBytes/2)
			} else {
				credentials = []string{"safe"}
			}
			bundle := chain.ApprovalBundle{Files: []chain.ApprovalFile{{Path: "dry-run.txt"}}, Contents: map[string][]byte{"dry-run.txt": []byte(contents)}}
			_, err := approvalPostText(stage, bundle, post, s, credentials, nil)
			if err == nil || !strings.Contains(err.Error(), "post_"+variant) {
				t.Fatal(err)
			}
		})
	}
	_ = a
}
func TestApprovalChildSentNoticeComparisonAndDuplicates(t *testing.T) {
	for _, variant := range []string{"valid", "edited_before_first_read", "duplicate", "impostor"} {
		t.Run(variant, func(t *testing.T) {
			a, f, s, _ := approvalPostFixture(t)
			now := time.Now().UTC()
			n := a.notice()
			log, _ := n.load()
			log.Notices = append(log.Notices, noticeRecord{Kind: startedNotice, Text: "runtime fixed text", CommentID: 960, PostedAt: &now})
			if err := n.save(log); err != nil {
				t.Fatal(err)
			}
			body := "runtime fixed text"
			author := int64(99)
			if variant == "edited_before_first_read" {
				body = "edited"
			}
			if variant == "impostor" {
				author = 8
			}
			approvalAdd(t, f, 960, author, body, nil)
			if variant == "duplicate" {
				approvalAdd(t, f, 970, 99, body, nil)
			}
			_, err := a.read(context.Background(), s, "c")
			if variant == "valid" || variant == "duplicate" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, errApprovalTampered) {
				t.Fatal(err)
			}
		})
	}
}
func TestApprovalChildStopsWithDamagedRecords(t *testing.T) {
	a, f, s, _ := approvalPostFixture(t)
	if err := os.WriteFile(filepath.Join(a.directory, "comments-seen.json"), []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	approvalAdd(t, f, 999, 7, "停止", nil)
	if _, err := a.read(context.Background(), s, "c"); !errors.Is(err, errApprovalStopped) {
		t.Fatal(err)
	}
}

func TestApprovalNotificationsAreNewComments(t *testing.T) {
	cfg := declaringConfig(t)
	cfg.Workflow.Stages[len(cfg.Workflow.Stages)-1].Approval = &chain.Approval{Bundle: "bundle", Show: []string{"show"}, ExpiryHours: 72}
	fixture := &noticeTracker{approvalIssue: watchedIssue(51, "Original conditions", "2026-01-03T00:00:00Z"), approvalRule: "markdown"}
	fixture.install(t, alwaysChoose("done"))
	directory, began := declaringJob(t, cfg)
	at := time.Now().UTC()
	for i := 0; i < 3; i++ {
		var history []chain.Result
		for j := 0; j < i; j++ {
			history = append(history, forcedNote("elicit", at))
		}
		writeJobHistory(t, directory, chain.State{History: history})
		launched(t, directory, "elicit", i, "maker/test")
		declareModels(context.Background(), cfg, announcedIssue(), directory, began, func(s string) { t.Log(s) })
	}
	if len(fixture.rewritten()) != 0 || len(fixture.all()) != 3 {
		t.Fatalf("posts=%d edits=%d", len(fixture.all()), len(fixture.rewritten()))
	}
}
func TestApprovalRefusalDoesNotAnnounceStageLaunch(t *testing.T) {
	cfg := announcingStagesConfig(t)
	_, directory := noticeJob(t, chain.State{History: []chain.Result{chain.ApprovalFailureRecord("verify", "post_size")}})
	fixture := &noticeTracker{}
	fixture.install(t, alwaysChoose("done"))
	announceStages(context.Background(), cfg, announcedIssue(), directory, func(string) {})
	if len(fixture.all()) != 0 {
		t.Fatal("refusal announced as launch")
	}
}
func TestApprovalTestBypassCannotBeConfigured(t *testing.T) {
	if _, err := readConfig([]byte(`{"allowApprovalForTest":true}`)); err == nil {
		t.Fatal("test flag accepted as configuration")
	}
	cfg := approvalConfiguration()
	if err := validateApprovalStages(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestApprovalChildStopsOnTheFinalConsumptionRead(t *testing.T) {
	a, f, s, p := approvalPostFixture(t)
	approvalAcceptedGO(t, a, f, s, p)
	s.AwaitingApproval = nil
	reads := 0
	f.approvalReadHook = func() {
		reads++
		if reads == 2 {
			approvalAdd(t, f, 999, 7, "停止", nil)
		}
	}
	if err := a.before(context.Background(), &s, chain.Assignment{Role: "c"}); !errors.Is(err, errApprovalStopped) {
		t.Fatal(err)
	}
	records, err := chain.ReadApprovalRecords[chain.ApprovalGO](a.approvalDirectory(), "go")
	if err != nil || records[0].ExpiredAt.IsZero() || !records[0].ConsumedAt.IsZero() {
		t.Fatal("GO survived stop")
	}
}
func TestApprovalChildRunsThroughEngineEntry(t *testing.T) {
	a, f, s := approvalFixture(t)
	s.History = append([]chain.Result{{Role: "a", Speaker: "runtime"}}, s.History...)
	a.cfg.ModelSelection = &selectionConfig{Fixed: "maker/test"}
	writeJobHistory(t, a.directory, s)
	configPath := filepath.Join(a.directory, "engine.json")
	requestPath := filepath.Join(a.directory, "request.txt")
	a.cfg.AssignedIssue = a.issue.Key
	if err := writeRuntimeFile(configPath, configurationJSON(t, a.cfg)); err != nil {
		t.Fatal(err)
	}
	if err := writeRuntimeFile(requestPath, []byte(noticeRequest)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	f.approvalReadHook = func() {
		if err := os.WriteFile(configPath, []byte("changed after configuration was loaded"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx = context.WithValue(ctx, acceptedTimeFactsKey{}, requestTimeFacts(a.directory))
	var log bytes.Buffer
	err := run(ctx, []string{"--config", configPath, "--request", requestPath, "--run-dir", filepath.Join(a.directory, "run")}, io.Discard, &log)
	if !errors.Is(err, chain.ErrAwaitingApproval) {
		t.Fatalf("err=%v log=%s", err, &log)
	}
	if saved := loadJobState(t, a.directory); saved.AwaitingApproval == nil || saved.Pending != nil {
		t.Fatal("entry did not checkpoint approval wait")
	}
}

func TestApprovalChildFirstPostWithoutNotices(t *testing.T) {
	a, f, s := approvalFixture(t)
	if err := os.Remove(a.notice().path()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := a.before(ctx, &s, chain.Assignment{Role: "c"}); !errors.Is(err, chain.ErrAwaitingApproval) || f.attempts() != 1 || s.AwaitingApproval == nil {
		t.Fatalf("err=%v posts=%d", err, f.attempts())
	}
}

func TestApprovalChildPostRecordRecoveryDoesNotRepost(t *testing.T) {
	a, f, s, p := approvalPostFixture(t)
	if err := os.Remove(filepath.Join(a.approvalDirectory(), "post-1.json")); err != nil {
		t.Fatal(err)
	}
	s.AwaitingApproval = nil
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := a.before(ctx, &s, chain.Assignment{Role: "c"}); !errors.Is(err, chain.ErrAwaitingApproval) {
		t.Fatal(err)
	}
	if f.attempts() != 1 {
		t.Fatalf("POST attempts=%d, want 1", f.attempts())
	}
	log, err := a.notice().load()
	if err != nil || len(log.Notices) != 1 {
		t.Fatalf("kind/number duplicated: notices=%d err=%v", len(log.Notices), err)
	}
	posts, err := chain.ReadApprovalRecords[chain.ApprovalPost](a.approvalDirectory(), "post")
	if err != nil || len(posts) != 1 || posts[0].CommentID != p.CommentID || posts[0].Digest != p.Digest || s.AwaitingApproval.PostN != 1 {
		t.Fatalf("posts=%+v err=%v", posts, err)
	}
}

type approvalAfterComments struct {
	tracker.Tracker
	after func()
}

func (s approvalAfterComments) Comments(ctx context.Context, issue tracker.Issue) ([]json.RawMessage, error) {
	rows, err := s.Tracker.Comments(ctx, issue)
	if err == nil {
		s.after()
	}
	return rows, err
}
func TestApprovalChildConfirmedNoticeAfterCommentRead(t *testing.T) {
	for _, variant := range []string{"new_id", "new_time", "old_missing"} {
		t.Run(variant, func(t *testing.T) {
			a, f, s, _ := approvalPostFixture(t)
			original := a.source
			once := false
			a.source = recordTrackerComments(approvalAfterComments{Tracker: a.cfg.unrecordedSource(), after: func() {
				if once {
					return
				}
				once = true
				id := int64(960)
				posted := time.Now().UTC()
				if variant != "new_id" {
					id = 850
				}
				if variant == "old_missing" {
					posted = posted.Add(-time.Hour)
				}
				n := a.notice()
				release := n.hold()
				defer release()
				log, err := n.load()
				if err != nil {
					t.Fatal(err)
				}
				log.Notices = append(log.Notices, noticeRecord{Kind: startedNotice, Text: "confirmed while reading", CommentID: id, PostedAt: &posted})
				if err := n.save(log); err != nil {
					t.Fatal(err)
				}
				if variant != "old_missing" {
					approvalAdd(t, f, id, 99, "confirmed while reading", nil)
					f.set(func(f *noticeTracker) {
						sort.Slice(f.rows, func(i, j int) bool {
							var left, right struct{ ID int64 }
							if err := json.Unmarshal(f.rows[i], &left); err != nil {
								t.Fatal(err)
							}
							if err := json.Unmarshal(f.rows[j], &right); err != nil {
								t.Fatal(err)
							}
							return left.ID < right.ID
						})
					})
					// The watcher's readback records the notice before the child locks.
					if _, err := original.Comments(context.Background(), a.issue); err != nil {
						t.Fatal(err)
					}
				}
			}}, a.cfg.commentJobs)
			_, err := a.read(context.Background(), s, "c")
			if variant == "old_missing" {
				if !errors.Is(err, errApprovalTampered) {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, errApprovalRetry) {
				t.Fatalf("first read: %v", err)
			}
			if _, err := a.read(context.Background(), s, "c"); err != nil {
				t.Fatalf("second read: %v", err)
			}
		})
	}
}

func TestApprovalChildDeletedLatestNoticeDoesNotRetry(t *testing.T) {
	for _, afterPost := range []bool{false, true} {
		t.Run(fmt.Sprintf("after_post_%t", afterPost), func(t *testing.T) {
			a, f, s := approvalFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if afterPost {
				if err := a.before(ctx, &s, chain.Assignment{Role: "c"}); !errors.Is(err, chain.ErrAwaitingApproval) {
					t.Fatal(err)
				}
				s.AwaitingApproval = nil
			}
			postsBefore := len(f.all())
			posted := time.Now().UTC().Add(-time.Hour)
			n := a.notice()
			log, err := n.load()
			if err != nil {
				t.Fatal(err)
			}
			log.Notices = append(log.Notices, noticeRecord{Kind: startedNotice, Text: "latest notification", CommentID: 960, PostedAt: &posted})
			if err := n.save(log); err != nil {
				t.Fatal(err)
			}
			approvalAdd(t, f, 960, 99, "latest notification", nil)
			if _, err := a.source.Comments(ctx, a.issue); err != nil {
				t.Fatal(err)
			}
			f.set(func(f *noticeTracker) { f.rows = f.rows[:len(f.rows)-1] })
			reads := 0
			a.source = recordTrackerComments(approvalAfterComments{Tracker: a.cfg.unrecordedSource(), after: func() { reads++ }}, a.cfg.commentJobs)
			err = a.before(ctx, &s, chain.Assignment{Role: "c"})
			if !errors.Is(err, chain.ErrApprovalPause) || reads != 1 || len(f.all()) != postsBefore {
				t.Fatalf("err = %v, reads = %d, new posts = %d", err, reads, len(f.all())-postsBefore)
			}
		})
	}
}

func TestApprovalChildProcessedCommentsKeepApprover(t *testing.T) {
	for _, result := range []string{"rejection", "unreadable_go"} {
		t.Run(result, func(t *testing.T) {
			a, f, s, first := approvalPostFixture(t)
			body := "please revise"
			if result == "unreadable_go" {
				body = "GO invalid"
			}
			// PR5 records every processed comment in this shared format.
			record := chain.ApprovalGO{Result: result, PostN: first.N, PostCommentID: first.CommentID, GOCommentID: 902, AuthorID: 7, Body: body, Digest: first.Digest}
			approvalAdd(t, f, record.GOCommentID, 7, body, nil)
			if _, err := a.source.Comments(context.Background(), a.issue); err != nil {
				t.Fatal(err)
			}
			if err := record.Save(a.approvalDirectory()); err != nil {
				t.Fatal(err)
			}
			// Simulate the next attempt after the parent's rejection/deadline handling.
			s.History = append(s.History, chain.Result{Role: "c", Speaker: "runtime", Error: "parent ended approval wait", FinishedAt: time.Now().UTC()})
			s.AwaitingApproval = nil
			if err := a.before(context.Background(), &s, chain.Assignment{Role: "c"}); !errors.Is(err, chain.ErrAwaitingApproval) {
				t.Fatal(err)
			}
			posts, err := chain.ReadApprovalRecords[chain.ApprovalPost](a.approvalDirectory(), "post")
			if err != nil || len(posts) != 2 {
				t.Fatalf("posts=%+v err=%v", posts, err)
			}
			second := posts[1]
			if second.N != 2 {
				t.Fatal("no second post")
			}
			approvalAcceptedGO(t, a, f, s, second)
			s.AwaitingApproval = nil
			if err := a.before(context.Background(), &s, chain.Assignment{Role: "c"}); err != nil {
				t.Fatal(err)
			}
			gos, err := chain.ReadApprovalRecords[chain.ApprovalGO](a.approvalDirectory(), "go")
			if err != nil || len(gos) != 2 || !gos[0].ConsumedAt.IsZero() || !gos[0].ExpiredAt.IsZero() || gos[1].ConsumedAt.IsZero() {
				t.Fatalf("records=%+v err=%v", gos, err)
			}
			approvalEditRow(t, f, record.GOCommentID, func(row map[string]any) { row["content"] = "edited" })
			if _, err := a.read(context.Background(), s, "c"); !errors.Is(err, errApprovalTampered) {
				t.Fatalf("processed non-GO edit accepted: %v", err)
			}
		})
	}
}
