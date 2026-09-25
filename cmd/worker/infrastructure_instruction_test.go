package main

import (
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/worker"
)

func infrastructureDraft() (worker.TicketDraft, worker.ConsumerConfig, worker.AgentConfig) {
	draft := worker.TicketDraft{IssueKey: "TEST-1", Summary: "件名", Request: "本文", Repository: "example/target"}
	consumer := worker.ConsumerConfig{Repository: "example/target", Mode: worker.ModeConfig{
		AllowedFilePrefixes: []string{"README.md", "docs/"},
		MaxFiles:            5, MaxChangedLines: 400, MaxChangedBytes: 32768, MaxFileBytes: 65536,
	}}
	return draft, consumer, worker.AgentConfig{ID: "implementer", Command: "agent"}
}

// A destination's standing permission is a setting nobody acts on unless
// the instruction carries it: the agent has the credential in its
// environment, no statement that it may use it, no statement of which kinds
// are allowed, and no way to say what it made.
func TestTheInstructionCarriesTheInfrastructureTheAgentMayUse(t *testing.T) {
	draft, consumer, agent := infrastructureDraft()
	consumer.Infrastructure = &worker.InfrastructureConfig{
		Provider: "aws", Region: "ap-northeast-1", Credential: "cloud",
		Resources: []string{"sqs", "s3"}, NamingPrefix: "lassdas-",
	}
	prompt, err := implementPrompt(draft, consumer, agent, nil, nil, nil, nil, nil, "/work/repo", []string{"AWS_SHARED_CREDENTIALS_FILE"})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"## 使ってよい基盤",
		"提供元: aws",
		"地域: ap-northeast-1",
		"AWS_SHARED_CREDENTIALS_FILE",
		"作ってよい種類: sqs / s3",
		"lassdas-",
		worker.AgentResourcesFile,
		`{"kind": "種類"`,
	} {
		if !strings.Contains(prompt, expected) {
			t.Fatalf("the instruction lacks %q:\n%s", expected, prompt)
		}
	}
	// The instruction must not ask for the change and forbid it in the same
	// breath: the line about not touching credentials is the one that would.
	if strings.Contains(prompt, "自動化・リリース手順・資格情報・権限設定には触れないでください") {
		t.Fatalf("the instruction forbids what it just permitted:\n%s", prompt)
	}
}

// A destination that declared none keeps the instruction it always had.
func TestAnInstructionWithoutInfrastructureIsUnchanged(t *testing.T) {
	draft, consumer, agent := infrastructureDraft()
	prompt, err := implementPrompt(draft, consumer, agent, nil, nil, nil, nil, nil, "/work/repo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prompt, "使ってよい基盤") || strings.Contains(prompt, worker.AgentResourcesFile) {
		t.Fatalf("a destination that declared nothing was told about infrastructure:\n%s", prompt)
	}
	if !strings.Contains(prompt, "自動化・リリース手順・資格情報・権限設定には触れないでください") {
		t.Fatalf("the boundary the instruction always had is gone:\n%s", prompt)
	}
}

// An account the engine may use but create nothing in says so, and a card
// that was handed no credential says that instead of naming one.
func TestTheInstructionIsHonestAboutWhatIsMissing(t *testing.T) {
	draft, consumer, agent := infrastructureDraft()
	consumer.Infrastructure = &worker.InfrastructureConfig{Provider: "aws"}
	prompt, err := implementPrompt(draft, consumer, agent, nil, nil, nil, nil, nil, "/work/repo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "新しく作ってよい種類はありません") {
		t.Fatalf("the instruction does not say nothing may be created:\n%s", prompt)
	}
	if !strings.Contains(prompt, "資格情報はこの実行には渡されていません") {
		t.Fatalf("the instruction does not say the credential is missing:\n%s", prompt)
	}
}
