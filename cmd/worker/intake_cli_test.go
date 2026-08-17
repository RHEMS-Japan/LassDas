package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/worker"
)

// proseTicketDescription is a ticket written the way people actually write
// them: headings, a table, proposed acceptance criteria, and not one line of
// the format the automation used to demand.
func proseTicketDescription() string {
	return strings.Join([]string{
		"## 背景",
		"",
		"発注者指摘: 編集ダイアログの用語が伝わらない。",
		"",
		"## 完了条件 (案)",
		"",
		"- [ ] 「RPM制限」を「1分あたりのリクエスト数上限」に置き換える",
		"- [ ] 設定画面 /settings で表示を確認する",
		"",
		"## 放置した場合の影響",
		"",
		"設定のたびに問い合わせが発生する。",
	}, "\n")
}

// TestRunReadTicketAcceptsProseThatTheOldParserRejects is the behaviour the
// redesign exists for. The same ticket must be refused by the header parser
// and accepted by intake: that difference is the entire change.
func TestRunReadTicketAcceptsProseThatTheOldParserRejects(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.json")
	envelopePath := filepath.Join(directory, "envelope.json")
	writeTestJSON(t, configPath, cliTestConfig())
	writeTestJSON(t, envelopePath, cliTestEnvelopeWithDescription(t, proseTicketDescription()))

	rejectedByOldParser := run(context.Background(), []string{
		"parse-ticket", "--config", configPath, "--tool-sha", cliToolSHA,
		"--envelope", envelopePath, "--out", filepath.Join(directory, "ticket.json"),
		"--draft-out", filepath.Join(directory, "draft.json"),
	})
	var rejection ticketInputRejection
	if !errors.As(rejectedByOldParser, &rejection) {
		t.Fatalf("the fixture must be one the header parser rejects, got %v", rejectedByOldParser)
	}

	rawPath := filepath.Join(directory, "raw.json")
	if err := run(context.Background(), []string{
		"read-ticket", "--config", configPath, "--tool-sha", cliToolSHA,
		"--envelope", envelopePath, "--out", rawPath,
	}); err != nil {
		t.Fatalf("intake must accept a ticket written in prose, got error = %v", err)
	}

	var raw worker.RawTicket
	if err := worker.ReadJSONFile(rawPath, worker.MaxTicketJSONBytes, &raw); err != nil {
		t.Fatal(err)
	}
	if raw.Validate(cliTestConfig()) != nil {
		t.Fatal("the sealed raw ticket must revalidate")
	}
	if !strings.Contains(raw.Description, "RPM制限") {
		t.Fatalf("the ticket must be carried through unchanged, got %q", raw.Description)
	}
}

// TestRunBuildDraftStopsOnOpenQuestionsWithoutClaimingBadInput fixes the
// distinction the terminal report depends on: a ticket that left something
// unanswered is not a malformed ticket, and must not be reported as one.
func TestRunBuildDraftStopsOnOpenQuestionsWithoutClaimingBadInput(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.json")
	envelopePath := filepath.Join(directory, "envelope.json")
	rawPath := filepath.Join(directory, "raw.json")
	config := cliTestConfig()
	writeTestJSON(t, configPath, config)
	writeTestJSON(t, envelopePath, cliTestEnvelopeWithDescription(t, proseTicketDescription()))

	if err := run(context.Background(), []string{
		"read-ticket", "--config", configPath, "--tool-sha", cliToolSHA,
		"--envelope", envelopePath, "--out", rawPath,
	}); err != nil {
		t.Fatal(err)
	}
	var raw worker.RawTicket
	if err := worker.ReadJSONFile(rawPath, worker.MaxTicketJSONBytes, &raw); err != nil {
		t.Fatal(err)
	}

	intake := sealedTestIntake(t, raw, config, []worker.IntakeGap{{
		Field: "verification_path", Question: "どの画面で確認しますか",
		Choices: []worker.IntakeChoice{
			{ID: "a", Label: "設定", Effect: "設定画面の表示を確認する"},
			{ID: "b", Label: "ログ", Effect: "ログ画面の表示を確認する"},
		},
	}})
	intakePath := filepath.Join(directory, "intake.json")
	writeTestJSON(t, intakePath, intake)

	err := run(context.Background(), []string{
		"build-draft", "--config", configPath, "--tool-sha", cliToolSHA,
		"--raw", rawPath, "--intake", intakePath, "--out", filepath.Join(directory, "draft.json"),
	})
	var rejection ticketInputRejection
	if !errors.As(err, &rejection) {
		t.Fatalf("an open question must stop the run distinguishably, got %v", err)
	}
}

func TestRunBuildDraftCompletesTheContractWhenNothingIsLeftOpen(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.json")
	envelopePath := filepath.Join(directory, "envelope.json")
	rawPath := filepath.Join(directory, "raw.json")
	draftPath := filepath.Join(directory, "draft.json")
	config := cliTestConfig()
	writeTestJSON(t, configPath, config)
	writeTestJSON(t, envelopePath, cliTestEnvelopeWithDescription(t, proseTicketDescription()))

	if err := run(context.Background(), []string{
		"read-ticket", "--config", configPath, "--tool-sha", cliToolSHA,
		"--envelope", envelopePath, "--out", rawPath,
	}); err != nil {
		t.Fatal(err)
	}
	var raw worker.RawTicket
	if err := worker.ReadJSONFile(rawPath, worker.MaxTicketJSONBytes, &raw); err != nil {
		t.Fatal(err)
	}
	intakePath := filepath.Join(directory, "intake.json")
	writeTestJSON(t, intakePath, sealedTestIntake(t, raw, config, nil))

	if err := run(context.Background(), []string{
		"build-draft", "--config", configPath, "--tool-sha", cliToolSHA,
		"--raw", rawPath, "--intake", intakePath, "--out", draftPath,
	}); err != nil {
		t.Fatalf("build-draft() error = %v", err)
	}
	var draft worker.TicketDraft
	if err := worker.ReadJSONFile(draftPath, worker.MaxTicketJSONBytes, &draft); err != nil {
		t.Fatal(err)
	}
	if draft.AbsentText != "RPM制限" || draft.VerificationPath != "/settings" {
		t.Fatalf("draft = %+v", draft)
	}
	if draft.Mode != config.Consumers[0].Mode.ID {
		t.Fatalf("the mode must come from configuration, not from the ticket: %q", draft.Mode)
	}
}

// sealedTestIntake builds a ContractIntake the way the model path would, so a
// CLI test can exercise build-draft without a model.
func sealedTestIntake(t *testing.T, raw worker.RawTicket, config worker.Config, gaps []worker.IntakeGap) worker.ContractIntake {
	t.Helper()
	endpoint := config.Models.Readiness.Assessor
	intake := worker.ContractIntake{
		SchemaVersion: 1, PromptVersion: 1,
		DeliveryID: raw.DeliveryID, InputSHA256: raw.InputSHA256,
		ConfigSHA256: raw.ConfigSHA256, ToolSHA: raw.ToolSHA, RawSHA256: raw.RawSHA256,
		AssessorID: endpoint.ID, Vendor: endpoint.Vendor, Model: endpoint.Model, BaseURL: endpoint.BaseURL,
		Effort: endpoint.Effort, StructuredOutput: endpoint.StructuredOutput, MaxOutputTokens: endpoint.MaxOutputTokens,
		Repository:   config.Consumers[0].Repository,
		ExpectedText: "1分あたりのリクエスト数上限", AbsentText: "RPM制限",
		Request:   "用語を日本語の説明に置き換える",
		Gaps:      gaps,
		Rationale: "完了条件から読んだ",
		Invocation: worker.InvocationUsage{
			RequestedModel: endpoint.Model, RequestID: "test-request", StopReason: worker.ChatFinishStop,
			InputTokens: 10, OutputTokens: 5, TotalTokens: 15,
		},
		ReadAt: time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC),
	}
	if len(gaps) == 0 {
		intake.VerificationPath = "/settings"
	}
	sealed, err := worker.SealContractIntake(intake)
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}
