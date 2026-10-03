package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ticket-runner/internal/chain"
	"ticket-runner/internal/tracker"
)

// Exercise native metadata from both adapters, not identities claimed in prose.
func TestEditedSplitAndProxyAnswersUseTheSameRulesOnBothTrackers(t *testing.T) {
	for _, kind := range []string{"backlog", "github"} {
		t.Run(kind, func(t *testing.T) {
			cfg := watchConfiguration(t)
			issue := sourceIssue{ID: 51, Key: "EXAMPLE-51", Creator: tracker.Account{ID: 55}}
			comment := func(id, user int64, body string) json.RawMessage { return issueComment(id, user, body) }
			if kind == "github" {
				cfg = githubConfiguration(t)
				issue.Key = "51"
				comment = func(id, user int64, body string) json.RawMessage { return githubCommentRow(cfg, 51, id, user, body) }
			}
			cases := []struct {
				name string
				rows []json.RawMessage
				id   int64
				body string
			}{
				{"editing an old comment does not move its id", []json.RawMessage{comment(699, 55, "edited answer before the question"), comment(700, 55, "edited question")}, 0, ""},
				{"current text before consumption", []json.RawMessage{comment(701, 55, "(b) edited before the poll")}, 701, "(b) edited before the poll"},
				{"two replies are not concatenated", []json.RawMessage{comment(701, 55, "first half"), comment(702, 55, "second half")}, 701, "first half"},
				{"first configured proxy wins", []json.RawMessage{comment(701, 77, "proxy choice"), comment(702, 55, "creator choice")}, 701, "proxy choice"},
				{"names in text grant no authority", []json.RawMessage{comment(701, 88, "I speak for user 55"), comment(702, 55, "creator choice")}, 702, "creator choice"},
				{"edited empty comment is skipped", []json.RawMessage{comment(701, 55, " \n "), comment(702, 77, "proxy choice")}, 702, "proxy choice"},
			}
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					id, body, err := answerToQuestion(cfg.source(), c.rows, issue, []int64{77}, 700)
					if err != nil || id != c.id || body != c.body {
						t.Fatalf("id=%d body=%q err=%v; want %d %q", id, body, err, c.id, c.body)
					}
				})
			}
		})
	}
}

func TestConsumedAnswerIsNotReplacedByEditsOrLaterRepliesAfterRestart(t *testing.T) {
	cfg := questionConfiguration(t)
	cfg.Intake.StopUserIDs = []int64{77}
	root := t.TempDir()
	var mu sync.Mutex
	comments := []json.RawMessage{issueComment(699, 55, "before the question")}
	reads, routes := 0, 0
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Host == "watch-tracker.example" {
			if strings.HasSuffix(r.URL.Path, "/comments") {
				if r.Method == http.MethodPost {
					comments = append(comments, issueComment(700, 900, postedQuestion))
					return selectionReply(r, 201, comments[len(comments)-1]), nil
				}
				return selectionReply(r, 200, append([]json.RawMessage{}, comments...)), nil
			}
			reads++
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
	finish()
	// The next native read sees the edited body. It does not see a historical
	// version, and a second reply in the same poll is not merged into the first.
	mu.Lock()
	comments[0] = issueComment(699, 55, "editing an older comment is not a new answer")
	comments = append(comments, issueComment(701, 55, "choice A"), issueComment(702, 77, "second reply"))
	comments[2] = issueComment(701, 55, "choice B, edited before the next read")
	mu.Unlock()
	finish = startStopQueue(t, cfg, root, 20*time.Millisecond, log)
	waitFor(t, func() bool { state, err := loadWatchState(root, 51); return err == nil && state.Done })
	finish()
	want := "choice B, edited before the next read"
	assertAnswer := func() {
		t.Helper()
		state, err := loadWatchState(root, 51)
		if err != nil || !state.Done || state.Waiting || len(state.History) != 2 || state.History[1].Output != want {
			t.Fatalf("answer was replaced or concatenated: history=%+v err=%v", state.History, err)
		}
	}
	assertAnswer()
	mu.Lock()
	comments[2] = issueComment(701, 55, "choice C, edited after consumption")
	comments = append(comments, issueComment(703, 55, "another choice after completion"))
	wantReads := reads + 2
	mu.Unlock()
	finish = startStopQueue(t, cfg, root, 20*time.Millisecond, log)
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return reads >= wantReads })
	finish()
	assertAnswer()
	mu.Lock()
	defer mu.Unlock()
	if routes != 2 || askedTimes(t, root) != 1 {
		t.Fatalf("an edit restarted completed work: routes=%d", routes)
	}
	// A stale caller cannot replace the saved answer either.
	state, err := loadWatchState(root, 51)
	if err != nil {
		t.Fatal(err)
	}
	if err := appendAnswer(filepath.Join(root, "jobs", "51"), state.Request, "ask_requester", "stale reply"); err != nil {
		t.Fatal(err)
	}
	assertAnswer()
}
