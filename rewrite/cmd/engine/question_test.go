package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
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
	cfg.Roles = []chain.Role{{Name: "ask_requester", Purpose: "Ask the requester the points only they can decide.",
		Processes: []chain.Process{{Name: "questioner", TrackerAccess: "comment",
			Command: []string{"/bin/sh", "-c", `cat > received.txt; printf 'asked\n' >> asked.txt; printf 'Posted one question and read it back.\n'`}}}}}
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
	if after, ok := questionBoundaryAt(t, root); !ok || after != 701 {
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
		if after, ok := questionBoundaryAt(t, root); !ok || after != 701 {
			t.Fatalf("%s: the question was recorded again at %d (present=%t)", reason, after, ok)
		}
		if starts, asked := log.starts(), askedTimes(t, root); starts != 1 || asked != 1 {
			t.Fatalf("%s: engine starts=%d questions asked=%d", reason, starts, asked)
		}
	}
	held("a comment below the boundary and another user's comment")
	mu.Lock()
	comments = append(comments, issueComment(702, 88, "別の人が上に書いた返事"))
	mu.Unlock()
	held("another user's comment above the boundary")
	finish()
	restarted := startStopQueue(t, cfg, root, 20*time.Millisecond, log)
	defer restarted()
	held("a collector restart while waiting")
	mu.Lock()
	comments = append(comments, issueComment(703, 55, requesterAnswer))
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
	if _, err := os.Stat(filepath.Join(root, "jobs", "51", "answer-703.json")); err != nil {
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
	comments = append(comments, issueComment(702, 55, "停止\nもう決めなくていい"))
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
			err := watchRequests(context.Background(), cfg, root, &lockedLog{})
			if err == nil || !strings.Contains(err.Error(), "intake.question_role") {
				t.Fatalf("unusable question role accepted: %v", err)
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatal("refused configuration created a queue")
			}
		})
	}
}
