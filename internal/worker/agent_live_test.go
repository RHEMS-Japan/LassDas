//go:build liveagents

package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests run the real configured agents against the real configured
// endpoint. They are behind a build tag because they cost money and need
// credentials; everything else about the agent path is exercised with stand-in
// programs. Run with:
//
//	go test -tags=liveagents ./internal/worker/ -run TestLive -v
//
// What they prove is the one thing a stand-in cannot: that the shipped
// configuration actually drives the shipped binaries through the shipped
// endpoint, and that what comes back seals.

func liveConfig(t *testing.T) Config {
	t.Helper()
	for _, name := range []string{"MODEL_API_KEY_IMPLEMENTER", "MODEL_API_KEY_REVIEWER", "BACKLOG_API_KEY"} {
		if os.Getenv(name) == "" {
			t.Fatalf("%s is not set; the live agents cannot reach the endpoint without it", name)
		}
	}
	config, err := LoadConfig(filepath.Join("..", "..", "config", "m1-consumer.json"))
	if err != nil {
		t.Fatal(err)
	}
	return config
}

// liveWorkspace builds a small but realistic destination: a file whose visible
// wording is the thing to change, plus a caller that a careful reviewer would
// notice. Neither file is named to the agent.
func liveWorkspace(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	writeAgentFile(t, root, "client/src/components/Settings.tsx", strings.Join([]string{
		"import { confirmLabel } from '../labels';",
		"",
		"export function Settings() {",
		"  return <button aria-label={confirmLabel}>{confirmLabel}</button>;",
		"}",
		"",
	}, "\n"))
	writeAgentFile(t, root, "client/src/labels.ts", strings.Join([]string{
		"export const confirmLabel = '設定を保存';",
		"export const cancelLabel = 'キャンセル';",
		"",
	}, "\n"))
	writeAgentFile(t, root, "README.md", "fixture\n")
	// The implementing agent's configuration names its ticket-tracker tool in
	// a file next to the workspace; without it the agent cannot start.
	mcp := `{"mcpServers":{"backlog":{"command":"npx","args":["-y","backlog-mcp-server@0.15.1"],` +
		`"env":{"BACKLOG_DOMAIN":"${BACKLOG_DOMAIN}","BACKLOG_API_KEY":"${BACKLOG_API_KEY}"}}}}`
	if err := os.WriteFile(filepath.Join(filepath.Dir(root), "agent-mcp.json"), []byte(mcp), 0o600); err != nil {
		t.Fatal(err)
	}
	agentGit(t, root, "init", "--initial-branch=stg")
	agentGit(t, root, "add", "-A")
	agentGit(t, root, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "base")
	return root, copyAgentBase(t, root)
}

// TestLiveAgentWorksUnderTheConfiguredRules proves the rules actually reach
// the agent. It asks the agent to report what it is working under; if the
// rules were not placed, it has nothing to report.
func TestLiveAgentWorksUnderTheConfiguredRules(t *testing.T) {
	config := liveConfig(t)
	if config.Agents.Implementer.Knowledge.Empty() {
		t.Skip("this agent is configured to be given no knowledge")
	}
	root, _ := liveWorkspace(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	placed, err := PlaceKnowledge(config.Agents.Implementer.Knowledge, filepath.Join("..", ".."), home, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("置いたもの: %v", placed)

	outcome, err := RunAgent(context.Background(), config.Agents.Implementer, root,
		"あなたが今どんな規範の下で作業することになっているかを、"+
			"実際に読み込まれている内容から具体的に 3 つ、短く挙げてください。"+
			"何も読み込まれていないならその旨だけ答えてください。ファイルは変更しないでください。",
		nil, nil)
	t.Logf("エージェントの答え: %s", tail(outcome.Transcript, 900))
	if err != nil {
		t.Fatalf("the agent did not finish: %v", err)
	}
	if strings.Contains(outcome.Transcript, "読み込まれていません") ||
		strings.Contains(outcome.Transcript, "読み込まれていない") {
		t.Fatal("the agent reports it was given no rules")
	}
}

// TestLiveAgentConsultsTheKnowledgeLibrary proves the agent can reach what was
// placed for it and actually uses it. A library nobody reads is the same as no
// library at all.
func TestLiveAgentConsultsTheKnowledgeLibrary(t *testing.T) {
	config := liveConfig(t)
	library := config.Agents.Implementer.Knowledge.Library
	if library == nil {
		t.Skip("this agent is configured with no library")
	}
	root, _ := liveWorkspace(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	placed, err := PlaceKnowledge(config.Agents.Implementer.Knowledge, filepath.Join("..", ".."), home, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("置いた数: %d 件", len(placed))

	outcome, err := RunAgent(context.Background(), config.Agents.Implementer, root,
		"`"+library.To+"/` に過去の判断が置いてあります。"+
			"その中から、本番のデータベースについて何が決まっているかを調べて、"+
			"根拠にしたファイル名と一緒に 2 行で答えてください。"+
			"見つからなければ「見つからない」と答えてください。ファイルは変更しないでください。",
		nil, nil)
	t.Logf("エージェントの答え: %s", tail(outcome.Transcript, 700))
	if err != nil {
		t.Fatalf("the agent did not finish: %v", err)
	}
	if strings.Contains(outcome.Transcript, "見つからない") {
		t.Fatal("the agent could not reach the library placed for it")
	}
	// Placed knowledge must still be invisible as a change, or the run that
	// used it would be thrown away.
	changed, err := ChangedFilesUnder(root, []string{"client/src/"}, nil)
	if err != nil || len(changed) != 0 {
		t.Fatalf("the library was seen as a change: %v (%v)", changed, err)
	}
}

// TestLiveImplementerReadsTheTicketTracker proves the agent can reach the
// ticket tracker through its configured tool: authentication, the tool
// wiring, and the permission to call it, all at once.
func TestLiveImplementerReadsTheTicketTracker(t *testing.T) {
	config := liveConfig(t)
	root, _ := liveWorkspace(t)

	outcome, err := RunAgent(context.Background(), config.Agents.Implementer, root,
		"あなたにはチケットトラッカーを読む道具 (mcp__backlog__...) が渡されています。"+
			"get_myself を呼び、成功したら「TRACKER_OK: <取得したユーザー名>」、"+
			"呼べなかったら「TRACKER_NG: <理由を一行>」とだけ答えてください。ファイルは変更しないでください。",
		nil, nil)
	t.Logf("エージェントの答え: %s", tail(outcome.Transcript, 500))
	if err != nil {
		t.Fatalf("the agent did not finish: %v", err)
	}
	if !strings.Contains(outcome.Transcript, "TRACKER_OK") {
		t.Fatal("the agent could not reach the ticket tracker")
	}
}

// TestLiveReviewerWorksUnderTheConfiguredRules measures the one placement
// nothing had verified: whether the reviewing agent loads the rules placed in
// its home. The reviewer needs its real home for its endpoint configuration,
// so the rules are placed there and removed afterwards; an existing file is
// never overwritten.
func TestLiveReviewerWorksUnderTheConfiguredRules(t *testing.T) {
	config := liveConfig(t)
	if len(config.Agents.Reviewer.Knowledge.Rules) == 0 {
		t.Skip("the reviewer is configured with no rules")
	}
	home := os.Getenv("HOME")
	if home == "" {
		t.Skip("no home to place rules in")
	}
	target := filepath.Join(home, filepath.FromSlash(config.Agents.Reviewer.Knowledge.Rules[0].To))
	if _, err := os.Lstat(target); err == nil {
		t.Skipf("%s already exists; not overwriting a real file", target)
	}
	root, _ := liveWorkspace(t)
	rules := KnowledgeConfig{Rules: config.Agents.Reviewer.Knowledge.Rules}
	if _, err := PlaceKnowledge(rules, filepath.Join("..", ".."), home, root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(target) })

	outcome, err := RunAgent(context.Background(), config.Agents.Reviewer, root,
		"あなたに作業規範が渡されているか確かめてください。渡されているなら、その中の具体的な規則を 1 つ引用して"+
			"「RULES_OK: <引用>」、何も渡されていないなら「RULES_NG」とだけ答えてください。ファイルは変更しないでください。",
		nil, nil)
	t.Logf("エージェントの答え: %s", tail(outcome.Transcript, 500))
	if err != nil {
		t.Fatalf("the agent did not finish: %v", err)
	}
	if !strings.Contains(outcome.Transcript, "RULES_OK") {
		t.Fatal("the reviewing agent does not load the rules placed for it")
	}
}

func liveConsumer(t *testing.T, config Config) ConsumerConfig {
	t.Helper()
	consumer, err := config.ConsumerFor("example/consumer")
	if err != nil {
		t.Fatal(err)
	}
	return consumer
}

// TestLiveImplementingAgentFindsAndChangesTheFile is the measurement that
// matters: nobody tells the agent which file holds the wording, and the change
// has to come back as sealed artifacts.
func TestLiveImplementingAgentFindsAndChangesTheFile(t *testing.T) {
	config := liveConfig(t)
	consumer := liveConsumer(t, config)
	root, base := liveWorkspace(t)

	prompt := strings.Join([]string{
		"あなたはこのリポジトリで、依頼された変更を実装します。",
		"",
		"## 依頼",
		"設定画面の保存ボタンの文言を「設定を保存」から「変更を保存」に変えてください。",
		"",
		"## 守ること",
		"- 変更してよいのは " + strings.Join(consumer.Mode.AllowedFilePrefixes, " / ") + " の下だけです。",
		"- 新しいファイルは作らないでください。",
		"- 依頼に書かれていない改善はしないでください。",
		"- 変更が終わったら、何をどう変えたかを数行で述べて終了してください。コミットはしないでください。",
	}, "\n")

	started := time.Now()
	outcome, err := RunAgent(context.Background(), config.Agents.Implementer, root, prompt, consumer.Mode.AllowedFilePrefixes, nil)
	t.Logf("実装 %s: exit=%d 所要=%s 変更=%v", config.Agents.Implementer.Command, outcome.ExitCode, time.Since(started).Round(time.Second), outcome.ChangedFiles)
	t.Logf("実装が述べたこと: %s", tail(outcome.Transcript, 600))
	if err != nil {
		t.Fatalf("the implementing agent did not finish: %v", err)
	}

	observed, err := ReadObservedChanges(root, base, outcome.ChangedFiles, consumer)
	if err != nil {
		t.Fatalf("what the agent changed could not be read: %v", err)
	}
	for _, change := range observed {
		if string(change.Before) == string(change.After) {
			t.Fatalf("%s was reported as changed but its bytes are identical", change.Path)
		}
	}
	changed := strings.Join(outcome.ChangedFiles, " ")
	if !strings.Contains(changed, "labels.ts") {
		t.Fatalf("the agent did not find where the wording lives: %v", outcome.ChangedFiles)
	}
	after := ""
	for _, change := range observed {
		after += string(change.After)
	}
	if !strings.Contains(after, "変更を保存") || strings.Contains(after, "設定を保存") {
		t.Fatalf("the wording was not replaced:\n%s", after)
	}
}

// TestLiveReviewingAgentReportsAVerdictThatSeals proves the reviewer reaches
// its endpoint, reads the repository, leaves it alone, and prints a verdict
// this framework can seal.
func TestLiveReviewingAgentReportsAVerdictThatSeals(t *testing.T) {
	config := liveConfig(t)
	consumer := liveConsumer(t, config)
	root, _ := liveWorkspace(t)
	writeAgentFile(t, root, "client/src/labels.ts", strings.Join([]string{
		"export const confirmLabel = '変更を保存';",
		"export const cancelLabel = 'キャンセル';",
		"",
	}, "\n"))

	prompt := strings.Join([]string{
		"あなたはこの変更を通すかどうかを判定するレビュアーです。作業ディレクトリには変更が適用済みです。",
		"",
		"## 依頼",
		"設定画面の保存ボタンの文言を「設定を保存」から「変更を保存」に変えること。",
		"",
		"### 変更されたファイル",
		"client/src/labels.ts",
		"",
		"## やること",
		"- 変更されたファイルを読み、依頼を満たしているか判定してください。",
		"- ファイルは一切変更しないでください。",
		"",
		"## 答え方 (最後にこの形の JSON だけを出力する)",
		`{"verdict":"pass","findings":[]}`,
		"または",
		`{"verdict":"revise","findings":[{"code":"短い識別子","path":"client/src/labels.ts","line":0,"message":"何がどう問題かを一文で"}]}`,
	}, "\n")

	started := time.Now()
	outcome, err := RunAgent(context.Background(), config.Agents.Reviewer, root, prompt, nil)
	t.Logf("レビュー %s: exit=%d 所要=%s", config.Agents.Reviewer.Command, outcome.ExitCode, time.Since(started).Round(time.Second))
	t.Logf("レビューが述べたこと: %s", tail(outcome.Transcript, 600))
	if err != nil {
		t.Fatalf("the reviewing agent did not finish: %v", err)
	}

	output, err := DecodeAgentReviewOutput(outcome.Transcript)
	if err != nil {
		t.Fatalf("the reviewer printed no verdict this framework can read: %v", err)
	}
	t.Logf("判定: %s (指摘 %d 件)", output.Verdict, len(output.Findings))
	if output.Verdict != "pass" && output.Verdict != "revise" {
		t.Fatalf("verdict = %q", output.Verdict)
	}
	candidate := Candidate{Files: []CandidateFile{{
		Path: "client/src/labels.ts",
		Content: "export const confirmLabel = '変更を保存';\n" +
			"export const cancelLabel = 'キャンセル';\n",
	}}}
	if err := ConfirmTreeMatchesCandidate(root, candidate, consumer); err != nil {
		t.Fatalf("the reviewer changed the tree it was asked to read: %v", err)
	}
}

func tail(text string, limit int) string {
	text = strings.TrimSpace(text)
	if len(text) <= limit {
		return text
	}
	return "…" + text[len(text)-limit:]
}
