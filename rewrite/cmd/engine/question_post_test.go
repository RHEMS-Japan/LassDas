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
	if os.Getenv("QUESTION_SKIP_POST") == "1" {
		fmt.Println("The requested feature already exists; no comment posted.")
		return
	}
	if os.Getenv("QUESTION_FAIL_AFTER_POST") == "1" {
		asked, err := os.ReadFile("asked.txt")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(string(asked), "asked") > 1 {
			fmt.Println("The earlier attempt already posted the question.")
			return
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
	if os.Getenv("QUESTION_FAIL_AFTER_POST") == "1" {
		os.Exit(3)
	}
}

func TestQuestionWaitRequiresAnActualPost(t *testing.T) {
	for _, mode := range []string{"nothing-posted", "question-posted", "recover-posted", "tracker-unreadable"} {
		t.Run(mode, func(t *testing.T) {
			cfg := questionConfiguration(t)
			expectsPost := mode == "question-posted" || mode == "recover-posted"
			if !expectsPost {
				cfg.Roles[0].Processes[0].Env["QUESTION_SKIP_POST"] = "1"
			}
			if mode == "recover-posted" {
				cfg.Roles[0].Processes[0].Env["QUESTION_FAIL_AFTER_POST"] = "1"
			}
			root := t.TempDir()
			n := requestNotices(cfg, sourceIssue{ID: 51, Key: "EXAMPLE-51"}, filepath.Join(root, "jobs", "51"))
			if err := os.MkdirAll(n.directory, 0700); err != nil {
				t.Fatal(err)
			}
			if err := n.save(noticeLog{Notices: []noticeRecord{{Kind: "declare:ask_requester", Text: postedQuestion, CommentID: 703, Predates: true}}}); err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			blocked := mode == "tracker-unreadable"
			rows := []json.RawMessage{issueComment(700, 900, "The question role is starting."), issueComment(701, 900, "")}
			routedAfterQuestion := false
			betweenQuestions := false
			transport := http.DefaultTransport
			useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "watch-tracker.example" && r.URL.Host != "watch-model.example" {
					return transport.RoundTrip(r)
				}
				mu.Lock()
				defer mu.Unlock()
				if r.URL.Host == "watch-tracker.example" {
					if strings.HasSuffix(r.URL.Path, "/comments") {
						if r.Method == http.MethodPost {
							rows = append(rows, issueComment(702, 900, postedQuestion))
							return selectionReply(r, 201, rows[len(rows)-1]), nil
						}
						if blocked {
							state, err := loadWatchState(root, 51)
							if err == nil {
								for _, result := range state.History {
									if result.Role == "ask_requester" && result.Speaker != "runtime" && result.Error == "" {
										return selectionReply(r, 503, map[string]string{"message": "tracker temporarily unavailable"}), nil
									}
								}
							}
						}
						visible := append([]json.RawMessage{}, rows...)
						if askedTimes(t, root) > 0 {
							// A controller notice, another person's note and an empty
							// status change are not this role's question.
							visible = append(visible, issueComment(703, 900, postedQuestion), issueComment(704, 88, "An unrelated note."), issueComment(705, 900, ""))
						}
						if betweenQuestions {
							visible = append(visible, issueComment(710, 900, "Another role's report between the two question launches."))
						}
						return selectionReply(r, 200, visible), nil
					}
					if strings.HasSuffix(r.URL.Path, "/myself") {
						return selectionReply(r, 200, map[string]any{"id": 900}), nil
					}
					issue := watchedIssue(51, "Use the existing feature if present.", "2026-01-03T00:00:00Z")
					if strings.HasSuffix(r.URL.Path, "/issues") {
						return selectionReply(r, 200, []any{issue}), nil
					}
					return selectionReply(r, 200, issue), nil
				}
				var input struct{ State chain.State }
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					return nil, err
				}
				choice := "ask_requester"
				questions := 0
				for _, result := range input.State.History {
					if result.Role == "ask_requester" {
						choice = "done"
						routedAfterQuestion = true
						questions++
					}
				}
				if mode == "nothing-posted" && questions == 1 {
					betweenQuestions = true
					choice = "ask_requester"
				}
				if mode == "recover-posted" && input.State.Recovering {
					choice = "ask_requester"
					routedAfterQuestion = false
				}
				return selectionReply(r, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": choice}}}), nil
			})
			log := &lockedLog{}
			finish := startStopQueue(t, cfg, root, 20*time.Millisecond, log)
			if mode == "tracker-unreadable" {
				waitFor(t, func() bool {
					log.mu.Lock()
					defer log.mu.Unlock()
					return strings.Contains(log.text.String(), "work paused while stop instructions are unavailable")
				})
				mu.Lock()
				if routedAfterQuestion {
					t.Error("an unreadable tracker was taken to mean no question was posted")
				}
				blocked = false
				mu.Unlock()
			}
			waitFor(t, func() bool {
				state, err := loadWatchState(root, 51)
				if expectsPost {
					_, recorded := questionBoundaryAt(t, root)
					return err == nil && (state.Done || state.Waiting && recorded)
				}
				return err == nil && state.Done
			})
			finish()
			state, err := loadWatchState(root, 51)
			if err != nil {
				t.Fatal(err)
			}
			if expectsPost {
				if mode == "recover-posted" && askedTimes(t, root) != 2 {
					t.Fatal("the question was not recovered once")
				}
				if after, ok := questionBoundaryAt(t, root); !ok || after != 702 || routedAfterQuestion {
					t.Fatalf("posted question did not keep its boundary: %d %t routed=%t", after, ok, routedAfterQuestion)
				}
				// A successful POST followed by a read that omits that comment
				// is uncertainty, not proof that the role posted nothing.
				mu.Lock()
				rows = nil
				mu.Unlock()
				q := questionProcesses{cfg: cfg, key: "EXAMPLE-51", path: filepath.Join(root, "jobs", "51", "run", "question-post.json")}
				if _, err := q.posted(context.Background()); err == nil {
					t.Fatal("a stored POST missing from the next read was treated as no question")
				}
			} else {
				found := false
				for _, result := range state.History {
					found = found || result.Speaker == "runtime" && strings.Contains(result.Output, "質問役は質問を投稿しなかったので、そのまま進める")
				}
				if !state.Done || state.Waiting || !found || !routedAfterQuestion {
					t.Fatalf("questionless exit did not reach the next decision with its reason: %+v", state)
				}
				if _, ok := questionBoundaryAt(t, root); ok {
					t.Fatal("an invisible question was recorded")
				}
			}
		})
	}
}

func TestQuestionNoPostLimitSurvivesRestartUntilRequesterReplies(t *testing.T) {
	for _, mode := range []string{"free-default", "connected-one"} {
		t.Run(mode, func(t *testing.T) {
			cfg := questionConfiguration(t)
			root := t.TempDir()
			cfg.Roles[0].Processes[0].Directory = root
			cfg.Roles[0].Processes[0].Env["QUESTION_SKIP_POST"] = "1"
			cfg.Roles[0].Processes[0].Env["QUESTION_TEST_FILES"] = "0"
			cfg.Roles = append(cfg.Roles, chain.Role{Name: "work", Purpose: "Continue the accepted work.",
				Processes: []chain.Process{{Name: "worker", Directory: root, Command: []string{"/bin/sh", "-c", "cat > /dev/null; echo continued"}}}})
			limit := 2
			if mode == "connected-one" {
				limit = 1
				cfg.Intake.QuestionNoPostLimit = &limit
				cfg.Workflow = &chain.Workflow{Start: []string{"ask_requester"},
					After:   map[string][]string{"ask_requester": {"ask_requester", "work", "done"}, "work": {"ask_requester", "work", "done"}},
					Recover: map[string][]string{"ask_requester": {"ask_requester", "work"}, "work": {"work"}}}
			}
			directory := filepath.Join(root, "job")
			if err := os.MkdirAll(directory, 0700); err != nil {
				t.Fatal(err)
			}
			issue := sourceIssue{ID: 51, Key: "EXAMPLE-51"}
			n := requestNotices(cfg, issue, directory)
			if err := n.save(noticeLog{Notices: []noticeRecord{{Kind: "declare:work", Text: "Work is starting.", CommentID: 703}}}); err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(root, "operator.json")
			if err := os.WriteFile(configPath, configurationJSON(t, cfg), 0600); err != nil {
				t.Fatal(err)
			}
			rows := []json.RawMessage{issueComment(700, 55, "Earlier instruction.")}
			phase, calls, otherWork := 1, 0, 0
			var cancel context.CancelFunc
			useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Host == "watch-tracker.example" {
					if r.Method != http.MethodGet {
						t.Errorf("a questionless run posted to the tracker")
					}
					if strings.HasSuffix(r.URL.Path, "/comments") {
						return selectionReply(r, 200, rows), nil
					}
					if strings.HasSuffix(r.URL.Path, "/myself") {
						return selectionReply(r, 200, map[string]any{"id": 900}), nil
					}
					return selectionReply(r, 200, watchedIssue(51, "Deliver the requested result.", "2026-01-03T00:00:00Z")), nil
				}
				if r.URL.Host != "watch-model.example" {
					return nil, fmt.Errorf("unexpected fixture host")
				}
				var input struct {
					State     chain.State
					Questions map[string]struct{ Criteria map[string]string }
				}
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					return nil, err
				}
				calls++
				if calls > 12 {
					t.Error("the question role kept being offered without posting")
					cancel()
					return nil, context.Canceled
				}
				_, offered := input.Questions["next"].Criteria["ask_requester"]
				replied, askedAgain := false, false
				for _, result := range input.State.History {
					if result.Speaker == "requester" {
						replied = true
						if result.Output != requesterAnswer {
							t.Errorf("reply changed or an unrelated comment was accepted: %q", result.Output)
						}
					} else if replied && result.Role == "ask_requester" && result.Speaker != "runtime" {
						askedAgain = true
					}
				}
				choice := "ask_requester"
				switch {
				case askedAgain:
					choice = "done"
				case replied:
					if !offered || input.State.QuestionsWithoutPost != 0 {
						t.Error("new requester words did not make the question role available")
					}
				case phase == 2 || input.State.QuestionsWithoutPost >= limit:
					if offered || input.State.QuestionsWithoutPost != limit {
						t.Error("the limit was lost or the question role remained offered")
					}
					if otherWork == phase {
						cancel()
						return nil, context.Canceled
					}
					otherWork++
					choice = "work"
					if phase == 2 {
						rows = append(rows, issueComment(706, 55, requesterAnswer))
					}
				}
				return selectionReply(r, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": choice}}}), nil
			})
			for phase = 1; phase <= 2; phase++ {
				ctx, stop := context.WithTimeout(context.Background(), 15*time.Second)
				cancel = stop
				err := run(ctx, []string{"--config", configPath, "--issue", "EXAMPLE-51", "--run-dir", filepath.Join(directory, "run")}, io.Discard, io.Discard)
				stop()
				if phase == 1 && !errors.Is(err, context.Canceled) || phase == 2 && err != nil {
					t.Fatalf("phase %d: %v", phase, err)
				}
				state, err := savedHistory(directory)
				if err != nil {
					t.Fatal(err)
				}
				if state.Waiting || state.Done != (phase == 2) {
					t.Fatalf("questionless launches changed completion or waiting: %+v", state)
				}
				if phase == 1 {
					if state.QuestionUnavailable != "ask_requester" || state.QuestionReplyAfter != 700 {
						t.Fatalf("the unavailable question role was not saved: %+v", state)
					}
					rows = append(rows, issueComment(701, 55, " \n"), issueComment(702, 88, "unrelated"), issueComment(703, 55, "Work is starting."))
				}
			}
			if otherWork != 2 {
				t.Fatalf("other work did not continue before and after restart: %d", otherWork)
			}
		})
	}
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

func TestQuestionWithoutAReceiptDoesNotDiscardNewReplies(t *testing.T) {
	source := watchConfiguration(t).source()
	issue := sourceIssue{ID: 51}
	for _, receipt := range []string{`{"after":null}`, `{"after":-1}`, `not json`} {
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
