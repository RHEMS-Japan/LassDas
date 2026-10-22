package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"ticket-runner/internal/chain"
	"time"
)

func TestApprovalParentEnablesValidatedConfiguration(t *testing.T) {
	if err := validateApprovalStages(approvalConfiguration()); err != nil {
		t.Fatal(err)
	}
}

func parentTickTest(t *testing.T, a approvalChild, s *chain.State) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ready, err := a.parentTick(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	return ready
}
func TestApprovalParentGOConsumptionAndRepost(t *testing.T) {
	a, f, s, p := approvalPostFixture(t)
	approvalAdd(t, f, 950, 7, "GO "+p.DigestPrefix+" \t\r\n", nil)
	if !parentTickTest(t, a, &s) || s.AwaitingApproval != nil {
		t.Fatal("GO not accepted")
	}
	if err := a.before(context.Background(), &s, chain.Assignment{Role: "c"}); err != nil {
		t.Fatal(err)
	}
	gos, _ := chain.ReadApprovalRecords[chain.ApprovalGO](a.approvalDirectory(), "go")
	if len(gos) != 1 || gos[0].ConsumedAt.IsZero() || gos[0].LaunchHistoryLen != len(s.History) {
		t.Fatal("GO not consumed at launch position")
	}
	if used, err := a.consume(&s, p, approvalView{gos: gos}, nil); used || err != nil {
		t.Fatal("GO consumed twice")
	}
	s.History = append(s.History, chain.Result{Role: "c", Speaker: "runtime", Error: "synthetic failure", FinishedAt: time.Now().UTC()})
	next, err := (chain.StageRouter{}).Next(context.Background(), s)
	if err != nil || next.Role != "a" {
		t.Fatalf("recovery=%v %v", next, err)
	}
	s.History = append(s.History, chain.Result{Role: "a", Speaker: "runtime", FinishedAt: time.Now().UTC()}, chain.Result{Role: "b", Speaker: "runtime", FinishedAt: time.Now().UTC()})
	if err := a.before(context.Background(), &s, chain.Assignment{Role: "c"}); !errors.Is(err, chain.ErrAwaitingApproval) {
		t.Fatal(err)
	}
	posts, _ := chain.ReadApprovalRecords[chain.ApprovalPost](a.approvalDirectory(), "post")
	var again chain.ApprovalPost
	for _, post := range posts {
		if post.N == 2 {
			again = post
		}
	}
	if again.Digest != p.Digest || again.CommentID == p.CommentID || !strings.Contains(f.all()[len(f.all())-1], "正常に終了しなかった") {
		t.Fatal("repost lost bundle or previous ending")
	}
	if parentTickTest(t, a, &s) {
		t.Fatal("old GO accepted on new post")
	}
	approvalAdd(t, f, again.CommentID+10, 7, "GO "+again.DigestPrefix, nil)
	if !parentTickTest(t, a, &s) {
		t.Fatal("new GO not accepted")
	}
}

func TestApprovalParentClassification(t *testing.T) {
	for _, tt := range []struct{ name, body, kind string }{
		{"lower", "go 12345678", "unreadable"}, {"fullwidth", "ＧＯ １２３４５６７８", "unreadable"}, {"short", "GO 123", "unreadable"}, {"extra_space", "GO  12345678", "unreadable"}, {"wrong_digest", "GO 00000000", "unreadable"}, {"resume", "再開\n補足", "unreadable"}, {"two_lines", "GO 12345678\n追加", "rejected"}, {"quote", "> GO 12345678", "rejected"}, {"ordinary", "計画を直してください", "rejected"}, {"whitespace", " \t", "rejected"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, f, s, _ := approvalPostFixture(t)
			before := len(s.History)
			step, recovering := s.Step, s.Recovering
			approvalAdd(t, f, 950, 7, tt.body, nil)
			ready := parentTickTest(t, a, &s)
			if s.Step != step || s.Recovering != recovering {
				t.Fatal("refusal changed restart markers")
			}
			if tt.kind == "unreadable" {
				if ready || s.AwaitingApproval == nil || len(s.History) != before || f.count(approvalUnreadableText) != 1 {
					t.Fatal("malformed GO did not wait with one fixed notice")
				}
				parentTickTest(t, a, &s)
				if f.count(approvalUnreadableText) != 1 {
					t.Fatal("duplicate malformed notice")
				}
			} else {
				if !ready || s.AwaitingApproval != nil || len(s.History) != before+2 || chain.ApprovalFailureReason(s.History[before]) != "rejected" || s.History[before+1].Speaker != "requester" || s.History[before+1].Role != "" {
					t.Fatalf("bad rejection: %+v", s)
				}
				if _, count := s.FailedInARow(); count != 0 {
					t.Fatal("rejection counted as launch")
				}
				next, err := (chain.StageRouter{}).Next(context.Background(), s)
				if err != nil || next.Role != "a" {
					t.Fatal("rejection did not route to work")
				}
			}
		})
	}
}
func TestApprovalParentRejectionWinsAndRecoversOnce(t *testing.T) {
	a, f, s, p := approvalPostFixture(t)
	before := s
	approvalAdd(t, f, 950, 7, "GO "+p.DigestPrefix, nil)
	approvalAdd(t, f, 951, 7, "first correction", nil)
	approvalAdd(t, f, 952, 7, "second correction", nil)
	if !parentTickTest(t, a, &s) || len(s.History) != len(before.History)+2 || s.History[len(s.History)-1].Output != "first correction\n\nsecond correction" {
		t.Fatal("not one combined rejection")
	}
	gos, _ := chain.ReadApprovalRecords[chain.ApprovalGO](a.approvalDirectory(), "go")
	if len(gos) != 0 {
		t.Fatal("GO accepted despite rejection")
	}
	// Replaying after the post receipt was saved but before state was saved.
	if !parentTickTest(t, a, &before) || len(before.History) != len(s.History) {
		t.Fatal("resolution receipt not replayed")
	}
	s.AwaitingApproval = &chain.AwaitingApproval{Stage: "c", PostN: p.N, PostCommentID: p.CommentID, Deadline: p.Deadline}
	parentTickTest(t, a, &s)
	if len(before.History) != len(s.History) {
		t.Fatal("rejection duplicated")
	}
}
func TestApprovalParentExpiryClassifiesBeforePause(t *testing.T) {
	a, f, s, p := approvalPostFixture(t)
	before := len(s.History)
	a.now = func() time.Time { return p.Deadline }
	approvalAdd(t, f, 950, 7, "GO "+p.DigestPrefix, nil)
	if parentTickTest(t, a, &s) || s.ApprovalPaused() != "approval_expired" || len(s.History) != before+2 {
		t.Fatal("not expiry pause")
	}
	writeJobHistory(t, a.directory, s)
	a.cfg.Intake.MaxStageAttempts = 1
	if err := pauseApproval(a.directory, a.clock()); err != nil {
		t.Fatal(err)
	}
	if err := pauseStageAttempts(a.cfg, a.directory, a.clock()); err != nil {
		t.Fatal(err)
	}
	record, err := readWorkLimit(a.directory)
	if err != nil || record.Pauses[0].Reason != "approval_expired" {
		t.Fatal("expiry replaced by attempt limit")
	}
	s.History = append(s.History, chain.Result{Speaker: "requester", Output: "再開", FinishedAt: a.clock()})
	next, err := (chain.StageRouter{}).Next(context.Background(), s)
	if err != nil || next.Role != "a" {
		t.Fatal("expiry resume did not select work")
	}
	s.History = append(s.History, chain.Result{Role: "a", Speaker: "runtime", FinishedAt: a.clock()}, chain.Result{Role: "b", Speaker: "runtime", FinishedAt: a.clock()})
	a.now = nil
	if _, err := a.read(context.Background(), s, "c"); err != nil {
		t.Fatal("expired GO excluded its author", err)
	}
}
func TestApprovalParentStopWins(t *testing.T) {
	for _, tampered := range []bool{false, true} {
		t.Run(fmt.Sprint(tampered), func(t *testing.T) {
			a, f, s, p := approvalPostFixture(t)
			approvalAdd(t, f, 950, 7, "GO "+p.DigestPrefix, nil)
			approvalAdd(t, f, 951, 7, "停止\n理由", nil)
			if tampered {
				approvalEditRow(t, f, 800, func(row map[string]any) { row["content"] = "changed" })
			}
			count := len(f.all())
			ready, err := a.parentTick(context.Background(), &s)
			if !ready || !errors.Is(err, errApprovalStopped) || s.ApprovalPaused() != "" || len(f.all()) != count {
				t.Fatal("stop lost to approval")
			}
			if _, err := os.Stat(filepath.Join(a.directory, "stop-request.json")); err != nil {
				t.Fatal("stop was not retained")
			}
		})
	}
}
func TestApprovalParentGOReadbackAndWholeDigest(t *testing.T) {
	for _, variant := range []string{"digest", "deleted", "edited", "author", "updated"} {
		t.Run(variant, func(t *testing.T) {
			a, f, s, p := approvalPostFixture(t)
			approvalAdd(t, f, 950, 7, "GO "+p.DigestPrefix, nil)
			if variant == "digest" {
				os.WriteFile(filepath.Join(p.BundlePath(a.approvalDirectory()), "plan.txt"), []byte("changed"), 0600)
			} else {
				reads := 0
				f.approvalReadHook = func() {
					reads++
					if reads != 2 {
						return
					}
					if variant == "deleted" {
						f.set(func(f *noticeTracker) {
							for i, raw := range f.rows {
								var c struct{ ID int64 }
								json.Unmarshal(raw, &c)
								if c.ID == p.CommentID {
									f.rows = append(f.rows[:i], f.rows[i+1:]...)
									break
								}
							}
						})
						return
					}
					approvalEditRow(t, f, p.CommentID, func(row map[string]any) {
						switch variant {
						case "edited":
							row["content"] = "changed"
						case "author":
							row["createdUser"] = map[string]any{"id": 7}
						case "updated":
							row["updated"] = "2027-01-01T00:00:00Z"
						}
					})
				}
			}
			if parentTickTest(t, a, &s) || s.ApprovalPaused() != "approval_tampered" {
				t.Fatalf("accepted %s", variant)
			}
		})
	}
}
func TestApprovalParentOrdinaryAfterAcceptance(t *testing.T) {
	a, f, s, p := approvalPostFixture(t)
	approvalAdd(t, f, 950, 7, "GO "+p.DigestPrefix, nil)
	if !parentTickTest(t, a, &s) {
		t.Fatal("GO not accepted")
	}
	approvalAdd(t, f, 951, 7, "change the plan", nil)
	if err := a.before(context.Background(), &s, chain.Assignment{Role: "c"}); !errors.Is(err, chain.ErrAwaitingApproval) {
		t.Fatal(err)
	}
	if !parentTickTest(t, a, &s) || s.History[len(s.History)-1].Output != "change the plan" {
		t.Fatal("late rejection lost")
	}
}
func TestApprovalParentCommentBetweenPostAndWait(t *testing.T) {
	a, f, s := approvalFixture(t)
	n := 0
	f.approvalReadHook = func() {
		n++
		if n == 2 {
			approvalAdd(t, f, 950, 7, "fix plan", nil)
		}
	}
	if err := a.before(context.Background(), &s, chain.Assignment{Role: "c"}); !errors.Is(err, chain.ErrAwaitingApproval) {
		t.Fatal(err)
	}
	if !parentTickTest(t, a, &s) || s.History[len(s.History)-1].Output != "fix plan" {
		t.Fatal("post-to-wait comment excluded approver")
	}
}
func TestApprovalPauseWordsAndEarlyReturn(t *testing.T) {
	for _, reason := range []string{"approval_expired", "approval_post_mismatch", "approval_tampered", "approval_no_approver", "approval_config_mismatch"} {
		t.Run(reason, func(t *testing.T) {
			s := chain.State{Step: "b", Recovering: true}
			s.PauseApproval(reason, time.Now().UTC())
			if !knownPauseReason(reason) || s.ApprovalPaused() != reason || s.Step != "b" || !s.Recovering {
				t.Fatal("wrong pause identity")
			}
			text := pausedWorkText(pauseEpisode{Reason: reason}, nil, nil)
			if reason != "approval_expired" && (strings.Contains(text, "続ける場合") || strings.Contains(text, "保存した条件で")) {
				t.Fatal("unsafe resume invitation")
			}
			_, dir := noticeJob(t, s)
			store, err := chain.Open(filepath.Join(dir, "run"), noticeRequest)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			c := chain.Chain{Store: store, Router: chain.StageRouter{}, Executor: chain.Processes{}}
			if err := c.Run(context.Background()); !errors.Is(err, chain.ErrApprovalPause) {
				t.Fatal("paused state did not return early", err)
			}
		})
	}
}

func TestApprovalBlockedResumeAndAssignee(t *testing.T) {
	for _, reason := range []string{"approval_post_mismatch", "approval_tampered", "approval_no_approver", "approval_config_mismatch"} {
		t.Run(reason, func(t *testing.T) {
			a, f, s, p := approvalPostFixture(t)
			a.cfg.Intake.Assign = true
			switch reason {
			case "approval_post_mismatch":
				approvalEditRow(t, f, p.CommentID, func(row map[string]any) { row["content"] = "changed" })
			case "approval_tampered":
				approvalEditRow(t, f, 800, func(row map[string]any) { row["content"] = "changed" })
			case "approval_no_approver":
				a.issue.Creator.ID = 7
			case "approval_config_mismatch":
				s.Workflow = approvalConfiguration().Workflow
				s.Workflow.Stages[2].Approval = nil
			}
			s.PauseApproval(reason, time.Now().UTC())
			writeJobHistory(t, a.directory, s)
			hold := func() {
				t.Helper()
				held, err := holdPausedRequest(context.Background(), a.cfg, a.issue, a.directory, noticeRequest, time.Second, func(string) {})
				if err != nil || !held {
					t.Fatalf("held=%t err=%v", held, err)
				}
			}
			hold()
			if f.approvalAssigned != 7 {
				t.Fatalf("assigned=%d", f.approvalAssigned)
			}
			record, _ := readWorkLimit(a.directory)
			id := record.Pauses[len(record.Pauses)-1].NoticeID
			approvalAdd(t, f, id+10, 7, "再開", nil)
			hold()
			saved := loadJobState(t, a.directory)
			expected := reason
			if reason == "approval_post_mismatch" {
				expected = "approval_tampered"
			}
			if saved.ApprovalPaused() != expected || saved.Pending != nil || saved.Step != s.Step || saved.Recovering != s.Recovering {
				t.Fatalf("resume bypassed checks: pause=%s", saved.ApprovalPaused())
			}
			hold()
			for _, text := range f.all() {
				if strings.Contains(text, "続ける場合") || strings.Contains(text, "実行枠と利用枠が使えるようになってから、保存した条件で続けます") {
					t.Fatal("unsafe resume words")
				}
			}
		})
	}
}

func TestApprovalPrePostResumeAndMalformedStillAllowGO(t *testing.T) {
	a, f, s := approvalFixture(t)
	approvalAdd(t, f, 850, 7, "再開\n補足", nil)
	if _, err := a.source.Comments(context.Background(), a.issue); err != nil {
		t.Fatal(err)
	}
	if err := a.before(context.Background(), &s, chain.Assignment{Role: "c"}); !errors.Is(err, chain.ErrAwaitingApproval) {
		t.Fatal(err)
	}
	posts, _ := chain.ReadApprovalRecords[chain.ApprovalPost](a.approvalDirectory(), "post")
	p := posts[0]
	approvalAdd(t, f, 950, 7, "go typo", nil)
	parentTickTest(t, a, &s)
	approvalAdd(t, f, 960, 7, "GO "+p.DigestPrefix, nil)
	if !parentTickTest(t, a, &s) {
		t.Fatal("resume or malformed GO excluded author")
	}
}

func TestApprovalParentAccountExclusions(t *testing.T) {
	for _, variant := range []string{"creator", "read_author", "runtime", "removed"} {
		t.Run(variant, func(t *testing.T) {
			a, f, s, p := approvalPostFixture(t)
			switch variant {
			case "creator":
				a.issue.Creator.ID = 7
			case "read_author":
				approvalAdd(t, f, 850, 7, "ordinary before post", nil)
				f.set(func(f *noticeTracker) {
					sort.Slice(f.rows, func(i, j int) bool {
						var x, y struct{ ID int64 }
						json.Unmarshal(f.rows[i], &x)
						json.Unmarshal(f.rows[j], &y)
						return x.ID < y.ID
					})
				})
			case "runtime":
				f.me = 7
			case "removed":
				a.cfg.Workflow.Stages[2].Approval = nil
			}
			approvalAdd(t, f, 950, 7, "GO "+p.DigestPrefix, nil)
			if parentTickTest(t, a, &s) || s.ApprovalPaused() == "" {
				t.Fatal("ineligible GO accepted")
			}
		})
	}
}

func TestApprovalParentIgnoredGOStillCompared(t *testing.T) {
	for _, why := range []string{"rejected", "expired", "malformed"} {
		t.Run(why, func(t *testing.T) {
			a, f, s, p := approvalPostFixture(t)
			body := "GO " + p.DigestPrefix
			if why == "malformed" {
				body = "go typo"
			}
			approvalAdd(t, f, 950, 7, body, nil)
			if why == "rejected" {
				approvalAdd(t, f, 951, 7, "correction", nil)
			}
			if why == "expired" {
				a.now = func() time.Time { return p.Deadline }
			}
			parentTickTest(t, a, &s)
			approvalEditRow(t, f, 950, func(row map[string]any) { row["content"] = "edited" })
			if _, err := a.read(context.Background(), s, "c"); !errors.Is(err, errApprovalTampered) {
				t.Fatal("unaccepted GO exempted from comparison", err)
			}
		})
	}
}

func TestApprovalEndToEndCommandLaunchesOncePerGO(t *testing.T) {
	a, f, s, p := approvalPostFixture(t)
	workspace := a.cfg.Roles[0].Processes[0].Directory
	for i := range a.cfg.Roles {
		process := &a.cfg.Roles[i].Processes[0]
		process.ModelEnv = ""
		switch a.cfg.Roles[i].Name {
		case "a":
			process.Command = []string{"/bin/sh", "-c", "cat >/dev/null; echo work >> work-count"}
		case "b":
			process.Command = []string{"/bin/sh", "-c", "cat >/dev/null; printf 'checked\\n' > ops/bundle/dry-run.txt"}
		case "c":
			process.Receipt = "receipt.json"
			process.Command = []string{"/bin/sh", "-c", `cat >/dev/null; test -d "$TASK_APPROVED_BUNDLE" || exit 9; test -n "$TASK_APPROVED_DIGEST" || exit 9; cat "$TASK_APPROVED_BUNDLE/plan.txt" >> applied-plan; echo launch >> apply-count; printf '{}' > receipt.json; exit 1`}
		}
	}
	s.History = append([]chain.Result{{Role: "a", Speaker: "runtime", FinishedAt: time.Now().UTC().Add(-2 * time.Hour)}}, s.History...)
	writeJobHistory(t, a.directory, s)
	runner := func() {
		t.Helper()
		store, err := chain.Open(filepath.Join(a.directory, "run"), noticeRequest)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		roles := map[string]chain.Role{}
		for _, role := range a.cfg.Roles {
			roles[role.Name] = role
		}
		engine := chain.Chain{Store: store, Router: chain.StageRouter{}, Executor: chain.Processes{Roles: roles, ApprovalDirectory: a.approvalDirectory()}, BeforeApproval: a.before, RetryDelay: time.Millisecond}
		if err := engine.Run(ctx); !errors.Is(err, chain.ErrAwaitingApproval) {
			t.Fatal(err)
		}
	}
	runner()
	if _, err := os.Stat(filepath.Join(workspace, "apply-count")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("launch before GO")
	}
	for attempt := 1; attempt <= 2; attempt++ {
		approvalAdd(t, f, p.CommentID+10, 7, "GO "+p.DigestPrefix, nil)
		ready, stop, err := resumeApprovalRequest(context.Background(), a.cfg, a.issue, a.directory, noticeRequest, time.Second)
		if err != nil || !ready || stop {
			t.Fatalf("accept ready=%t stop=%t err=%v", ready, stop, err)
		}
		runner()
		count, err := os.ReadFile(filepath.Join(workspace, "apply-count"))
		if err != nil || strings.Count(string(count), "launch") != attempt {
			t.Fatalf("launch count=%q %v", count, err)
		}
		works, _ := os.ReadFile(filepath.Join(workspace, "work-count"))
		if strings.Count(string(works), "work") != attempt {
			t.Fatal("failure did not return through work")
		}
		s = loadJobState(t, a.directory)
		if s.AwaitingApproval == nil || s.Pending != nil {
			t.Fatal("not waiting after failure")
		}
		records, _ := chain.ReadApprovalRecords[chain.ApprovalGO](a.approvalDirectory(), "go")
		consumed := 0
		for _, g := range records {
			if !g.ConsumedAt.IsZero() {
				consumed++
			}
		}
		if consumed != attempt {
			t.Fatal("consumption did not match launches")
		}
		posts, _ := chain.ReadApprovalRecords[chain.ApprovalPost](a.approvalDirectory(), "post")
		old := p
		for _, post := range posts {
			if post.N == s.AwaitingApproval.PostN {
				p = post
			}
		}
		if p.N != old.N+1 || p.Digest != old.Digest || !strings.Contains(f.all()[len(f.all())-1], "正常に終了しなかった") {
			t.Fatalf("repost n=%d/%d digest=%s/%s text=%s", p.N, old.N, p.Digest, old.Digest, f.all()[len(f.all())-1])
		}
		runner()
		count, _ = os.ReadFile(filepath.Join(workspace, "apply-count"))
		if strings.Count(string(count), "launch") != attempt {
			t.Fatal("wait launched twice")
		}
	}
}

func TestApprovalStopDuringApplyThroughWatcher(t *testing.T) {
	a, f, s, p := approvalPostFixture(t)
	s.History = append([]chain.Result{{Role: "a", Speaker: "runtime", FinishedAt: time.Now().UTC().Add(-2 * time.Hour)}}, s.History...)
	a.cfg.ModelSelection = &selectionConfig{Fixed: "maker/test"}
	a.cfg.AssignedIssue = a.issue.Key
	process := &a.cfg.Roles[2].Processes[0]
	process.Receipt = "receipt.json"
	process.Command = []string{"/bin/sh", "-c", `cat >/dev/null; echo started > apply-started; while :; do sleep 1; done`}
	writeJobHistory(t, a.directory, s)
	approvalAdd(t, f, 950, 7, "GO "+p.DigestPrefix, nil)
	ready, _, err := resumeApprovalRequest(context.Background(), a.cfg, a.issue, a.directory, noticeRequest, time.Second)
	if err != nil || !ready {
		t.Fatal("GO not accepted", err)
	}
	configPath, requestPath := filepath.Join(a.directory, "engine.json"), filepath.Join(a.directory, "request.txt")
	if err := writeRuntimeFile(configPath, configurationJSON(t, a.cfg)); err != nil {
		t.Fatal(err)
	}
	if err := writeRuntimeFile(requestPath, []byte(noticeRequest)); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	f.approvalReadHook = func() {
		if _, err := os.Stat(filepath.Join(process.Directory, "apply-started")); err == nil {
			once.Do(func() { approvalAdd(t, f, 960, 7, "停止", nil) })
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var log bytes.Buffer
	err = runWatchedRequest(ctx, a.cfg, a.issue, a.directory, configPath, requestPath, 20*time.Millisecond, newTurnstile(1), &log)
	if err != nil {
		t.Fatalf("watch stop: %v %s", err, log.String())
	}
	if _, err := os.Stat(filepath.Join(a.directory, "stop-request.json")); err != nil {
		t.Fatal("apply stop not recorded", err)
	}
	stopped := loadJobState(t, a.directory)
	for _, r := range stopped.History {
		if r.Role == "c" && r.Interrupted {
			return
		}
	}
	t.Fatal("apply interruption was not recorded")
}

func TestApprovalAcceptanceRecoveryAndMultipleGO(t *testing.T) {
	a, f, s, p := approvalPostFixture(t)
	before := s
	approvalAdd(t, f, 950, 7, "GO "+p.DigestPrefix, nil)
	approvalAdd(t, f, 951, 7, "GO "+p.DigestPrefix, nil)
	if !parentTickTest(t, a, &s) {
		t.Fatal("GO not accepted")
	}
	gos, _ := chain.ReadApprovalRecords[chain.ApprovalGO](a.approvalDirectory(), "go")
	if len(gos) != 1 || gos[0].GOCommentID != 950 {
		t.Fatal("more than the first GO accepted")
	}
	if !parentTickTest(t, a, &before) || len(before.History) != len(s.History) {
		t.Fatal("acceptance not recovered")
	}
	if err := a.before(context.Background(), &before, chain.Assignment{Role: "c"}); err != nil {
		t.Fatal("recovered acceptance cannot be consumed", err)
	}
}

func TestApprovalRejectionThenSameApproverGO(t *testing.T) {
	a, f, s, _ := approvalPostFixture(t)
	approvalAdd(t, f, 950, 7, "修正してください", nil)
	parentTickTest(t, a, &s)
	s.History = append(s.History, chain.Result{Role: "a", Speaker: "runtime", FinishedAt: time.Now().UTC()}, chain.Result{Role: "b", Speaker: "runtime", FinishedAt: time.Now().UTC()})
	if err := a.before(context.Background(), &s, chain.Assignment{Role: "c"}); !errors.Is(err, chain.ErrAwaitingApproval) {
		t.Fatal(err)
	}
	posts, _ := chain.ReadApprovalRecords[chain.ApprovalPost](a.approvalDirectory(), "post")
	for _, p := range posts {
		if p.N == s.AwaitingApproval.PostN {
			approvalAdd(t, f, p.CommentID+10, 7, "GO "+p.DigestPrefix, nil)
		}
	}
	if !parentTickTest(t, a, &s) {
		t.Fatal("rejection excluded its author")
	}
}

func TestApprovalPauseStopSurvivesDamagedLedger(t *testing.T) {
	a, f, s, _ := approvalPostFixture(t)
	s.PauseApproval("approval_tampered", time.Now().UTC())
	writeJobHistory(t, a.directory, s)
	approvalAdd(t, f, 950, 7, "停止", nil)
	if err := os.WriteFile(filepath.Join(a.directory, "comments-seen.json"), []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	before := len(f.all())
	held, err := holdPausedRequest(context.Background(), a.cfg, a.issue, a.directory, noticeRequest, time.Second, func(string) {})
	if err != nil || !held || len(f.all()) != before {
		t.Fatal("stop lost to damaged ledger", err)
	}
	if _, err := os.Stat(filepath.Join(a.directory, "stop-request.json")); err != nil {
		t.Fatal(err)
	}
}

func TestApprovalRefusalWordsAndNoRoleQuotes(t *testing.T) {
	cfg := approvalConfiguration()
	for _, reason := range []string{"rejected", "expired"} {
		state := chain.State{Workflow: cfg.Workflow, History: []chain.Result{chain.ApprovalFailureRecord("c", reason)}}
		next, err := (chain.StageRouter{}).Next(context.Background(), state)
		if err != nil || !strings.Contains(next.Instruction, reason) || strings.Contains(next.Instruction, "did not exit 0") {
			t.Fatal("incorrect recovery words", err)
		}
		text, _ := rerunNoticeText(cfg, t.TempDir(), "a", state.History, "fixture")
		if !strings.Contains(text, reason) || strings.Contains(text, "通りません") {
			t.Fatal("refusal described as failed command")
		}
		cfg.Intake.Announce = true
		cfg.Workflow.Stages[2].Announce = "stage began"
		_, directory := noticeJob(t, state)
		fixture := &noticeTracker{}
		fixture.install(t, func(r *http.Request) (*http.Response, error) { return nil, fmt.Errorf("unexpected request") })
		announceStages(context.Background(), cfg, announcedIssue(), directory, func(string) {})
		if len(fixture.all()) != 0 {
			t.Fatal("refusal announced as stage launch")
		}
	}
	state := chain.State{Workflow: cfg.Workflow, History: []chain.Result{{Role: "a", Speaker: "worker", Output: "ROLE_PRIVATE_WORDS"}}}
	if strings.Contains(environmentReason(cfg, state), "ROLE_PRIVATE_WORDS") {
		t.Fatal("environment notice quoted a role")
	}
}

func TestApprovalPauseUsesTheCurrentGate(t *testing.T) {
	cfg := approvalConfiguration()
	cfg.Workflow.Stages = append(cfg.Workflow.Stages, chain.Stage{Name: "later", Kind: chain.CommandStage, Approval: &chain.Approval{ApproverUserIDs: []int64{8}}})
	s := chain.State{Workflow: cfg.Workflow, History: []chain.Result{{Role: "e", Speaker: "runtime"}}}
	s.PauseApproval("approval_no_approver", time.Now().UTC())
	if approvalAssignee(cfg, s) != 8 || approvalStageName(s) != "later" {
		t.Fatal("pause selected the earlier approval gate")
	}
}

func TestApprovalRemovedCurrentGateStillPausesChild(t *testing.T) {
	a, _, s := approvalFixture(t)
	s.History = append([]chain.Result{{Role: "a", Speaker: "runtime", FinishedAt: time.Now().UTC().Add(-2 * time.Hour)}}, s.History...)
	writeJobHistory(t, a.directory, s)
	a.cfg.Workflow = approvalConfiguration().Workflow
	a.cfg.Workflow.Stages[2].Approval = nil
	a.cfg.ModelSelection = &selectionConfig{Fixed: "maker/test"}
	a.cfg.AssignedIssue = a.issue.Key
	configPath, requestPath := filepath.Join(a.directory, "engine.json"), filepath.Join(a.directory, "request.txt")
	if err := writeRuntimeFile(configPath, configurationJSON(t, a.cfg)); err != nil {
		t.Fatal(err)
	}
	if err := writeRuntimeFile(requestPath, []byte(noticeRequest)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, acceptedTimeFactsKey{}, requestTimeFacts(a.directory))
	var out bytes.Buffer
	if err := run(ctx, []string{"--config", configPath, "--request", requestPath, "--run-dir", filepath.Join(a.directory, "run")}, &out, &out); !errors.Is(err, chain.ErrApprovalPause) {
		t.Fatalf("%v %s", err, out.String())
	}
	saved := loadJobState(t, a.directory)
	if saved.ApprovalPaused() != "approval_no_approver" || saved.Pending != nil {
		t.Fatal("removed current gate bypassed approval")
	}
}
