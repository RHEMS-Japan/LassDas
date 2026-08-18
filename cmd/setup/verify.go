package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/huh/spinner"
)

// verifyPreflight proves the whole wiring with the engine's own smoke path:
// a model-preflight dispatch builds the pinned engine on the instance's
// runner and calls both model roles for real. Green here means checkout,
// secrets, gateway and models all work; ingestion stays off until the
// operator says otherwise.
func verifyPreflight(state *State) error {
	a := &state.Answers

	dispatchedAt := time.Now().UTC().Add(-5 * time.Second)
	if _, err := gh("workflow", "run", "receive-backlog-ticket.yml", "-R", a.InstanceRepo, "-f", "operation=model-preflight"); err != nil {
		return errors.New("model-preflight の起動に失敗しました (workflow が push されているか確認)")
	}

	// The scheduled ingress shares this workflow and leaves all-skipped
	// "success" runs behind; watching "the latest run" measurably reported a
	// green that belonged to one of those. Only a dispatch run created after
	// our own dispatch counts.
	type runInfo struct {
		ID         int64     `json:"databaseId"`
		Status     string    `json:"status"`
		Conclusion string    `json:"conclusion"`
		CreatedAt  time.Time `json:"createdAt"`
	}
	var conclusion string
	work := func() {
		deadline := time.Now().Add(20 * time.Minute)
		var runID int64
		for time.Now().Before(deadline) {
			if runID == 0 {
				out, err := gh("run", "list", "-R", a.InstanceRepo,
					"--workflow", "receive-backlog-ticket.yml",
					"--event", "workflow_dispatch", "--limit", "5",
					"--json", "databaseId,status,conclusion,createdAt")
				if err == nil {
					var runs []runInfo
					if json.Unmarshal([]byte(out), &runs) == nil {
						for _, run := range runs {
							if run.ID > 0 && !run.CreatedAt.Before(dispatchedAt) {
								runID = run.ID
								break
							}
						}
					}
				}
				time.Sleep(5 * time.Second)
				continue
			}
			out, err := gh("run", "view", fmt.Sprintf("%d", runID), "-R", a.InstanceRepo, "--json", "status,conclusion")
			if err == nil {
				var run runInfo
				if json.Unmarshal([]byte(out), &run) == nil && run.Status == "completed" {
					conclusion = run.Conclusion
					return
				}
			}
			time.Sleep(10 * time.Second)
		}
	}
	if nonInteractive {
		fmt.Println(styleFaint.Render("  · model-preflight を実行中 (数分かかります)"))
		work()
	} else if err := spinner.New().Title("model-preflight を実行中 (エンジンのビルドとモデル呼び出しの実測・数分かかります)").Action(work).Run(); err != nil {
		return err
	}
	if conclusion != "success" {
		return fmt.Errorf("model-preflight が %q で終わりました — 実行ログ: https://github.com/%s/actions", conclusion, a.InstanceRepo)
	}
	stepOK("model-preflight 全ジョブ成功 (エンジン取得・secrets・モデル呼び出しの実証)")

	if a.AppID == "" {
		fmt.Println(styleWarn.Render("  ! 納品用 GitHub App が未設定です。受付を有効化する前に:"))
		fmt.Println(styleFaint.Render(strings.Join([]string{
			"    1. https://github.com/settings/apps/new で App を作成",
			"       権限: Contents=Read&Write / Pull requests=Read&Write / Checks=Read",
			"    2. App を納品先 repo (" + a.ConsumerRepo + ") にインストール",
			"    3. gh secret set TARGET_AUTOMATION_APP_CLIENT_ID -R " + a.InstanceRepo,
			"       gh secret set TARGET_AUTOMATION_APP_PRIVATE_KEY -R " + a.InstanceRepo + " < 秘密鍵.pem",
		}, "\n")))
	}

	enable := a.AppID != ""
	if nonInteractive {
		enable = false
	} else {
		prompt := "チケットの受付を今すぐ有効化しますか? (vars TICKET_INGRESS_ENABLED)"
		if a.AppID == "" {
			prompt += " — App 未設定のため納品段で止まります"
		}
		if err := huh.NewConfirm().Title(prompt).Value(&enable).Run(); err != nil {
			return err
		}
	}
	if enable {
		if out, err := gh("variable", "set", "TICKET_INGRESS_ENABLED", "-R", a.InstanceRepo, "--body", "true"); err != nil {
			return errors.New("有効化に失敗: " + out)
		}
		stepOK("受付を有効化しました (5 分毎の定期便が動き始めます)")
	} else {
		step("受付は無効のまま (有効化: gh variable set TICKET_INGRESS_ENABLED -R " + a.InstanceRepo + " --body true)")
	}
	return nil
}
