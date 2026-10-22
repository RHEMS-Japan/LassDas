package main

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"ticket-runner/internal/chain"
)

const approvalUnreadableNotice = "approval_unreadable_go"
const approvalUnreadableText = "GO を読み取れませんでした。承認の投稿にある先頭 8 桁を使い、半角の「GO <8桁>」だけを新しいコメントに書いてください。元のコメントは編集・削除しないでください。承認待ちの「再開」では作業は進みません。"
const approvalResumeBlockedText = "再開の指示を記録しました。承認の検査に通らない限り作業は続きません。停止して起票し直してください。"

var approvalGOFormat = regexp.MustCompile(`^GO [0-9a-f]{8}$`)

func approvalPauseReason(err error) string {
	switch {
	case errors.Is(err, errApprovalPostMismatch):
		return "approval_post_mismatch"
	case errors.Is(err, errApprovalTampered):
		return "approval_tampered"
	case errors.Is(err, errApprovalNoApprover):
		return "approval_no_approver"
	}
	return ""
}

// The private post is the recovery receipt for a rejection or expiry. Save it
// before history, so a restart can finish the same transition exactly once.
func resolveApproval(state *chain.State, p chain.ApprovalPost) {
	for _, r := range state.History {
		if r.Role == p.Stage && r.Speaker == "runtime" && r.Error == chain.ApprovalFailureText(p.Resolution) && r.FinishedAt.Equal(p.ResolvedAt) {
			state.AwaitingApproval = nil
			return
		}
	}
	r := chain.ApprovalFailureRecord(p.Stage, p.Resolution)
	r.FinishedAt = p.ResolvedAt
	state.History = append(state.History, r)
	state.AwaitingApproval = nil
	if p.Resolution == "rejected" {
		state.History = append(state.History, chain.Result{Speaker: "requester", Output: p.ResolutionBody, FinishedAt: p.ResolvedAt})
	} else {
		state.PauseApproval("approval_expired", p.ResolvedAt)
	}
}

func (a approvalChild) parentTick(ctx context.Context, state *chain.State) (bool, error) {
	if state.AwaitingApproval == nil {
		return true, nil
	}
	a.parent = true
	wait := *state.AwaitingApproval
	v, err := a.read(ctx, *state, wait.Stage)
	if err != nil {
		if errors.Is(err, errApprovalStopped) {
			return true, err
		}
		if reason := approvalPauseReason(err); reason != "" {
			state.PauseApproval(reason, a.clock())
			return false, nil
		}
		return false, err
	}
	var p chain.ApprovalPost
	for _, post := range v.posts {
		if post.N == wait.PostN && post.Stage == wait.Stage && post.CommentID == wait.PostCommentID {
			p = post
		}
	}
	if p.N == 0 {
		state.PauseApproval("approval_tampered", a.clock())
		return false, nil
	}
	if p.Resolution != "" {
		if err := a.expireOutstanding(v.gos); err != nil {
			return false, err
		}
		resolveApproval(state, p)
		return p.Resolution == "rejected", nil
	}
	processed := map[int64]bool{}
	for _, post := range v.posts {
		for _, id := range post.ProcessedCommentIDs {
			processed[id] = true
		}
	}
	for _, g := range v.gos {
		processed[g.GOCommentID] = true
	}
	sort.Slice(v.comments, func(i, j int) bool { return v.comments[i].ID < v.comments[j].ID })
	var candidates []chain.ApprovalGO
	var rejections []string
	for _, c := range v.comments {
		if c.ID <= v.boundary || processed[c.ID] || !slices.Contains(v.approvers, c.Author.ID) || c.Body == "" {
			continue
		}
		body := strings.TrimRight(c.Body, " \t\r\n")
		kind := "rejection"
		if approvalGOFormat.MatchString(body) && body == "GO "+p.DigestPrefix {
			kind = "go"
		} else if !approvalOrdinary(c.Body) {
			kind = "unreadable_go"
		}
		if kind == "rejection" {
			rejections = append(rejections, c.Body)
		}
		candidates = append(candidates, chain.ApprovalGO{Result: kind, PostN: p.N, PostCommentID: p.CommentID, GOCommentID: c.ID, AuthorID: c.Author.ID, Body: c.Body, Digest: p.Digest})
		p.ProcessedCommentIDs = append(p.ProcessedCommentIDs, c.ID)
	}
	now := a.clock()
	// Classify before expiry, including GO just before D. None is accepted at D.
	if !now.Before(p.Deadline) || len(rejections) > 0 {
		p.Resolution, p.ResolvedAt = "rejected", now
		p.ResolutionBody = strings.Join(rejections, "\n\n")
		if !now.Before(p.Deadline) {
			p.Resolution = "expired"
			p.ResolutionBody = ""
		}
		if err := p.Save(a.approvalDirectory()); err != nil {
			return false, err
		}
		if err := a.expireOutstanding(v.gos); err != nil {
			return false, err
		}
		resolveApproval(state, p)
		return p.Resolution == "rejected", nil
	}
	// A saved acceptance can survive a crash before the waiting flag was cleared.
	for _, g := range v.gos {
		if g.PostN == p.N && g.Result == "go" && g.ConsumedAt.IsZero() && g.ExpiredAt.IsZero() {
			if g.LaunchHistoryLen == len(state.History)+2 {
				recordApprovalAcceptance(state, g.AcceptedAt)
			}
			if g.LaunchHistoryLen == len(state.History)+1 {
				state.AwaitingApproval = nil
				return true, nil
			}
		}
	}
	accepted := false
	for _, g := range candidates {
		if g.Result == "unreadable_go" {
			if _, err := a.notice().sayPauseEvent(ctx, approvalUnreadableNotice, strconv.FormatInt(g.GOCommentID, 10), approvalUnreadableText); err != nil {
				return false, err
			}
			if err := g.Save(a.approvalDirectory()); err != nil {
				return false, err
			}
			continue
		}
		if accepted {
			continue
		}
		bundle, err := chain.ReadApprovalBundle(p.BundlePath(a.approvalDirectory()))
		if err != nil || bundle.Digest != p.Digest || !slices.Equal(bundle.Files, p.Files) {
			state.PauseApproval("approval_tampered", a.clock())
			return false, nil
		}
		// A separate readback binds the acceptance to the exact runtime post.
		rows, err := a.source.Comments(ctx, a.issue)
		if err != nil {
			return false, err
		}
		stop, err := stopInstruction(a.source, rows, a.issue, a.cfg.Intake.StopUserIDs)
		if err != nil {
			return false, err
		}
		if stop != nil {
			if err := writeRuntimeFile(filepath.Join(a.directory, "stop-request.json"), stop); err != nil {
				return false, err
			}
			return true, errApprovalStopped
		}
		found := false
		for _, raw := range rows {
			c, e := a.source.ReadComment(raw, a.issue)
			if e != nil {
				return false, e
			}
			if c.ID == p.CommentID {
				found = c.OnIssue && c.Author.ID == v.me && !c.Created.IsZero() && c.Created.Equal(c.Updated) && chain.ApprovalSHA256([]byte(c.Body)) == p.SentTextSHA256
			}
		}
		if !found {
			state.PauseApproval("approval_tampered", a.clock())
			return false, nil
		}
		now = a.clock()
		if !now.Before(p.Deadline) {
			p.Resolution = "expired"
			p.ResolvedAt = now
			if err := p.Save(a.approvalDirectory()); err != nil {
				return false, err
			}
			resolveApproval(state, p)
			return false, nil
		}
		g.AcceptedAt, g.PendingSince, g.LaunchHistoryLen = now, now, len(state.History)+2
		if err := g.Save(a.approvalDirectory()); err != nil {
			return false, err
		}
		recordApprovalAcceptance(state, now)
		accepted = true
	}
	if err := p.Save(a.approvalDirectory()); err != nil {
		return false, err
	}
	if accepted {
		state.AwaitingApproval = nil
	}
	return accepted, nil
}

func resumeApprovalRequest(ctx context.Context, cfg config, issue sourceIssue, directory, request string, interval time.Duration) (bool, bool, error) {
	store, err := chain.Open(filepath.Join(directory, "run"), request)
	if err != nil {
		return false, false, err
	}
	defer store.Close()
	state, err := store.Load()
	if err != nil {
		return false, false, err
	}
	readCtx, cancel := context.WithTimeout(ctx, interval)
	defer cancel()
	a := approvalChild{cfg: cfg, source: cfg.source(), issue: issue, directory: directory}
	ready, err := a.parentTick(readCtx, &state)
	if errors.Is(err, errApprovalStopped) {
		return true, true, nil
	}
	if err != nil {
		return false, false, err
	}
	if err := store.Save(state); err != nil {
		return false, false, err
	}
	return ready, false, nil
}

func pauseApproval(directory string, now time.Time) error {
	state, err := savedHistory(directory)
	if err != nil {
		return err
	}
	reason := state.ApprovalPaused()
	if reason == "" {
		return nil
	}
	return recordWorkPause(directory, reason, now)
}

func approvalAssignee(cfg config, state chain.State) int64 {
	stage := approvalStageName(state)
	for _, s := range approvalStages(cfg) {
		if s.Name == stage && s.Approval != nil && len(s.Approval.ApproverUserIDs) > 0 {
			return s.Approval.ApproverUserIDs[0]
		}
	}
	for _, s := range approvalStages(cfg) {
		if s.Approval != nil && len(s.Approval.ApproverUserIDs) > 0 {
			return s.Approval.ApproverUserIDs[0]
		}
	}
	return 0
}
func assignApproval(ctx context.Context, cfg config, issue sourceIssue, directory string, state chain.State, observe func(string)) {
	id := approvalAssignee(cfg, state)
	if id <= 0 {
		return
	}
	cfg.runtimeUser.ID = id
	assignTurn(ctx, cfg, issue, directory, "approver:"+strconv.FormatInt(id, 10), observe)
}

// Resume is an instruction receipt, not permission to bypass approval checks.
func recheckApprovalResume(ctx context.Context, cfg config, issue sourceIssue, directory, request string) (bool, error) {
	store, err := chain.Open(filepath.Join(directory, "run"), request)
	if err != nil {
		return false, err
	}
	defer store.Close()
	state, err := store.Load()
	if err != nil {
		return false, err
	}
	stage := approvalStageName(state)
	if stage == "" {
		for _, s := range approvalStages(cfg) {
			if s.Approval != nil {
				stage = s.Name
				break
			}
		}
	}
	a := approvalChild{cfg: cfg, source: cfg.source(), issue: issue, directory: directory, parent: true}
	_, readErr := a.read(ctx, state, stage)
	if errors.Is(readErr, errApprovalStopped) {
		return false, nil
	}
	reason := approvalPauseReason(readErr)
	if readErr != nil && reason == "" {
		return false, readErr
	}
	if reason == "" {
		if err := prepareStoredApprovals(&cfg, store); err != nil {
			return false, err
		}
		state, err = store.Load()
		if err != nil {
			return false, err
		}
		reason = state.ApprovalPaused()
	}
	if reason != "" {
		state.PauseApproval(reason, time.Now().UTC())
		if err := store.Save(state); err != nil {
			return false, err
		}
		if err := pauseApproval(directory, time.Now().UTC()); err != nil {
			return false, err
		}
		return false, nil
	}
	gos, err := chain.ReadApprovalRecords[chain.ApprovalGO](a.approvalDirectory(), "go")
	if err != nil {
		return false, err
	}
	if err := a.expireOutstanding(gos); err != nil {
		return false, err
	}
	return true, nil
}

func recordApprovalAcceptance(state *chain.State, at time.Time) {
	state.History = append(state.History, chain.Result{Role: "router", Speaker: "runtime", Output: "承認の GO を受理した", FinishedAt: at})
}

// Identify the gate that is waiting or whose immediately preceding command ran.
func approvalStageName(state chain.State) string {
	if state.AwaitingApproval != nil {
		return state.AwaitingApproval.Stage
	}
	if state.Pending != nil {
		if _, _, ok := state.ApprovalStage(state.Pending.Role); ok {
			return state.Pending.Role
		}
	}
	if state.Workflow == nil {
		return ""
	}
	for i := len(state.History) - 1; i >= 0; i-- {
		r := state.History[i]
		if r.Speaker != "runtime" {
			continue
		}
		for j, s := range state.Workflow.Stages {
			if s.Approval != nil && (s.Name == r.Role || j > 0 && state.Workflow.Stages[j-1].Name == r.Role) {
				return s.Name
			}
		}
	}
	for _, s := range state.Workflow.Stages {
		if s.Approval != nil {
			return s.Name
		}
	}
	return ""
}
