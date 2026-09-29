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

func issueFixture(id int) map[string]any {
	return map[string]any{"id": id, "projectId": 17, "issueKey": fmt.Sprintf("EXAMPLE-%d", id),
		"summary": "Original title", "description": "Keep all conditions.\n{\"unknown\":null} `code` 日本語\n",
		"futureField": map[string]any{"kept": true}}
}

func TestIssueDiscoveryReadsEveryPageAndPreservesOriginalRecords(t *testing.T) {
	t.Setenv("ISSUE_LIST_TEST_KEY", "synthetic-issue-list")
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		q := r.URL.Query()
		if r.Method != "GET" || r.URL.Path != "/api/v2/issues" || q.Get("projectId[]") != "17" || q.Get("count") != "100" || q.Get("sort") != "created" || q.Get("order") != "asc" || q.Get("apiKey") != "synthetic-issue-list" {
			t.Error("unscoped or incorrect issue query")
		}
		offset, err := strconv.Atoi(q.Get("offset"))
		if err != nil || offset != (calls-1)*100 {
			t.Errorf("offset did not advance: %d %v", offset, err)
		}
		page := []any{}
		for id := offset + 1; id <= 205 && len(page) < 100; id++ {
			page = append(page, issueFixture(id))
		}
		json.NewEncoder(w).Encode(page)
	}))
	defer server.Close()
	b := Backlog{BaseURL: server.URL + "/api/v2", KeyEnv: "ISSUE_LIST_TEST_KEY", Client: server.Client()}
	issues, err := b.Issues(context.Background(), 17)
	if err != nil || len(issues) != 205 || calls != 3 {
		t.Fatalf("pages lost: count=%d calls=%d error=%v", len(issues), calls, err)
	}
	for i, raw := range issues {
		var got map[string]any
		json.Unmarshal(raw, &got)
		want, _ := json.Marshal(issueFixture(i + 1))
		actual, _ := json.Marshal(got)
		if string(actual) != string(want) {
			t.Fatalf("original record changed: %s", actual)
		}
	}
}

func TestIssueDiscoveryDoesNotReturnPartialOrOutOfScopeData(t *testing.T) {
	for _, failure := range []string{"second-page-outage", "repeated-page", "wrong-project", "missing-id", "missing-key", "not-array", "null", "oversized-page"} {
		t.Run(failure, func(t *testing.T) {
			t.Setenv("ISSUE_LIST_TEST_KEY", "synthetic-issue-list")
			calls := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls > 2 {
					fmt.Fprint(w, "[]")
					return
				}
				if failure == "second-page-outage" && calls == 2 {
					w.WriteHeader(503)
					fmt.Fprint(w, "second page unavailable: synthetic-issue-list")
					return
				}
				if failure == "not-array" || failure == "null" {
					if failure == "null" {
						fmt.Fprint(w, "null")
					} else {
						fmt.Fprint(w, `{}`)
					}
					return
				}
				page := []any{}
				count := 100
				if failure == "oversized-page" {
					count++
				}
				for id := 1; id <= count; id++ {
					row := issueFixture(id)
					switch failure {
					case "wrong-project":
						row["projectId"] = 18
					case "missing-id":
						delete(row, "id")
					case "missing-key":
						delete(row, "issueKey")
					}
					page = append(page, row)
				}
				json.NewEncoder(w).Encode(page)
			}))
			defer server.Close()
			b := Backlog{BaseURL: server.URL, KeyEnv: "ISSUE_LIST_TEST_KEY", Client: server.Client()}
			rows, err := b.Issues(context.Background(), 17)
			wantCalls := 1
			wantReason := "bounded issue array"
			switch failure {
			case "second-page-outage":
				wantCalls, wantReason = 2, "second page unavailable"
			case "repeated-page":
				wantCalls, wantReason = 2, "repeated an issue"
			case "wrong-project":
				wantReason = "outside the configured project"
			case "missing-id", "missing-key":
				wantReason = "readable identity"
			}
			if err == nil || rows != nil || calls != wantCalls || !strings.Contains(err.Error(), wantReason) {
				t.Fatalf("bad list accepted: rows=%d calls=%d error=%v", len(rows), calls, err)
			}
			if failure == "second-page-outage" && (!strings.Contains(err.Error(), "second page unavailable") || strings.Contains(err.Error(), "synthetic-issue-list")) {
				t.Fatalf("cause lost or credential leaked: %v", err)
			}
		})
	}
}

func TestIssueDiscoveryNeedsAnExplicitProjectAndHandlesEmptyProjects(t *testing.T) {
	t.Setenv("ISSUE_LIST_TEST_KEY", "synthetic")
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprint(w, "[]")
	}))
	defer server.Close()
	b := Backlog{BaseURL: server.URL, KeyEnv: "ISSUE_LIST_TEST_KEY", Client: server.Client()}
	for _, project := range []int64{-1, 0} {
		if _, err := b.Issues(context.Background(), project); err == nil || calls != 0 {
			t.Fatal("unscoped project query was made")
		}
	}
	rows, err := b.Issues(context.Background(), 17)
	if err != nil || rows == nil || len(rows) != 0 || calls != 1 {
		t.Fatalf("empty project is not a successful empty list: %v %v", rows, err)
	}
}
