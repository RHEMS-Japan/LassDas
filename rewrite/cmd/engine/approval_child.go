package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"ticket-runner/internal/chain"
	"ticket-runner/internal/tracker"
)

const approvalNotice = "approval_post"

var (
	errApprovalTampered     = errors.New("approval comparison mismatch")
	errApprovalNoApprover   = errors.New("approval has no eligible approver")
	errApprovalPostMismatch = errors.New("approval post readback mismatch")
	errApprovalStopped      = errors.New("approval stopped by an authorized instruction")
	errApprovalRetry        = errors.New("approval is waiting for confirmed notices")
)

// The same comparison is used by the supervised child and the parent.
type approvalChild struct {
	cfg       config
	source    tracker.Tracker
	issue     sourceIssue
	directory string
	now       func() time.Time
	retry     time.Duration
	parent    bool
}

func (a approvalChild) clock() time.Time {
	if a.now != nil {
		return a.now().UTC()
	}
	return time.Now().UTC()
}
func (a approvalChild) notice() notices {
	return notices{source: a.source, issue: a.issue, directory: a.directory, queue: filepath.Dir(filepath.Dir(a.directory)), approval: true}
}
func (a approvalChild) approvalDirectory() string { return filepath.Join(a.directory, "approval") }

type approvalView struct {
	me        int64
	comments  []tracker.Comment
	approvers []int64
	posts     []chain.ApprovalPost
	gos       []chain.ApprovalGO
	log       noticeLog
	boundary  int64
}

// Stop wins even when local comparison records or notification settlement fail.
func (a approvalChild) read(ctx context.Context, state chain.State, stage string) (approvalView, error) {
	v := approvalView{}
	cfg := a.cfg
	var operators []int64
	if cfg.Intake != nil {
		operators = cfg.Intake.StopUserIDs
	}
	base, recorder := approvalTracker(a.source)
	_, seenBeforeErr := os.Stat(filepath.Join(a.directory, "comments-seen.json"))
	stopped, err := savedStop(base, a.directory, a.issue, operators)
	if err != nil {
		return v, err
	}
	if stopped {
		return v, errApprovalStopped
	}
	readStarted := a.clock()
	rows, err := base.Comments(ctx, a.issue)
	if err != nil {
		return v, err
	}
	stop, err := stopInstruction(base, rows, a.issue, operators)
	if err != nil {
		return v, err
	}
	decoded := make([]tracker.Comment, 0, len(rows))
	for _, raw := range rows {
		c, e := base.ReadComment(raw, a.issue)
		if e != nil || !c.OnIssue {
			return v, errApprovalTampered
		}
		decoded = append(decoded, c)
	}
	var recordErr error
	if recorder != nil {
		recordErr = recorder.record(a.issue, decoded)
	}
	if stop != nil {
		if err := writeRuntimeFile(filepath.Join(a.directory, "stop-request.json"), stop); err != nil {
			return v, err
		}
		return v, errApprovalStopped
	}
	if seenBeforeErr != nil || recordErr != nil {
		return v, errApprovalTampered
	}
	me, err := a.source.Myself(ctx)
	if err != nil {
		return v, err
	}
	if me.ID <= 0 {
		return v, errors.New("approval runtime account is unavailable")
	}
	v.me = me.ID
	// The watcher can confirm a notice while Comments is in flight. Read the
	// durable notice log only after that read, under its existing mutex.
	n := a.notice()
	if a.parent {
		if err := n.flush(ctx); err != nil {
			release := n.hold()
			_, loadErr := n.load()
			release()
			if loadErr != nil {
				return v, errApprovalTampered
			}
			return v, err
		}
	}
	release := n.hold()
	defer release()
	v.log, err = n.load()
	if err != nil {
		return v, errApprovalTampered
	}
	if pendingNotice(v.log) >= 0 {
		return v, errApprovalRetry
	}
	v.posts, err = chain.ReadApprovalRecords[chain.ApprovalPost](a.approvalDirectory(), "post")
	if err != nil {
		return v, errApprovalTampered
	}
	if len(v.posts) > 0 {
		if _, err := os.Stat(n.path()); err != nil {
			return v, errApprovalTampered
		}
	}
	v.gos, err = chain.ReadApprovalRecords[chain.ApprovalGO](a.approvalDirectory(), "go")
	if err != nil {
		return v, errApprovalTampered
	}
	for _, p := range v.posts {
		if p.Resolution != "" && (p.Resolution != "rejected" && p.Resolution != "expired" || p.ResolvedAt.IsZero()) {
			return v, errApprovalTampered
		}
		for _, id := range p.ProcessedCommentIDs {
			if id <= 0 {
				return v, errApprovalTampered
			}
		}
		if p.N <= 0 || p.CommentID <= 0 || len(p.Digest) != 64 || p.DigestPrefix != p.Digest[:8] || p.CreatedAt.IsZero() || p.Deadline.IsZero() || len(p.Files) == 0 {
			return v, errApprovalTampered
		}
	}
	for _, g := range v.gos {
		if g.GOCommentID <= 0 || g.PostN <= 0 || g.AuthorID <= 0 || g.PostCommentID <= 0 || len(g.Digest) != 64 {
			return v, errApprovalTampered
		}
		switch g.Result {
		case "go":
			if g.AcceptedAt.IsZero() {
				return v, errApprovalTampered
			}
		case "unreadable_go", "rejection":
		default:
			return v, errApprovalTampered
		}
	}
	var configured []int64
	for _, s := range approvalStages(cfg) {
		if s.Name == stage && s.Approval != nil {
			configured = s.Approval.ApproverUserIDs
		}
	}
	byID := map[int64]tracker.Comment{}
	for _, c := range decoded {
		if !c.OnIssue || c.ID <= 0 || c.Author.ID <= 0 {
			return v, errApprovalTampered
		}
		byID[c.ID] = c
		v.comments = append(v.comments, c)
	}
	// A watcher readback may also have recorded the new comment in seen.
	// Retry before comparing that ledger against this older comment snapshot.
	// Only confirmation after this read started proves the snapshot is stale;
	// even the highest comment ID may have been deleted before the read.
	for _, record := range v.log.Notices {
		if record.Predates || record.Obsolete || record.PostedAt == nil || record.CommentID <= 0 {
			continue
		}
		if _, ok := byID[record.CommentID]; !ok && record.PostedAt.After(readStarted) {
			return v, errApprovalRetry
		}
	}
	accepted := map[int64]bool{}
	processed := map[int64]bool{}
	for _, p := range v.posts {
		for _, id := range p.ProcessedCommentIDs {
			processed[id] = true
		}
	}
	for _, g := range v.gos {
		processed[g.GOCommentID] = true
		if g.Result == "go" && strings.TrimRight(g.Body, " \t\r\n") == "GO "+g.Digest[:8] {
			accepted[g.GOCommentID] = true
		}
	}
	raw, err := os.ReadFile(filepath.Join(a.directory, "comments-seen.json"))
	if err != nil {
		return v, errApprovalTampered
	}
	var seen []seenComment
	if json.Unmarshal(raw, &seen) != nil {
		return v, errApprovalTampered
	}
	for _, s := range seen {
		if accepted[s.ID] {
			continue
		}
		c, ok := byID[s.ID]
		if !ok || c.Author.ID != s.Author || chain.ApprovalSHA256([]byte(c.Body)) != s.SHA256 || c.Created.IsZero() || c.Updated.IsZero() || !c.Created.Equal(s.Created) || !c.Updated.Equal(s.Updated) {
			return v, errApprovalTampered
		}
	}
	request, err := a.source.Request(ctx, a.issue.Key)
	if err != nil {
		return v, err
	}
	if request != state.Request {
		return v, errApprovalTampered
	}
	var original struct{ Updated time.Time }
	if json.Unmarshal(a.issue.Raw, &original) != nil || original.Updated.IsZero() {
		return v, errApprovalTampered
	}
	rule, err := a.source.TextFormattingRule(ctx)
	if err != nil {
		return v, err
	}
	if rule != "markdown" {
		return v, errApprovalTampered
	}
	// Sent text, not first-read text, is authoritative for every runtime post.
	known := map[int64]bool{}
	first := int64(0)
	current := int64(0)
	for _, record := range v.log.Notices {
		if record.Predates || record.Obsolete {
			continue
		}
		if record.PostedAt == nil || record.CommentID <= 0 {
			return v, errApprovalRetry
		}
		c, ok := byID[record.CommentID]
		if !ok || c.Author.ID != v.me || c.Body != record.Text || c.Created.IsZero() || !c.Created.Equal(c.Updated) {
			return v, errApprovalTampered
		}
		boundary := record.CommentID
		for _, c := range v.comments {
			if c.Author.ID == v.me && c.Body == record.Text && !c.Created.IsZero() && c.Created.Equal(c.Updated) {
				known[c.ID] = true
				if c.ID < boundary {
					boundary = c.ID
				}
			}
		}
		if record.Kind == approvalNotice {
			if first == 0 || boundary < first {
				first = boundary
			}
			if record.Approval != nil && record.Approval.Stage == stage {
				if boundary > current {
					current = boundary
				}
			}
		}
	}
	for _, p := range v.posts {
		c, ok := byID[p.CommentID]
		if !ok || c.Author.ID != v.me || !known[c.ID] || chain.ApprovalSHA256([]byte(c.Body)) != p.SentTextSHA256 {
			return v, errApprovalTampered
		}
	}
	for _, c := range v.comments {
		if c.ChangedTitleOrDescription && c.Created.After(original.Updated) {
			return v, errApprovalTampered
		}
		if accepted[c.ID] {
			continue
		}
		if c.Created.IsZero() || c.Updated.IsZero() {
			return v, errApprovalTampered
		}
		if first > 0 && c.ID > first {
			if c.Author.ID == v.me && c.Body != "" && !known[c.ID] {
				return v, errApprovalTampered
			}
			if c.Author.ID != v.me && !slices.Contains(configured, c.Author.ID) && (c.Body != "" || c.HasAttachment) {
				return v, errApprovalTampered
			}
		}
	}
	excluded := map[int64]bool{a.issue.Creator.ID: true, v.me: true}
	for _, s := range seen {
		if processed[s.ID] || known[s.ID] {
			continue
		}
		if current > 0 && s.ID > current {
			continue
		}
		c, ok := byID[s.ID]
		if !ok {
			return v, errApprovalTampered
		}
		control := firstInstructionLine(c.Body)
		if slices.Contains(configured, s.Author) && (control == "停止" || control == "再開") {
			continue
		}
		excluded[s.Author] = true
	}
	for _, id := range configured {
		if id > 0 && !excluded[id] && !slices.Contains(v.approvers, id) {
			v.approvers = append(v.approvers, id)
		}
	}
	v.boundary = current
	if len(v.approvers) == 0 {
		return v, errApprovalNoApprover
	}
	return v, nil
}

// Translate only posted text; saved failure reasons retain their internal names.
func approvalFailurePostText(reason, recorded string) string {
	descriptions := map[string]string{
		"rejected":           "承認者から修正を求められました",
		"expired":            "確認の期限が切れました",
		"bundle_name":        "確認用のファイルが決まりに合いません (名前)",
		"bundle_kind":        "確認用のファイルが決まりに合いません (種類)",
		"bundle_limit":       "確認用のファイルが決まりに合いません (量や形式)",
		"bundle_credential":  "確認用のファイルに認証情報が含まれていました",
		"show_missing":       "試し実行の結果のファイルがありません",
		"post_size":          "確認のコメントが長すぎるか、投稿できませんでした",
		"post_fence":         "確認のコメントに使えない文字や書式が含まれています",
		"post_masked":        "確認のコメントに認証情報が含まれています",
		"digest_collision":   "確認の対象を区別できませんでした",
		"receipt_unreadable": "実行結果を読み取れませんでした",
		"summary_unreadable": "やることの説明文が無いか、読めません",
	}
	text := "確認の途中で止まりました。理由: " + descriptions[reason]
	if strings.HasPrefix(recorded, "起動した・結果は読めない (効果不明): ") {
		text = "実行を開始しましたが、結果が分かりません。理由: " + descriptions[reason]
	}
	return text
}

func approvalPostText(stage chain.Stage, bundle chain.ApprovalBundle, post chain.ApprovalPost, state chain.State, credentials []string, posts []chain.ApprovalPost) (string, error) {
	var b strings.Builder
	b.WriteString("本番の操作の確認をお願いします。\n\n")
	summary, err := stage.Approval.SummaryPath()
	if err != nil {
		return "", err
	}
	if summary != "" {
		data := bundle.Contents[summary]
		if err := chain.ApprovalSummary(data); err != nil {
			return "", err
		}
		b.WriteString("やること:\n")
		b.Write(data)
		if !strings.HasSuffix(string(data), "\n") {
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}
	location, err := stage.Approval.TimeLocation()
	if err != nil {
		return "", err
	}
	fmt.Fprintf(&b, "実行してよければ、次の 1 行だけを新しいコメントに書いてください:\nGO %s\n\n期限: %s まで。過ぎると一時停止します。続けるときは「再開」と書いてください (計画を作り直して、もう一度確認します)。\n", post.DigestPrefix, post.Deadline.In(location).Format("2006-01-02 15:04 (MST)"))
	ending := "まだ実行していません。"
	for i := len(state.History) - 1; i >= 0; i-- {
		r := state.History[i]
		if r.Role != stage.Name || r.Speaker != "runtime" {
			continue
		}
		// The stage summary has no Error: the process rows immediately
		// before it carry the actual exit and interruption status.
		for j := i - 1; j >= 0 && state.History[j].Role == stage.Name && state.History[j].Speaker != "runtime"; j-- {
			prior := state.History[j]
			r.Interrupted = r.Interrupted || prior.Interrupted
			if r.Error == "" {
				r.Error = prior.Error
			}
		}
		switch {
		case strings.HasPrefix(r.Error, "起動した・結果は読めない (効果不明): "):
			ending = "途中で止まり、結果が分かりません。もう一度実行してよいか確認します。"
		case chain.ApprovalFailureReason(r) != "":
			continue
		case r.Interrupted:
			ending = "途中で止まり、結果が分かりません。もう一度実行してよいか確認します。"
		case r.Error != "":
			ending = "失敗しました。もう一度実行してよいか確認します。"
		default:
			ending = "正常に終わりました。もう一度実行してよいか確認します。"
		}
		break
	}
	fmt.Fprintf(&b, "前回の実行: %s\n", ending)
	if post.N > 1 {
		for _, previous := range posts {
			if previous.N == post.N-1 && previous.Stage == post.Stage {
				if previous.Digest == post.Digest {
					b.WriteString("計画は前回の確認と同じです。\n")
				} else {
					b.WriteString("計画は前回の確認から変わりました。\n")
				}
				break
			}
		}
	}
	b.WriteString("止めたいときは「停止」とだけ書いてください。GO を消したり書き直したりしても取り消しにはなりません。\n確認待ちの間に承認者が GO・停止・再開以外のコメントを書くと、修正の依頼として扱い、計画を作り直してから改めて確認します。\n最初の確認の投稿より後は、確認待ちかどうかに関わらず、承認者以外の人のコメントや、コメントの編集・削除 (受け付けた GO を除く) があると、安全のため実行せずに止まり、起票し直しになります。\n確認待ちでない間に承認者がコメントを書くと (停止・再開を除く)、その人は承認者から外れます。他に承認者がいなければ止まり、起票し直しになります。承認者が複数いるときは残りの承認者で続け、外れた人のコメントは修正の依頼として扱いません。\n")
	for _, show := range []bool{true, false} {
		if show {
			b.WriteString("\n## 試し実行の結果\n")
		} else {
			b.WriteString("\n## AI が書いた計画\n")
		}
		for _, f := range bundle.Files {
			if f.Path == summary || slices.Contains(stage.Approval.Show, f.Path) != show {
				continue
			}
			fmt.Fprintf(&b, "\n```\n%s\n", f.Path)
			b.Write(bundle.Contents[f.Path])
			if !strings.HasSuffix(string(bundle.Contents[f.Path]), "\n") {
				b.WriteByte('\n')
			}
			b.WriteString("```\n")
		}
	}
	text := b.String()
	if len(text) > chain.ApprovalBytes {
		return "", chain.ApprovalFailure("post_size")
	}
	for _, value := range credentials {
		if value != "" && (strings.Contains(text, value) || strings.Contains(text, url.QueryEscape(value))) {
			return "", chain.ApprovalFailure("post_masked")
		}
	}
	if !utf8.ValidString(text) {
		return "", chain.ApprovalFailure("post_fence")
	}
	for _, r := range text {
		if unicode.Is(unicode.Cf, r) || unicode.IsControl(r) && r != '\n' && r != '\t' {
			return "", chain.ApprovalFailure("post_fence")
		}
	}
	return text, nil
}

func (a approvalChild) post(ctx context.Context, stage chain.Stage, state chain.State, v approvalView) (chain.ApprovalPost, error) {
	n := a.notice()
	var prepared *noticeRecord
	lastStageEnd := time.Time{}
	for _, r := range state.History {
		if r.Role == stage.Name && r.Speaker == "runtime" && r.FinishedAt.After(lastStageEnd) {
			lastStageEnd = r.FinishedAt
		}
	}
	number := 1
	for i := range v.log.Notices {
		r := &v.log.Notices[i]
		if r.Kind != approvalNotice || r.Approval == nil {
			continue
		}
		number = max(number, r.Approval.N+1)
		if r.Approval.Stage == stage.Name && r.Approval.CreatedAt.After(lastStageEnd) {
			prepared = r
		}
	}
	if prepared == nil {
		cfg := a.cfg
		workspace := ""
		for _, role := range a.cfg.Roles {
			if role.Name == stage.Name && len(role.Processes) == 1 {
				workspace = role.Processes[0].Env["TASK_WORKSPACE"]
				if workspace == "" {
					workspace = role.Processes[0].Directory
				}
			}
		}
		if workspace == "" {
			return chain.ApprovalPost{}, chain.ApprovalFailure("bundle_kind")
		}
		now := a.clock()
		post := chain.ApprovalPost{N: number, Stage: stage.Name, CreatedAt: now, Deadline: now.Add(time.Duration(stage.Approval.ExpiryHours) * time.Hour).Truncate(time.Second)}
		// A crash before recording the notice leaves only an unposted copy. Remove
		// that private copy before rebuilding; a recorded copy is never rebuilt.
		if err := os.RemoveAll(post.BundlePath(a.approvalDirectory())); err != nil {
			return post, err
		}
		bundle, err := chain.CollectApprovalBundle(workspace, stage.Approval, post.BundlePath(a.approvalDirectory()), credentialValues(cfg))
		if err != nil {
			return post, err
		}
		post.Digest, post.DigestPrefix, post.Files = bundle.Digest, bundle.Digest[:8], bundle.Files
		if err = chain.CheckApprovalCollision(post.Digest, v.posts); err != nil {
			return post, err
		}
		text, err := approvalPostText(stage, bundle, post, state, credentialValues(cfg), v.posts)
		if err != nil {
			return post, err
		}
		post.SentTextSHA256 = chain.ApprovalSHA256([]byte(text))
		prepared = &noticeRecord{Kind: approvalNotice, Event: strconv.Itoa(number), Text: text, Approval: &post}
	}
	post := *prepared.Approval
	copy, copyErr := chain.ReadApprovalBundle(post.BundlePath(a.approvalDirectory()))
	if copyErr != nil || copy.Digest != post.Digest || !slices.Equal(copy.Files, post.Files) {
		return post, errApprovalTampered
	}
	if prepared.Obsolete {
		return post, chain.ApprovalFailure("post_size")
	}
	if _, err := kindSince(n.queue, approvalNotice, time.Now().UTC()); err != nil {
		return post, err
	}
	err := n.sayAs(ctx, approvalNotice, prepared.Event, time.Now().UTC(), func(log noticeLog, _ time.Time) (noticeRecord, bool) {
		for _, r := range log.Notices {
			if r.Kind == approvalNotice && r.Event == prepared.Event {
				return r, false
			}
		}
		return *prepared, true
	})
	if err != nil {
		return post, err
	}
	release := n.hold()
	log, err := n.load()
	release()
	if err != nil {
		return post, err
	}
	text := ""
	for _, r := range log.Notices {
		if r.Kind == approvalNotice && r.Event == prepared.Event {
			if r.Obsolete {
				return post, chain.ApprovalFailure("post_size")
			}
			if r.PostedAt != nil && !r.Predates && r.CommentID > 0 {
				post.CommentID = r.CommentID
				text = r.Text
			}
		}
	}
	if post.CommentID <= 0 {
		return post, errApprovalRetry
	}
	rows, err := a.source.Comments(ctx, a.issue)
	if err != nil {
		return post, err
	}
	found := false
	for _, raw := range rows {
		c, e := a.source.ReadComment(raw, a.issue)
		if e != nil {
			return post, errApprovalPostMismatch
		}
		if c.ID == post.CommentID {
			found = c.OnIssue && c.Author.ID == v.me && c.Body == text && !c.Created.IsZero() && c.Created.Equal(c.Updated)
		}
	}
	if !found || chain.ApprovalSHA256([]byte(text)) != post.SentTextSHA256 {
		return post, errApprovalPostMismatch
	}
	if err = post.Save(a.approvalDirectory()); err != nil {
		return post, err
	}
	if err = a.pruneBundles(post.N); err != nil {
		return post, err
	}
	return post, nil
}

func approvalOrdinary(body string) bool {
	trimmed := strings.TrimRight(body, " \t\r\n")
	if strings.ContainsAny(trimmed, "\r\n") {
		return firstInstructionLine(body) != "停止" && firstInstructionLine(body) != "再開"
	}
	normalized := strings.Map(func(r rune) rune {
		if r >= '！' && r <= '～' {
			return r - '！' + '!'
		}
		return r
	}, trimmed)
	return firstInstructionLine(body) != "停止" && firstInstructionLine(body) != "再開" && !strings.HasPrefix(strings.ToUpper(normalized), "GO")
}
func (a approvalChild) consume(state *chain.State, post chain.ApprovalPost, v approvalView, readErr error) (bool, error) {
	for _, g := range v.gos {
		if g.Result != "go" || !g.ConsumedAt.IsZero() || !g.ExpiredAt.IsZero() {
			continue
		}
		valid := readErr == nil && g.PostN == post.N && g.PostCommentID == post.CommentID && g.Digest == post.Digest && a.clock().Before(post.Deadline) && slices.Contains(v.approvers, g.AuthorID) && g.LaunchHistoryLen == len(state.History)+1 && !g.PendingSince.IsZero() && !g.PendingSince.Before(g.AcceptedAt)
		if strings.TrimRight(g.Body, " \t\r\n") != "GO "+post.DigestPrefix {
			valid = false
		}
		for _, c := range v.comments {
			if c.ID == g.GOCommentID {
				continue
			}
			if c.ID > g.GOCommentID || c.Created.After(g.AcceptedAt) {
				if firstInstructionLine(c.Body) == "停止" || firstInstructionLine(c.Body) == "再開" || slices.Contains(v.approvers, c.Author.ID) && approvalOrdinary(c.Body) {
					valid = false
				}
			}
		}
		bundle, err := chain.ReadApprovalBundle(post.BundlePath(a.approvalDirectory()))
		if err != nil || bundle.Digest != post.Digest || !slices.Equal(bundle.Files, post.Files) {
			valid = false
		}
		if !valid {
			g.ExpiredAt = a.clock()
			if err = g.Save(a.approvalDirectory()); err != nil {
				return false, err
			}
			state.History = append(state.History, chain.Result{Role: "router", Speaker: "runtime", Output: "承認の GO は失効した", FinishedAt: a.clock()})
			continue
		}
		g.ConsumedAt = a.clock()
		if err = g.Save(a.approvalDirectory()); err != nil {
			return false, err
		}
		state.History = append(state.History, chain.Result{Role: "router", Speaker: "runtime", Output: "承認の GO を消費した", FinishedAt: a.clock()})
		state.PendingSince = g.PendingSince
		state.AwaitingApproval = nil
		return true, nil
	}
	return false, readErr
}
func (a approvalChild) before(ctx context.Context, state *chain.State, next chain.Assignment) error {
	err := a.beforeRead(ctx, state, next)
	if reason := approvalPauseReason(err); reason != "" {
		state.PauseApproval(reason, a.clock())
		return chain.ErrApprovalPause
	}
	return err
}
func (a approvalChild) beforeRead(ctx context.Context, state *chain.State, next chain.Assignment) error {
	stage, _, ok := state.ApprovalStage(next.Role)
	if !ok {
		return nil
	}
	for {
		v, err := a.read(ctx, *state, stage.Name)
		if err == nil {
			// A posted bundle belongs to this attempt until an actual stage result
			// follows it. A parent clears AwaitingApproval to request consumption.
			var current *chain.ApprovalPost
			for i := range v.posts {
				p := &v.posts[i]
				if p.Stage != stage.Name {
					continue
				}
				used := false
				for _, r := range state.History {
					if r.Role == stage.Name && !r.FinishedAt.Before(p.CreatedAt) {
						used = true
					}
				}
				if !used && (current == nil || p.N > current.N) {
					current = p
				}
			}
			if current != nil {
				// read above is the required fresh stop/comments/settlement/comparison
				// read immediately before consumption, including current approvers.
				fresh, readErr := a.read(ctx, *state, stage.Name)
				if readErr != nil {
					if errors.Is(readErr, errApprovalTampered) || errors.Is(readErr, errApprovalNoApprover) || errors.Is(readErr, errApprovalStopped) {
						if err := a.expireOutstanding(v.gos); err != nil {
							return err
						}
						return readErr
					}
					if err := a.retryRead(ctx); err != nil {
						return err
					}
					continue
				}
				consumed, consumeErr := a.consume(state, *current, fresh, nil)
				if consumeErr != nil {
					return consumeErr
				}
				if consumed {
					return nil
				}
				state.AwaitingApproval = &chain.AwaitingApproval{Stage: stage.Name, PostN: current.N, PostCommentID: current.CommentID, Deadline: current.Deadline}
				return chain.ErrAwaitingApproval
			}
			post, postErr := a.post(ctx, stage, *state, v)
			err = postErr
			if err == nil {
				state.History = append(state.History, chain.Result{Role: "router", Speaker: "runtime", Output: "承認の投稿を読み戻して確認した", FinishedAt: a.clock()})
				state.AwaitingApproval = &chain.AwaitingApproval{Stage: stage.Name, PostN: post.N, PostCommentID: post.CommentID, Deadline: post.Deadline}
				return chain.ErrAwaitingApproval
			}
		}
		if errors.Is(err, errApprovalTampered) || errors.Is(err, errApprovalNoApprover) || errors.Is(err, errApprovalStopped) {
			// Failures before a view is complete still expire outstanding accepted GO.
			gos, loadErr := chain.ReadApprovalRecords[chain.ApprovalGO](a.approvalDirectory(), "go")
			if loadErr == nil {
				if saveErr := a.expireOutstanding(gos); saveErr != nil {
					return saveErr
				}
			}
			return err
		}
		if errors.Is(err, errApprovalRetry) {
			n := a.notice()
			release := n.hold()
			log, loadErr := n.load()
			i := pendingNotice(log)
			if loadErr == nil && i >= 0 && log.Notices[i].Kind == approvalNotice {
				err = n.settle(ctx, &log, i, false)
			}
			release()
		}
		var refusal chain.ApprovalFailure
		if errors.As(err, &refusal) || errors.Is(err, errApprovalPostMismatch) {
			return err
		}
		if err := a.retryRead(ctx); err != nil {
			return err
		}
	}
}

// Retain only the current copy and copies consumed by actual launch records.
func (a approvalChild) pruneBundles(current int) error {
	gos, err := chain.ReadApprovalRecords[chain.ApprovalGO](a.approvalDirectory(), "go")
	if err != nil {
		return err
	}
	keep := map[int]bool{current: true}
	for _, g := range gos {
		if !g.ConsumedAt.IsZero() {
			keep[g.PostN] = true
		}
	}
	entries, err := os.ReadDir(a.approvalDirectory())
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "bundle-") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimPrefix(entry.Name(), "bundle-"))
		if err != nil || n <= 0 {
			return errApprovalTampered
		}
		if !keep[n] {
			if err = os.RemoveAll(filepath.Join(a.approvalDirectory(), entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a approvalChild) expireOutstanding(gos []chain.ApprovalGO) error {
	for _, g := range gos {
		if g.Result == "go" && g.ConsumedAt.IsZero() && g.ExpiredAt.IsZero() {
			g.ExpiredAt = a.clock()
			if err := g.Save(a.approvalDirectory()); err != nil {
				return err
			}
		}
	}
	return nil
}
func (a approvalChild) retryRead(ctx context.Context) error {
	delay := a.retry
	if delay <= 0 {
		delay = 10 * time.Second
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func approvalTracker(source tracker.Tracker) (tracker.Tracker, *recordingTracker) {
	switch s := source.(type) {
	case *recordingTracker:
		return s.Tracker, s
	case recordingCommentEditor:
		return s.Tracker, s.recordingTracker
	default:
		return source, nil
	}
}

// Read native stop controls even if the approval ledger cannot be written.
// The read is still recorded whenever possible, but a broken ledger is never
// permission to keep running past an authorized stop.
func readStopComments(ctx context.Context, source tracker.Tracker, issue sourceIssue, operators []int64) ([]json.RawMessage, json.RawMessage, error) {
	base, recorder := approvalTracker(source)
	rows, err := base.Comments(ctx, issue)
	if err != nil {
		return rows, nil, err
	}
	stop, err := stopInstruction(base, rows, issue, operators)
	if err != nil {
		return rows, nil, err
	}
	if recorder != nil {
		var comments []tracker.Comment
		for _, raw := range rows {
			c, e := base.ReadComment(raw, issue)
			if e == nil && c.OnIssue {
				comments = append(comments, c)
			}
		}
		if err := recorder.record(issue, comments); err != nil && stop == nil {
			return rows, nil, err
		}
	}
	return rows, stop, nil
}
