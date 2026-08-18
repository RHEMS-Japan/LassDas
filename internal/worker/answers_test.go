package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func preservedTestClarification(raw RawTicket) *ClarificationContext {
	return &ClarificationContext{
		SHA256:      strings.Repeat("d", 64),
		Revision:    2,
		DeliveryID:  raw.DeliveryID,
		InputSHA256: raw.InputSHA256,
		Exchanges: []ClarificationExchange{{
			Questions: []ReadinessQuestion{
				{
					ID: "Q1", Dimension: "user_visible_behavior",
					Question:    "入力欄の通貨はどちらで固定しますか?",
					WhyBlocking: "入力単位が変わると保存される値が変わる",
					Choices: []ReadinessChoice{
						{ID: "a", Label: "JPY 固定", Effect: "入力も表示も円になる"},
						{ID: "c", Label: "USD 固定 + 換算併記", Effect: "入力は USD のまま、隣に円換算が出る"},
					},
				},
				{
					ID: "Q2", Dimension: "acceptance_criterion",
					Question: "JSON 欄はどう扱いますか?\r\nラベル\x07だけ変えますか?",
					Choices: []ReadinessChoice{
						{ID: "a", Label: "ラベルのみ USD 明示", Effect: "JSON の中身は変えない"},
						{ID: "b", Label: "値も換算", Effect: "JSON の数値が書き換わる"},
					},
				},
			},
			Answers: map[string]string{"Q1": "c", "Q2": "手で書いた回答"},
		}},
	}
}

func preservedTestInputs(t *testing.T) (Config, RawTicket) {
	t.Helper()
	config := validTestConfig()
	config.AnswerKnowledge = &AnswerKnowledgeConfig{To: "knowledge/library/answers"}
	raw, err := ReadRawTicket(validTicketEnvelope(t, realTicketDescription()), config, strings.Repeat("c", 40))
	if err != nil {
		t.Fatalf("raw ticket: %v", err)
	}
	return config, raw
}

func TestPreserveAnswerKnowledgeRendersTheDecidedRecord(t *testing.T) {
	config, raw := preservedTestInputs(t)

	artifact, err := PreserveAnswerKnowledge(config, raw, preservedTestClarification(raw))
	if err != nil {
		t.Fatalf("preserve: %v", err)
	}
	if !artifact.Enabled || artifact.Path != "knowledge/library/answers/TICKET-3.md" || artifact.IssueKey != "TICKET-3" {
		t.Fatalf("artifact = %+v", artifact)
	}
	content := string(artifact.Content)
	// The chosen option is spelled out with its user-visible effect, so the
	// next run reads the decision, not just a letter.
	for _, expected := range []string{
		"# 起票者の回答記録: TICKET-3",
		"- 回答 revision: 2",
		"- 記録 digest: sha256:" + strings.Repeat("d", 64),
		"## 確認 1-Q1: 入力欄の通貨はどちらで固定しますか?",
		"- なぜ確認したか: 入力単位が変わると保存される値が変わる",
		"- **回答: c — USD 固定 + 換算併記**",
		"- 採用された結果: 入力は USD のまま、隣に円換算が出る",
		"- **回答: 手で書いた回答**",
	} {
		if !strings.Contains(content, expected) {
			t.Fatalf("render is missing %q:\n%s", expected, content)
		}
	}
	// A recorded string must never break out of its line: the question that
	// carried a newline and a control byte renders as one clean heading.
	if !strings.Contains(content, "## 確認 1-Q2: JSON 欄はどう扱いますか? ラベルだけ変えますか?") {
		t.Fatalf("control characters were not flattened:\n%s", content)
	}
	if strings.ContainsRune(content, '\x07') || strings.Contains(content, "\r") {
		t.Fatalf("control characters leaked into the render:\n%q", content)
	}
}

func TestPreserveAnswerKnowledgeRendersLaterRoundsAndGaps(t *testing.T) {
	config, raw := preservedTestInputs(t)
	clarification := preservedTestClarification(raw)
	clarification.Exchanges = append(clarification.Exchanges, ClarificationExchange{
		Questions: []ReadinessQuestion{{
			ID: "Q1", Dimension: "acceptance_criterion",
			Question: "二巡目の確認です。どうしますか?",
			Choices: []ReadinessChoice{
				{ID: "a", Label: "先へ", Effect: "続行される"},
				{ID: "b", Label: "戻す", Effect: "取り消される"},
			},
		}},
		// The recorded answer names a question that was never asked; nothing
		// can be said about it, so it must not be rendered. The asked
		// question stays visibly unanswered instead of borrowing it.
		Answers: map[string]string{"Q9": "a"},
	})

	artifact, err := PreserveAnswerKnowledge(config, raw, clarification)
	if err != nil {
		t.Fatalf("preserve: %v", err)
	}
	content := string(artifact.Content)
	if !strings.Contains(content, "## 確認 2-Q1: 二巡目の確認です。どうしますか?") {
		t.Fatalf("the second round must be numbered round 2:\n%s", content)
	}
	if !strings.Contains(content, "- **回答: (記録なし)**") {
		t.Fatalf("an unanswered question must say so:\n%s", content)
	}
	if strings.Contains(content, "Q9") {
		t.Fatalf("an answer to a question never asked must not be rendered:\n%s", content)
	}
}

func TestPreserveAnswerKnowledgeIsDisabledWithoutADestination(t *testing.T) {
	config := validTestConfig()
	raw, err := ReadRawTicket(validTicketEnvelope(t, realTicketDescription()), config, strings.Repeat("c", 40))
	if err != nil {
		t.Fatalf("raw ticket: %v", err)
	}

	artifact, err := PreserveAnswerKnowledge(config, raw, preservedTestClarification(raw))
	if err != nil {
		t.Fatalf("an instance without a destination must not fail, got %v", err)
	}
	if artifact.Enabled || artifact.Path != "" || len(artifact.Content) != 0 {
		t.Fatalf("nothing must be produced without a destination, got %+v", artifact)
	}
}

func TestPreserveAnswerKnowledgeRefusesAForeignClarification(t *testing.T) {
	config, raw := preservedTestInputs(t)

	foreignDelivery := preservedTestClarification(raw)
	foreignDelivery.DeliveryID = "delivery_" + strings.Repeat("0", 32)
	foreignInput := preservedTestClarification(raw)
	foreignInput.InputSHA256 = strings.Repeat("e", 64)

	for name, clarification := range map[string]*ClarificationContext{
		"another delivery": foreignDelivery,
		"another input":    foreignInput,
		"no record":        nil,
		"empty record":     {DeliveryID: raw.DeliveryID, InputSHA256: raw.InputSHA256},
	} {
		if _, err := PreserveAnswerKnowledge(config, raw, clarification); err == nil {
			t.Fatalf("%s must be refused", name)
		}
	}
}

func TestAnswerKnowledgeDestinationValidation(t *testing.T) {
	for _, destination := range []string{"knowledge/library/answers", "answers"} {
		config := validTestConfig()
		config.AnswerKnowledge = &AnswerKnowledgeConfig{To: destination}
		if err := config.Validate(); err != nil {
			t.Fatalf("destination %q must be accepted: %v", destination, err)
		}
	}
	for _, destination := range []string{
		"", "/absolute", "../escape", "a/../b", ".github/workflows", "knowledge/.hidden/answers", "a\\b",
	} {
		config := validTestConfig()
		config.AnswerKnowledge = &AnswerKnowledgeConfig{To: destination}
		if err := config.Validate(); err == nil {
			t.Fatalf("destination %q must be rejected", destination)
		}
	}
}

// Preserved answers reach both readiness prompts and are digest-bound to the
// pair: a checker judging against different answers than the assessor saw is
// refused. The repeat it prevents was measured live - the same settled
// question asked three times across generations.
func TestPreservedAnswersReachReadinessAndBindThePair(t *testing.T) {
	config, request, source := validArtifactFixture(t)
	answers := []PreservedAnswer{{Name: "T-1.md", Content: "# 回答\n空配列は拒否する。\n"}}

	api := &fakeChatAPI{output: chatOutput(`{"decision":"ready","questions":[],"assumptions":[],"reject_code":""}`)}
	invoker, err := NewModelInvoker(api)
	if err != nil {
		t.Fatal(err)
	}
	assessment, _, err := invoker.AssessReadiness(context.Background(), 1, nil, nil, nil, answers, source, request, config)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(api.request.Messages[len(api.request.Messages)-1].Content, "空配列は拒否する") {
		t.Fatal("the assessor prompt does not carry the preserved answer")
	}
	if assessment.AnswersSHA256 == "" {
		t.Fatal("the assessment is not bound to the answers it saw")
	}

	checkerAPI := &fakeChatAPI{output: chatOutput(`{"verdict":"pass","reasons":[]}`)}
	checker, _ := NewModelInvoker(checkerAPI)
	if _, _, err := checker.CheckReadiness(context.Background(), assessment, nil, nil, source, request, config); err == nil {
		t.Fatal("a checker without the assessor's answers must be refused")
	}
	check, _, err := checker.CheckReadiness(context.Background(), assessment, nil, answers, source, request, config)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(checkerAPI.request.Messages[len(checkerAPI.request.Messages)-1].Content, "空配列は拒否する") {
		t.Fatal("the checker prompt does not carry the preserved answer")
	}
	if check.AnswersSHA256 != assessment.AnswersSHA256 {
		t.Fatal("the check is not bound to the same answers")
	}
}

// LoadPreservedAnswers reads the configured directory deterministically and
// treats absence as the empty, valid state.
func TestLoadPreservedAnswersReadsTheConfiguredDirectory(t *testing.T) {
	config := validTestConfig()
	config.AnswerKnowledge = &AnswerKnowledgeConfig{To: "knowledge/library/answers"}
	root := t.TempDir()
	directory := filepath.Join(root, "knowledge", "library", "answers")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "b.md"), []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "a.md"), []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "ignore.txt"), []byte("not markdown"), 0o644); err != nil {
		t.Fatal(err)
	}
	answers, dropped, err := LoadPreservedAnswers(root, config)
	if err != nil || dropped != 0 {
		t.Fatalf("error = %v, dropped = %d", err, dropped)
	}
	if len(answers) != 2 || answers[0].Name != "a.md" || answers[1].Name != "b.md" {
		t.Fatalf("answers = %+v", answers)
	}
	if got, _, _ := LoadPreservedAnswers(t.TempDir(), config); got != nil {
		t.Fatal("an absent directory must be the empty state")
	}
	if got, _, _ := LoadPreservedAnswers("", config); got != nil {
		t.Fatal("an empty root must be the empty state")
	}
}

// Past the total budget the newest records win and the loader reports what
// it dropped instead of stalling every ticket - the growth cliff was
// measurably reachable through ordinary accumulation.
func TestLoadPreservedAnswersKeepsTheNewestWithinBudget(t *testing.T) {
	config := validTestConfig()
	config.AnswerKnowledge = &AnswerKnowledgeConfig{To: "knowledge/library/answers"}
	root := t.TempDir()
	directory := filepath.Join(root, "knowledge", "library", "answers")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("x", 50*1024)
	for _, name := range []string{"TICKET-9.md", "TICKET-56.md", "TICKET-102.md"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(big), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	answers, dropped, err := LoadPreservedAnswers(root, config)
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 1 || len(answers) != 2 {
		t.Fatalf("answers = %d, dropped = %d, want the newest two", len(answers), dropped)
	}
	if answers[0].Name != "TICKET-56.md" || answers[1].Name != "TICKET-102.md" {
		t.Fatalf("kept = %s, %s — trailing numbers must order numerically", answers[0].Name, answers[1].Name)
	}
}
