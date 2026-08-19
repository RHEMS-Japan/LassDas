package main

import (
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The console's one write: answering an open question with lines the
// question itself printed. The server re-reads the question and refuses
// anything else, so the browser can never post free text through here -
// free-form answers are a separate engine contract change.

type answerRequest struct {
	QuestionCommentID string   `json:"question_comment_id"`
	Lines             []string `json:"lines"`
}

type answerResponse struct {
	PostedCommentID string `json:"posted_comment_id"`
}

func (s *consoleServer) handleAnswer(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if !issueKeyPattern.MatchString(key) {
		http.Error(w, "invalid ticket key", http.StatusBadRequest)
		return
	}
	// A hostile page can fire a cross-site POST without preflight as long
	// as it uses a form content type; it cannot set application/json (with
	// or without parameters) without the browser asking first, and this
	// server answers no preflight. Requiring the media type is the CSRF
	// defense; the fetch metadata check is its independent second layer
	// for browsers that send it (absence means a non-browser client, which
	// same-host binding already scopes to this machine).
	mediaType, _, typeErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if typeErr != nil || mediaType != "application/json" {
		http.Error(w, "answers travel as application/json", http.StatusUnsupportedMediaType)
		return
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		http.Error(w, "cross-site answer refused", http.StatusForbidden)
		return
	}
	var request answerRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024)).Decode(&request); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	questionCommentID, err := strconv.ParseInt(request.QuestionCommentID, 10, 64)
	if err != nil || questionCommentID <= 0 {
		http.Error(w, "invalid question comment id", http.StatusBadRequest)
		return
	}

	// The ledger is the authority on the question, not the comment stream:
	// which comment is the current question, that the run is still waiting,
	// who may answer, and until when. A question-shaped comment anyone else
	// posted - the engine's marker is printed in public comments and can be
	// copied - matches none of these and dies here.
	rows, err := s.scanState(r.Context())
	if err != nil {
		http.Error(w, "the state table could not be read", http.StatusBadGateway)
		return
	}
	ledger := answerContextFromRows(rows, key)
	if s.keyOwnerID == 0 || ledger.AnswererID == 0 || s.keyOwnerID != ledger.AnswererID {
		http.Error(w, "this tracker key's owner is not the ticket's allowed answerer", http.StatusForbidden)
		return
	}
	if ledger.State != "awaiting_answer" || ledger.QuestionCommentID == 0 {
		http.Error(w, "this ticket is not awaiting an answer", http.StatusConflict)
		return
	}
	if questionCommentID != ledger.QuestionCommentID {
		http.Error(w, "that comment is not the current question", http.StatusConflict)
		return
	}
	if ledger.AnswerDeadlineAt == 0 || time.Now().UnixMilli() >= ledger.AnswerDeadlineAt {
		http.Error(w, "the answer deadline has passed", http.StatusConflict)
		return
	}

	// The question body is re-read from the tracker, not trusted from the
	// browser: the engine's marker must close it, the lines must be
	// printed in it, and nothing may have closed the question since.
	comments, err := s.rawComments(r.Context(), key)
	if err != nil {
		http.Error(w, "tracker unavailable", http.StatusBadGateway)
		return
	}
	if gateErr := evaluateAnswerGate(comments, questionCommentID, request.Lines, ledger.AnswererID); gateErr != nil {
		http.Error(w, gateErr.Reason, gateErr.Status)
		return
	}

	posted, err := s.postComment(r.Context(), key, strings.Join(request.Lines, "\n"))
	if err != nil {
		http.Error(w, "the answer could not be posted", http.StatusBadGateway)
		return
	}
	writeJSON(w, answerResponse{PostedCommentID: posted})
}

// answerContext is what the ledger knows about a ticket's clarification:
// who the engine adopts answers from, whether the run is waiting, which
// comment carries the current question, and when it expires. Zeroes mean
// unknown, and unknown keeps answering off - the fail-closed direction.
type answerContext struct {
	AnswererID        int64
	State             string
	QuestionCommentID int64
	AnswerDeadlineAt  int64
}

// answerContextFromRows extracts the ticket's answer context from a state
// scan. Among multiple run rows for one ticket (stale generations), a row
// that is awaiting an answer wins; scan order settles nothing else.
func answerContextFromRows(rows []map[string]string, key string) answerContext {
	var result answerContext
	for _, row := range rows {
		if !strings.HasPrefix(row["pk"], "run#") || row["record_type"] != "run" || row["run_id"] != key {
			continue
		}
		candidate := answerContext{
			AnswererID: envelopeCreatorID(row["envelope_json"]),
			State:      row["state"],
		}
		candidate.QuestionCommentID, _ = strconv.ParseInt(row["question_comment_id"], 10, 64)
		candidate.AnswerDeadlineAt = questionDeadline(row["question_record_json"])
		if result.State != "awaiting_answer" || candidate.State == "awaiting_answer" {
			result = candidate
		}
	}
	return result
}

// questionDeadline extracts the sealed answer deadline from the run row's
// question record. Zero means unknown.
func questionDeadline(encoded string) int64 {
	var record struct {
		AnswerDeadlineAt int64 `json:"answer_deadline_at"`
	}
	if err := json.Unmarshal([]byte(encoded), &record); err != nil {
		return 0
	}
	return record.AnswerDeadlineAt
}

// envelopeCreatorID extracts the intake snapshot's creator. The engine only
// ever records runs whose creator is the configured allowed requester, so
// this is the allowed answerer for the ticket.
func envelopeCreatorID(encoded string) int64 {
	var envelope struct {
		Snapshot struct {
			CreatorID int64 `json:"creator_id"`
		} `json:"snapshot"`
	}
	if err := json.Unmarshal([]byte(encoded), &envelope); err != nil {
		return 0
	}
	return envelope.Snapshot.CreatorID
}

type rawComment struct {
	ID      int64
	UserID  int64
	Content string
}

// rawComments reads the full comment stream once, oldest first, with each
// comment's author - the answer gate tells the answerer's own comments apart
// from bystanders'.
func (s *consoleServer) rawComments(ctx context.Context, key string) ([]rawComment, error) {
	events := make([]rawComment, 0, 64)
	minID := int64(0)
	for {
		endpoint := fmt.Sprintf("https://%s/api/v2/issues/%s/comments?apiKey=%s&order=asc&count=100&minId=%d",
			s.config.TrackerDomain, key, url.QueryEscape(s.config.TrackerKey), minID)
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, fmt.Errorf("request could not be built")
		}
		response, err := s.client.Do(request)
		if err != nil {
			return nil, fmt.Errorf("tracker unreachable")
		}
		var page []struct {
			ID      int64  `json:"id"`
			Content string `json:"content"`
			User    struct {
				ID int64 `json:"id"`
			} `json:"createdUser"`
		}
		decodeErr := json.NewDecoder(response.Body).Decode(&page)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("tracker returned %d", response.StatusCode)
		}
		if decodeErr != nil {
			return nil, fmt.Errorf("tracker response unreadable")
		}
		previousMin := minID
		for _, comment := range page {
			minID = comment.ID
			events = append(events, rawComment{ID: comment.ID, UserID: comment.User.ID, Content: comment.Content})
		}
		if len(page) < 100 || minID == previousMin {
			return events, nil
		}
	}
}

func (s *consoleServer) postComment(ctx context.Context, key, content string) (string, error) {
	endpoint := fmt.Sprintf("https://%s/api/v2/issues/%s/comments?apiKey=%s",
		s.config.TrackerDomain, key, url.QueryEscape(s.config.TrackerKey))
	form := url.Values{"content": {content}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("request could not be built")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := s.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("tracker unreachable")
	}
	defer func() { _ = response.Body.Close() }()
	var posted struct {
		ID int64 `json:"id"`
	}
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tracker returned %d", response.StatusCode)
	}
	if err := json.NewDecoder(response.Body).Decode(&posted); err != nil || posted.ID == 0 {
		return "", fmt.Errorf("tracker response unreadable")
	}
	return strconv.FormatInt(posted.ID, 10), nil
}

// trackerKeyOwner asks the tracker who the configured key belongs to. The
// answer decides at startup whether the answering write is offered at all.
func trackerKeyOwner(client *http.Client, domain, key string) (int64, string, error) {
	endpoint := fmt.Sprintf("https://%s/api/v2/users/myself?apiKey=%s", domain, url.QueryEscape(key))
	response, err := client.Get(endpoint)
	if err != nil {
		return 0, "", fmt.Errorf("tracker unreachable")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return 0, "", fmt.Errorf("tracker returned %d", response.StatusCode)
	}
	var user struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(response.Body).Decode(&user); err != nil || user.ID == 0 {
		return 0, "", fmt.Errorf("tracker response unreadable")
	}
	return user.ID, user.Name, nil
}
