package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// backlogCall is the minimal tracker client the wizard needs; the engine's
// own client stays untouched because the wizard runs outside the sealed rail.
// It always speaks as the bot user, which doubles as the proof that the bot
// key actually works before it is sealed away for the runtime.
func backlogCall(a *Answers, method, path string, form url.Values, out any) error {
	return backlogCallWithKey(a, a.BotAPIKey, method, path, form, out)
}

// backlogAdminCall speaks as the project administrator when a key for one was
// given, for the few provisioning calls Backlog restricts to administrators
// (board columns, webhooks). Without one it falls back to the bot key - the
// consumer may have chosen to make the bot an administrator instead.
func backlogAdminCall(a *Answers, method, path string, form url.Values, out any) error {
	key := a.TrackerAdminKey
	if key == "" {
		key = a.BotAPIKey
	}
	return backlogCallWithKey(a, key, method, path, form, out)
}

func backlogCallWithKey(a *Answers, apiKey, method, path string, form url.Values, out any) error {
	endpoint := "https://" + a.BacklogDomain + path
	query := url.Values{"apiKey": {apiKey}}
	var body io.Reader
	if method == http.MethodGet && form != nil {
		for key, values := range form {
			for _, value := range values {
				query.Add(key, value)
			}
		}
		form = nil
	}
	endpoint += "?" + query.Encode()
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	request, err := http.NewRequest(method, endpoint, body)
	if err != nil {
		// The parse error would echo the full URL, API key included.
		return errors.New("トラッカーへのリクエストを作れません")
	}
	if form != nil {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	client := &http.Client{Timeout: 20 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		// The transport error would echo the request URL, and the URL
		// carries the API key in its query - name the failure, not the URL.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return errors.New("トラッカーに届きません: " + urlErr.Err.Error())
		}
		return errors.New("トラッカーに届きません")
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode < 200 || response.StatusCode > 299 {
		message := fmt.Sprintf("トラッカー API が %d を返しました: %s", response.StatusCode, strings.TrimSpace(string(payload)))
		// Backlog error codes 4 (access denied) and 5 (unauthorized
		// operation) both mean the key's user lacks a role, and the raw
		// body does not say which role or where to grant it.
		var trouble struct {
			Errors []struct {
				Code int `json:"code"`
			} `json:"errors"`
		}
		_ = json.Unmarshal(payload, &trouble)
		for _, item := range trouble.Errors {
			if item.Code == 4 || item.Code == 5 {
				message += "\n  → この操作には対象プロジェクトの「プロジェクト管理者」権限が要ります。" +
					"再実行してセットアップ質問で管理者の API キー (セットアップ限り・保存しません) を入れるか" +
					" (--non-interactive では環境変数 LASSDAS_SETUP_TRACKER_ADMIN_KEY)、" +
					"Backlog のプロジェクト設定 → 参加ユーザー でこの API キーのユーザーを管理者にしてください。"
				break
			}
		}
		return errors.New(message)
	}
	if out != nil {
		return json.Unmarshal(payload, out)
	}
	return nil
}

// resolveTrackerProject turns the interviewed project key into its numeric
// ID and, as a side effect, proves the bot key works before anything is
// built on top of it.
func resolveTrackerProject(a *Answers) error {
	if a.ResolvedProjectID != 0 && a.ResolvedProjectKey == a.ProjectKey {
		return nil
	}
	a.ResolvedProjectID = 0
	var project struct {
		ID int64 `json:"id"`
	}
	if err := backlogCall(a, http.MethodGet, "/api/v2/projects/"+a.ProjectKey, nil, &project); err != nil {
		return errors.New("プロジェクト " + a.ProjectKey + " が読めません (bot キーの権限を確認): " + err.Error())
	}
	if project.ID <= 0 {
		return errors.New("プロジェクト ID が取れませんでした")
	}
	a.ResolvedProjectID = project.ID
	a.ResolvedProjectKey = a.ProjectKey
	return nil
}

type namedID struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

func findByName(items []namedID, name string) int64 {
	for _, item := range items {
		if item.Name == name {
			return item.ID
		}
	}
	return 0
}

// provisionTracker creates what the tracker side needs - the automation
// category, the two board columns, and the webhook aimed at the hook - and
// then teaches the hook their IDs.
func provisionTracker(state *State) error {
	a := &state.Answers
	o := &state.Outputs
	project := fmt.Sprintf("%d", a.ResolvedProjectID)

	var categories []namedID
	if err := backlogCall(a, http.MethodGet, "/api/v2/projects/"+project+"/categories", nil, &categories); err != nil {
		return err
	}
	o.CategoryID = findByName(categories, a.CategoryName)
	if o.CategoryID == 0 {
		var created namedID
		if err := backlogCall(a, http.MethodPost, "/api/v2/projects/"+project+"/categories",
			url.Values{"name": {a.CategoryName}}, &created); err != nil {
			return errors.New("カテゴリー作成に失敗: " + err.Error())
		}
		o.CategoryID = created.ID
	}
	stepOK(fmt.Sprintf("カテゴリー「%s」(ID %d)", a.CategoryName, o.CategoryID))

	var statuses []namedID
	if err := backlogCall(a, http.MethodGet, "/api/v2/projects/"+project+"/statuses", nil, &statuses); err != nil {
		return err
	}
	ensureStatus := func(name, color string) (int64, error) {
		if id := findByName(statuses, name); id != 0 {
			return id, nil
		}
		var created namedID
		if err := backlogAdminCall(a, http.MethodPost, "/api/v2/projects/"+project+"/statuses",
			url.Values{"name": {name}, "color": {color}}, &created); err != nil {
			return 0, errors.New("ボード列「" + name + "」の作成に失敗: " + err.Error())
		}
		return created.ID, nil
	}
	var err error
	if o.StatusRunning, err = ensureStatus("自動処理中", "#3b9dbd"); err != nil {
		return err
	}
	if o.StatusWaiting, err = ensureStatus("回答待ち", "#eda62a"); err != nil {
		return err
	}
	stepOK(fmt.Sprintf("ボード列「自動処理中」(%d)・「回答待ち」(%d)", o.StatusRunning, o.StatusWaiting))

	// The gate IDs reach the hook before the webhook starts feeding it, so
	// not even a moment of uncategorized enqueue exists.
	if err := updateLambdaEnvironment(a, map[string]string{
		"TICKET_INGRESS_REQUIRED_CATEGORY_ID": fmt.Sprintf("%d", o.CategoryID),
		"BOARD_STATUS_RUNNING":                fmt.Sprintf("%d", o.StatusRunning),
		"BOARD_STATUS_AWAITING_ANSWER":        fmt.Sprintf("%d", o.StatusWaiting),
		"BOARD_STATUS_DELIVERED":              "3",
		"BOARD_STATUS_NEEDS_ATTENTION":        "1",
	}); err != nil {
		return err
	}

	secret, err := readRuntimeSecret(a, o.SecretARN)
	if err != nil {
		return err
	}
	parsed, err := url.Parse(o.FunctionURL)
	if err != nil {
		return errors.New("受け口 URL が壊れています")
	}
	hookURL := "https://" + secret.HookBasicUsername + ":" + secret.HookBasicPassword + "@" + parsed.Host + "/backlog"

	type webhook struct {
		ID      int64  `json:"id"`
		Name    string `json:"name"`
		HookURL string `json:"hookUrl"`
	}
	var webhooks []webhook
	if err := backlogAdminCall(a, http.MethodGet, "/api/v2/projects/"+project+"/webhooks", nil, &webhooks); err != nil {
		return err
	}
	exists := false
	for _, hook := range webhooks {
		if strings.Contains(hook.HookURL, parsed.Host) {
			exists = true
			break
		}
	}
	if !exists {
		if err := backlogAdminCall(a, http.MethodPost, "/api/v2/projects/"+project+"/webhooks", url.Values{
			"name":              {"ticket-automation"},
			"hookUrl":           {hookURL},
			"description":       {"チケット自動処理の受け口 (課題の追加のみ)"},
			"allEvent":          {"false"},
			"activityTypeIds[]": {"1"},
		}, nil); err != nil {
			return errors.New("webhook 登録に失敗: " + err.Error())
		}
	}
	stepOK("webhook を受け口に接続")
	return nil
}
