package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ticket-runner/internal/chain"
)

const requesterAnswer = "(a) release/ でお願いします。上限は 3 件までで。"

func issueComment(id, user int64, body string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"id": id, "issueId": 51, "projectId": 17,
		"createdUser": map[string]any{"id": user}, "content": body, "unknown": "keep this native metadata"})
	return raw
}

// The queue and its children write while the test reads the same log.
type lockedLog struct {
	mu   sync.Mutex
	text bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.text.Write(p)
}

func (l *lockedLog) starts() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.text.String(), "starting accepted request 51")
}

func questionConfiguration(t *testing.T) config {
	t.Helper()
	cfg := watchConfiguration(t)
	cfg.Intake.QuestionRole = "ask_requester"
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Roles = []chain.Role{{Name: "ask_requester", Purpose: "Ask the requester the points only they can decide.",
		Processes: []chain.Process{{Name: "questioner", TrackerAccess: "comment",
			Command: []string{binary, "-test.run=^TestQuestionPostWorker$"},
			Env:     map[string]string{"QUESTION_POST_WORKER": "1", "QUESTION_TEST_FILES": "1"}}}}}
	return cfg
}

func questionBoundaryAt(t *testing.T, root string) (int64, bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "jobs", "51", "question.json"))
	if err != nil {
		return 0, false
	}
	var boundary questionBoundary
	if err := json.Unmarshal(raw, &boundary); err != nil || boundary.After == nil {
		t.Fatalf("unreadable recorded question: %s %v", raw, err)
	}
	return *boundary.After, true
}

func askedTimes(t *testing.T, root string) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "jobs", "51", "workspace", "asked.txt"))
	if err != nil {
		return 0
	}
	return strings.Count(string(raw), "asked\n")
}

// A question waits for the person who filed the request, not for anyone whose
// comment happens to arrive. Nothing about the answer's wording is required;
// only its author and its position after the question are.
func TestWaitingRequestResumesOnlyOnTheRequestersLaterAnswer(t *testing.T) {
	cfg := questionConfiguration(t)
	root := t.TempDir()
	var mu sync.Mutex
	comments := []json.RawMessage{
		issueComment(700, 55, "起票のあとで自分が書いた連絡。これは質問への答えではない。"),
		issueComment(701, 88, "別の人の連絡"),
	}
	var routes int
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Host == "watch-tracker.example" {
			if strings.HasSuffix(r.URL.Path, "/comments") {
				if r.Method == http.MethodPost {
					comments = append(comments, issueComment(702, 900, postedQuestion))
					return selectionReply(r, 201, comments[len(comments)-1]), nil
				}
				return selectionReply(r, 200, append([]json.RawMessage{}, comments...)), nil
			}
			return selectionReply(r, 200, []any{watchedIssue(51, "Original conditions", "2026-01-03T00:00:00Z")}), nil
		}
		var input struct{ State chain.State }
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			return nil, err
		}
		routes++
		choice := "ask_requester"
		for _, result := range input.State.History {
			if result.Speaker == "requester" {
				choice = "done"
			}
		}
		return selectionReply(r, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": choice}}}), nil
	})
	log := &lockedLog{}
	finish := startStopQueue(t, cfg, root, 20*time.Millisecond, log)
	waitFor(t, func() bool {
		state, err := loadWatchState(root, 51)
		_, recorded := questionBoundaryAt(t, root)
		return err == nil && state.Waiting && recorded
	})
	if after, ok := questionBoundaryAt(t, root); !ok || after != 702 {
		t.Fatalf("recorded question boundary=%d present=%t", after, ok)
	}
	held := func(reason string) {
		t.Helper()
		// Three intervals of an unchanged boundary and one engine start show
		// the request was left alone rather than asked again.
		time.Sleep(60 * time.Millisecond)
		state, err := loadWatchState(root, 51)
		if err != nil || !state.Waiting || state.Done {
			t.Fatalf("%s: state=%+v err=%v", reason, state, err)
		}
		if after, ok := questionBoundaryAt(t, root); !ok || after != 702 {
			t.Fatalf("%s: the question was recorded again at %d (present=%t)", reason, after, ok)
		}
		if starts, asked := log.starts(), askedTimes(t, root); starts != 1 || asked != 1 {
			t.Fatalf("%s: engine starts=%d questions asked=%d", reason, starts, asked)
		}
	}
	held("a comment below the boundary and another user's comment")
	mu.Lock()
	comments = append(comments, issueComment(703, 88, "別の人が上に書いた返事"))
	mu.Unlock()
	held("another user's comment above the boundary")
	mu.Lock()
	comments = append(comments, issueComment(704, 55, ""))
	mu.Unlock()
	held("a status change by the requester, which the tracker records as a comment without words")
	finish()
	restarted := startStopQueue(t, cfg, root, 20*time.Millisecond, log)
	defer restarted()
	held("a collector restart while waiting")
	mu.Lock()
	comments = append(comments, issueComment(705, 55, requesterAnswer))
	mu.Unlock()
	waitFor(t, func() bool { state, err := loadWatchState(root, 51); return err == nil && state.Done })
	restarted()
	state, err := loadWatchState(root, 51)
	if err != nil || state.Waiting || len(state.History) != 2 {
		t.Fatalf("resumed state: %+v %v", state, err)
	}
	answer := state.History[1]
	if answer.Speaker != "requester" || answer.Role != "ask_requester" || answer.Output != requesterAnswer {
		t.Fatalf("the requester's own words did not reach the history unchanged: %+v", answer)
	}
	if starts, asked := log.starts(), askedTimes(t, root); starts != 2 || asked != 1 {
		t.Fatalf("engine starts=%d questions asked=%d", starts, asked)
	}
	if _, ok := questionBoundaryAt(t, root); ok {
		t.Fatal("the answered question is still open")
	}
	if _, err := os.Stat(filepath.Join(root, "jobs", "51", "answer-705.json")); err != nil {
		t.Fatal("the answered question was not kept", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if routes != 2 {
		t.Fatalf("decisions taken while waiting: %d", routes)
	}
}

// A stop is a stop even where an answer was expected. The waiting request is
// not carried on by it, and its text never becomes a reply in the history.
func TestAStopDuringAQuestionIsNotAnAnswer(t *testing.T) {
	cfg := questionConfiguration(t)
	root := t.TempDir()
	var mu sync.Mutex
	comments := []json.RawMessage{issueComment(701, 88, "別の人の連絡")}
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Host == "watch-tracker.example" {
			if strings.HasSuffix(r.URL.Path, "/comments") {
				if r.Method == http.MethodPost {
					comments = append(comments, issueComment(702, 900, postedQuestion))
					return selectionReply(r, 201, comments[len(comments)-1]), nil
				}
				return selectionReply(r, 200, append([]json.RawMessage{}, comments...)), nil
			}
			return selectionReply(r, 200, []any{watchedIssue(51, "Original conditions", "2026-01-03T00:00:00Z")}), nil
		}
		return selectionReply(r, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": "ask_requester"}}}), nil
	})
	log := &lockedLog{}
	finish := startStopQueue(t, cfg, root, 20*time.Millisecond, log)
	defer finish()
	waitFor(t, func() bool {
		state, err := loadWatchState(root, 51)
		_, recorded := questionBoundaryAt(t, root)
		return err == nil && state.Waiting && recorded
	})
	mu.Lock()
	comments = append(comments, issueComment(703, 55, "停止\nもう決めなくていい"))
	mu.Unlock()
	waitFor(t, func() bool {
		_, err := os.Stat(filepath.Join(root, "jobs", "51", "stop-request.json"))
		return err == nil
	})
	time.Sleep(60 * time.Millisecond)
	finish()
	state, err := loadWatchState(root, 51)
	if err != nil || !state.Waiting || state.Done || len(state.History) != 1 {
		t.Fatalf("a stop was read as an answer: %+v %v", state, err)
	}
	if _, ok := questionBoundaryAt(t, root); !ok {
		t.Fatal("the unanswered question was closed by a stop")
	}
	if asked := askedTimes(t, root); asked != 1 {
		t.Fatalf("questions asked=%d", asked)
	}
}

// A requester who writes the stop instruction while the runtime waits for
// their answer is told nothing about an answer, and the issue does not pass
// through the working status or the runtime's hands on its way to stopped.
// Before this, the queue treated the stop like an answer on the way to the
// stop machinery: it posted that the reply was received and the work went on,
// moved the issue back to processing and assigned it to the runtime, a moment
// before stopping it. The stop also does not wait for the model budget: it
// launches no model, and before this a short budget held it, told the
// requester the work was paused and would carry on, and repeated the moves
// above on every tick until the budget returned.
func TestAStopWhileWaitingIsNotAnnouncedAsAnAnswer(t *testing.T) {
	for name, shortBudget := range map[string]bool{"budget not limited": false, "budget below its floor": true} {
		t.Run(name, func(t *testing.T) { stopWhileWaiting(t, shortBudget) })
	}
}

func stopWhileWaiting(t *testing.T, shortBudget bool) {
	cfg := questionConfiguration(t)
	cfg.Intake.Announce = true
	cfg.Intake.Assign = true
	cfg.Intake.Statuses = &statusConfig{Processing: 1001, AwaitingRequester: 1002, Delivered: 3, Stopped: 1}
	var mu sync.Mutex
	remaining, shortReadings := "20", 0
	if shortBudget {
		cfg.Intake.MinModelCredit = 5
		server := creditServer(t, func() string {
			mu.Lock()
			defer mu.Unlock()
			if remaining == "3" {
				shortReadings++
			}
			return `{"data":{"limit":50,"usage":30,"limit_remaining":` + remaining + `,"limit_reset":"2026-10-01"}}`
		})
		cfg.Intake.ModelCreditURL, cfg.Intake.Client = server.URL, server.Client()
	}
	root := t.TempDir()
	comments := []json.RawMessage{}
	var posted, moves []string
	// The tracker lists comments by id, so every new one gets the next id.
	next := int64(800)
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Host != "watch-tracker.example" {
			return selectionReply(r, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": "ask_requester"}}}), nil
		}
		switch {
		case r.URL.Path == "/api/v2/users/myself":
			// The runtime's own account is neither the requester nor an operator.
			return selectionReply(r, 200, map[string]any{"id": 900}), nil
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			return selectionReply(r, 200, append([]json.RawMessage{}, comments...)), nil
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			if err := r.ParseForm(); err != nil {
				return nil, err
			}
			posted = append(posted, r.PostForm.Get("content"))
			next++
			comments = append(comments, issueComment(next, 900, r.PostForm.Get("content")))
			return selectionReply(r, 201, map[string]any{"id": next, "content": r.PostForm.Get("content")}), nil
		case r.Method == http.MethodPatch:
			if err := r.ParseForm(); err != nil {
				return nil, err
			}
			reply := map[string]any{"id": 51}
			if id := r.PostForm.Get("statusId"); id != "" {
				moves = append(moves, "status:"+id)
				n, _ := strconv.Atoi(id)
				reply["status"] = map[string]any{"id": n}
			}
			if id := r.PostForm.Get("assigneeId"); id != "" {
				moves = append(moves, "assignee:"+id)
				n, _ := strconv.Atoi(id)
				reply["assignee"] = map[string]any{"id": n}
			}
			return selectionReply(r, 200, reply), nil
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues"):
			return selectionReply(r, 200, []any{watchedIssue(51, "Original conditions", "2026-01-03T00:00:00Z")}), nil
		}
		return nil, http.ErrNotSupported
	})
	log := &lockedLog{}
	finish := startStopQueue(t, cfg, root, 20*time.Millisecond, log)
	moved := func(move string) bool {
		mu.Lock()
		defer mu.Unlock()
		return len(moves) > 0 && moves[len(moves)-1] == move
	}
	// The question was put, and the issue is in the requester's hands.
	waitFor(t, func() bool {
		state, err := loadWatchState(root, 51)
		_, recorded := questionBoundaryAt(t, root)
		return err == nil && state.Waiting && recorded && moved("assignee:55")
	})
	if shortBudget {
		// The budget is short before the stop is written, and the queue
		// has read it so on ticks of its own.
		mu.Lock()
		remaining = "3"
		mu.Unlock()
		waitFor(t, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return shortReadings >= 2
		})
	}
	mu.Lock()
	next++
	comments = append(comments, issueComment(next, 55, "停止"))
	mu.Unlock()
	waitFor(t, func() bool { return moved("status:1") })
	// The budget returns: a stopped request is not told its work resumed.
	mu.Lock()
	remaining = "20"
	mu.Unlock()
	time.Sleep(100 * time.Millisecond) // several more ticks
	finish()
	mu.Lock()
	if got := strings.Join(posted, " | "); got != acceptedNoticeText(0, "")+" | "+postedQuestion {
		t.Fatalf("the requester was told: %s", got)
	}
	if got := strings.Join(moves, ","); got != "status:1001,assignee:900,status:1002,assignee:55,status:1" {
		t.Fatalf("the issue moved %s; a stop while waiting goes from awaiting straight to stopped and stays with the requester", got)
	}
	mu.Unlock()
	if asked := askedTimes(t, root); asked != 1 {
		t.Fatalf("questions asked=%d", asked)
	}
}

// The setting names an existing role that can actually reach the requester.
// A missing or read-only role is refused before any work is accepted.
func TestQuestionRoleIsRefusedBeforeIntakeUnlessItCanComment(t *testing.T) {
	for _, name := range []string{"no_such_role", "elicit", "done"} {
		t.Run(name, func(t *testing.T) {
			cfg := questionConfiguration(t)
			cfg.Roles = append(cfg.Roles, chain.Role{Name: "elicit", Purpose: "Settle the requirements.",
				Processes: []chain.Process{{Name: "requirements", TrackerAccess: "read", Command: []string{"/bin/sh", "-c", "cat > /dev/null"}}}})
			cfg.Intake.QuestionRole = name
			useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
				t.Error("intake started with an unusable question role")
				return nil, fmt.Errorf("unconfigured")
			})
			root := filepath.Join(t.TempDir(), "must-not-be-created")
			// The refusal comes before intake, so a configuration that is
			// accepted here starts polling and is reported as the timeout.
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := watchRequests(ctx, cfg, root, &lockedLog{})
			if err == nil || !strings.Contains(err.Error(), "intake.question_role") {
				t.Fatalf("unusable question role accepted: %v", err)
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatal("refused configuration created a queue")
			}
		})
	}
}

func TestQuestionNoPostLimitNeedsAPositiveSettingAndAQuestionRole(t *testing.T) {
	for _, limit := range []int{-1, 0, 1, 3} {
		cfg := questionConfiguration(t)
		cfg.Intake.QuestionNoPostLimit = &limit
		if err := validateQuestionRole(cfg); (err == nil) != (limit > 0) {
			t.Fatalf("limit %d: %v", limit, err)
		}
		cfg.Intake.QuestionRole = ""
		if err := validateQuestionRole(cfg); err == nil {
			t.Fatal("a limit without a question role was accepted")
		}
	}
}

// The tracker records a status or field change as a comment with no words,
// and the runtime makes such changes itself while it waits. Words from the
// creator or an operator are the answer; a stop line is not.
func TestAnswerToQuestionTakesOnlyWords(t *testing.T) {
	source := watchConfiguration(t).source()
	issue := sourceIssue{ID: 51}
	issue.Creator.ID = 55
	rows := []json.RawMessage{
		issueComment(700, 55, "below the boundary"),
		issueComment(701, 55, ""),
		issueComment(702, 55, "  \n\t"),
		issueComment(703, 88, "someone else's words"),
		issueComment(704, 55, "停止\nnot an answer"),
	}
	if id, answer, err := answerToQuestion(source, rows, issue, []int64{90}, 700); err != nil || id != 0 || answer != "" {
		t.Fatalf("a comment without words, a stranger's or a stop line was read as an answer: id=%d %q err=%v", id, answer, err)
	}
	rows = append(rows, issueComment(705, 90, "an operator's reply"), issueComment(706, 55, requesterAnswer))
	if id, answer, err := answerToQuestion(source, rows, issue, []int64{90}, 700); err != nil || id != 705 || answer != "an operator's reply" {
		t.Fatalf("operator's reply: id=%d %q err=%v", id, answer, err)
	}
	if id, answer, err := answerToQuestion(source, rows, issue, []int64{90}, 705); err != nil || id != 706 || answer != requesterAnswer {
		t.Fatalf("creator's reply: id=%d %q err=%v", id, answer, err)
	}
}

// A list holding a comment whose record places it on another issue or in
// another project is not read for a question's boundary or its answer at
// all, as it is not read for a stop.
func TestAListWithACommentPlacedElsewhereIsNotReadForTheQuestion(t *testing.T) {
	source := watchConfiguration(t).source()
	issue := sourceIssue{ID: 51}
	issue.Creator.ID = 55
	for name, elsewhere := range map[string]json.RawMessage{
		"another issue":   []byte(`{"id":702,"issueId":52,"projectId":17,"createdUser":{"id":55},"content":"words for another issue"}`),
		"another project": []byte(`{"id":702,"issueId":51,"projectId":99,"createdUser":{"id":55},"content":"words in another project"}`),
	} {
		rows := []json.RawMessage{issueComment(701, 88, "someone else's words"), elsewhere, issueComment(703, 55, requesterAnswer)}
		if highest, err := latestComment(source, rows, issue); err == nil {
			t.Errorf("%s: the question's boundary was read as %d", name, highest)
		}
		if id, answer, err := answerToQuestion(source, rows, issue, nil, 0); err == nil {
			t.Errorf("%s: an answer was read: id=%d %q", name, id, answer)
		}
	}
}
