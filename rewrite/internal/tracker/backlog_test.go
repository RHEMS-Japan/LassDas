package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestReadsOriginalProseWithoutModelReception(t *testing.T) {
	t.Setenv("TRACKER_TEST_KEY", "synthetic+/=token")
	const original = "Please finish this.\nExample JSON: {\"gaps\":null,\"unknown\":true}\nKeep the original acceptance conditions."
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/issues/EXAMPLE-1" || r.URL.Query().Get("apiKey") != "synthetic+/=token" {
			t.Error("wrong source request")
		}
		json.NewEncoder(w).Encode(map[string]any{"issueKey": "EXAMPLE-1", "summary": "A plain request", "description": original, "newAPIField": true})
	}))
	defer server.Close()
	request, err := (Backlog{BaseURL: server.URL + "/api/v2", KeyEnv: "TRACKER_TEST_KEY", Client: server.Client()}).Request(context.Background(), "EXAMPLE-1")
	if err != nil || !strings.HasSuffix(request, original) {
		t.Fatalf("request=%q err=%v", request, err)
	}
}

func TestCommentProsePostsOnceAndCanBeReadBack(t *testing.T) {
	t.Setenv("TRACKER_TEST_KEY", "synthetic+/=token")
	const content = "\nできるようになったことは本文に書く。\nUnknown fields: {\"anything\":null}\nLiteral $(not-a-command), `code`, & + % 日本語\n"
	posts := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("apiKey") != "synthetic+/=token" {
			t.Error("missing credential")
		}
		switch r.Method + " " + r.URL.Path {
		case "POST /api/v2/issues/EXAMPLE-1/comments":
			posts++
			if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
				t.Error("wrong form type")
			}
			if err := r.ParseForm(); err != nil || r.PostForm.Get("content") != content || len(r.PostForm) != 1 {
				t.Errorf("prose was rewritten or additional changes requested: %q %v", r.PostForm, err)
			}
			w.WriteHeader(201)
		case "GET /api/v2/issues/EXAMPLE-1/comments/7":
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{"id": 7, "content": content, "futureMetadata": map[string]any{"kept": true}})
	}))
	defer server.Close()
	b := Backlog{BaseURL: server.URL + "/api/v2", KeyEnv: "TRACKER_TEST_KEY", Client: server.Client()}
	receipt, err := b.AddComment(context.Background(), "EXAMPLE-1", content)
	if err != nil || posts != 1 {
		t.Fatalf("posts=%d error=%v", posts, err)
	}
	readback, err := b.Comment(context.Background(), "EXAMPLE-1", 7)
	if err != nil || string(receipt) != string(readback) || !strings.Contains(string(readback), "futureMetadata") {
		t.Fatalf("readback lost content/metadata: %s (%v)", readback, err)
	}
}

func TestCommentsReadEveryPageWithoutLosingInclusiveBoundary(t *testing.T) {
	for _, inclusive := range []bool{false, true} {
		t.Run(strconv.FormatBool(inclusive), func(t *testing.T) {
			t.Setenv("TRACKER_TEST_KEY", "synthetic")
			calls := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				q := r.URL.Query()
				if r.Method != "GET" || q.Get("count") != "100" || q.Get("order") != "asc" {
					t.Error("wrong pagination")
				}
				minID, err := strconv.Atoi(q.Get("minId"))
				if err != nil {
					t.Error(err)
				}
				start := minID + 1
				if inclusive && minID > 0 {
					start = minID
				}
				page := []map[string]any{}
				for id := start; id <= 205 && len(page) < 100; id++ {
					page = append(page, map[string]any{"id": id, "content": fmt.Sprintf("comment %d\n自由文", id), "newField": true})
				}
				json.NewEncoder(w).Encode(page)
			}))
			defer server.Close()
			b := Backlog{BaseURL: server.URL, KeyEnv: "TRACKER_TEST_KEY", Client: server.Client()}
			all, err := b.Comments(context.Background(), "EXAMPLE-1", 0)
			if err != nil || len(all) != 205 || calls != 3 {
				t.Fatalf("count=%d calls=%d error=%v", len(all), calls, err)
			}
			for i, raw := range all {
				var row struct {
					ID       int
					Content  string
					NewField bool
				}
				if err := json.Unmarshal(raw, &row); err != nil {
					t.Fatal(err)
				}
				if row.ID != i+1 || !row.NewField || row.Content != fmt.Sprintf("comment %d\n自由文", i+1) {
					t.Fatalf("lost/duplicated record: %s", raw)
				}
			}
			after, err := b.Comments(context.Background(), "EXAMPLE-1", 205)
			if err != nil || len(after) != 0 {
				t.Fatalf("boundary returned twice: %s %v", after, err)
			}
		})
	}
}

func TestUncertainPostDoesNotRetryOrHideTheReason(t *testing.T) {
	for _, outcome := range []string{"disconnect", "unreadable", "missing-id", "redirect", "rate-limit"} {
		t.Run(outcome, func(t *testing.T) {
			t.Setenv("TRACKER_TEST_KEY", "synthetic+/=token")
			calls := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				switch outcome {
				case "disconnect":
					c, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					c.Close()
				case "unreadable":
					w.WriteHeader(201)
					fmt.Fprint(w, "not JSON")
				case "missing-id":
					w.WriteHeader(201)
					fmt.Fprint(w, `{"content":"received"}`)
				case "redirect":
					w.Header().Set("Location", "/elsewhere")
					w.WriteHeader(307)
					fmt.Fprint(w, "moved synthetic+/=token")
				case "rate-limit":
					w.WriteHeader(429)
					fmt.Fprint(w, "try later synthetic%2B%2F%3Dtoken")
				}
			}))
			defer server.Close()
			b := Backlog{BaseURL: server.URL, KeyEnv: "TRACKER_TEST_KEY", Client: server.Client()}
			data, err := b.AddComment(context.Background(), "EXAMPLE-1", "original prose")
			if err == nil || data != nil || calls != 1 || !strings.Contains(err.Error(), "inspect comments") || strings.Contains(err.Error(), "synthetic") {
				t.Fatalf("ambiguous post retried/hidden/leaked: calls=%d data=%s err=%v", calls, data, err)
			}
			if outcome == "rate-limit" && !strings.Contains(err.Error(), "429: try later") {
				t.Fatalf("reason lost: %v", err)
			}
		})
	}
}

func TestCommentsDoNotReturnPartialOrMisidentifiedData(t *testing.T) {
	for _, bad := range []string{`null`, `{}`, `[{"id":0}]`, `[{"id":2},{"id":1}]`, `[{"id":2},{"id":2}]`, "service error"} {
		t.Run(bad, func(t *testing.T) {
			t.Setenv("TRACKER_TEST_KEY", "synthetic")
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, bad) }))
			defer server.Close()
			b := Backlog{BaseURL: server.URL, KeyEnv: "TRACKER_TEST_KEY", Client: server.Client()}
			if data, err := b.Comments(context.Background(), "EXAMPLE-1", 0); err == nil || data != nil {
				t.Fatalf("invalid list returned: %s %v", data, err)
			}
		})
	}
	t.Setenv("TRACKER_TEST_KEY", "synthetic")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/7") {
			fmt.Fprint(w, `{"id":8,"content":"different comment"}`)
			return
		}
		if r.URL.Query().Get("minId") == "0" {
			page := []map[string]any{}
			for id := 1; id <= 100; id++ {
				page = append(page, map[string]any{"id": id})
			}
			json.NewEncoder(w).Encode(page)
			return
		}
		w.WriteHeader(503)
		fmt.Fprint(w, "page unavailable")
	}))
	defer server.Close()
	b := Backlog{BaseURL: server.URL, KeyEnv: "TRACKER_TEST_KEY", Client: server.Client()}
	if data, err := b.Comments(context.Background(), "EXAMPLE-1", 0); err == nil || data != nil || !strings.Contains(err.Error(), "page unavailable") {
		t.Fatalf("partial list claimed complete: %s %v", data, err)
	}
	if data, err := b.Comment(context.Background(), "EXAMPLE-1", 7); err == nil || data != nil {
		t.Fatalf("wrong comment accepted: %s %v", data, err)
	}
}

func TestTrackerErrorKeepsReasonButNotKeyAndDoesNotRedirect(t *testing.T) {
	t.Setenv("TRACKER_TEST_KEY", "synthetic+/=token")
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Location", "/other")
		w.WriteHeader(http.StatusFound)
		fmt.Fprint(w, "upstream detail synthetic+/=token synthetic%2B%2F%3Dtoken")
	}))
	defer server.Close()
	_, err := (Backlog{BaseURL: server.URL, KeyEnv: "TRACKER_TEST_KEY", Client: server.Client()}).Request(context.Background(), "EXAMPLE-1")
	if err == nil || !strings.Contains(err.Error(), "upstream detail") || strings.Contains(err.Error(), "synthetic") || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestCommentTransportBoundsWithoutLeakingOrTruncating(t *testing.T) {
	t.Setenv("TRACKER_TEST_KEY", "synthetic+/=token")
	for _, outcome := range []string{"oversized", "error-excerpt"} {
		t.Run(outcome, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if outcome == "oversized" {
					// Valid JSON before padding would still parse if silently cut.
					fmt.Fprint(w, `{"id":7,"content":"valid prefix"}`+strings.Repeat(" ", 4<<20))
				} else {
					w.WriteHeader(500)
					fmt.Fprint(w, strings.Repeat("x", 4080)+"synthetic+/=token")
				}
			}))
			defer server.Close()
			b := Backlog{BaseURL: server.URL, KeyEnv: "TRACKER_TEST_KEY", Client: server.Client()}
			data, err := b.Comment(context.Background(), "EXAMPLE-1", 7)
			if err == nil || data != nil || strings.Contains(err.Error(), "synthetic") {
				t.Fatalf("response truncated or credential leaked: bytes=%d error=%v", len(data), err)
			}
			if outcome == "oversized" && !strings.Contains(err.Error(), "exceeds 4 MiB") {
				t.Fatalf("size reason lost: %v", err)
			}
		})
	}
}

func TestSetStatusAsksForOneStatusAndReadsTheConfirmationBack(t *testing.T) {
	t.Setenv("TRACKER_TEST_KEY", "synthetic-token")
	patches := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method+" "+r.URL.Path != "PATCH /api/v2/issues/EXAMPLE-1" || r.URL.Query().Get("apiKey") != "synthetic-token" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := r.ParseForm(); err != nil || r.PostForm.Get("statusId") != "1001" || len(r.PostForm) != 1 {
			t.Errorf("more than the status was changed: %q %v", r.PostForm, err)
		}
		patches++
		if patches == 2 {
			json.NewEncoder(w).Encode(map[string]any{"id": 1, "status": map[string]any{"id": 2}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": 1, "status": map[string]any{"id": 1001, "name": "anything"}})
	}))
	defer server.Close()
	b := Backlog{BaseURL: server.URL + "/api/v2", KeyEnv: "TRACKER_TEST_KEY", Client: server.Client()}
	if err := b.SetStatus(context.Background(), "EXAMPLE-1", 1001); err != nil {
		t.Fatal(err)
	}
	if err := b.SetStatus(context.Background(), "EXAMPLE-1", 1001); err == nil {
		t.Fatal("a status the tracker did not confirm was reported as set")
	}
	if err := b.SetStatus(context.Background(), "EXAMPLE-1", 0); err == nil || patches != 2 {
		t.Fatalf("a missing status id was sent: patches=%d err=%v", patches, err)
	}
}

func TestSetCategoriesSendsTheWholeListAndReadsTheConfirmationBack(t *testing.T) {
	t.Setenv("TRACKER_TEST_KEY", "synthetic-token")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method+" "+r.URL.Path != "PATCH /api/v2/issues/EXAMPLE-1" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := r.ParseForm(); err != nil || strings.Join(r.PostForm["categoryId[]"], ",") != "7,2001" || len(r.PostForm) != 1 {
			t.Errorf("more or less than the categories was changed: %q %v", r.PostForm, err)
		}
		json.NewEncoder(w).Encode(map[string]any{"id": 1, "category": []map[string]any{{"id": 7}, {"id": 2001}}})
	}))
	defer server.Close()
	b := Backlog{BaseURL: server.URL + "/api/v2", KeyEnv: "TRACKER_TEST_KEY", Client: server.Client()}
	if err := b.SetCategories(context.Background(), "EXAMPLE-1", []int64{7, 2001}); err != nil {
		t.Fatal(err)
	}
	if err := b.SetCategories(context.Background(), "EXAMPLE-1", []int64{0}); err == nil {
		t.Fatal("a zero category id was sent")
	}
}

func TestMyselfAssigneeAndActualHoursGoThroughTheSameGuardedCall(t *testing.T) {
	t.Setenv("TRACKER_TEST_KEY", "synthetic-token")
	var seen []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		seen = append(seen, r.Method+" "+r.URL.Path+" "+r.PostForm.Encode())
		switch {
		case r.URL.Path == "/api/v2/users/myself":
			json.NewEncoder(w).Encode(map[string]any{"id": 1797983, "name": "runtime"})
		case r.PostForm.Get("assigneeId") != "":
			json.NewEncoder(w).Encode(map[string]any{"id": 1, "assignee": map[string]any{"id": json.Number(r.PostForm.Get("assigneeId"))}})
		default:
			json.NewEncoder(w).Encode(map[string]any{"id": 1})
		}
	}))
	defer server.Close()
	b := Backlog{BaseURL: server.URL + "/api/v2", KeyEnv: "TRACKER_TEST_KEY", Client: server.Client()}
	me, err := b.Myself(context.Background())
	if err != nil || me != 1797983 {
		t.Fatalf("myself: %d %v", me, err)
	}
	if err := b.SetAssignee(context.Background(), "EXAMPLE-1", 55); err != nil {
		t.Fatal(err)
	}
	if err := b.SetActualHours(context.Background(), "EXAMPLE-1", 0.25); err != nil {
		t.Fatal(err)
	}
	if err := b.SetAssignee(context.Background(), "EXAMPLE-1", 0); err == nil {
		t.Fatal("a missing user id was sent")
	}
	want := []string{"GET /api/v2/users/myself ", "PATCH /api/v2/issues/EXAMPLE-1 assigneeId=55", "PATCH /api/v2/issues/EXAMPLE-1 actualHours=0.25"}
	if strings.Join(seen, "|") != strings.Join(want, "|") {
		t.Fatalf("requests: %q", seen)
	}
}
