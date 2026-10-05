package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ticket-runner/internal/chain"
	"ticket-runner/internal/tracker"
)

const postedQuestion = "質問: (a) この範囲で進める (b) 別の範囲にする\r\n"

func TestQuestionPostWorker(t *testing.T) {
	if os.Getenv("QUESTION_POST_WORKER") != "1" {
		return
	}
	if os.Getenv("QUESTION_TEST_FILES") == "1" {
		input, err := io.ReadAll(os.Stdin)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile("received.txt", input, 0600); err != nil {
			t.Fatal(err)
		}
		file, err := os.OpenFile("asked.txt", os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprintln(file, "asked"); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	client, err := tracker.CertificateClient(os.Getenv("TASK_TRACKER_CERT"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	source := tracker.Backlog{BaseURL: os.Getenv("TASK_TRACKER_URL"), KeyEnv: "TASK_TRACKER_KEY", Client: client}
	if _, err := source.AddComment(context.Background(), os.Getenv("TASK_TRACKER_ISSUE"), postedQuestion); err != nil {
		t.Fatal(err)
	}
	fmt.Println("The question was posted.")
}

// The asking process really posts through its scoped client. The tracker and
// model are fixtures. Replies arrive before the watch records its boundary,
// including across a restart and among actual controller notice submissions.
func TestRepliesBetweenQuestionPostAndNextWatchReadAreNotLost(t *testing.T) {
	for _, kind := range []string{"backlog", "github"} {
		for _, grant := range []string{"comment", "every-comment"} {
			for _, mode := range []string{"immediate", "restart", "pause-and-restore", "pending-notice", "notices-only", "next-question"} {
				t.Run(kind+"/"+grant+"/"+mode, func(t *testing.T) {
					cfg := watchConfiguration(t)
					key := "EXAMPLE-51"
					if kind == "github" {
						cfg = githubConfiguration(t)
						key = "51"
					}
					cfg.Intake.QuestionRole = "ask"
					binary, err := os.Executable()
					if err != nil {
						t.Fatal(err)
					}
					cfg.Roles = []chain.Role{{Name: "ask", Processes: []chain.Process{{Name: "worker", TrackerAccess: grant,
						Command: []string{binary, "-test.run=^TestQuestionPostWorker$"}, Env: map[string]string{"QUESTION_POST_WORKER": "1"}}}}}
					issue := sourceIssue{ID: 51, Key: key, Creator: tracker.Account{ID: 55}}
					var mu sync.Mutex
					var rows []json.RawMessage
					next := int64(899)
					comment := func(id int64, body string) json.RawMessage {
						if kind == "github" {
							return githubCommentRow(cfg, 51, id, 55, body)
						}
						return issueComment(id, 55, body)
					}
					transport := http.DefaultTransport
					useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
						if r.URL.Host != "watch-tracker.example" && r.URL.Host != "github-tracker.example" && r.URL.Host != "watch-model.example" {
							return transport.RoundTrip(r)
						}
						mu.Lock()
						defer mu.Unlock()
						if r.URL.Host == "watch-model.example" {
							return selectionReply(r, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": "ask"}}}), nil
						}
						if strings.HasSuffix(r.URL.Path, "/comments") {
							if r.Method == http.MethodPost {
								var body string
								if kind == "github" {
									var payload map[string]string
									if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
										return nil, err
									}
									body = payload["body"]
								} else {
									if err := r.ParseForm(); err != nil {
										return nil, err
									}
									body = r.PostForm.Get("content")
								}
								next++
								row := comment(next, body)
								rows = append(rows, row)
								return selectionReply(r, 201, row), nil
							}
							return selectionReply(r, 200, append([]json.RawMessage{}, rows...)), nil
						}
						if kind == "github" {
							return selectionReply(r, 200, githubIssueRow(cfg, 51, []string{"automation"})), nil
						}
						return selectionReply(r, 200, watchedIssue(51, "Original conditions", "2026-01-03T00:00:00Z")), nil
					})
					root := t.TempDir()
					directory := filepath.Join(root, "jobs", "51")
					configPath := filepath.Join(root, "operator.json")
					if err := os.WriteFile(configPath, configurationJSON(t, cfg), 0600); err != nil {
						t.Fatal(err)
					}
					ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
					defer cancel()
					err = run(ctx, []string{"--config", configPath, "--issue", key, "--run-dir", filepath.Join(directory, "run")}, io.Discard, io.Discard)
					if !errors.Is(err, chain.ErrWaiting) {
						t.Fatalf("question did not return waiting: %v", err)
					}
					n := requestNotices(cfg, issue, directory)
					if mode == "pause-and-restore" || mode == "notices-only" || mode == "pending-notice" {
						if err := n.post(ctx, pausedNotice, pausedNoticeText, time.Now().UTC()); err != nil {
							t.Fatal(err)
						}
					}
					answerID := int64(0)
					if mode != "notices-only" {
						mu.Lock()
						next++
						answerID = next
						rows = append(rows, comment(answerID, requesterAnswer))
						mu.Unlock()
					}
					if mode == "pause-and-restore" || mode == "notices-only" || mode == "pending-notice" {
						if err := n.post(ctx, restoredNotice, restoredNoticeText, time.Now().UTC()); err != nil {
							t.Fatal(err)
						}
					}
					if mode == "pending-notice" {
						log, err := n.load()
						if err != nil {
							t.Fatal(err)
						}
						// The POST arrived before the controller retained its receipt.
						log.Notices[0].PostedAt, log.Notices[0].CommentID = nil, 0
						if err := n.save(log); err != nil {
							t.Fatal(err)
						}
					}
					if mode != "restart" {
						if err := recordQuestion(cfg.source(), directory, rows, issue); err != nil {
							t.Fatal(err)
						}
					}
					state, err := savedHistory(directory)
					if err != nil {
						t.Fatal(err)
					}
					resumed, answered, err := resumeWaitingRequest(ctx, cfg, issue, directory, state.Request, state, time.Second)
					if err != nil {
						t.Fatal(err)
					}
					if mode == "notices-only" {
						if resumed || answered {
							t.Fatal("a controller notice was read as the requester's reply")
						}
						return
					}
					if !resumed || !answered {
						t.Fatalf("answer %d was lost before the next read: resume=%t answered=%t", answerID, resumed, answered)
					}
					state, err = savedHistory(directory)
					if err != nil || state.Waiting || len(state.History) != 2 || state.History[1].Output != requesterAnswer {
						t.Fatalf("the original answer was not preserved: %+v %v", state, err)
					}
					if _, err := os.Stat(filepath.Join(directory, "run", "question-post.json")); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("the answered question's receipt could be reused by a later role that posts nothing: %v", err)
					}
					if mode == "next-question" {
						if err := run(ctx, []string{"--config", configPath, "--issue", key, "--run-dir", filepath.Join(directory, "run")}, io.Discard, io.Discard); !errors.Is(err, chain.ErrWaiting) {
							t.Fatalf("second question: %v", err)
						}
						state, err = savedHistory(directory)
						if err != nil {
							t.Fatal(err)
						}
						resumed, answered, err = resumeWaitingRequest(ctx, cfg, issue, directory, state.Request, state, time.Second)
						if err != nil || resumed || answered {
							t.Fatalf("first answer reused for second question: %t %t %v", resumed, answered, err)
						}
						mu.Lock()
						next++
						rows = append(rows, comment(next, "second answer"))
						mu.Unlock()
						resumed, answered, err = resumeWaitingRequest(ctx, cfg, issue, directory, state.Request, state, time.Second)
						if err != nil || !resumed || !answered {
							t.Fatalf("second answer lost: %t %t %v", resumed, answered, err)
						}
					}
				})
			}
		}
	}
}

func TestQuestionWithoutAReceiptRecordsTheLatestComment(t *testing.T) {
	source := watchConfiguration(t).source()
	issue := sourceIssue{ID: 51}
	for _, receipt := range []string{"", `{"after":null}`} {
		directory := t.TempDir()
		if receipt != "" {
			if err := os.Mkdir(filepath.Join(directory, "run"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, "run", "question-post.json"), []byte(receipt), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := recordQuestion(source, directory, []json.RawMessage{issueComment(901, 55, "earlier comment")}, issue); err != nil {
			t.Fatalf("absent receipt %q prevents waiting: %v", receipt, err)
		}
		raw, err := os.ReadFile(filepath.Join(directory, "question.json"))
		var boundary questionBoundary
		if err != nil || json.Unmarshal(raw, &boundary) != nil || boundary.After == nil || *boundary.After != 901 {
			t.Fatalf("latest comment not recorded: %s %v", raw, err)
		}
	}
}

func TestQuestionWithAnUnreadableReceiptDoesNotInventABoundary(t *testing.T) {
	source := watchConfiguration(t).source()
	issue := sourceIssue{ID: 51}
	for _, receipt := range []string{`{"after":-1}`, `not json`} {
		directory := t.TempDir()
		if err := os.Mkdir(filepath.Join(directory, "run"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "run", "question-post.json"), []byte(receipt), 0600); err != nil {
			t.Fatal(err)
		}
		if err := recordQuestion(source, directory, []json.RawMessage{issueComment(901, 55, requesterAnswer)}, issue); err == nil {
			t.Fatalf("missing receipt discarded an answer: %q", receipt)
		}
		if _, err := os.Stat(filepath.Join(directory, "question.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("invented a boundary: %v", err)
		}
	}
}

func TestQuestionRoleWithoutAPostReceiptResumesOnALaterReply(t *testing.T) {
	cfg := questionConfiguration(t)
	cfg.Roles[0].Processes[0].Command = []string{"/bin/sh", "-c", "exit 0"}
	cfg.Roles[0].Processes[0].Env = nil
	root := t.TempDir()
	var mu sync.Mutex
	comments := []json.RawMessage{issueComment(700, 55, "earlier note")}
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Host == "watch-tracker.example" {
			if strings.HasSuffix(r.URL.Path, "/comments") {
				if r.Method != http.MethodGet {
					t.Error("a question was posted again instead of waiting")
				}
				return selectionReply(r, 200, append([]json.RawMessage{}, comments...)), nil
			}
			return selectionReply(r, 200, []any{watchedIssue(51, "Original conditions", "2026-01-03T00:00:00Z")}), nil
		}
		var input struct{ State chain.State }
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			return nil, err
		}
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
	defer finish()
	waitFor(t, func() bool {
		state, err := loadWatchState(root, 51)
		after, recorded := questionBoundaryAt(t, root)
		return err == nil && state.Waiting && recorded && after == 700
	})
	mu.Lock()
	comments = append(comments, issueComment(701, 55, requesterAnswer))
	mu.Unlock()
	waitFor(t, func() bool { state, err := loadWatchState(root, 51); return err == nil && state.Done })
	finish()
	state, err := loadWatchState(root, 51)
	if err != nil || state.Waiting || len(state.History) != 2 || state.History[1].Speaker != "requester" || state.History[1].Output != requesterAnswer || log.starts() != 2 {
		t.Fatalf("reply did not resume the same request once: %+v starts=%d err=%v", state, log.starts(), err)
	}
	t.Logf("done=%t requester_reply=%q starts=%d", state.Done, state.History[1].Output, log.starts())
}
