package backlog

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
)

const secretAPIKey = "BACKLOG-SECRET-SENTINEL"

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func testClient(t *testing.T, transport http.RoundTripper, maxBytes int64) *Client {
	t.Helper()
	if maxBytes == 0 {
		maxBytes = 64 * 1024
	}
	client, err := NewClient(Config{
		SpaceKey:         "example-space",
		Origin:           "https://example-space.backlog.com",
		APIKey:           secretAPIKey,
		Timeout:          time.Second,
		MaxResponseBytes: maxBytes,
	}, transport)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	return client
}

func response(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestGetActivityUsesFixedOriginAndParsesImmutableContent(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.Scheme != "https" || request.URL.Host != "example-space.backlog.com" || request.URL.Path != "/api/v2/activities/303" {
			t.Fatalf("unexpected request: method=%s host=%s path=%s", request.Method, request.URL.Host, request.URL.Path)
		}
		if request.URL.Query().Get("apiKey") != secretAPIKey {
			t.Fatal("api key was not sent")
		}
		return response(200, `{"id":303,"project":{"id":101,"projectKey":"TICKET"},"type":1,"content":{"id":404,"key_id":505,"summary":"sample","description":"body"},"createdUser":{"id":202},"created":"2026-08-02T03:04:05Z"}`), nil
	})

	activity, err := testClient(t, transport, 0).GetActivity(context.Background(), 303)

	if err != nil {
		t.Fatalf("GetActivity() error = %v", err)
	}
	if activity.ID != 303 || activity.IssueID != 404 || activity.IssueKeyID != 505 || activity.Summary != "sample" || activity.Description != "body" {
		t.Fatalf("activity = %+v", activity)
	}
}

func TestGetIssueUsesNumericCanonicalReference(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/api/v2/issues/404" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		return response(200, `{"id":404,"projectId":101,"issueKey":"TICKET-505","keyId":505,"createdUser":{"id":202},"created":"2026-08-02T03:04:05Z"}`), nil
	})

	issue, err := testClient(t, transport, 0).GetIssue(context.Background(), 404)

	if err != nil {
		t.Fatalf("GetIssue() error = %v", err)
	}
	if issue.ID != 404 || issue.ProjectID != 101 || issue.IssueKey != "TICKET-505" || issue.KeyID != 505 || issue.CreatorID != 202 {
		t.Fatalf("issue = %+v", issue)
	}
}

func TestFindExactCommentUsesOnlyLatestIssueComments(t *testing.T) {
	const fixedComment = "fixed terminal result\n[automation-report:0123456789abcdef]"
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.Path != "/api/v2/issues/404/comments" {
			t.Fatalf("unexpected request: method=%s path=%s", request.Method, request.URL.Path)
		}
		if request.URL.Query().Get("apiKey") != secretAPIKey || request.URL.Query().Get("count") != "100" || request.URL.Query().Get("order") != "desc" {
			t.Fatalf("query = %v", request.URL.Query())
		}
		return response(200, `[{"id":808,"issueId":404,"content":"other"},{"id":809,"issueId":404,"content":"fixed terminal result\n[automation-report:0123456789abcdef]"}]`), nil
	})
	commentID, found, err := testClient(t, transport, 0).FindExactComment(context.Background(), 404, fixedComment)
	if err != nil || !found || commentID != 809 {
		t.Fatalf("commentID=%d found=%v err=%v", commentID, found, err)
	}
}

// The marker lookup matches the marker as a comment's final line only: a
// newer comment that quotes the footer, or pastes it inside its body, is not
// the automation's comment and is not returned.
func TestFindCommentWithMarkerAnchorsToTheFinalLine(t *testing.T) {
	const marker = "[ticket-automation:v1:terminal:TICKET-505:model_failed:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" + "]"
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.Path != "/api/v2/issues/404/comments" {
			t.Fatalf("unexpected request: method=%s path=%s", request.Method, request.URL.Path)
		}
		return response(200, `[`+
			`{"id":811,"issueId":404,"content":"> `+marker+`\nこの結果について質問です"},`+
			`{"id":810,"issueId":404,"content":"`+marker+` was pasted here\nand more text"},`+
			`{"id":809,"issueId":404,"content":"自動処理の最終結果: model_failed\n合計: 1 unit\n`+marker+`"},`+
			`{"id":808,"issueId":404,"content":"other"}]`), nil
	})
	commentID, found, err := testClient(t, transport, 0).FindCommentWithMarker(context.Background(), 404, marker)
	if err != nil || !found || commentID != 809 {
		t.Fatalf("commentID=%d found=%v err=%v; want the automation's own comment 809", commentID, found, err)
	}
	if _, found, err := testClient(t, transport, 0).FindCommentWithMarker(context.Background(), 404, "[ticket-automation:v1:terminal:TICKET-505:success:"+strings.Repeat("f", 64)+"]"); err != nil || found {
		t.Fatalf("a marker nobody posted: found=%v err=%v", found, err)
	}
	if _, _, err := testClient(t, transport, 0).FindCommentWithMarker(context.Background(), 404, "not a marker"); err == nil {
		t.Fatal("an unshaped marker was accepted")
	}
}

func TestAddCommentUsesOnlyFixedCommentEndpointAndDoesNotChangeStatus(t *testing.T) {
	const fixedComment = "fixed terminal result\n[automation-report:0123456789abcdef]"
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/v2/issues/404/comments" {
			t.Fatalf("unexpected request: method=%s path=%s", request.Method, request.URL.Path)
		}
		if request.URL.Query().Get("apiKey") != secretAPIKey || request.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Fatalf("query=%v content-type=%q", request.URL.Query(), request.Header.Get("Content-Type"))
		}
		if err := request.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		if request.PostForm.Get("content") != fixedComment || len(request.PostForm) != 1 {
			t.Fatalf("post form = %v", request.PostForm)
		}
		return response(201, `{"id":808,"issueId":404,"content":"fixed terminal result\n[automation-report:0123456789abcdef]"}`), nil
	})
	commentID, err := testClient(t, transport, 0).AddComment(context.Background(), 404, fixedComment)
	if err != nil || commentID != 808 {
		t.Fatalf("commentID=%d err=%v", commentID, err)
	}
}

func TestCommentMethodsFailClosedWithoutLeakingContentOrSecrets(t *testing.T) {
	const content = "FIXED-COMMENT-SENTINEL"
	tests := []struct {
		name string
		run  func(*Client) error
	}{
		{name: "lookup transport", run: func(client *Client) error {
			_, _, err := client.FindExactComment(context.Background(), 404, content)
			return err
		}},
		{name: "add transport", run: func(client *Client) error {
			_, err := client.AddComment(context.Background(), 404, content)
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := testClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New(secretAPIKey + content)
			}), 0)
			err := test.run(client)
			if err == nil || strings.Contains(err.Error(), secretAPIKey) || strings.Contains(err.Error(), content) {
				t.Fatalf("error = %v", err)
			}
		})
	}

	client := testClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(201, `{"id":808,"issueId":999,"content":"`+content+`"}`), nil
	}), 0)
	if _, err := client.AddComment(context.Background(), 404, content); err == nil || strings.Contains(err.Error(), content) {
		t.Fatalf("mismatched response error = %v", err)
	}
}

func TestCommentMethodsRejectInvalidInputBeforeNetwork(t *testing.T) {
	calls := 0
	client := testClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return response(500, ""), nil
	}), 0)
	if _, _, err := client.FindExactComment(context.Background(), 0, "fixed"); err == nil {
		t.Fatal("FindExactComment accepted a zero issue")
	}
	if _, err := client.AddComment(context.Background(), 404, ""); err == nil {
		t.Fatal("AddComment accepted empty content")
	}
	if _, err := client.AddComment(context.Background(), 404, "bad\x00content"); err == nil {
		t.Fatal("AddComment accepted NUL content")
	}
	if calls != 0 {
		t.Fatalf("network calls = %d", calls)
	}
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	calls := 0
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		calls++
		value := response(http.StatusFound, "redirect")
		value.Header.Set("Location", "https://attacker.invalid/steal")
		return value, nil
	})

	_, err := testClient(t, transport, 0).GetActivity(context.Background(), 303)

	class, code := hook.FailureDetails(err)
	if calls != 1 || class != hook.FailureRejected || code != "unexpected_status" {
		t.Fatalf("calls = %d, class = %s, code = %s, err = %v", calls, class, code, err)
	}
}

func TestClientClassifiesFailuresWithoutSecretMaterial(t *testing.T) {
	tests := []struct {
		name      string
		transport http.RoundTripper
		class     hook.FailureClass
		code      string
	}{
		{name: "network", transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New(secretAPIKey) }), class: hook.FailureUnknown, code: "request_failed"},
		{name: "unauthorized", transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return response(401, secretAPIKey), nil }), class: hook.FailureRejected, code: "authentication_failed"},
		{name: "not found", transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return response(404, secretAPIKey), nil }), class: hook.FailureRejected, code: "not_found"},
		{name: "rate limited", transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return response(429, secretAPIKey), nil }), class: hook.FailureRetryable, code: "rate_limited"},
		{name: "server", transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return response(503, secretAPIKey), nil }), class: hook.FailureRetryable, code: "server_error"},
		{name: "bad json", transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return response(200, secretAPIKey), nil }), class: hook.FailureRejected, code: "invalid_response"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := testClient(t, tt.transport, 0).GetActivity(context.Background(), 303)
			class, code := hook.FailureDetails(err)
			if class != tt.class || code != tt.code {
				t.Fatalf("class = %s, code = %s, err = %v", class, code, err)
			}
			if strings.Contains(err.Error(), secretAPIKey) {
				t.Fatalf("error leaked secret: %v", err)
			}
		})
	}
}

func TestClientResponseLimit(t *testing.T) {
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(200, strings.Repeat("x", 11)), nil
	})
	_, err := testClient(t, transport, 10).GetActivity(context.Background(), 303)
	class, code := hook.FailureDetails(err)
	if class != hook.FailureRejected || code != "response_too_large" {
		t.Fatalf("class = %s, code = %s", class, code)
	}
}

func TestConfigRejectsNonExactHTTPSOrigin(t *testing.T) {
	base := Config{SpaceKey: "example-space", Origin: "https://example-space.backlog.com", APIKey: "key", Timeout: time.Second, MaxResponseBytes: 1}
	tests := map[string]func(*Config){
		"http":       func(c *Config) { c.Origin = "http://example-space.backlog.com" },
		"wrong host": func(c *Config) { c.Origin = "https://attacker.invalid" },
		"userinfo":   func(c *Config) { c.Origin = "https://user@example-space.backlog.com" },
		"port":       func(c *Config) { c.Origin = "https://example-space.backlog.com:443" },
		"path":       func(c *Config) { c.Origin = "https://example-space.backlog.com/api" },
		"query":      func(c *Config) { c.Origin = "https://example-space.backlog.com?x=1" },
		"empty key":  func(c *Config) { c.APIKey = "" },
		"timeout":    func(c *Config) { c.Timeout = 0 },
		"limit":      func(c *Config) { c.MaxResponseBytes = 0 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			config := base
			mutate(&config)
			if err := config.Validate(); err == nil {
				t.Fatal("Validate() accepted unsafe config")
			}
		})
	}
}

func TestConfigAcceptsOfficialSpaceDomains(t *testing.T) {
	for _, origin := range []string{
		"https://example-space.backlog.com",
		"https://example-space.backlog.jp",
		"https://example-space.backlogtool.com",
	} {
		config := Config{SpaceKey: "example-space", Origin: origin, APIKey: "key", Timeout: time.Second, MaxResponseBytes: 1}
		if err := config.Validate(); err != nil {
			t.Fatalf("Validate(%q) error = %v", origin, err)
		}
	}
}

func TestListCommentsFollowsPaginationWithAuthorAndServerTime(t *testing.T) {
	pages := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.Path != "/api/v2/issues/404/comments" {
			t.Fatalf("unexpected request: method=%s path=%s", request.Method, request.URL.Path)
		}
		query := request.URL.Query()
		if query.Get("apiKey") != secretAPIKey || query.Get("count") != "100" || query.Get("order") != "asc" {
			t.Fatalf("query = %v", query)
		}
		pages++
		switch query.Get("minId") {
		case "100":
			// A full page of 100: an inclusive echo of the cursor plus 101..199.
			body := `[{"id":100,"issueId":404,"content":"echo","createdUser":{"id":7},"created":"2026-08-02T03:04:05Z"}`
			for id := 101; id <= 199; id++ {
				body += `,{"id":` + itoa(id) + `,"issueId":404,"content":"c","createdUser":{"id":7},"created":"2026-08-02T03:04:05Z"}`
			}
			return response(200, body+`]`), nil
		case "199":
			return response(200, `[{"id":200,"issueId":404,"content":"c","createdUser":{"id":7},"created":"2026-08-02T03:04:05Z"},{"id":201,"issueId":404,"content":"答え","createdUser":{"id":9},"created":"2026-08-02T04:00:00+09:00"}]`), nil
		default:
			t.Fatalf("unexpected minId %q", query.Get("minId"))
			return nil, nil
		}
	})
	comments, err := testClient(t, transport, 0).ListComments(context.Background(), 404, 100)
	if err != nil {
		t.Fatalf("ListComments() error = %v", err)
	}
	if pages != 2 || len(comments) != 101 {
		t.Fatalf("pages = %d, comments = %d", pages, len(comments))
	}
	last := comments[len(comments)-1]
	if last.CommentID != 201 || last.UserID != 9 || last.Body != "答え" {
		t.Fatalf("last comment = %+v", last)
	}
	// 04:00 JST is 19:00 UTC the previous day: server timestamps must land as
	// absolute instants.
	if last.PostedAt != time.Date(2026, 8, 1, 19, 0, 0, 0, time.UTC).UnixMilli() {
		t.Fatalf("posted at = %d", last.PostedAt)
	}
}

func itoa(value int) string {
	return strconv.Itoa(value)
}

func TestListCommentsFailsClosedInsteadOfDecidingOnATruncatedView(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		minID, _ := strconv.Atoi(request.URL.Query().Get("minId"))
		body := ""
		for id := minID + 1; id <= minID+100; id++ {
			if body != "" {
				body += ","
			}
			body += `{"id":` + itoa(id) + `,"issueId":404,"content":"c","createdUser":{"id":7},"created":"2026-08-02T03:04:05Z"}`
		}
		return response(200, `[`+body+`]`), nil
	})
	_, err := testClient(t, transport, 0).ListComments(context.Background(), 404, 100)
	class, code := hook.FailureDetails(err)
	if class != hook.FailureRetryable || code != "comment_window_exhausted" {
		t.Fatalf("class = %s, code = %s", class, code)
	}
	for _, run := range []struct {
		name string
		body string
	}{
		{name: "descending ids", body: `[{"id":300,"issueId":404,"content":"c","createdUser":{"id":7},"created":"2026-08-02T03:04:05Z"},{"id":250,"issueId":404,"content":"c","createdUser":{"id":7},"created":"2026-08-02T03:04:05Z"}]`},
		{name: "missing author", body: `[{"id":300,"issueId":404,"content":"c","created":"2026-08-02T03:04:05Z"}]`},
		{name: "broken timestamp", body: `[{"id":300,"issueId":404,"content":"c","createdUser":{"id":7},"created":"yesterday"}]`},
		{name: "foreign issue", body: `[{"id":300,"issueId":405,"content":"c","createdUser":{"id":7},"created":"2026-08-02T03:04:05Z"}]`},
	} {
		t.Run(run.name, func(t *testing.T) {
			transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
				return response(200, run.body), nil
			})
			if _, err := testClient(t, transport, 0).ListComments(context.Background(), 404, 100); err == nil {
				t.Fatal("broken listing was accepted")
			}
		})
	}
}

func TestAddCommentNotifyingSendsTheNotifiedUserForm(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if err := request.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		if request.PostForm.Get("content") != "確認のお願い" || len(request.PostForm) != 2 {
			t.Fatalf("post form = %v", request.PostForm)
		}
		if users := request.PostForm["notifiedUserId[]"]; len(users) != 1 || users[0] != "9903853" {
			t.Fatalf("notified users = %v", request.PostForm["notifiedUserId[]"])
		}
		return response(201, `{"id":808,"issueId":404,"content":"確認のお願い"}`), nil
	})
	commentID, err := testClient(t, transport, 0).AddCommentNotifying(context.Background(), 404, "確認のお願い", []int64{9903853})
	if err != nil || commentID != 808 {
		t.Fatalf("commentID=%d err=%v", commentID, err)
	}
	if _, err := testClient(t, transport, 0).AddCommentNotifying(context.Background(), 404, "x", []int64{0}); err == nil {
		t.Fatal("a non-positive notified user was accepted")
	}
}

func TestFindCommentByMarkerMatchesTheIdentifierAlone(t *testing.T) {
	marker := "[ticket-automation:v1:question:run_20260802_alpha:C1]"
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Query().Get("order") != "desc" {
			t.Fatalf("query = %v", request.URL.Query())
		}
		return response(200, `[
			{"id":900,"issueId":404,"content":"unrelated"},
			{"id":880,"issueId":404,"content":"質問本文...\n`+marker+`"}
		]`), nil
	})
	commentID, found, err := testClient(t, transport, 0).FindCommentByMarker(context.Background(), 404, marker)
	if err != nil || !found || commentID != 880 {
		t.Fatalf("commentID=%d found=%v err=%v", commentID, found, err)
	}
	if _, _, err := testClient(t, transport, 0).FindCommentByMarker(context.Background(), 404, "bad\nmarker"); err == nil {
		t.Fatal("a marker with control characters was accepted")
	}
}

func TestProjectRecentUpdatesSeedsFromTheNewestPageAndAscends(t *testing.T) {
	var seen []string
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/api/v2/projects/101/activities" {
			t.Fatalf("path = %s", request.URL.Path)
		}
		query := request.URL.Query()
		seen = append(seen, query.Get("order")+"/"+query.Get("minId")+"/"+query.Get("count"))
		return response(200, `[
			{"id":52,"type":1,"project":{"id":101,"projectKey":"TICKET"},"createdUser":{"id":202},"content":{"id":404,"key_id":505},"created":"2026-08-02T03:04:05Z"},
			{"id":50,"type":1,"project":{"id":101,"projectKey":"TICKET"},"createdUser":{"id":202},"content":{"id":403,"keyId":504},"created":"2026-08-02T03:04:05Z"},
			{"id":51,"type":3,"project":{"id":101,"projectKey":"TICKET"},"createdUser":{"id":202},"content":{"id":0},"created":"2026-08-02T03:04:05Z"}
		]`), nil
	})
	client := testClient(t, transport, 0)

	// With no cursor the scan starts at the present, not at the project's
	// first ever activity, and the caller always receives ascending IDs.
	hints, err := client.ProjectRecentUpdates(context.Background(), 101, 0)
	if err != nil {
		t.Fatalf("ProjectRecentUpdates() error = %v", err)
	}
	if len(hints) != 3 || hints[0].ActivityID != 50 || hints[1].ActivityID != 51 || hints[2].ActivityID != 52 {
		t.Fatalf("hints = %+v, want ascending 50,51,52", hints)
	}
	// Both key spellings map onto the same field.
	if hints[0].IssueKeyID != 504 || hints[2].IssueKeyID != 505 {
		t.Fatalf("key ids = %d, %d", hints[0].IssueKeyID, hints[2].IssueKeyID)
	}
	if hints[0].ActivityType != 1 || hints[0].ProjectID != 101 || hints[0].CreatorID != 202 || hints[0].IssueID != 403 {
		t.Fatalf("hint = %+v", hints[0])
	}

	// With a cursor the scan walks forward and never returns what it already saw.
	hints, err = client.ProjectRecentUpdates(context.Background(), 101, 51)
	if err != nil {
		t.Fatalf("ProjectRecentUpdates() error = %v", err)
	}
	if len(hints) != 1 || hints[0].ActivityID != 52 {
		t.Fatalf("hints = %+v, want only 52", hints)
	}
	if len(seen) != 2 || seen[0] != "desc//20" || seen[1] != "asc/51/20" {
		t.Fatalf("queries = %v", seen)
	}
	if _, err := client.ProjectRecentUpdates(context.Background(), 0, 0); err == nil {
		t.Fatal("a non-positive project was accepted")
	}
}

func TestGetIssueParsesCategoryIDs(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return response(200, `{"id":404,"projectId":101,"issueKey":"TICKET-505","keyId":505,"createdUser":{"id":202},"category":[{"id":31,"name":"one"},{"id":77,"name":"two"}],"created":"2026-08-02T03:04:05Z"}`), nil
	})

	issue, err := testClient(t, transport, 0).GetIssue(context.Background(), 404)

	if err != nil {
		t.Fatalf("GetIssue() error = %v", err)
	}
	if len(issue.CategoryIDs) != 2 || issue.CategoryIDs[0] != 31 || issue.CategoryIDs[1] != 77 {
		t.Fatalf("category ids = %v", issue.CategoryIDs)
	}
}
