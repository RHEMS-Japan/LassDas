package initwizard

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/backlog"
	"automation.internal/ticket-ingress/internal/worker"
)

type API struct{ HTTP *http.Client }

type APIError struct {
	Service string
	Status  int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s API が %d を返しました。鍵と対象への権限を確認してください", e.Service, e.Status)
}
func (a API) call(ctx context.Context, service, method, endpoint, key string, body []byte, contentType string, out any) error {
	request, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("API リクエストの接続先が不正です")
	}
	if key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	request.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	if a.HTTP != nil {
		copy := *a.HTTP
		client = &copy
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return errors.New(service + " API に接続できません")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &APIError{service, response.StatusCode}
	}
	if out == nil {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 4*1024*1024+1))
	if err != nil || len(data) > 4*1024*1024 {
		return errors.New(service + " API 応答を読み取れません")
	}
	if json.Unmarshal(data, out) != nil {
		return errors.New(service + " API 応答の形式が違います")
	}
	return nil
}

var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func (a API) GitHub(ctx context.Context, path, key string, out any) error {
	return a.call(ctx, "GitHub", http.MethodGet, "https://api.github.com"+path, key, nil, "", out)
}

func (a API) Repository(ctx context.Context, name, key string) (int64, string, error) {
	if !repositoryPattern.MatchString(name) {
		return 0, "", errors.New("repo は owner/name の形式で入力してください")
	}
	var repo struct {
		ID            int64  `json:"id"`
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
	}
	if err := a.GitHub(ctx, "/repos/"+name, key, &repo); err != nil {
		return 0, "", err
	}
	if repo.ID <= 0 || !strings.EqualFold(repo.FullName, name) || repo.DefaultBranch == "" {
		return 0, "", errors.New("repo の身元を確認できません")
	}
	return repo.ID, repo.DefaultBranch, nil
}

func (a API) Branch(ctx context.Context, repository, branch, key string) (string, error) {
	if branch == "" || strings.ContainsAny(branch, "\r\n\x00") || strings.Contains(branch, "..") {
		return "", errors.New("取り込み枝を指定してください")
	}
	var result struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := a.GitHub(ctx, "/repos/"+repository+"/branches/"+url.PathEscape(branch), key, &result); err != nil {
		return "", err
	}
	if !worker.ValidToolSHA(result.Commit.SHA) {
		return "", errors.New("枝の SHA を確認できません")
	}
	return result.Commit.SHA, nil
}

func (a API) File(ctx context.Context, repository, path, sha, key string) ([]byte, error) {
	var result struct {
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
		Size     int    `json:"size"`
	}
	if err := a.GitHub(ctx, "/repos/"+repository+"/contents/"+path+"?ref="+url.QueryEscape(sha), key, &result); err != nil {
		return nil, err
	}
	if result.Encoding != "base64" || result.Size > 256*1024 {
		return nil, errors.New("repo ファイルが大きすぎるか通常ファイルではありません")
	}
	value, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(result.Content, "\n", ""))
	if err != nil {
		return nil, errors.New("repo ファイルの内容を読み取れません")
	}
	return value, nil
}

func (a API) Tracker(ctx context.Context, s *State, key, method, path string, form url.Values, out any) error {
	config := backlog.Config{Origin: s.Tracker.Origin, SpaceKey: s.Tracker.SpaceKey, APIKey: key, Timeout: 30 * time.Second, MaxResponseBytes: 4 * 1024 * 1024}
	if err := config.Validate(); err != nil {
		return errors.New("Backlog 接続先または鍵が不正です")
	}
	query := url.Values{"apiKey": {key}}
	body := []byte(form.Encode())
	if method == http.MethodGet {
		for name, values := range form {
			query[name] = append([]string(nil), values...)
		}
		body = nil
	}
	endpoint := s.Tracker.Origin + path + "?" + query.Encode()
	return a.call(ctx, "Backlog", method, endpoint, "", body, "application/x-www-form-urlencoded", out)
}

type NamedID struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

func findID(items []NamedID, name string) int64 {
	for _, item := range items {
		if item.Name == name {
			return item.ID
		}
	}
	return 0
}
func containsID(items []NamedID, id int64) bool {
	for _, item := range items {
		if item.ID == id {
			return true
		}
	}
	return false
}

func (w *Wizard) tracker(ctx context.Context, s *State, secrets Secrets, save func() error) error {
	key := secrets["BACKLOG_API_KEY"]
	var bot NamedID
	if err := w.API.Tracker(ctx, s, key, "GET", "/api/v2/users/myself", nil, &bot); err != nil {
		return err
	}
	if bot.ID <= 0 {
		return errors.New("bot の本人情報を確認できません")
	}
	var project struct {
		ID         int64  `json:"id"`
		ProjectKey string `json:"projectKey"`
	}
	if err := w.API.Tracker(ctx, s, key, "GET", "/api/v2/projects/"+url.PathEscape(s.Tracker.ProjectKey), nil, &project); err != nil {
		return err
	}
	if project.ID <= 0 || project.ProjectKey != s.Tracker.ProjectKey {
		return errors.New("トラッカー project の身元が一致しません")
	}
	s.Tracker.ProjectID = project.ID
	base := "/api/v2/projects/" + strconv.FormatInt(project.ID, 10)
	var users []NamedID
	if err := w.API.Tracker(ctx, s, key, "GET", base+"/users", nil, &users); err != nil {
		return err
	}
	if !containsID(users, s.Tracker.AllowedCreatorID) || s.Tracker.AllowedCreatorID == bot.ID {
		return errors.New("許可起票者は project の利用者で、bot と異なる本人の ID にしてください")
	}
	var categories, statuses []NamedID
	if err := w.API.Tracker(ctx, s, key, "GET", base+"/categories", nil, &categories); err != nil {
		return err
	}
	if err := w.API.Tracker(ctx, s, key, "GET", base+"/statuses", nil, &statuses); err != nil {
		return err
	}
	s.Tracker.RequiredCategoryID = findID(categories, s.Category)
	ids := []*int64{&s.Tracker.BoardStatuses.Running, &s.Tracker.BoardStatuses.AwaitingAnswer, &s.Tracker.BoardStatuses.Delivered, &s.Tracker.BoardStatuses.NeedsAttention}
	missing := []string{}
	if s.Tracker.RequiredCategoryID == 0 {
		missing = append(missing, "カテゴリ: "+s.Category)
	}
	for i, name := range s.StatusNames {
		*ids[i] = findID(statuses, name)
		if *ids[i] == 0 {
			missing = append(missing, "状態: "+name)
		}
	}
	if len(missing) > 0 {
		if err := w.confirm(s, "不足する項目だけ作成します:\n"+strings.Join(missing, "\n")); err != nil {
			return err
		}
		adminKey := ""
		defer func() { adminKey = "" }()
		create := func(kind, name, color string) (int64, error) {
			form := url.Values{"name": {name}}
			if color != "" {
				form.Set("color", color)
			}
			var created NamedID
			activeKey := key
			if adminKey != "" {
				activeKey = adminKey
			}
			err := w.API.Tracker(ctx, s, activeKey, "POST", base+"/"+kind, form, &created)
			var status *APIError
			if errors.As(err, &status) && (status.Status == 401 || status.Status == 403) && adminKey == "" {
				var inputErr error
				adminKey, inputErr = w.ask(s, "tracker-admin-key", "プロジェクト管理者が外で取得した API キー (この作成限り・保存しません)", "", true)
				if inputErr != nil {
					return 0, inputErr
				}
				// Retry is safe only after an explicit rejected response, never a transport error.
				err = w.API.Tracker(ctx, s, adminKey, "POST", base+"/"+kind, form, &created)
			}
			if err != nil {
				return 0, err
			}
			if created.ID <= 0 {
				return 0, errors.New("作成結果の ID がありません。再実行時に実在一覧から照合します")
			}
			return created.ID, nil
		}
		var err error
		if s.Tracker.RequiredCategoryID == 0 {
			s.Tracker.RequiredCategoryID, err = create("categories", s.Category, "")
			if err != nil {
				return err
			}
			if err = save(); err != nil {
				return err
			}
		}
		colors := []string{"#3b9dbd", "#eda62a", "#393939", "#ea2c00"}
		for i, id := range ids {
			if *id == 0 {
				*id, err = create("statuses", s.StatusNames[i], colors[i])
				if err != nil {
					return err
				}
				if err = save(); err != nil {
					return err
				}
			}
		}
	}
	seen := map[int64]bool{}
	for _, id := range ids {
		if *id <= 0 || seen[*id] {
			return errors.New("4 状態は異なる実在 ID にしてください")
		}
		seen[*id] = true
	}
	s.Tracker.AllowedActivityType = 1
	s.Tracker.OperatorUserIDs = []int64{s.Tracker.AllowedCreatorID}
	return save()
}

type modelAPI struct {
	API     API
	Secrets Secrets
}

func (a modelAPI) ChatCompletions(ctx context.Context, endpoint worker.ModelEndpoint, request worker.ChatRequest) (*worker.ChatResponse, error) {
	if err := worker.ValidateModelEndpoint(endpoint); err != nil {
		return nil, err
	}
	key := a.Secrets[endpoint.APIKeyEnv]
	if key == "" {
		return nil, errors.New("モデルの鍵がありません")
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	var response worker.ChatResponse
	err = a.API.call(ctx, "モデル", "POST", endpoint.BaseURL+"/chat/completions", key, raw, "application/json", &response)
	return &response, err
}
func (w *Wizard) modelPreflight(ctx context.Context, s *State, secrets Secrets) error {
	if err := DistinctKeys(s, secrets); err != nil {
		return err
	}
	if err := w.confirm(s, "全身元の疎通を試します。モデル API の呼び出しには費用が発生します"); err != nil {
		return err
	}
	invoker, _ := worker.NewModelInvoker(modelAPI{w.API, secrets})
	roles := append([]string{"intake-target"}, allRoles(s)...)
	for _, role := range roles {
		endpoint := s.Models[role]
		if role == "intake-target" {
			endpoint = s.Models["implementer"]
			endpoint.APIKeyEnv = "LASSDAS_INTAKE_TARGET_KEY"
		}
		if _, err := invoker.Preflight(ctx, endpoint); err != nil {
			w.UI.Info(role + ": 不合格")
			return fmt.Errorf("%s の疎通に失敗しました。鍵・API 互換性・残高を確認して再実行してください", role)
		}
		w.UI.Info(role + ": 合格")
	}
	return nil
}
