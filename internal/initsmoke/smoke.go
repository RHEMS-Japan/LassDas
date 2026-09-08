// Package initsmoke follows one explicitly confirmed request through the
// existing tracker and runtime. It owns no queue, dispatcher, or retry worker.
package initsmoke

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"automation.internal/ticket-ingress/internal/initwizard"
)

var ErrPending = errors.New("動作確認は進行中です。init は未完了です")

// Record contains public request data and observations, never the requester's
// temporary credential. Attempted is persisted before sending the POST.
type Record struct {
	Correlation  string    `json:"correlation"`
	ProjectID    int64     `json:"project_id"`
	CreatorID    int64     `json:"creator_id"`
	Repository   string    `json:"repository"`
	RepositoryID int64     `json:"repository_id"`
	Branch       string    `json:"branch"`
	BaseSHA      string    `json:"base_sha"`
	Path         string    `json:"path"`
	BeforeSHA256 string    `json:"before_sha256"`
	After        string    `json:"after"`
	Addition     string    `json:"addition"`
	Summary      string    `json:"summary"`
	Description  string    `json:"description"`
	Attempted    bool      `json:"attempted"`
	IssueID      int64     `json:"issue_id,omitempty"`
	IssueKey     string    `json:"issue_key,omitempty"`
	DeliveryID   string    `json:"delivery_id,omitempty"`
	PRURL        string    `json:"pr_url,omitempty"`
	Step         string    `json:"step,omitempty"`
	VerifiedAt   time.Time `json:"verified_at,omitempty"`
}

type Observation struct {
	DeliveryID, Step, Detail, Terminal, PRURL string
	Verified                                  bool
}
type Observer interface {
	Observe(context.Context, *initwizard.State, Record, initwizard.Secrets) (Observation, error)
}
type Runner struct {
	UI                        initwizard.UI
	API                       initwizard.API
	Observer                  Observer
	WaitTimeout, PollInterval time.Duration
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func marker(correlation string) string { return "ticket-automation-init:" + correlation }

func (r Runner) Run(ctx context.Context, s *initwizard.State, secrets initwizard.Secrets, save func() error) error {
	if r.UI == nil || r.Observer == nil || s == nil || save == nil {
		return errors.New("動作確認の接続がありません")
	}
	var record Record
	if len(s.Smoke) > 0 {
		if json.Unmarshal(s.Smoke, &record) != nil || len(record.Correlation) != 32 {
			return errors.New("動作確認の台帳が不正です")
		}
		if record.ProjectID != s.Tracker.ProjectID || record.CreatorID != s.Tracker.AllowedCreatorID || record.Repository != s.Repository || record.RepositoryID != s.RepositoryID || record.Branch != s.Branch {
			return errors.New("保存済みの動作確認と接続先が違います。同じ接続先で再開してください")
		}
	} else {
		var err error
		record, err = r.propose(ctx, s, secrets)
		if err != nil {
			return err
		}
	}
	persist := func() error {
		raw, err := json.Marshal(record)
		if err != nil {
			return err
		}
		s.Smoke = raw
		return save()
	}
	if err := persist(); err != nil {
		return err
	}
	if record.IssueID == 0 {
		if record.Attempted {
			issue, err := r.reconcile(ctx, s, secrets, record)
			if err != nil {
				return err
			}
			record.IssueID, record.IssueKey = issue.ID, issue.Key
			if err := persist(); err != nil {
				return err
			}
		} else if err := r.create(ctx, s, secrets, &record, persist); err != nil {
			return err
		}
	}
	r.UI.Info("動作確認: " + record.IssueKey + " を追跡します")
	wait := r.WaitTimeout
	if wait <= 0 {
		wait = 30 * time.Minute
	}
	interval := r.PollInterval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	previous := ""
	for {
		observation, err := r.Observer.Observe(waitCtx, s, record, secrets)
		if err != nil {
			if waitCtx.Err() != nil {
				break
			}
			return err
		}
		if observation.DeliveryID != "" {
			if record.DeliveryID != "" && record.DeliveryID != observation.DeliveryID {
				return errors.New("同じ課題に別の delivery が見つかりました。保存した実行との照合が必要です")
			}
			record.DeliveryID = observation.DeliveryID
		}
		record.Step = observation.Step
		if observation.Verified {
			if record.DeliveryID == "" || observation.PRURL == "" {
				return errors.New("PR の確定記録が不足しています")
			}
			record.PRURL = observation.PRURL
			record.VerifiedAt = time.Now().UTC()
			if err := persist(); err != nil {
				return err
			}
			r.UI.Info("PR と工程記録の照合完了: " + record.PRURL)
			return nil
		}
		if err := persist(); err != nil {
			return err
		}
		message := strings.TrimSpace(observation.Step + " " + observation.Detail)
		if message != "" && message != previous {
			r.UI.Info(message)
			previous = message
		}
		if observation.Terminal != "" && observation.Terminal != "success" {
			return fmt.Errorf("動作確認は %s で停止しています。課題 %s の報告を確認してください", observation.Terminal, record.IssueKey)
		}
		if observation.Step == "question" || observation.Step == "attention" {
			return fmt.Errorf("%w。課題 %s の質問・要確認事項に対応して同じ init を再実行してください", ErrPending, record.IssueKey)
		}
		timer := time.NewTimer(interval)
		select {
		case <-waitCtx.Done():
			timer.Stop()
		case <-timer.C:
		}
		if waitCtx.Err() != nil {
			break
		}
	}
	r.UI.Info("本体は継続中です。同じ init を再実行すると " + record.IssueKey + " の追跡を再開します")
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return ErrPending
}

func allowedFile(name string, scopes []string) bool {
	if name == "" || path.Clean(name) != name || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\\r\n\x00") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") {
			return false
		}
	}
	for _, scope := range scopes {
		if name == scope || strings.HasSuffix(scope, "/") && strings.HasPrefix(name, scope) {
			return true
		}
	}
	return false
}

func (r Runner) propose(ctx context.Context, s *initwizard.State, secrets initwizard.Secrets) (Record, error) {
	var tree struct {
		Truncated bool
		Tree      []struct{ Path, Type, Mode string }
	}
	if err := r.API.GitHub(ctx, "/repos/"+s.Repository+"/git/trees/"+s.BaseSHA+"?recursive=1", secrets["TARGET_GITHUB_TOKEN"], &tree); err != nil {
		return Record{}, err
	}
	if tree.Truncated {
		return Record{}, errors.New("動作確認のファイル一覧が省略されました")
	}
	var choices []string
	for _, file := range tree.Tree {
		if file.Type == "blob" && file.Mode != "120000" && allowedFile(file.Path, s.Mode.AllowedFilePrefixes) {
			switch strings.ToLower(path.Ext(file.Path)) {
			case ".md", ".txt", ".go", ".js", ".ts":
				choices = append(choices, file.Path)
			}
		}
	}
	if len(choices) == 0 {
		return Record{}, errors.New("許可範囲に小変更を提案できる通常テキストファイルがありません")
	}
	sort.Strings(choices)
	suggested := choices[0]
	for _, file := range choices {
		if file == "README.md" {
			suggested = file
		}
	}
	filename, err := r.UI.Ask("smoke-file", "動作確認で末尾に 1 行だけ追加するファイル\n"+strings.Join(choices, "\n"), suggested, false)
	if err != nil {
		return Record{}, err
	}
	found := false
	for _, file := range choices {
		found = found || file == filename
	}
	if !found {
		return Record{}, errors.New("表示した許可ファイルを選んでください")
	}
	before, err := r.API.File(ctx, s.Repository, filename, s.BaseSHA, secrets["TARGET_GITHUB_TOKEN"])
	if err != nil {
		return Record{}, err
	}
	if len(before) > 64*1024 || !utf8.Valid(before) || strings.ContainsRune(string(before), '\x00') {
		return Record{}, errors.New("動作確認には 64 KiB 以下の UTF-8 テキストを選んでください")
	}
	suffix := make([]byte, 16)
	if _, err := rand.Read(suffix); err != nil {
		return Record{}, err
	}
	correlation := hex.EncodeToString(suffix)
	line := "Initial request check: " + correlation
	switch strings.ToLower(path.Ext(filename)) {
	case ".go", ".js", ".ts":
		line = "// " + line
	}
	addition := ""
	if len(before) > 0 && before[len(before)-1] != '\n' {
		addition = "\n"
	}
	addition += line + "\n"
	record := Record{Correlation: correlation, ProjectID: s.Tracker.ProjectID, CreatorID: s.Tracker.AllowedCreatorID, Repository: s.Repository, RepositoryID: s.RepositoryID, Branch: s.Branch, BaseSHA: s.BaseSHA, Path: filename, BeforeSHA256: digest(string(before)), After: string(before) + addition, Addition: addition, Summary: "CLI の初回動作確認"}
	record.Description = fmt.Sprintf("対象は %s。%s の末尾に次の 1 行を追加してください。既存の内容と他のファイルは維持してください。\n\n%s\n\n登録済みの検証コマンドを通し、%s 枝に PR を出してください。調査・設計、設計レビュー、写し役、候補レビュー、検証を経た変更を確認します。\n\n%s", s.Repository, filename, line, s.Branch, marker(correlation))
	return record, nil
}

type trackerIssue struct {
	ID          int64  `json:"id"`
	Key         string `json:"issueKey"`
	Summary     string `json:"summary"`
	Description string `json:"description"`
	ProjectID   int64  `json:"projectId"`
	CreatedUser struct {
		ID int64 `json:"id"`
	} `json:"createdUser"`
}

func issueMatches(issue trackerIssue, record Record) bool {
	return issue.ID > 0 && issue.Key != "" && issue.ProjectID == record.ProjectID && issue.CreatedUser.ID == record.CreatorID && issue.Summary == record.Summary && issue.Description == record.Description
}

func (r Runner) create(ctx context.Context, s *initwizard.State, secrets initwizard.Secrets, record *Record, persist func() error) error {
	key, err := r.UI.Ask("smoke-personal-key", "外で取得した起票者本人の API キー (この操作限り・保存しません)", "", true)
	if err != nil {
		return err
	}
	defer func() { key = "" }()
	var myself initwizard.NamedID
	if err = r.API.Tracker(ctx, s, key, "GET", "/api/v2/users/myself", nil, &myself); err != nil {
		return err
	}
	if myself.ID != s.Tracker.AllowedCreatorID {
		return errors.New("本人の ID と許可起票者が一致しません。起票していません")
	}
	var kinds, priorities []initwizard.NamedID
	if err = r.API.Tracker(ctx, s, key, "GET", fmt.Sprintf("/api/v2/projects/%d/issueTypes", s.Tracker.ProjectID), nil, &kinds); err != nil {
		return err
	}
	if err = r.API.Tracker(ctx, s, key, "GET", "/api/v2/priorities", nil, &priorities); err != nil {
		return err
	}
	kind, err := r.selectID("smoke-kind", "起票する課題種別", kinds)
	if err != nil {
		return err
	}
	priority, err := r.selectID("smoke-priority", "起票する優先度", priorities)
	if err != nil {
		return err
	}
	yes, err := r.UI.Confirm(fmt.Sprintf("%s に本人名義で課題を 1 件作成します。\n%s\n\nファイル: %s\n追加する内容:\n%s\nPR の宛先: %s / %s", s.Tracker.ProjectKey, record.Summary, record.Path, record.Addition, record.Repository, record.Branch))
	if err != nil {
		return err
	}
	if !yes {
		return errors.New("動作確認の起票を中断しました")
	}
	record.Attempted = true
	if err = persist(); err != nil {
		return err
	}
	form := url.Values{"projectId": {strconv.FormatInt(record.ProjectID, 10)}, "summary": {record.Summary}, "description": {record.Description}, "issueTypeId": {strconv.FormatInt(kind, 10)}, "priorityId": {strconv.FormatInt(priority, 10)}, "categoryId[]": {strconv.FormatInt(s.Tracker.RequiredCategoryID, 10)}}
	var issue trackerIssue
	err = r.API.Tracker(ctx, s, key, "POST", "/api/v2/issues", form, &issue)
	if err != nil {
		var apiErr *initwizard.APIError
		if errors.As(err, &apiErr) && (apiErr.Status == 400 || apiErr.Status == 401 || apiErr.Status == 403 || apiErr.Status == 404 || apiErr.Status == 422) {
			record.Attempted = false
			if saveErr := persist(); saveErr != nil {
				return saveErr
			}
			return err
		}
		return errors.New("起票の成否が不明です。再実行時は相関 ID を検索し、未確認のまま再 POST しません")
	}
	if !issueMatches(issue, *record) {
		return errors.New("起票応答の身元・内容を確認できません。再実行時に作成済み課題を照合します")
	}
	record.IssueID, record.IssueKey = issue.ID, issue.Key
	return persist()
}

func (r Runner) selectID(id, label string, items []initwizard.NamedID) (int64, error) {
	if len(items) == 0 {
		return 0, errors.New(label + "の実在一覧が空です")
	}
	var options []string
	for _, item := range items {
		if item.ID <= 0 {
			return 0, errors.New(label + "の ID が不正です")
		}
		options = append(options, fmt.Sprintf("%d: %s", item.ID, item.Name))
	}
	value, err := r.UI.Ask(id, label+" (数値 ID)\n"+strings.Join(options, "\n"), strconv.FormatInt(items[0].ID, 10), false)
	if err != nil {
		return 0, err
	}
	number, err := strconv.ParseInt(value, 10, 64)
	if err == nil {
		for _, item := range items {
			if item.ID == number {
				return number, nil
			}
		}
	}
	return 0, errors.New(label + "は一覧の ID で指定してください")
}

func (r Runner) reconcile(ctx context.Context, s *initwizard.State, secrets initwizard.Secrets, record Record) (trackerIssue, error) {
	var found []trackerIssue
	for page := 0; page < 10; page++ {
		form := url.Values{"projectId[]": {strconv.FormatInt(record.ProjectID, 10)}, "createdUserId[]": {strconv.FormatInt(record.CreatorID, 10)}, "keyword": {marker(record.Correlation)}, "count": {"100"}, "offset": {strconv.Itoa(page * 100)}}
		var issues []trackerIssue
		if err := r.API.Tracker(ctx, s, secrets["BACKLOG_API_KEY"], "GET", "/api/v2/issues", form, &issues); err != nil {
			return trackerIssue{}, err
		}
		for _, issue := range issues {
			if issueMatches(issue, record) {
				found = append(found, issue)
			}
		}
		if len(issues) < 100 {
			if len(found) != 1 {
				return trackerIssue{}, errors.New("起票の成否を一意に照合できません。再 POST はしていません。対象 project の作成結果を確認してください")
			}
			return found[0], nil
		}
	}
	return trackerIssue{}, errors.New("作成結果の検索上限に達しました。再 POST はしていません")
}
