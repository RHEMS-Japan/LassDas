package worker

import (
	"context"
	"strings"
	"testing"
)

// realTicketDescription is a ticket written the way tickets are actually
// written in the tracker: prose with headings, tables and a proposed set of
// acceptance criteria, and not one line of the format the automation used to
// demand. Processing this is the whole point of intake.
func realTicketDescription() string {
	return strings.Join([]string{
		"## 背景",
		"",
		"2026-08-05 発注者指摘: 管理画面のバーチャルキー編集ダイアログの用語が伝わらない。",
		"",
		"## 実測で判明した問題",
		"",
		"- 「RPM制限」は 1 分あたりのリクエスト数の上限だが、略語のままで日本語話者に伝わらない",
		"",
		"## 完了条件 (案)",
		"",
		"- [ ] 用語を日本語の説明に置き換える (RPM制限 → 1分あたりのリクエスト数上限)",
		"",
		"## 放置した場合の影響",
		"",
		"用語が現行と合っていないため、設定のたびに運営への問い合わせが発生する。",
	}, "\n")
}

func TestReadRawTicketAcceptsProseWithoutAnyHeaders(t *testing.T) {
	config := validTestConfig()
	toolSHA := strings.Repeat("c", 40)

	raw, err := ReadRawTicket(validTicketEnvelope(t, realTicketDescription()), config, toolSHA)
	if err != nil {
		t.Fatalf("a ticket without the fixed header block must be accepted, got error = %v", err)
	}
	if raw.IssueKey != "TICKET-3" || raw.ToolSHA != toolSHA {
		t.Fatalf("raw = %+v", raw)
	}
	if err := raw.Validate(config); err != nil {
		t.Fatalf("sealed raw ticket must revalidate: %v", err)
	}
}

// TestReadRawTicketRejectsOnlyUnprocessableInput fixes the boundary the design
// depends on: the only reasons to refuse a ticket at the door are that it
// cannot be processed at all. Formatting is never one of them.
func TestReadRawTicketRejectsOnlyUnprocessableInput(t *testing.T) {
	config := validTestConfig()
	toolSHA := strings.Repeat("c", 40)

	accepted := map[string]string{
		"prose with headings":   realTicketDescription(),
		"a single sentence":     "ログイン画面の「送信」を「ログイン」に変えてください。",
		"the old header format": validTicketDescription(),
		"markdown table":        "| 項目 | 値 |\n|---|---|\n| 用語 | RPM制限 |",
		"no trailing newline":   "文言を直してほしい",
	}
	for name, description := range accepted {
		if _, err := ReadRawTicket(validTicketEnvelope(t, description), config, toolSHA); err != nil {
			t.Errorf("%s must be accepted, got error = %v", name, err)
		}
	}

	rejected := map[string]string{
		"empty":            "",
		"NUL byte":         "文言を直す\x00",
		"control sequence": "文言を直す\x07",
	}
	for name, description := range rejected {
		if _, err := ReadRawTicket(validTicketEnvelope(t, description), config, toolSHA); err == nil {
			t.Errorf("%s must be refused as unprocessable", name)
		}
	}
}

func TestReadContractReadsTheContractOutOfProse(t *testing.T) {
	config := validTestConfig()
	raw, err := ReadRawTicket(validTicketEnvelope(t, realTicketDescription()), config, strings.Repeat("c", 40))
	if err != nil {
		t.Fatal(err)
	}
	invoker, err := NewModelInvoker(&fakeChatAPI{output: chatOutput(
		`{"verification_path":"/settings","expected_text":"1分あたりのリクエスト数上限","absent_text":"RPM制限","request":"用語を日本語の説明に置き換える","gaps":[],"rationale":"完了条件から読んだ"}`,
	)})
	if err != nil {
		t.Fatal(err)
	}

	intake, _, err := invoker.ReadContract(context.Background(), raw, config)
	if err != nil {
		t.Fatalf("ReadContract() error = %v", err)
	}
	if !intake.Complete() {
		t.Fatalf("a fully readable ticket must produce no gaps, got %+v", intake.Gaps)
	}
	draft, err := intake.ToDraft(raw, config)
	if err != nil {
		t.Fatalf("ToDraft() error = %v", err)
	}
	if draft.AbsentText != "RPM制限" || draft.ExpectedText != "1分あたりのリクエスト数上限" || draft.VerificationPath != "/settings" {
		t.Fatalf("draft = %+v", draft)
	}
	if draft.Mode != config.Consumers[0].Mode.ID {
		t.Fatalf("mode must be derived from configuration, not declared by the requester: %q", draft.Mode)
	}
}

// TestReadContractSettlesAnUnreadableWordingFieldWithoutAsking pins the
// wording rule (2026-08-07, measured on the first live ticket): the wording
// promise is optional, so an unreadable part of it is settled to "no promise"
// and the run proceeds — it never becomes a quiz for the requester. Questions
// are reserved for what genuinely blocks work, which the repository tests
// below cover.
func TestReadContractSettlesAnUnreadableWordingFieldWithoutAsking(t *testing.T) {
	config := validTestConfig()
	raw, err := ReadRawTicket(validTicketEnvelope(t, realTicketDescription()), config, strings.Repeat("c", 40))
	if err != nil {
		t.Fatal(err)
	}
	invoker, err := NewModelInvoker(&fakeChatAPI{output: chatOutput(
		`{"verification_path":"","expected_text":"1分あたりのリクエスト数上限","absent_text":"RPM制限","request":"用語を置き換える",` +
			`"gaps":[{"field":"verification_path","question":"どの画面で確認しますか","choices":[` +
			`{"id":"a","label":"バーチャルキー編集","effect":"編集ダイアログの表示を確認する"},` +
			`{"id":"b","label":"ガードレールログ","effect":"ログ画面の表示を確認する"}]}],"rationale":"画面が本文にない"}`,
	)})
	if err != nil {
		t.Fatal(err)
	}

	intake, _, err := invoker.ReadContract(context.Background(), raw, config)
	if err != nil {
		t.Fatalf("an unreadable wording field must settle, not fail: %v", err)
	}
	if !intake.Complete() {
		t.Fatalf("wording must never become a question, got %+v", intake.Gaps)
	}
	draft, err := intake.ToDraft(raw, config)
	if err != nil {
		t.Fatalf("ToDraft() error = %v", err)
	}
	if draft.VerificationPath != "" || draft.ExpectedText != "" || draft.AbsentText != "" {
		t.Fatalf("a partial wording reading must settle to no promise, got %+v", draft)
	}
}

// TestReadContractSettlesAFilledWordingFieldTheModelAlsoDoubted: a drafted
// wording question beside a filled value means the reading was in doubt, and
// doubt settles to "no promise" — the half-trusted value must not survive into
// a verification gate.
func TestReadContractSettlesAFilledWordingFieldTheModelAlsoDoubted(t *testing.T) {
	config := validTestConfig()
	raw, err := ReadRawTicket(validTicketEnvelope(t, realTicketDescription()), config, strings.Repeat("c", 40))
	if err != nil {
		t.Fatal(err)
	}
	invoker, err := NewModelInvoker(&fakeChatAPI{output: chatOutput(
		`{"verification_path":"/settings","expected_text":"新しい文言","absent_text":"RPM制限","request":"置き換える",` +
			`"gaps":[{"field":"verification_path","question":"どの画面ですか","choices":[` +
			`{"id":"a","label":"A","effect":"Aを確認する"},{"id":"b","label":"B","effect":"Bを確認する"}]}],"rationale":"両方"}`,
	)})
	if err != nil {
		t.Fatal(err)
	}
	intake, _, err := invoker.ReadContract(context.Background(), raw, config)
	if err != nil {
		t.Fatalf("a doubted wording reading must settle, not fail: %v", err)
	}
	if !intake.Complete() || intake.VerificationPath != "" || intake.ExpectedText != "" || intake.AbsentText != "" {
		t.Fatalf("doubt must settle to no promise, got %+v", intake)
	}
}

// TestReadContractRequiresChoicesOnEveryGap enforces the principle that a
// question is asked with alternatives worked out, not handed back open-ended.
// The wording fields no longer produce gaps, so the principle is enforced on
// the one field that still asks: the repository.
func TestReadContractRequiresChoicesOnEveryGap(t *testing.T) {
	config := validTestConfig()
	err := validateIntakeContent("", "", "", "", "request text", []IntakeGap{{
		Field: "repository", Question: "どの納品先ですか", Choices: []IntakeChoice{
			{ID: "a", Label: "A", Effect: "Aへ納品する"},
		},
	}}, config)
	if err == nil {
		t.Fatal("a gap offering fewer than two choices must be refused")
	}
}

func TestContractIntakeRejectsAnUnknownGapField(t *testing.T) {
	config := validTestConfig()
	err := validateIntakeContent(config.Consumers[0].Repository, "/settings", "new-wording", "old-wording", "request", []IntakeGap{{
		Field: "target_files", Question: "どれ", Choices: []IntakeChoice{
			{ID: "a", Label: "A", Effect: "A"}, {ID: "b", Label: "B", Effect: "B"},
		},
	}}, config)
	if err == nil {
		t.Fatal("intake may only report gaps for the fields it reads")
	}
}

// secondTestConsumer widens the configuration to two destinations, which is
// what makes the repository a real question.
func secondTestConsumer() ConsumerConfig {
	return ConsumerConfig{
		Repository: "example/gateway", RepositoryID: 202,
		Description: "The LLM gateway service.",
		Delivery:    DeliverPullRequest, IntegrationBranch: "stg", ReleaseBranch: "prod",
		StagingOrigin: "https://gateway-stg.example.com", ProductionOrigin: "https://gateway.example.com",
		StagingWorkflow: "deploy-stg.yml", ProductionWorkflow: "deploy.yml",
		GitHub: testConsumerGitHubContract(),
		Mode: ModeConfig{
			ID: "service-change", AllowedFilePrefixes: []string{"internal/"},
			ForbiddenCandidateText: []string{"forbidden-project-name"},
			MaxFiles:               3, MaxFileBytes: 256 * 1024, MaxTotalBytes: 512 * 1024,
			MaxChangedLines: 200, MaxChangedBytes: 64 * 1024,
			Toolchain:              []ToolRequirement{{Binary: "node", Version: "22", StripVPrefix: true}, {Binary: "pnpm", Version: "9.15.4"}},
			VerifyWorkingDirectory: ".",
			InstallCommand:         []string{"go", "mod", "download"},
			VerifyCommands:         [][]string{{"go", "test", "./..."}},
		},
	}
}

// TestReadContractResolvesTheSoleRepositoryWithoutAsking: with one configured
// destination there is nothing to ask; an empty reading binds to it.
func TestReadContractResolvesTheSoleRepositoryWithoutAsking(t *testing.T) {
	config := validTestConfig()
	raw, err := ReadRawTicket(validTicketEnvelope(t, realTicketDescription()), config, strings.Repeat("c", 40))
	if err != nil {
		t.Fatal(err)
	}
	invoker, _ := NewModelInvoker(&fakeChatAPI{output: chatOutput(
		`{"repository":"","verification_path":"/settings","expected_text":"1分あたりのリクエスト数上限","absent_text":"RPM制限","request":"用語を置き換える","gaps":[],"rationale":"完了条件から読んだ"}`,
	)})
	intake, _, err := invoker.ReadContract(context.Background(), raw, config)
	if err != nil {
		t.Fatalf("ReadContract() error = %v", err)
	}
	if intake.Repository != config.Consumers[0].Repository || !intake.Complete() {
		t.Fatalf("intake = %+v", intake)
	}
}

// TestReadContractSynthesizesTheRepositoryQuestion: with two destinations and
// a ticket that names neither, the question is synthesized from the
// configuration — its choices are exactly the configured repositories.
func TestReadContractSynthesizesTheRepositoryQuestion(t *testing.T) {
	config := validTestConfig()
	config.Consumers = append(config.Consumers, secondTestConsumer())
	raw, err := ReadRawTicket(validTicketEnvelope(t, realTicketDescription()), config, strings.Repeat("c", 40))
	if err != nil {
		t.Fatal(err)
	}
	invoker, _ := NewModelInvoker(&fakeChatAPI{output: chatOutput(
		`{"repository":"","verification_path":"","expected_text":"","absent_text":"","request":"通知経路を追加する","gaps":[],"rationale":"本文から読んだ"}`,
	)})
	intake, _, err := invoker.ReadContract(context.Background(), raw, config)
	if err != nil {
		t.Fatalf("ReadContract() error = %v", err)
	}
	if intake.Complete() || len(intake.Gaps) != 1 || intake.Gaps[0].Field != "repository" {
		t.Fatalf("gaps = %+v", intake.Gaps)
	}
	choices := intake.Gaps[0].Choices
	if len(choices) != 2 || choices[0].Label != config.Consumers[0].Repository || choices[1].Label != config.Consumers[1].Repository {
		t.Fatalf("choices = %+v", choices)
	}
	if choices[1].Effect != "The LLM gateway service." {
		t.Fatalf("the consumer's own description must become the effect text: %+v", choices[1])
	}
	if _, err := intake.ToDraft(raw, config); err == nil {
		t.Fatal("an intake with an open repository question must not become a draft")
	}
}

// TestReadContractRejectsAnUnconfiguredRepository: a reading outside the
// configuration is never adopted; with several destinations it becomes the
// synthesized question instead.
func TestReadContractRejectsAnUnconfiguredRepository(t *testing.T) {
	config := validTestConfig()
	config.Consumers = append(config.Consumers, secondTestConsumer())
	raw, err := ReadRawTicket(validTicketEnvelope(t, realTicketDescription()), config, strings.Repeat("c", 40))
	if err != nil {
		t.Fatal(err)
	}
	invoker, _ := NewModelInvoker(&fakeChatAPI{output: chatOutput(
		`{"repository":"attacker/elsewhere","verification_path":"","expected_text":"","absent_text":"","request":"どこかを変える","gaps":[],"rationale":"読み違い"}`,
	)})
	intake, _, err := invoker.ReadContract(context.Background(), raw, config)
	if err != nil {
		t.Fatalf("ReadContract() error = %v", err)
	}
	if intake.Repository != "" || intake.Complete() || len(intake.Gaps) != 1 || intake.Gaps[0].Field != "repository" {
		t.Fatalf("intake = %+v", intake)
	}
}

// TestReadContractAcceptsATicketWithoutAWordingPromise is the general shape:
// a request that changes behavior rather than wording carries no wording
// contract and raises no wording questions.
func TestReadContractAcceptsATicketWithoutAWordingPromise(t *testing.T) {
	config := validTestConfig()
	config.Consumers = append(config.Consumers, secondTestConsumer())
	raw, err := ReadRawTicket(validTicketEnvelope(t, "通知の再送経路を追加してほしい。詳細は本文の通り。"), config, strings.Repeat("c", 40))
	if err != nil {
		t.Fatal(err)
	}
	invoker, _ := NewModelInvoker(&fakeChatAPI{output: chatOutput(
		`{"repository":"example/gateway","verification_path":"","expected_text":"","absent_text":"","request":"通知の再送経路を追加する","gaps":[],"rationale":"本文から読んだ"}`,
	)})
	intake, _, err := invoker.ReadContract(context.Background(), raw, config)
	if err != nil {
		t.Fatalf("ReadContract() error = %v", err)
	}
	if !intake.Complete() || intake.Repository != "example/gateway" {
		t.Fatalf("intake = %+v", intake)
	}
	draft, err := intake.ToDraft(raw, config)
	if err != nil {
		t.Fatalf("ToDraft() error = %v", err)
	}
	if draft.Repository != "example/gateway" || draft.Mode != "service-change" {
		t.Fatalf("draft = %+v", draft)
	}
	if draft.VerificationPath != "" || draft.ExpectedText != "" || draft.AbsentText != "" {
		t.Fatalf("a ticket without a wording promise must carry none: %+v", draft)
	}
}

// TestReadContractSettlesAPartialWordingPromiseToNone: half a promise cannot
// be verified, and since 2026-08-07 it is settled to no promise instead of
// refused — the request itself was readable, and the pull request review is
// where the wording lands in front of the requester.
func TestReadContractSettlesAPartialWordingPromiseToNone(t *testing.T) {
	config := validTestConfig()
	raw, err := ReadRawTicket(validTicketEnvelope(t, realTicketDescription()), config, strings.Repeat("c", 40))
	if err != nil {
		t.Fatal(err)
	}
	invoker, _ := NewModelInvoker(&fakeChatAPI{output: chatOutput(
		`{"repository":"","verification_path":"","expected_text":"新しい文言だけがある","absent_text":"","request":"文言を変える","gaps":[],"rationale":"半端"}`,
	)})
	intake, _, err := invoker.ReadContract(context.Background(), raw, config)
	if err != nil {
		t.Fatalf("a partial wording promise must settle, not fail: %v", err)
	}
	if !intake.Complete() || intake.ExpectedText != "" || intake.AbsentText != "" || intake.VerificationPath != "" {
		t.Fatalf("intake = %+v", intake)
	}
}

// TestContractIntakeStillRefusesAPartialSealedPromise keeps the sealed-artifact
// invariant: a replayed intake carrying half a promise is invalid even though
// the construction path can no longer produce one.
func TestContractIntakeStillRefusesAPartialSealedPromise(t *testing.T) {
	config := validTestConfig()
	if err := validateIntakeContent(config.Consumers[0].Repository, "", "新しい文言だけがある", "", "request text", nil, config); err == nil {
		t.Fatal("a sealed partial wording promise must stay invalid")
	}
}
