package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
	"automation.internal/ticket-ingress/internal/worker"
)

type consoleServer struct {
	config        consoleConfig
	dynamo        *dynamodb.Client
	local         *state.LocalStore
	runtimeConfig *runtime.Config
	workerConfig  *worker.Config
	hermes        *runtime.Hermes
	client        *http.Client
	// The tracker key's owner, read once at startup. Zero means unknown,
	// and unknown keeps the answering write switched off.
	keyOwnerID   int64
	keyOwnerName string
}

// sourceHealth names each canonical source's outcome for one response. A
// source that could not be read renders as its own error - never as empty
// success.
type sourceHealth struct {
	StateTable string `json:"state_table"`
	Tracker    string `json:"tracker"`
	Workflow   string `json:"workflow"`
}

type overviewTicket struct {
	IssueKey        string `json:"issue_key"`
	State           string `json:"state"`
	TerminalCode    string `json:"terminal_code,omitempty"`
	Attempt         int    `json:"attempt,omitempty"`
	InputRevision   int    `json:"input_revision,omitempty"`
	WorkflowRunID   string `json:"workflow_run_id,omitempty"`
	QueuedAt        string `json:"queued_at,omitempty"`
	ClaimedAt       string `json:"claimed_at,omitempty"`
	CompletedAt     string `json:"completed_at,omitempty"`
	OpenQuestion    bool   `json:"open_question"`
	NextActor       string `json:"next_actor"`
	TicketURL       string `json:"ticket_url"`
	WorkflowRunURL  string `json:"workflow_run_url,omitempty"`
	ClarificationNo int    `json:"clarifications,omitempty"`
}

type overviewResponse struct {
	GeneratedAt string           `json:"generated_at"`
	Sources     sourceHealth     `json:"sources"`
	Tickets     []overviewTicket `json:"tickets"`
	CursorID    string           `json:"ingest_cursor,omitempty"`
	PendingSlot bool             `json:"pending_slot"`
}

func (s *consoleServer) handleOverview(w http.ResponseWriter, r *http.Request) {
	response := overviewResponse{GeneratedAt: time.Now().UTC().Format(time.RFC3339), Sources: sourceHealth{Tracker: "not_consulted", Workflow: "not_consulted"}}
	rows, err := s.scanState(r.Context())
	if err != nil {
		response.Sources.StateTable = "unavailable: " + err.Error()
		writeJSON(w, response)
		return
	}
	response.Sources.StateTable = "ok"

	byRun := map[string]*overviewTicket{}
	clarifications := map[string]int{}
	for _, row := range rows {
		pk := row["pk"]
		switch {
		case strings.HasPrefix(pk, "ingest#"):
			response.CursorID = row["last_activity_id"]
		case strings.HasPrefix(pk, "pending#"):
			response.PendingSlot = true
		case strings.HasPrefix(pk, "run#") && row["record_type"] == "run":
			ticket := &overviewTicket{
				IssueKey:      row["run_id"],
				State:         row["state"],
				TerminalCode:  row["terminal_code"],
				WorkflowRunID: row["workflow_run_id"],
				QueuedAt:      millisToTime(row["queued_at"]),
				ClaimedAt:     millisToTime(row["claimed_at"]),
				CompletedAt:   millisToTime(row["terminal_completed_at"]),
				TicketURL:     "https://" + s.config.TrackerDomain + "/view/" + row["run_id"],
			}
			ticket.Attempt, _ = strconv.Atoi(row["run_attempt"])
			ticket.InputRevision, _ = strconv.Atoi(row["input_revision"])
			if ticket.WorkflowRunID != "" {
				ticket.WorkflowRunURL = "https://github.com/" + s.config.InstanceRepo + "/actions/runs/" + ticket.WorkflowRunID
			}
			byRun[basePK(pk)] = ticket
		case strings.Contains(pk, "#clarification#"):
			clarifications[basePK(pk)]++
		}
	}
	for base, ticket := range byRun {
		ticket.ClarificationNo = clarifications[base]
		ticket.NextActor, ticket.OpenQuestion = nextActor(*ticket)
		response.Tickets = append(response.Tickets, *ticket)
	}
	// Newest activity first: the operator's question is "what needs me
	// now", not "what happened two weeks ago".
	sort.Slice(response.Tickets, func(i, j int) bool {
		left, right := response.Tickets[i], response.Tickets[j]
		if left.QueuedAt != right.QueuedAt {
			return left.QueuedAt > right.QueuedAt
		}
		return left.IssueKey > right.IssueKey
	})
	writeJSON(w, response)
}

// nextActor states whose move it is - the single most useful column of the
// agreed list view.
func nextActor(t overviewTicket) (string, bool) {
	switch t.State {
	case "queued":
		return "自動処理 (取り込み待ち)", false
	case "claimed":
		return "自動処理 (実行中)", false
	case "awaiting_answer":
		return "起票者 (回答待ち)", true
	case "terminal":
		if t.TerminalCode == "success" {
			return "なし (完了)", false
		}
		return "運用者 (失敗の対処)", false
	}
	return "不明", false
}

type ticketEvent struct {
	Kind      string `json:"kind"`
	At        string `json:"at,omitempty"`
	CommentID string `json:"comment_id,omitempty"`
	Body      string `json:"body"`
	URL       string `json:"url,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	Code      string `json:"code,omitempty"`
}

type jobNode struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`
	URL        string `json:"url,omitempty"`
}

type ticketResponse struct {
	IssueKey    string          `json:"issue_key"`
	GeneratedAt string          `json:"generated_at"`
	Sources     sourceHealth    `json:"sources"`
	Current     *overviewTicket `json:"current,omitempty"`
	Timeline    []ticketEvent   `json:"timeline"`
	CurrentJobs []jobNode       `json:"current_jobs,omitempty"`
	// CanAnswer is true when the tracker key's owner is this ticket's
	// allowed answerer - the identity whose answers the engine adopts.
	CanAnswer bool `json:"can_answer"`
	// AnsweringReason explains an absent answer panel: "not_answerer" or
	// "key_owner_unknown". Empty when CanAnswer is true.
	AnsweringReason string `json:"answering_reason,omitempty"`
	// QuestionCommentID is the ledger's current question comment when the
	// run is awaiting an answer - the only comment the panel may bind to.
	QuestionCommentID string     `json:"question_comment_id,omitempty"`
	RunStatus         *runStatus `json:"run_status,omitempty"`
}

type runStatus struct {
	Now         string `json:"now"`
	SoFar       string `json:"so_far"`
	Next        string `json:"next"`
	Estimate    string `json:"estimate"`
	TerminalURL string `json:"terminal_url,omitempty"`
}

var issueKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*-[0-9]+$`)

func (s *consoleServer) handleTicket(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if !issueKeyPattern.MatchString(key) {
		http.Error(w, "invalid ticket key", http.StatusBadRequest)
		return
	}
	response := ticketResponse{IssueKey: key, GeneratedAt: time.Now().UTC().Format(time.RFC3339)}

	ledger := answerContext{}
	rows, err := s.scanState(r.Context())
	if err != nil {
		response.Sources.StateTable = "unavailable: " + err.Error()
	} else {
		response.Sources.StateTable = "ok"
		for _, row := range rows {
			if strings.HasPrefix(row["pk"], "run#") && row["record_type"] == "run" && row["run_id"] == key {
				ticket := overviewTicket{
					IssueKey: key, State: row["state"], TerminalCode: row["terminal_code"],
					WorkflowRunID: row["workflow_run_id"],
					QueuedAt:      millisToTime(row["queued_at"]), ClaimedAt: millisToTime(row["claimed_at"]),
					CompletedAt: millisToTime(row["terminal_completed_at"]),
					TicketURL:   "https://" + s.config.TrackerDomain + "/view/" + key,
				}
				ticket.Attempt, _ = strconv.Atoi(row["run_attempt"])
				if ticket.WorkflowRunID != "" {
					ticket.WorkflowRunURL = "https://github.com/" + s.config.InstanceRepo + "/actions/runs/" + ticket.WorkflowRunID
				}
				ticket.NextActor, ticket.OpenQuestion = nextActor(ticket)
				response.Current = &ticket
			}
		}
		ledger = answerContextFromRows(rows, key)
	}
	response.CanAnswer = s.keyOwnerID != 0 && ledger.AnswererID != 0 && s.keyOwnerID == ledger.AnswererID
	if !response.CanAnswer {
		if s.keyOwnerID == 0 {
			response.AnsweringReason = "key_owner_unknown"
		} else {
			response.AnsweringReason = "not_answerer"
		}
	}
	if ledger.State == "awaiting_answer" && ledger.QuestionCommentID > 0 {
		response.QuestionCommentID = strconv.FormatInt(ledger.QuestionCommentID, 10)
	}

	timeline, trackerErr := s.trackerTimeline(r.Context(), key, ledger.AnswererID)
	if trackerErr != nil {
		response.Sources.Tracker = "unavailable: " + trackerErr.Error()
	} else {
		response.Sources.Tracker = "ok"
		response.Timeline = timeline
	}

	response.Sources.Workflow = "not_consulted"
	if response.Current != nil && response.Current.WorkflowRunID != "" {
		jobs, workflowErr := s.workflowJobs(r.Context(), response.Current.WorkflowRunID)
		if workflowErr != nil {
			response.Sources.Workflow = "unavailable: " + workflowErr.Error()
		} else {
			response.Sources.Workflow = "ok"
			response.CurrentJobs = jobs
		}
	}
	if response.Current != nil && s.local != nil {
		response.RunStatus = s.localRunStatus(r.Context(), key, response.Current)
	}
	writeJSON(w, response)
}

func (s *consoleServer) localRunStatus(ctx context.Context, key string, current *overviewTicket) *runStatus {
	runs, err := s.local.ScanRuns(ctx)
	if err != nil {
		return &runStatus{Now: "不明 (ledger read failed)", SoFar: "不明", Next: "不明", Estimate: "不明"}
	}
	var run *state.RunOverview
	for i := range runs {
		if runs[i].IssueKey == key || runs[i].RunID == key {
			run = &runs[i]
			break
		}
	}
	if run == nil {
		return nil
	}
	stage, cycle, review, decision := "不明", 0, "不明", run.TerminalCode
	status := &runStatus{}
	if run.ClaimedAt > 0 {
		end := time.Now().UTC().UnixMilli()
		if run.CompletedAt > 0 {
			end = run.CompletedAt
		}
		status.Estimate = "残り時間は算出不能 (経過 " + time.Duration(end-run.ClaimedAt).Round(time.Second).String() + ")"
	}
	if s.hermes != nil {
		cards, err := s.hermes.ListBoardTasks(ctx)
		if err != nil {
			stage = "不明 (kanban read failed)"
		} else {
			stage, cycle = selectBoardStage(cards, run.DeliveryID)
		}
	}
	validatedCycle := 0
	if s.runtimeConfig != nil && s.workerConfig != nil && run.DeliveryID != "" {
		runDir := runtime.RunDirectory(s.runtimeConfig.Chain, run.DeliveryID)
		history := runDir + "/history"
		if summary, err := worker.LoadTrailSummary(history, *s.workerConfig, s.runtimeConfig.Identity.EngineSHA); err == nil {
			parts := make([]string, 0, len(summary.Cycles))
			for _, cycle := range summary.Cycles {
				reviews := make([]string, 0, len(cycle.Reviews))
				for _, item := range cycle.Reviews {
					reviews = append(reviews, fmt.Sprintf("%s:%s 指摘%d件 %s", item.Reviewer, item.Verdict, item.Findings, time.Duration(item.DurationMillis)*time.Millisecond))
				}
				parts = append(parts, fmt.Sprintf("%d周目 %s", cycle.Number, strings.Join(reviews, "/")))
			}
			review, decision = strings.Join(parts, " | "), summary.Decision
			if len(summary.Cycles) > 0 {
				validatedCycle = summary.Cycles[len(summary.Cycles)-1].Number
			}
		} else if stage == "terminal" || run.State == "terminal" {
			review = "不明 (sealed trail read failed)"
		}
		if run.State == "terminal" && run.TerminalCode == string(hook.TerminalSuccess) && validatedCycle > 0 {
			status.TerminalURL = sealedTerminalURL(filepath.Join(runDir, runner.ChainOutcomeFile), validatedCycle, s.runtimeConfig.Identity.Repository)
		}
	}
	if decision == "" {
		decision = "進行中"
	}
	if run.State == "terminal" {
		stage = "terminal"
	}
	status.Now = fmt.Sprintf("%s (%d周目)", stage, cycle)
	status.SoFar = review
	status.Next = nextRunStep(stage, decision)
	if status.TerminalURL != "" {
		status.Next = "完了: 最終結果を確認"
	}
	if status.Estimate == "" {
		status.Estimate = "未開始"
	}
	return status
}

func nextRunStep(stage, decision string) string {
	switch stage {
	case runtime.StageImplement:
		return "実装後にレビュー A へ進みます"
	case runtime.StageReviewA:
		return "レビュー A 通過後にレビュー B へ進みます"
	case runtime.StageReviewB:
		return "レビュー B 通過後に判定へ進みます"
	case runtime.StageValidate:
		return "検証通過後に納品へ進みます"
	case runtime.StagePublish:
		return "納品結果を封緘します"
	case "terminal":
		return "終了: " + decision
	default:
		return decision
	}
}

func selectBoardStage(cards []runtime.BoardTask, deliveryID string) (string, int) {
	order := map[string]int{runtime.StageImplement: 0, runtime.StageReviewA: 1, runtime.StageReviewB: 2, runtime.StageValidate: 3, runtime.StagePublish: 4}
	stage, round, rank := "不明", 0, 99
	for _, card := range cards {
		delivery, candidate, candidateRound, ok := runtime.ParseChainCardKey(card.IdempotencyKey)
		candidateRank, known := order[candidate]
		if !ok || !known || delivery != deliveryID || card.Status == "archived" || card.Status == "done" {
			continue
		}
		if candidateRound > round || candidateRound == round && candidateRank < rank {
			stage, round, rank = candidate, candidateRound, candidateRank
		}
	}
	return stage, round
}

func sealedTerminalURL(path string, validatedCycle int, repository string) string {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64*1024 {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var outcome runner.ChainOutcome
	if json.Unmarshal(raw, &outcome) != nil || outcome.Stage != validatedCycle {
		return ""
	}
	value := outcome.Evidence["pull_request_url"]
	parsed, err := url.Parse(value)
	expectedPrefix := "/" + repository + "/pull/"
	if err != nil || parsed.Scheme != "https" || parsed.Host != "github.com" ||
		!strings.HasPrefix(parsed.Path, expectedPrefix) || parsed.RawQuery != "" || parsed.Fragment != "" {
		return ""
	}
	pullID := strings.TrimPrefix(parsed.Path, expectedPrefix)
	if _, err := strconv.ParseInt(pullID, 10, 64); err != nil || strings.Contains(pullID, "/") {
		return ""
	}
	return value
}

// trackerTimeline reads every comment on the ticket and classifies the
// automation's own posts - reception, question, answer receipt, terminal
// report - plus the requester's answers, into one chronological story.
func (s *consoleServer) trackerTimeline(ctx context.Context, key string, answererID int64) ([]ticketEvent, error) {
	events := make([]ticketEvent, 0, 32)
	minID := int64(0)
	for {
		endpoint := fmt.Sprintf("https://%s/api/v2/issues/%s/comments?apiKey=%s&order=asc&count=100&minId=%d",
			s.config.TrackerDomain, key, url.QueryEscape(s.config.TrackerKey), minID)
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, fmt.Errorf("request could not be built")
		}
		httpResponse, err := s.client.Do(request)
		if err != nil {
			return nil, fmt.Errorf("tracker unreachable")
		}
		var comments []struct {
			ID      int64  `json:"id"`
			Content string `json:"content"`
			Created string `json:"created"`
			User    struct {
				ID int64 `json:"id"`
			} `json:"createdUser"`
		}
		decodeErr := json.NewDecoder(httpResponse.Body).Decode(&comments)
		_ = httpResponse.Body.Close()
		if httpResponse.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("tracker returned %d", httpResponse.StatusCode)
		}
		if decodeErr != nil {
			return nil, fmt.Errorf("tracker response unreadable")
		}
		previousMin := minID
		for _, comment := range comments {
			// The cursor advances on every comment, skipped or kept: a page
			// of content-less records once left it standing and this loop
			// re-fetched the same page forever - 700 tracker requests a
			// second from one open ticket view, measured in review.
			minID = comment.ID
			if comment.Content == "" {
				continue
			}
			event := classifyComment(comment.Content, comment.User.ID, answererID)
			event.CommentID = strconv.FormatInt(comment.ID, 10)
			event.At = comment.Created
			event.URL = "https://" + s.config.TrackerDomain + "/view/" + key + "#comment-" + event.CommentID
			events = append(events, event)
		}
		if len(comments) < 100 || minID == previousMin {
			break
		}
	}
	return events, nil
}

var runURLPattern = regexp.MustCompile(`/actions/runs/([0-9]+)`)

// classifyComment names each comment for the story. The automation's own
// posts identify themselves by the marker on their final line - the same
// anchor the engine trusts - so a marker-shaped or headline-shaped string
// inside anyone's prose stays prose. A human comment is an answer or cancel
// only when the ticket's allowed answerer wrote it: the engine ignores
// everyone else, and so does the story.
func classifyComment(content string, userID, answererID int64) ticketEvent {
	first := content
	if index := strings.IndexByte(first, '\n'); index >= 0 {
		first = first[:index]
	}
	event := ticketEvent{Kind: "note", Body: first}
	kind, qualifiers := markerParts(hook.ExtractCommentMarker(content))
	switch kind {
	case "terminal":
		event.Kind = "terminal"
		if len(qualifiers) > 0 {
			event.Code = qualifiers[0]
		}
		if match := runURLPattern.FindStringSubmatch(content); match != nil {
			event.RunID = match[1]
		}
	case "question":
		event.Kind = "question"
		event.Body = content
	case "answer-receipt":
		event.Kind = "answer_received"
	case "ack":
		event.Kind = "reception"
	case "":
		if answererID != 0 && userID == answererID && answerOrCancelCandidate(content) {
			if strings.HasPrefix(firstContentLine(content), "中止") {
				event.Kind = "cancel"
			} else {
				event.Kind = "answer"
			}
		}
	}
	return event
}

// workflowJobs reads the jobs of one workflow run, steps included, so the
// tree can show which stage the run is in without opening the workflow UI.
var workflowRunIDPattern = regexp.MustCompile(`^[0-9]{1,20}$`)

func (s *consoleServer) workflowJobs(ctx context.Context, runID string) ([]jobNode, error) {
	if s.config.GitHubToken == "" {
		return nil, fmt.Errorf("no workflow credential (set GH_TOKEN or log in with gh)")
	}
	if !workflowRunIDPattern.MatchString(runID) {
		return nil, fmt.Errorf("workflow run id is not numeric")
	}
	endpoint := "https://api.github.com/repos/" + s.config.InstanceRepo + "/actions/runs/" + runID + "/jobs?per_page=100"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("request could not be built")
	}
	request.Header.Set("Authorization", "Bearer "+s.config.GitHubToken)
	request.Header.Set("Accept", "application/vnd.github+json")
	httpResponse, err := s.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("workflow API unreachable")
	}
	defer func() { _ = httpResponse.Body.Close() }()
	if httpResponse.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("workflow API returned %d", httpResponse.StatusCode)
	}
	var payload struct {
		Jobs []struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			StartedAt  string `json:"started_at"`
			HTMLURL    string `json:"html_url"`
		} `json:"jobs"`
	}
	if err := json.NewDecoder(httpResponse.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("workflow response unreadable")
	}
	jobs := make([]jobNode, 0, len(payload.Jobs))
	for _, job := range payload.Jobs {
		jobs = append(jobs, jobNode{
			Name: job.Name, Status: job.Status, Conclusion: job.Conclusion,
			StartedAt: job.StartedAt, URL: job.HTMLURL,
		})
	}
	return jobs, nil
}

// scanState reads the whole state table. The table holds one row per live
// generation plus a handful of bookkeeping rows - a scan is the honest
// simple read at this scale.
func (s *consoleServer) scanState(ctx context.Context) ([]map[string]string, error) {
	if s.local != nil {
		runs, err := s.local.ScanRuns(ctx)
		if err != nil {
			return nil, err
		}
		rows := make([]map[string]string, 0, len(runs))
		for _, run := range runs {
			rows = append(rows, localRunRow(run))
		}
		return rows, nil
	}
	rows := make([]map[string]string, 0, 128)
	var start map[string]types.AttributeValue
	for {
		output, err := s.dynamo.Scan(ctx, &dynamodb.ScanInput{
			TableName:         aws.String(s.config.StateTable),
			ExclusiveStartKey: start,
		})
		if err != nil {
			return nil, fmt.Errorf("state table scan failed")
		}
		for _, item := range output.Items {
			row := make(map[string]string, len(item))
			for name, value := range item {
				switch typed := value.(type) {
				case *types.AttributeValueMemberS:
					row[name] = typed.Value
				case *types.AttributeValueMemberN:
					row[name] = typed.Value
				}
			}
			rows = append(rows, row)
		}
		if output.LastEvaluatedKey == nil {
			return rows, nil
		}
		start = output.LastEvaluatedKey
	}
}

func localRunRow(run state.RunOverview) map[string]string {
	runID := run.IssueKey
	if runID == "" {
		runID = run.RunID
	}
	row := map[string]string{
		"pk": run.Key, "record_type": "run", "run_id": runID, "delivery_id": run.DeliveryID,
		"state": run.State, "terminal_code": run.TerminalCode,
		"envelope_json": run.EnvelopeJSON, "question_record_json": run.QuestionRecordJSON,
	}
	if run.QuestionCommentID > 0 {
		row["question_comment_id"] = strconv.FormatInt(run.QuestionCommentID, 10)
	}
	if run.QueuedAt > 0 {
		row["queued_at"] = strconv.FormatInt(run.QueuedAt, 10)
	}
	if run.ClaimedAt > 0 {
		row["claimed_at"] = strconv.FormatInt(run.ClaimedAt, 10)
	}
	if run.CompletedAt > 0 {
		row["terminal_completed_at"] = strconv.FormatInt(run.CompletedAt, 10)
	}
	return row
}

func basePK(pk string) string {
	if index := strings.Index(pk, "#clarification#"); index >= 0 {
		return pk[:index]
	}
	if index := strings.Index(pk, "#comment#"); index >= 0 {
		return pk[:index]
	}
	return pk
}

func millisToTime(value string) string {
	millis, err := strconv.ParseInt(value, 10, 64)
	if err != nil || millis <= 0 {
		return ""
	}
	return time.UnixMilli(millis).UTC().Format(time.RFC3339)
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}
