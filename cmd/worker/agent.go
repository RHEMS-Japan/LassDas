package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/worker"
)

// runImplement hands the ticket to the configured coding agent, working in a
// checked-out copy of the destination repository. The agent finds the files
// itself, reads what it needs, and edits in place; this command records what
// changed and seals it into the same artifacts a model-authored change
// produces, so everything downstream is unchanged.
func runImplement(ctx context.Context, args []string) error {
	flags := commandFlags("implement")
	configPath := flags.String("config", "", "")
	toolSHA := flags.String("tool-sha", "", "")
	draftPath := flags.String("draft", "", "")
	repoRoot := flags.String("repo-root", "", "")
	baseRoot := flags.String("base-root", "", "")
	baseSHA := flags.String("base-sha", "", "")
	knowledgeRoot := flags.String("knowledge-root", "", "")
	stage := flags.Int("stage", 0, "")
	clarificationPath := flags.String("clarification", "", "")
	var findingsPaths stringList
	flags.Var(&findingsPaths, "previous-findings", "")
	runOutPath := flags.String("run-out", "", "")
	ticketOutPath := flags.String("ticket-out", "", "")
	sourceOutPath := flags.String("source-out", "", "")
	outputPath := flags.String("out", "", "")
	if !parseFlags(flags, args) ||
		!allPresent(*configPath, *toolSHA, *draftPath, *repoRoot, *baseRoot, *baseSHA, *runOutPath, *ticketOutPath, *sourceOutPath, *outputPath) ||
		!worker.ValidToolSHA(*toolSHA) || *stage < 1 {
		return errors.New("implement arguments are invalid")
	}
	config, err := readConfig(*configPath)
	if err != nil {
		return err
	}
	if *stage > config.MaxStages {
		return errors.New("implement stage is invalid")
	}
	var draft worker.TicketDraft
	if err := worker.ReadJSONFile(*draftPath, worker.MaxTicketJSONBytes, &draft); err != nil {
		return errors.New("ticket draft could not be read")
	}
	configSHA, err := config.SHA256()
	if err != nil || draft.ConfigSHA256 != configSHA || draft.ToolSHA != *toolSHA {
		return errors.New("ticket draft is not bound to this run")
	}
	consumer, err := config.ConsumerFor(draft.Repository)
	if err != nil {
		return errors.New("ticket draft repository is not a configured consumer")
	}

	clarification, err := readClarificationContext(*clarificationPath)
	if err != nil {
		return err
	}
	findings, err := readPreviousFindings(findingsPaths)
	if err != nil {
		return err
	}
	prompt, err := implementPrompt(draft, consumer, config.Agents.Implementer, clarification, findings)
	if err != nil {
		return errors.New("implement instruction could not be built")
	}
	if err := placeAgentKnowledge(config.Agents.Implementer, *knowledgeRoot, *repoRoot); err != nil {
		return err
	}

	outcome, runErr := worker.RunAgent(ctx, config.Agents.Implementer, *repoRoot, prompt, consumer.Mode.AllowedFilePrefixes)
	run, sealErr := worker.SealAgentRun(worker.AgentRun{
		SchemaVersion: worker.ArtifactSchemaVersion, Stage: *stage,
		DeliveryID: draft.DeliveryID, InputSHA256: draft.InputSHA256,
		ConfigSHA256: draft.ConfigSHA256, ToolSHA: draft.ToolSHA, BaseSHA: *baseSHA,
		AgentID: outcome.AgentID, Command: outcome.Command, PromptBytes: len(prompt), ExitCode: outcome.ExitCode,
		DurationMs: outcome.Duration.Milliseconds(), ChangedFiles: outcome.ChangedFiles,
		Transcript: outcome.Transcript, RanAt: time.Now().UTC(),
	})
	if sealErr == nil {
		// The run record is evidence of what happened, so it is written even
		// when the run failed.
		_ = worker.WriteJSONFileExclusive(*runOutPath, run, worker.MaxArtifactJSONBytes)
	}
	if runErr != nil {
		return errors.New("the implementing agent did not finish")
	}

	observed, err := worker.ReadObservedChanges(*repoRoot, *baseRoot, outcome.ChangedFiles, consumer)
	if err != nil {
		return err
	}
	request, err := worker.TicketWithObservedTargets(draft, observed, config)
	if err != nil {
		return errors.New("the files the agent changed do not form a valid contract")
	}
	source, err := worker.SourceFromObservedChanges(*baseSHA, observed, request, config)
	if err != nil {
		return err
	}
	candidate, err := worker.CandidateFromObservedChanges(*stage, observed, source, request, config, run, time.Now().UTC())
	if err != nil {
		return err
	}
	if err := worker.WriteJSONFileExclusive(*ticketOutPath, request, worker.MaxTicketJSONBytes); err != nil {
		return errors.New("ticket artifact could not be written")
	}
	if err := worker.WriteJSONFileExclusive(*sourceOutPath, source, worker.MaxArtifactJSONBytes); err != nil {
		return errors.New("source artifact could not be written")
	}
	if err := worker.WriteJSONFileExclusive(*outputPath, candidate, worker.MaxArtifactJSONBytes); err != nil {
		return errors.New("candidate artifact could not be written")
	}
	return nil
}

// runAgentReview hands the finished change to the reviewing agent, in the same
// working copy. Reviewing in the repository rather than on the diff alone lets
// it read whatever it needs to judge the change in context, which is the whole
// reason the reviewer is an agent and not a single question.

// transcriptTail is the last stretch of an attempt's transcript, enough to
// read the death without flooding the job log.
func transcriptTail(outcome worker.AgentOutcome) string {
	tail := outcome.Transcript
	if len(tail) > 2048 {
		tail = tail[len(tail)-2048:]
	}
	return tail
}

// reviewRetryPause is worker.ReviewRetryPause behind a seam: the retry tests
// exercise the loop below with fake agents, and a real 75-second sleep per
// retry measurably turned a seven-second suite into a five-minute one -
// inside the per-ticket tool-integrity gate, not just CI.
var reviewRetryPause = worker.ReviewRetryPause

func runAgentReview(ctx context.Context, args []string) error {
	flags := commandFlags("agent-review")
	configPath := flags.String("config", "", "")
	toolSHA := flags.String("tool-sha", "", "")
	ticketPath := flags.String("ticket", "", "")
	sourcePath := flags.String("source", "", "")
	candidatePath := flags.String("candidate", "", "")
	reviewerID := flags.String("reviewer", "", "")
	repoRoot := flags.String("repo-root", "", "")
	baseSHA := flags.String("base-sha", "", "")
	knowledgeRoot := flags.String("knowledge-root", "", "")
	clarificationPath := flags.String("clarification", "", "")
	runOutPath := flags.String("run-out", "", "")
	outputPath := flags.String("out", "", "")
	if !parseFlags(flags, args) ||
		!allPresent(*configPath, *toolSHA, *ticketPath, *sourcePath, *candidatePath, *reviewerID, *repoRoot, *baseSHA, *runOutPath, *outputPath) ||
		!worker.ValidToolSHA(*toolSHA) {
		return errors.New("agent-review arguments are invalid")
	}
	config, request, source, err := readBoundInputs(*configPath, *toolSHA, *ticketPath, *sourcePath)
	if err != nil {
		return err
	}
	var candidate worker.Candidate
	if err := worker.ReadJSONFile(*candidatePath, worker.MaxArtifactJSONBytes, &candidate); err != nil {
		return errors.New("candidate artifact could not be read")
	}
	if err := candidate.Validate(source, request, config); err != nil {
		return errors.New("candidate artifact was rejected")
	}
	endpoint, ok := configuredEndpoint(config, *reviewerID, true)
	if !ok {
		return errors.New("reviewer is not configured")
	}
	clarification, err := readClarificationContext(*clarificationPath)
	if err != nil {
		return err
	}
	prompt, err := reviewAgentPrompt(candidate, source, request, endpoint, clarification)
	if err != nil {
		// The builder's failures are static prose ("instruction is too
		// large") - naming them is what made the third live ticket's death
		// diagnosable in one glance instead of an artifact dig.
		return fmt.Errorf("review instruction could not be built: %w", err)
	}
	if err := placeAgentKnowledge(config.Agents.Reviewer, *knowledgeRoot, *repoRoot); err != nil {
		return err
	}

	// The reviewer is not told which files it may touch, because it is not
	// meant to touch any; a review that edits the tree is rejected below.
	//
	// A failed attempt that died fast is retried on a fresh conversation
	// (the upstream lottery - see worker.ReviewAttemptLimit); one that
	// burned real time is not, so the stage's worst case stays inside the
	// job's budget. Every failed attempt's tail goes to the job log, the
	// final one included, so nothing is masked.
	outcome, runErr := worker.RunAgent(ctx, config.Agents.Reviewer, *repoRoot, prompt, nil)
	for attempt := 1; runErr != nil && attempt < worker.ReviewAttemptLimit && worker.RetryableReviewFailure(outcome); attempt++ {
		fmt.Fprintf(os.Stderr, "worker: the reviewing agent did not finish (exit %d) on attempt %d, retrying in %s; attempt tail:\n%s\n", outcome.ExitCode, attempt, reviewRetryPause, transcriptTail(outcome))
		select {
		case <-ctx.Done():
			attempt = worker.ReviewAttemptLimit
			continue
		case <-time.After(reviewRetryPause):
		}
		outcome, runErr = worker.RunAgent(ctx, config.Agents.Reviewer, *repoRoot, prompt, nil)
	}
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "worker: the reviewing agent did not finish (exit %d) on its final attempt; tail:\n%s\n", outcome.ExitCode, transcriptTail(outcome))
	}
	run, sealErr := worker.SealAgentRun(worker.AgentRun{
		SchemaVersion: worker.ArtifactSchemaVersion, Stage: candidate.Stage,
		DeliveryID: request.DeliveryID, InputSHA256: request.InputSHA256,
		ConfigSHA256: request.ConfigSHA256, ToolSHA: request.ToolSHA, BaseSHA: *baseSHA,
		AgentID: outcome.AgentID, Command: outcome.Command, PromptBytes: len(prompt), ExitCode: outcome.ExitCode,
		DurationMs: outcome.Duration.Milliseconds(), ChangedFiles: nil,
		Transcript: outcome.Transcript, RanAt: time.Now().UTC(),
	})
	if sealErr == nil {
		_ = worker.WriteJSONFileExclusive(*runOutPath, run, worker.MaxArtifactJSONBytes)
	}
	if runErr != nil {
		return errors.New("the reviewing agent did not finish")
	}
	consumer, err := request.Consumer(config)
	if err != nil {
		return errors.New("ticket repository is not a configured consumer")
	}
	if err := worker.ConfirmTreeMatchesCandidate(*repoRoot, candidate, consumer); err != nil {
		return err
	}
	review, err := worker.AgentReviewFromRun(endpoint, run, candidate, source, request, config, time.Now().UTC())
	if err != nil {
		return err
	}
	if err := worker.WriteJSONFileExclusive(*outputPath, review, worker.MaxReviewJSONBytes); err != nil {
		return errors.New("review artifact could not be written")
	}
	return nil
}

// reviewAgentPrompt states what to judge and the exact shape of the answer.
// The reviewer is pointed at the ticket and the changed files but left free to
// read the rest of the repository, which is what makes its objections worth
// more than a reading of the diff.
func reviewAgentPrompt(
	candidate worker.Candidate,
	source worker.SourceSnapshot,
	request worker.TicketRequest,
	endpoint worker.ModelEndpoint,
	clarification *worker.ClarificationContext,
) (string, error) {
	changed := make([]string, 0, len(candidate.Files))
	for _, file := range candidate.Files {
		changed = append(changed, file.Path)
	}
	sections := []string{
		"あなたはこの変更を通すかどうかを判定するレビュアーです。作業ディレクトリには変更が適用済みです。",
		"",
		"## この実行環境について",
		"あなたは自動実行の中にいます。人は見ていません。",
		"- 作業規範に「着手前に承認を得る」「体制を宣言する」とあっても、この無人実行では承認できる人がいないため、それらは行わないでください。",
		"- 宣言・確認・挨拶の文章を出力せず、直ちにレビューして判定の JSON を出力してください。返事を待って止まると、このレビューは失敗として扱われます。",
		"- 依頼者に質問することはできません。判断に迷ったら、その迷いを findings の message に書いて revise にしてください。",
		"",
		"## 見る観点",
		endpoint.Lens,
		"",
		"## 依頼 (" + request.IssueKey + ")",
		"",
		"### 件名",
		request.Summary,
		"",
		"### 本文",
		request.Request,
		"",
		"### 変更されたファイル",
		strings.Join(changed, "\n"),
	}
	head := sections
	sections = nil
	if request.AbsentText != "" {
		sections = append(sections,
			"",
			"### 依頼者が確認すること",
			"- 見えなくなるはずの文言: "+request.AbsentText,
			"- 見えるようになるはずの文言: "+request.ExpectedText,
			"- 確認する画面: "+request.VerificationPath,
		)
	}
	if clarification != nil && len(clarification.Exchanges) > 0 {
		encoded, err := json.Marshal(clarification.Exchanges)
		if err != nil {
			return "", err
		}
		sections = append(sections, "", "### 依頼者が答えた内容 (決定事項)", string(encoded))
	}
	sections = append(sections,
		"",
		"## やること",
		"- 変更されたファイルを読み、必要なら周辺のコードも読んで、依頼を満たしているかを判定してください。",
		"- ファイルは一切変更しないでください。読むだけです。",
		"- 好みの問題は指摘しないでください。依頼が満たされないもの、壊れるもの、副作用のあるものだけを指摘してください。",
		"",
		"## 答え方 (最後にこの形の JSON だけを出力する)",
		`{"verdict":"pass","findings":[]}`,
		"または",
		`{"verdict":"revise","findings":[{"code":"英小文字とハイフンの短い識別子","path":"変更されたファイルのいずれか","line":0,"message":"何がどう問題かを一文で"}]}`,
		"- verdict が pass のときは findings を空にしてください。revise のときは 1 件以上必要です。",
		"- path は上の「変更されたファイル」に挙がっているものだけです。",
		worker.ReviewAnswerRulesTail,
	)
	tail := sections
	assemble := func(middle []string) string {
		parts := make([]string, 0, len(head)+len(middle)+len(tail))
		parts = append(parts, head...)
		parts = append(parts, middle...)
		parts = append(parts, tail...)
		return strings.Join(parts, "\n")
	}
	prompt := assemble(append([]string{
		"",
		"### 変更内容 (機械抽出の差分)",
		"- 下の差分が今回の変更の全てです。大きなファイルは全文を読み込まず、差分の行番号の周辺だけを必要に応じて部分的に読んでください (全文読み込みは接続を溢れさせます)。",
	}, worker.ChangedRegionSummaries(candidate, source)...))
	if len(prompt) > worker.MaxAgentPromptBytes {
		// The full patches outgrew the instruction. Fall back to naming the
		// changed line ranges and let the reviewer open exactly those spans
		// in the working tree; a review with a map beats no review at all.
		prompt = assemble(append([]string{
			"",
			"### 変更内容 (変更位置の一覧)",
			"- 変更が大きく、この指示文に差分の中身は収まりませんでした。下に挙がる行範囲が変更された箇所の全てです。",
			"- 各ファイルは作業ディレクトリに変更適用済みです。「変更後」の行番号で該当範囲とその周辺だけを開いて確認してください (全文読み込みは接続を溢れさせます)。",
			"- 範囲がファイル全体に及ぶものは、依頼に関係する箇所を探して部分的に読んでください。",
		}, worker.ChangedRegionOutlines(candidate, source)...))
	}
	if len(prompt) > worker.MaxAgentPromptBytes {
		return "", errors.New("instruction is too large")
	}
	return prompt, nil
}

// readPreviousFindings loads what every reviewer objected to last stage, so a
// second attempt starts from all the objections rather than from one
// reviewer's share of them.
func readPreviousFindings(filePaths []string) ([]worker.ModelFinding, error) {
	findings := make([]worker.ModelFinding, 0, 8)
	for _, filePath := range filePaths {
		var review worker.Review
		if err := worker.ReadJSONFile(filePath, worker.MaxReviewJSONBytes, &review); err != nil {
			return nil, errors.New("previous review could not be read")
		}
		findings = append(findings, review.Findings...)
	}
	return findings, nil
}

// implementPrompt states the request and the boundaries. It deliberately does
// not name files: finding them is the agent's work, and naming them would put
// the requester back in the position of having to know the codebase.
func implementPrompt(
	draft worker.TicketDraft,
	consumer worker.ConsumerConfig,
	agent worker.AgentConfig,
	clarification *worker.ClarificationContext,
	findings []worker.ModelFinding,
) (string, error) {
	sections := []string{
		"あなたはこのリポジトリで、依頼された変更を実装します。",
		"",
		"## 依頼 (" + draft.IssueKey + ")",
		"",
		"### 件名",
		draft.Summary,
		"",
		"### 本文",
		draft.Request,
	}
	if draft.AbsentText != "" {
		sections = append(sections,
			"",
			"### 画面で確認できること",
			"- 変更後に見えなくなる文言: "+draft.AbsentText,
			"- 変更後に見えるようになる文言: "+draft.ExpectedText,
			"- 確認する画面: "+draft.VerificationPath,
		)
	}
	if clarification != nil && len(clarification.Exchanges) > 0 {
		encoded, err := json.Marshal(clarification.Exchanges)
		if err != nil {
			return "", err
		}
		sections = append(sections,
			"",
			"### 依頼者が答えた内容 (これは決定事項として扱う)",
			string(encoded),
		)
	}
	if len(findings) > 0 {
		encoded, err := json.Marshal(findings)
		if err != nil {
			return "", err
		}
		sections = append(sections,
			"",
			"### 前回の指摘 (これを解消すること)",
			string(encoded),
		)
	}
	sections = append(sections,
		"",
		"## 守ること",
		"- 変更してよいのは "+strings.Join(consumer.Mode.AllowedFilePrefixes, " / ")+" の下だけです。それ以外を変更した実行は破棄されます。",
		"- 変更するファイルは最大 "+itoa(consumer.Mode.MaxFiles)+" 個までです。",
		"- 新しいファイルを作ってもかまいません。置けるのは上の変更してよい場所の下だけで、ファイル数の上限にも数えます。既存ファイルの変更で足りる依頼では、新しいファイルを増やさないでください。",
		"- 依頼に書かれていない改善・整理はしないでください。依頼を満たす最小の変更にしてください。",
		"- 自動化・リリース手順・資格情報・権限設定には触れないでください。",
		"- 変更が終わったら、何をどう変えたかを数行で述べて終了してください。コミットはしないでください。",
		"",
		environmentSection(agent),
	)
	prompt := strings.Join(sections, "\n")
	if len(prompt) > worker.MaxAgentPromptBytes {
		return "", errors.New("instruction is too large")
	}
	return prompt, nil
}

func itoa(value int) string {
	if value < 0 {
		return "0"
	}
	digits := ""
	if value == 0 {
		return "0"
	}
	for value > 0 {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
	}
	return digits
}

// placeAgentKnowledge puts what an agent is given to read where it reads it,
// before the agent starts. A destination that has written nothing down is a
// valid state: the agent then works from the ticket alone.
func placeAgentKnowledge(agent worker.AgentConfig, knowledgeRoot, workspace string) error {
	if agent.Knowledge.Empty() {
		return nil
	}
	if knowledgeRoot == "" {
		return errors.New("this agent is configured to be given knowledge, but none was provided")
	}
	home := os.Getenv("HOME")
	if home == "" {
		return errors.New("the agent has no home to place knowledge in")
	}
	if _, err := worker.PlaceKnowledge(agent.Knowledge, knowledgeRoot, home, workspace); err != nil {
		return err
	}
	return nil
}

// environmentSection tells the agent the truth about where it is running. The
// rules it works under were written for a person at a terminal who can ask a
// question and open a screen; without this the agent spends its turns trying
// to do things this environment cannot do, or reports itself blocked on them.
//
// This describes the framework, not any destination, so it stays here rather
// than in configuration.
func environmentSection(agent worker.AgentConfig) string {
	lines := []string{
		"## この実行環境について",
		"あなたは自動実行の中にいます。人は見ていません。以下を踏まえてください。",
		"- 依頼者に今その場で質問することはできません。判断に迷ったら、",
		"  最小で確実な方を選び、迷った理由を最後の報告に書いてください。",
		"- 画面を開いて確かめることはできません。ビルドとテストは後段で自動的に実行されます。",
		"- コミット・PR・デプロイはしないでください。後段が行います。",
		"- あなたの変更は、この後べつのレビュアーが読んで判定します。通らなければやり直しになります。",
	}
	if agent.Knowledge.Library != nil {
		lines = append(lines,
			"- この作業に関係しそうな過去の判断が `"+agent.Knowledge.Library.To+"/` にあります。",
			"  索引から読んでください。ここは納品先のコードではないので、変更してはいけません。")
	}
	return strings.Join(lines, "\n")
}
