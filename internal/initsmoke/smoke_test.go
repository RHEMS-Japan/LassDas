package initsmoke

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/initwizard"
	runtimeconfig "automation.internal/ticket-ingress/internal/runtime"
)

type testUI struct {
	key           string
	messages      []string
	personalReads int
}

func (u *testUI) Ask(id, _, value string, secret bool) (string, error) {
	if id == "smoke-personal-key" {
		if !secret {
			panic("personal key echoed")
		}
		u.personalReads++
		return u.key, nil
	}
	return value, nil
}
func (*testUI) Confirm(string) (bool, error) { return true, nil }
func (u *testUI) Info(value string)          { u.messages = append(u.messages, value) }

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func reply(value any) *http.Response {
	raw, _ := json.Marshal(value)
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(raw))), Header: make(http.Header)}
}

type observerFunc func(context.Context, *initwizard.State, Record, initwizard.Secrets) (Observation, error)

func (f observerFunc) Observe(ctx context.Context, s *initwizard.State, r Record, secrets initwizard.Secrets) (Observation, error) {
	return f(ctx, s, r, secrets)
}

func smokeState() (*initwizard.State, Record, initwizard.Secrets) {
	s := &initwizard.State{Version: 1, Project: "example", Repository: "example/cli", RepositoryID: 1, Branch: "develop", BaseSHA: strings.Repeat("a", 40), Tracker: runtimeconfig.TrackerConfig{Origin: "https://example.backlog.com", SpaceKey: "example", ProjectID: 1, ProjectKey: "EXAMPLE", AllowedCreatorID: 7, RequiredCategoryID: 9}}
	record := Record{Correlation: strings.Repeat("b", 32), ProjectID: 1, CreatorID: 7, Repository: s.Repository, RepositoryID: 1, Branch: s.Branch, BaseSHA: s.BaseSHA, Path: "README.md", BeforeSHA256: digest("before\n"), After: "before\ncheck\n", Addition: "check\n", Summary: "動作確認", Description: "Add the check line.\n" + marker(strings.Repeat("b", 32))}
	s.Smoke, _ = json.Marshal(record)
	return s, record, initwizard.Secrets{"BACKLOG_API_KEY": "artificial-bot", "TARGET_GITHUB_TOKEN": "artificial-target"}
}

func TestPersonalMismatchStopsBeforePostAndDoesNotPersistKey(t *testing.T) {
	s, _, secrets := smokeState()
	ui := &testUI{key: "artificial-personal"}
	calls := 0
	r := Runner{UI: ui, Observer: observerFunc(func(context.Context, *initwizard.State, Record, initwizard.Secrets) (Observation, error) {
		t.Fatal("observer ran before authorized creation")
		return Observation{}, nil
	}), API: initwizard.API{HTTP: &http.Client{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.Path != "/api/v2/users/myself" || req.Method != "GET" {
			t.Fatal("mutation before identity check")
		}
		return reply(initwizard.NamedID{ID: 8}), nil
	})}}}
	save := func() error {
		raw, _ := json.Marshal(s)
		if strings.Contains(string(raw), ui.key) {
			t.Fatal("personal key persisted")
		}
		return nil
	}
	if err := r.Run(context.Background(), s, secrets, save); err == nil || !strings.Contains(err.Error(), "一致しません") {
		t.Fatalf("mismatch = %v", err)
	}
	if calls != 1 || s.Tracker.AllowedCreatorID != 7 || ui.personalReads != 1 {
		t.Fatal("identity check changed admission")
	}
}

func TestLostPostResponseResumesSameIssueWithoutSecondPost(t *testing.T) {
	s, record, secrets := smokeState()
	ui := &testUI{key: "artificial-personal"}
	posts := 0
	searches := 0
	issue := trackerIssue{ID: 42, Key: "EXAMPLE-42", Summary: record.Summary, Description: record.Description, ProjectID: 1}
	issue.CreatedUser.ID = 7
	r := Runner{UI: ui, WaitTimeout: time.Millisecond, PollInterval: time.Millisecond, Observer: observerFunc(func(_ context.Context, _ *initwizard.State, got Record, _ initwizard.Secrets) (Observation, error) {
		if got.IssueID != 42 {
			t.Fatal("wrong issue followed")
		}
		return Observation{DeliveryID: "delivery_" + strings.Repeat("a", 32), Step: "done", Terminal: "success", PRURL: "https://github.com/example/cli/pull/1", Verified: true}, nil
	})}
	r.API = initwizard.API{HTTP: &http.Client{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/api/v2/users/myself":
			return reply(initwizard.NamedID{ID: 7}), nil
		case "/api/v2/projects/1/issueTypes", "/api/v2/priorities":
			return reply([]initwizard.NamedID{{ID: 1, Name: "既存"}}), nil
		case "/api/v2/issues":
			if req.Method == "POST" {
				posts++
				var saved Record
				_ = json.Unmarshal(s.Smoke, &saved)
				if !saved.Attempted || saved.Description != record.Description {
					t.Fatal("POST before durable intent")
				}
				if req.URL.Query().Get("apiKey") != ui.key {
					t.Fatal("bot posted smoke")
				}
				return nil, errors.New("lost response " + ui.key)
			}
			searches++
			q := req.URL.Query()
			if q.Get("projectId[]") != "1" || q.Get("createdUserId[]") != "7" || q.Get("keyword") != marker(record.Correlation) || q.Get("apiKey") != secrets["BACKLOG_API_KEY"] {
				t.Fatal("reconciliation did not bind project/creator/correlation")
			}
			return reply([]trackerIssue{issue}), nil
		}
		t.Fatalf("unexpected operation %s", req.URL.Path)
		return nil, nil
	})}}
	save := func() error {
		raw, _ := json.Marshal(s)
		if strings.Contains(string(raw), ui.key) {
			t.Fatal("personal key persisted")
		}
		return nil
	}
	if err := r.Run(context.Background(), s, secrets, save); err == nil || strings.Contains(err.Error(), ui.key) {
		t.Fatalf("lost POST = %v", err)
	}
	if err := r.Run(context.Background(), s, secrets, save); err != nil {
		t.Fatal(err)
	}
	var completed Record
	_ = json.Unmarshal(s.Smoke, &completed)
	if posts != 1 || searches != 1 || ui.personalReads != 1 || completed.IssueID != 42 || completed.PRURL == "" || completed.VerifiedAt.IsZero() {
		t.Fatal("resume did not preserve one request")
	}
	for _, message := range ui.messages {
		if strings.Contains(message, ui.key) {
			t.Fatal("key in output")
		}
	}
}

func TestUnresolvedPostNeverCreatesAnotherIssue(t *testing.T) {
	s, record, secrets := smokeState()
	record.Attempted = true
	s.Smoke, _ = json.Marshal(record)
	r := Runner{UI: &testUI{}, Observer: observerFunc(func(context.Context, *initwizard.State, Record, initwizard.Secrets) (Observation, error) {
		t.Fatal("unresolved request followed")
		return Observation{}, nil
	}), API: initwizard.API{HTTP: &http.Client{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != "GET" {
			t.Fatal("duplicate POST")
		}
		return reply([]trackerIssue{}), nil
	})}}}
	for i := 0; i < 2; i++ {
		if err := r.Run(context.Background(), s, secrets, func() error { return nil }); err == nil {
			t.Fatal("unknown creation marked complete")
		}
	}
}

func TestWaitLimitAndQuestionKeepSameIssueIncomplete(t *testing.T) {
	for _, step := range []string{"review", "question", "attention"} {
		s, record, secrets := smokeState()
		record.Attempted = true
		record.IssueID = 42
		record.IssueKey = "EXAMPLE-42"
		s.Smoke, _ = json.Marshal(record)
		r := Runner{UI: &testUI{}, WaitTimeout: 2 * time.Millisecond, PollInterval: time.Millisecond, Observer: observerFunc(func(context.Context, *initwizard.State, Record, initwizard.Secrets) (Observation, error) {
			return Observation{DeliveryID: "delivery_" + strings.Repeat("a", 32), Step: step}, nil
		})}
		if err := r.Run(context.Background(), s, secrets, func() error { return nil }); !errors.Is(err, ErrPending) {
			t.Fatalf("%s = %v", step, err)
		}
		var saved Record
		_ = json.Unmarshal(s.Smoke, &saved)
		if saved.IssueID != 42 || saved.Step != step || !saved.VerifiedAt.IsZero() {
			t.Fatal("pending run was reset or completed")
		}
	}
}
