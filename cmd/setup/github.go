package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/worker"
)

// Outputs are the machine facts each stage leaves for the later ones; they
// are part of the resumable state, and none of them is a secret.
type Outputs struct {
	EngineRepo      string `json:"engine_repo"`
	InstanceRepoID  int64  `json:"instance_repo_id"`
	AutomationRunID string `json:"automation_run_id"`
	FunctionURL     string `json:"function_url"`
	TableName       string `json:"table_name"`
	SecretARN       string `json:"secret_arn"`
	CategoryID      int64  `json:"category_id"`
	StatusRunning   int64  `json:"status_running"`
	StatusWaiting   int64  `json:"status_waiting"`
}

func gh(args ...string) (string, error) {
	out, err := exec.Command("gh", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func step(text string)   { fmt.Println(styleFaint.Render("  · " + text)) }
func stepOK(text string) { fmt.Println(styleOK.Render("  ✓ ") + text) }

// engineRepository reads this clone's origin as owner/name - the value the
// instance workflow pins with uses:/checkout.
func engineRepository() (string, error) {
	out, err := exec.Command("git", "remote", "get-url", "origin").Output()
	if err != nil {
		return "", errors.New("エンジン clone の origin が読めません")
	}
	url := strings.TrimSpace(string(out))
	url = strings.TrimSuffix(url, ".git")
	for _, prefix := range []string{"git@github.com:", "https://github.com/", "ssh://git@github.com/"} {
		if strings.HasPrefix(url, prefix) {
			return strings.TrimPrefix(url, prefix), nil
		}
	}
	return "", errors.New("origin が GitHub を指していません: " + url)
}

// provisionGitHub creates the private instance repository, materializes its
// tree (receive workflow pinned to the engine, config patched from the
// bundled example, knowledge skeleton), and stores the secrets and the
// static variables. URL variables arrive later, from the cloud stage.
func provisionGitHub(state *State) error {
	a := &state.Answers
	o := &state.Outputs

	if err := resolveTrackerProject(a); err != nil {
		return err
	}
	saveState(state)

	engineRepo, err := engineRepository()
	if err != nil {
		return err
	}
	o.EngineRepo = engineRepo

	if _, err := gh("repo", "view", a.InstanceRepo, "--json", "id"); err != nil {
		step("private repo を作成: " + a.InstanceRepo)
		if out, err := gh("repo", "create", a.InstanceRepo, "--private"); err != nil {
			return errors.New("repo 作成に失敗: " + out)
		}
	} else {
		step("repo は既存: " + a.InstanceRepo)
		branch, err := gh("api", "repos/"+a.InstanceRepo, "--jq", ".default_branch")
		if err != nil {
			return errors.New("既存 repo の default branch が読めません: " + a.InstanceRepo)
		}
		if branch != "main" {
			return errors.New("既存 repo の default branch が main ではありません (" + branch + ") — 定期起動と身分検証は main 前提です。main を default にしてから再実行してください")
		}
	}
	idOut, err := gh("api", "repos/"+a.InstanceRepo, "--jq", ".id")
	if err != nil {
		return errors.New("repo ID が取れません: " + idOut)
	}
	fmt.Sscanf(idOut, "%d", &o.InstanceRepoID)

	if o.AutomationRunID == "" {
		o.AutomationRunID = "run_" + time.Now().UTC().Format("20060102") + "_" + randomHex(12)
	}

	if err := pushInstanceTree(a, engineRepo); err != nil {
		return err
	}
	stepOK("受信 workflow・config・knowledge を push")

	secrets := map[string]string{
		"BACKLOG_API_KEY":           a.BotAPIKey,
		"MODEL_API_KEY_IMPLEMENTER": a.ImplementerKey,
		"MODEL_API_KEY_REVIEWER":    a.ReviewerKey,
	}
	if a.AppID != "" && a.AppKeyPath != "" {
		pem, err := os.ReadFile(a.AppKeyPath)
		if err != nil {
			return errors.New("App 秘密鍵が読めません: " + a.AppKeyPath)
		}
		secrets["TARGET_AUTOMATION_APP_CLIENT_ID"] = a.AppID
		secrets["TARGET_AUTOMATION_APP_PRIVATE_KEY"] = string(pem)
	}
	for name, value := range secrets {
		command := exec.Command("gh", "secret", "set", name, "-R", a.InstanceRepo)
		command.Stdin = strings.NewReader(value)
		if out, err := command.CombinedOutput(); err != nil {
			return errors.New("secret " + name + " の投入に失敗: " + strings.TrimSpace(string(out)))
		}
	}
	stepOK(fmt.Sprintf("secrets %d 本を投入", len(secrets)))

	variables := map[string]string{
		"TICKET_INGRESS_ENABLED":    "false",
		"BACKLOG_AUTOMATION_RUN_ID": o.AutomationRunID,
		"BACKLOG_SPACE_KEY":         spaceKey(a.BacklogDomain),
		"BACKLOG_PROJECT_KEY":       a.ProjectKey,
		"BACKLOG_PROJECT_ID":        fmt.Sprintf("%d", a.ResolvedProjectID),
		"BACKLOG_CREATOR_ID":        a.AllowedCreatorID,
		"BACKLOG_ACTIVITY_TYPE":     "1",
	}
	for name, value := range variables {
		if out, err := gh("variable", "set", name, "-R", a.InstanceRepo, "--body", value); err != nil {
			return errors.New("variable " + name + " の投入に失敗: " + out)
		}
	}
	stepOK(fmt.Sprintf("vars %d 本を投入 (受付は false で待機)", len(variables)))
	if a.AppID == "" {
		fmt.Println(styleWarn.Render("  ! 納品用 GitHub App は未設定 — 最後に作成ガイドを表示します (受付前に必須)"))
	}
	return nil
}

func spaceKey(domain string) string {
	return strings.SplitN(domain, ".", 2)[0]
}

func randomHex(bytes int) string {
	buffer := make([]byte, bytes)
	_, _ = rand.Read(buffer)
	return hex.EncodeToString(buffer)[:bytes*2]
}

func randomBase64(bytes int) (string, error) {
	buffer := make([]byte, bytes)
	if _, err := rand.Read(buffer); err != nil {
		return "", errors.New("乱数の生成に失敗しました")
	}
	return base64.StdEncoding.EncodeToString(buffer), nil
}

// materializeInstanceTree writes the wizard-owned files into root - the
// receive workflow with both placeholders resolved, the bundled example
// config patched with the interview answers, and a knowledge skeleton. Root
// already carries whatever the instance repository accumulated; only these
// files are overwritten, everything else is preserved.
func materializeInstanceTree(root string, a *Answers, engineRepo string) error {
	template, err := os.ReadFile("templates/receive-backlog-ticket.yml")
	if err != nil {
		return errors.New("templates/receive-backlog-ticket.yml が読めません (エンジン clone の根で実行していますか)")
	}
	workflow := strings.ReplaceAll(string(template), "ENGINE_REPOSITORY_PLACEHOLDER", engineRepo)
	workflow = strings.ReplaceAll(workflow, "ENGINE_PIN_PLACEHOLDER", a.EnginePin)
	if err := writeTree(root, ".github/workflows/receive-backlog-ticket.yml", workflow); err != nil {
		return err
	}

	config, err := patchedConsumerConfig(a)
	if err != nil {
		return err
	}
	if err := writeTree(root, "config/m1-consumer.json", config); err != nil {
		return err
	}
	// The engine's own loader judges the generated file right here, so a
	// rejected combination surfaces as an interview problem now instead of a
	// workflow failure twenty minutes later.
	if _, err := worker.LoadConfig(filepath.Join(root, "config", "m1-consumer.json")); err != nil {
		return errors.New("生成した config がエンジンの検証で弾かれました: " + err.Error())
	}

	rules, err := os.ReadFile("knowledge/rules.md")
	if err != nil {
		return errors.New("knowledge/rules.md が読めません")
	}
	if err := writeTree(root, "knowledge/rules.md", string(rules)); err != nil {
		return err
	}
	if err := writeTree(root, "knowledge/library/README.md",
		"# knowledge/library\n\n自動処理に読ませる、この製品の決定事項・方針・過去回答の置き場。1 ファイル 1 論点で増やす。\n"); err != nil {
		return err
	}
	return writeTree(root, "README.md",
		"# "+a.InstanceRepo+"\n\nチケット自動処理のインスタンス。エンジン ("+engineRepo+") を版数固定で呼び、この repo が設定・knowledge・実行履歴を持つ。\n\n- 受付の ON/OFF: repository variable `TICKET_INGRESS_ENABLED`\n- チケットの書き方: エンジン側 docs/TICKET_AUTHORING.md\n")
}

func writeTree(root, path, content string) error {
	full := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, []byte(content), 0o644)
}

// patchedConsumerConfig loads the bundled neutral example and overwrites
// exactly the interviewed fields, so the generated file always matches the
// schema the engine validates.
func patchedConsumerConfig(a *Answers) (string, error) {
	raw, err := os.ReadFile("config/m1-consumer.json")
	if err != nil {
		return "", errors.New("config/m1-consumer.json (見本) が読めません")
	}
	var config map[string]any
	if err := json.Unmarshal(raw, &config); err != nil {
		return "", errors.New("見本 config が壊れています")
	}

	consumers, _ := config["consumers"].([]any)
	if len(consumers) == 0 {
		return "", errors.New("見本 config に consumers がありません")
	}
	consumer, _ := consumers[0].(map[string]any)
	consumer["repository"] = a.ConsumerRepo
	consumer["delivery_branch"] = a.DeliveryBranch
	consumer["integration_branch"] = a.DeliveryBranch
	consumer["release_branch"] = a.ReleaseBranch
	consumer["staging_origin"] = a.StagingOrigin
	consumer["production_origin"] = a.ProductionOrigin
	consumer["staging_workflow"] = a.StagingWorkflow
	consumer["production_workflow"] = a.ProductionWorkflow
	if id, err := consumerRepoID(a.ConsumerRepo); err == nil {
		consumer["repository_id"] = id
	} else {
		return "", err
	}
	mode, ok := consumer["mode"].(map[string]any)
	if !ok {
		return "", errors.New("見本 config に mode がありません")
	}
	prefixes := make([]any, 0, 4)
	for _, p := range a.prefixList() {
		prefixes = append(prefixes, p)
	}
	mode["allowed_file_prefixes"] = prefixes
	config["consumers"] = consumers[:1]

	if err := patchGitHubContract(consumer, a); err != nil {
		return "", err
	}
	patchModels(config, a)

	encoded, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return "", err
	}
	return string(encoded) + "\n", nil
}

// patchGitHubContract aligns the contract block with the real consumer
// repository: the named workflows resolve to their live IDs, the merge
// settings copy what the repository actually has, and the default branch
// follows the interview. Everything the delivery path later compares with
// strict equality comes from the live repo, not from the example.
func patchGitHubContract(consumer map[string]any, a *Answers) error {
	contract, ok := consumer["github_contract"].(map[string]any)
	if !ok {
		return errors.New("見本 config に github_contract がありません")
	}
	repoJSON, err := gh("api", "repos/"+a.ConsumerRepo)
	if err != nil {
		return errors.New("納品先 repo の設定が読めません: " + a.ConsumerRepo)
	}
	var repo map[string]any
	if err := json.Unmarshal([]byte(repoJSON), &repo); err != nil {
		return errors.New("納品先 repo の応答が読めません")
	}
	// The delivery path compares these against the live repository with
	// strict equality, so live values are the only correct source: the
	// default branch is whatever the repo says it is, and every merge
	// setting must be present in the response - the fields vanish for
	// callers without push access, and silently keeping example values
	// measurably survived config validation only to die at delivery.
	liveDefault, _ := repo["default_branch"].(string)
	if liveDefault == "" {
		return errors.New("納品先 repo の default branch が読めません")
	}
	contract["default_branch"] = liveDefault
	if settings, ok := contract["merge_settings"].(map[string]any); ok {
		for key := range settings {
			value, exists := repo[key]
			if !exists || value == nil {
				return errors.New("納品先 repo の設定 " + key + " が API 応答にありません — 納品先への push 権限がある gh アカウントで実行してください")
			}
			settings[key] = value
		}
	}

	workflowsJSON, err := gh("api", "repos/"+a.ConsumerRepo+"/actions/workflows", "--paginate", "--jq", "[.workflows[] | {id, name, path}]")
	if err != nil {
		return errors.New("納品先 repo の workflow 一覧が読めません")
	}
	var workflows []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
		Path string `json:"path"`
	}
	// --paginate emits one JSON array per page; a single page is the common
	// case and later pages only matter for very workflow-heavy repos.
	decoder := json.NewDecoder(strings.NewReader(workflowsJSON))
	for decoder.More() {
		var page []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
			Path string `json:"path"`
		}
		if err := decoder.Decode(&page); err != nil {
			break
		}
		workflows = append(workflows, page...)
	}
	resolve := func(filename string) (map[string]any, bool) {
		path := ".github/workflows/" + filename
		for _, w := range workflows {
			if w.Path == path {
				return map[string]any{"id": w.ID, "name": w.Name, "path": w.Path}, true
			}
		}
		return nil, false
	}

	staging, found := resolve(a.StagingWorkflow)
	if !found {
		return errors.New("納品先に " + a.StagingWorkflow + " が見つかりません — stg のデプロイ workflow 名を確認してください")
	}
	contract["staging_workflow"] = staging
	production, found := resolve(a.ProductionWorkflow)
	if !found {
		return errors.New("納品先に " + a.ProductionWorkflow + " が見つかりません — 本番のデプロイ workflow 名を確認してください")
	}
	contract["production_workflows"] = []any{production}
	// The example's feature workflows are the example's; a fresh instance
	// starts without extra required gates and the operator adds real ones to
	// the config later.
	contract["feature_workflows"] = []any{}
	return nil
}

func consumerRepoID(repo string) (int64, error) {
	out, err := gh("api", "repos/"+repo, "--jq", ".id")
	if err != nil {
		return 0, errors.New("納品先 repo が読めません (" + repo + "): gh の権限を確認してください")
	}
	var id int64
	fmt.Sscanf(out, "%d", &id)
	return id, nil
}

// patchModels rewrites every model endpoint and agent env in the example to
// the interviewed gateway and model names, keeping roles and vendors as the
// example arranged them (implementer-family on Anthropic, adversary on the
// other vendor).
func patchModels(config map[string]any, a *Answers) {
	replaceEndpoint := func(endpoint map[string]any) {
		endpoint["base_url"] = a.GatewayBaseURL
		vendor, _ := endpoint["vendor"].(string)
		if vendor == "Anthropic" {
			endpoint["model"] = a.ImplementerModel
		} else {
			endpoint["model"] = a.ReviewerModel
		}
	}
	models, _ := config["models"].(map[string]any)
	for _, value := range models {
		switch typed := value.(type) {
		case map[string]any:
			if _, ok := typed["base_url"]; ok {
				replaceEndpoint(typed)
				continue
			}
			for _, nested := range typed {
				if endpoint, ok := nested.(map[string]any); ok {
					if _, ok := endpoint["base_url"]; ok {
						replaceEndpoint(endpoint)
					}
				}
			}
		case []any:
			for _, item := range typed {
				if endpoint, ok := item.(map[string]any); ok {
					replaceEndpoint(endpoint)
				}
			}
		}
	}
	agents, _ := config["agents"].(map[string]any)
	for _, value := range agents {
		agent, ok := value.(map[string]any)
		if !ok {
			continue
		}
		env, _ := agent["env"].(map[string]any)
		if env == nil {
			continue
		}
		if _, ok := env["ANTHROPIC_BASE_URL"]; ok {
			env["ANTHROPIC_BASE_URL"] = strings.TrimSuffix(a.GatewayBaseURL, "/v1")
		}
		if _, ok := env["ANTHROPIC_MODEL"]; ok {
			env["ANTHROPIC_MODEL"] = a.ImplementerModel
		}
		if _, ok := env["BACKLOG_DOMAIN"]; ok {
			env["BACKLOG_DOMAIN"] = a.BacklogDomain
		}
	}
}

// pushInstanceTree lays the wizard's files over whatever the instance
// repository already holds and pushes one commit. The remote content is
// checked out first, so a re-run refreshes the five wizard-owned files and
// preserves everything the instance accumulated since - the preserve job
// commits adopted answers into this tree, and a setup re-run must never
// erase them (an earlier draft measurably did).
func pushInstanceTree(a *Answers, engineRepo string) error {
	tree, err := os.MkdirTemp("", "lassdas-instance-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tree)
	git := func(args ...string) error {
		command := exec.Command("git", args...)
		command.Dir = tree
		command.Stdout = nil
		command.Stderr = os.Stderr
		return command.Run()
	}
	remote := "https://github.com/" + a.InstanceRepo + ".git"
	if err := git("init", "--quiet", "--initial-branch=main"); err != nil {
		return err
	}
	if err := git("remote", "add", "origin", remote); err != nil {
		return err
	}
	// An existing repository (resume, or operator-created) contributes both
	// its history and its working content; a brand-new one fails the fetch
	// and starts at the root.
	if err := git("fetch", "--quiet", "--depth", "1", "origin", "main"); err == nil {
		if err := git("reset", "--hard", "--quiet", "FETCH_HEAD"); err != nil {
			return err
		}
	}
	if err := materializeInstanceTree(tree, a, engineRepo); err != nil {
		return err
	}
	if err := git("add", "-A"); err != nil {
		return err
	}
	if err := git("-c", "user.name=lassdas-setup", "-c", "user.email=setup@invalid",
		"commit", "--quiet", "--allow-empty", "-m", "setup: instance scaffolding"); err != nil {
		return err
	}
	if err := git("push", "--quiet", "origin", "HEAD:main"); err != nil {
		return errors.New("push に失敗しました (https の git 資格情報を確認: gh auth setup-git で gh に委譲できます)")
	}
	return nil
}
