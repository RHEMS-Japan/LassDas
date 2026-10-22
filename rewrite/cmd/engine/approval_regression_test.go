package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"ticket-runner/internal/chain"
)

func TestApprovalParentExpiryWithoutComments(t *testing.T) {
	a, _, s, p := approvalPostFixture(t)
	before := len(s.History)
	a.now = func() time.Time { return p.Deadline.Add(-time.Nanosecond) }
	if parentTickTest(t, a, &s) || s.AwaitingApproval == nil || len(s.History) != before {
		t.Fatal("silent request did not wait before deadline")
	}
	a.now = func() time.Time { return p.Deadline }
	if parentTickTest(t, a, &s) || s.AwaitingApproval != nil || s.ApprovalPaused() != "approval_expired" || len(s.History) != before+2 || chain.ApprovalFailureReason(s.History[before]) != "expired" {
		t.Fatal("silent request did not expire at deadline")
	}
	gos, err := chain.ReadApprovalRecords[chain.ApprovalGO](a.approvalDirectory(), "go")
	if err != nil || len(gos) != 0 {
		t.Fatalf("unexpected GO records: %v %v", gos, err)
	}
}

func TestApprovalHoldExpiryBeforeAttemptLimit(t *testing.T) {
	a, f, s, p := approvalPostFixture(t)
	a.now = func() time.Time { return p.Deadline }
	parentTickTest(t, a, &s)
	a.cfg.Intake.MaxStageAttempts = 1
	if _, count := s.FailedInARow(); count < 1 {
		t.Fatal("fixture does not also reach the attempt limit")
	}
	writeJobHistory(t, a.directory, s)
	held, err := holdPausedRequest(context.Background(), a.cfg, a.issue, a.directory, noticeRequest, time.Second, func(string) {})
	if err != nil || !held {
		t.Fatalf("held=%t err=%v", held, err)
	}
	record, err := readWorkLimit(a.directory)
	if err != nil || len(record.Pauses) != 1 || record.Pauses[0].Reason != "approval_expired" || record.Pauses[0].NoticeID <= 0 {
		t.Fatalf("expiry lost priority: %+v %v", record, err)
	}
	texts := f.all()
	text := texts[len(texts)-1]
	if !strings.Contains(text, chain.ApprovalPauseText("approval_expired")) || strings.Contains(text, "回数上限") || strings.Contains(text, "上限の 1 回") {
		t.Fatalf("wrong pause notification: %s", text)
	}
	if !strings.Contains(text, "再開以外") || !strings.Contains(text, "遅れた GO") {
		t.Fatal("expiry notice did not explain late GO exclusion")
	}
}

func TestApprovalParentFlushesFailedNotice(t *testing.T) {
	a, f, s, p := approvalPostFixture(t)
	f.set(func(f *noticeTracker) { f.postFail = 1 })
	if _, err := a.notice().sayPauseEvent(context.Background(), approvalUnreadableNotice, "retry-fixture", approvalUnreadableText); err == nil {
		t.Fatal("first notification should fail")
	}
	log, err := a.notice().load()
	if err != nil || pendingNotice(log) < 0 {
		t.Fatal("missing unconfirmed notification", err)
	}
	approvalAdd(t, f, 950, 7, "GO "+p.DigestPrefix, nil)
	ready, err := a.parentTick(context.Background(), &s)
	if ready || !errors.Is(err, errApprovalRetry) || s.AwaitingApproval == nil {
		t.Fatalf("stale snapshot was used: ready=%t err=%v", ready, err)
	}
	log, err = a.notice().load()
	if err != nil || pendingNotice(log) >= 0 || f.count(approvalUnreadableText) != 1 {
		t.Fatal("parent tick did not confirm failed notification", err)
	}
	gos, err := chain.ReadApprovalRecords[chain.ApprovalGO](a.approvalDirectory(), "go")
	if err != nil || len(gos) != 0 {
		t.Fatal("GO accepted before confirmed notification was read", err)
	}
	if !parentTickTest(t, a, &s) || s.AwaitingApproval != nil || f.count(approvalUnreadableText) != 1 {
		t.Fatal("next tick did not accept GO after confirmation")
	}
}

func TestApprovalParentExclusionsWithTwoApprovers(t *testing.T) {
	for _, variant := range []string{"creator", "read_author", "runtime"} {
		t.Run(variant, func(t *testing.T) {
			a, f, s := approvalFixture(t)
			excluded := int64(8)
			if variant == "creator" {
				a.issue.Creator.ID = excluded
			} else if variant == "read_author" {
				approvalAdd(t, f, 850, excluded, "ordinary before post", nil)
				if _, err := a.source.Comments(context.Background(), a.issue); err != nil {
					t.Fatal(err)
				}
			} else if variant == "runtime" {
				excluded = 99
			}
			a.cfg.Workflow.Stages[2].Approval.ApproverUserIDs = []int64{excluded, 7}
			a.cfg.Intake.StopUserIDs = []int64{excluded, 7}
			if err := a.before(context.Background(), &s, chain.Assignment{Role: "c"}); !errors.Is(err, chain.ErrAwaitingApproval) {
				t.Fatal(err)
			}
			posts, err := chain.ReadApprovalRecords[chain.ApprovalPost](a.approvalDirectory(), "post")
			if err != nil || len(posts) != 1 {
				t.Fatal("missing approval post", err)
			}
			p := posts[0]
			if variant == "runtime" {
				// A known runtime notice avoids the independent unknown-runtime-comment
				// tamper guard, so this exercises the account exclusion itself.
				if _, err := a.notice().sayPauseEvent(context.Background(), approvalUnreadableNotice, "runtime-go-fixture", "GO "+p.DigestPrefix); err != nil {
					t.Fatal(err)
				}
			} else {
				approvalAdd(t, f, 950, excluded, "GO "+p.DigestPrefix, nil)
			}
			if parentTickTest(t, a, &s) || s.AwaitingApproval == nil || s.ApprovalPaused() != "" {
				t.Fatal("excluded GO did not leave the other approver eligible")
			}
			gos, err := chain.ReadApprovalRecords[chain.ApprovalGO](a.approvalDirectory(), "go")
			if err != nil || len(gos) != 0 {
				t.Fatal("excluded account produced a GO record", err)
			}
			approvalAdd(t, f, 960, 7, "GO "+p.DigestPrefix, nil)
			if !parentTickTest(t, a, &s) || s.AwaitingApproval != nil {
				t.Fatal("eligible second approver could not approve")
			}
			gos, err = chain.ReadApprovalRecords[chain.ApprovalGO](a.approvalDirectory(), "go")
			if err != nil || len(gos) != 1 || gos[0].AuthorID != 7 || gos[0].GOCommentID != 960 {
				t.Fatalf("wrong acceptance: %+v %v", gos, err)
			}
		})
	}
}

func TestApprovalExpiryRetainsRejection(t *testing.T) {
	a, f, s, p := approvalPostFixture(t)
	before := s
	count := len(s.History)
	approvalAdd(t, f, 950, 7, "first correction at expiry", nil)
	approvalAdd(t, f, 951, 7, "second correction at expiry", nil)
	a.now = func() time.Time { return p.Deadline }
	if parentTickTest(t, a, &s) || s.ApprovalPaused() != "approval_expired" {
		t.Fatal("expiry did not win")
	}
	if len(s.History) != count+3 || chain.ApprovalFailureReason(s.History[count]) != "expired" || s.History[count+1].Role != "" || s.History[count+1].Speaker != "requester" || s.History[count+1].Output != "first correction at expiry\n\nsecond correction at expiry" {
		t.Fatal("expiry discarded rejection text")
	}
	posts, err := chain.ReadApprovalRecords[chain.ApprovalPost](a.approvalDirectory(), "post")
	if err != nil || len(posts) != 1 || posts[0].ResolutionBody != s.History[count+1].Output || !slices.Equal(posts[0].ProcessedCommentIDs, []int64{950, 951}) {
		t.Fatal("resolution receipt lost rejection", err)
	}
	if parentTickTest(t, a, &before) || len(before.History) != len(s.History) || before.ApprovalPaused() != "approval_expired" {
		t.Fatal("expiry recovery lost rejection or pause")
	}
	before.AwaitingApproval = &chain.AwaitingApproval{Stage: "c", PostN: p.N, PostCommentID: p.CommentID, Deadline: p.Deadline}
	parentTickTest(t, a, &before)
	if len(before.History) != len(s.History) {
		t.Fatal("expiry recovery duplicated rejection")
	}
}

func TestApprovalPollRequestsTransitions(t *testing.T) {
	for _, outcome := range []string{"go", "rejected", "expired"} {
		t.Run(outcome, func(t *testing.T) {
			a, f, s, p := approvalPostFixture(t)
			s.History = append([]chain.Result{{Role: "a", Speaker: "runtime", FinishedAt: time.Now().UTC().Add(-2 * time.Hour)}}, s.History...)
			a.cfg.ModelSelection = &selectionConfig{Fixed: "maker/test"}
			workspace := filepath.Join(a.directory, "workspace")
			if err := os.MkdirAll(filepath.Join(workspace, "ops/bundle"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(workspace, "ops/bundle/plan.txt"), []byte("proposed\n"), 0600); err != nil {
				t.Fatal(err)
			}
			for i := range a.cfg.Roles {
				process := &a.cfg.Roles[i].Processes[0]
				process.Directory, process.Env = "", nil
				switch a.cfg.Roles[i].Name {
				case "a":
					process.Command = []string{"/bin/sh", "-c", "cat >/dev/null; echo work >> work-count"}
				case "b":
					process.Command = []string{"/bin/sh", "-c", "cat >/dev/null; printf 'checked\\n' > ops/bundle/dry-run.txt"}
				case "c":
					process.Receipt = "receipt.json"
					process.Command = []string{"/bin/sh", "-c", `cat >/dev/null; test -d "$TASK_APPROVED_BUNDLE" || exit 9; echo launch >> apply-count; printf '{}' > receipt.json; exit 1`}
				}
			}
			switch outcome {
			case "go":
				approvalAdd(t, f, 950, 7, "GO "+p.DigestPrefix, nil)
			case "rejected":
				approvalAdd(t, f, 950, 7, "revise this plan", nil)
			case "expired":
				p.Deadline = time.Now().UTC().Add(-time.Second)
				if err := p.Save(a.approvalDirectory()); err != nil {
					t.Fatal(err)
				}
				s.AwaitingApproval.Deadline = p.Deadline
			}
			writeJobHistory(t, a.directory, s)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			var log bytes.Buffer
			result := make(chan error, 1)
			go func() {
				result <- pollRequests(ctx, a.cfg, filepath.Dir(a.directory), time.Time{}, 20*time.Millisecond, 1, &serialLog{writer: &log})
			}()
			// Cleanup always joins the watcher, including failed assertions; its own
			// deferred workers.Wait joins every launched command before returning.
			joined := false
			defer func() {
				cancel()
				if !joined {
					<-result
				}
			}()
			var saved chain.State
			reached := false
			for ctx.Err() == nil {
				state, err := savedHistory(a.directory)
				if err == nil {
					saved = state
					if outcome == "expired" {
						record, e := readWorkLimit(a.directory)
						reached = state.ApprovalPaused() == "approval_expired" && e == nil && len(record.Pauses) == 1 && record.Pauses[0].NoticeID > 0
					} else {
						reached = state.AwaitingApproval != nil && state.AwaitingApproval.PostN == p.N+1
					}
				}
				if reached {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			// Let more than one tick observe the final wait/pause to catch duplicate launches.
			if reached {
				time.Sleep(60 * time.Millisecond)
			}
			cancel()
			err := <-result
			joined = true
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
			if !reached {
				t.Fatalf("watcher did not reach %s transition: %s", outcome, log.String())
			}
			count, readErr := os.ReadFile(filepath.Join(workspace, "apply-count"))
			if outcome == "go" {
				if readErr != nil || string(count) != "launch\n" {
					t.Fatalf("GO launch count=%q err=%v", count, readErr)
				}
				gos, err := chain.ReadApprovalRecords[chain.ApprovalGO](a.approvalDirectory(), "go")
				if err != nil || len(gos) != 1 || gos[0].ConsumedAt.IsZero() {
					t.Fatal("watcher GO not consumed once", err)
				}
			} else if !errors.Is(readErr, os.ErrNotExist) {
				t.Fatalf("%s launched apply: %q %v", outcome, count, readErr)
			}
			if outcome == "rejected" {
				found := 0
				for _, r := range saved.History {
					if r.Speaker == "requester" && r.Output == "revise this plan" {
						found++
					}
				}
				works, err := os.ReadFile(filepath.Join(workspace, "work-count"))
				if found != 1 || err != nil || string(works) != "work\n" {
					t.Fatal("watcher did not return rejection to work exactly once")
				}
			}
		})
	}
}
