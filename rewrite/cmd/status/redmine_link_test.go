package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ticket-runner/internal/chain"
)

// A Redmine issue is shown by its id and subject, and linked only under the
// configured Redmine's own HTTPS address: the stored record has no say in
// where the link goes.
func TestRedmineIssueIsNamedByItsIDAndLinkedUnderTheConfiguredAddress(t *testing.T) {
	for _, test := range []struct {
		name, settings, link string
	}{
		{"configured", `{"redmine":{"base_url":"https://redmine.example"}}`, "https://redmine.example/issues/12"},
		{"sub-path", `{"redmine":{"base_url":"https://host.example:8443/redmine/"}}`, "https://host.example:8443/redmine/issues/12"},
		{"http", `{"redmine":{"base_url":"http://redmine.example"}}`, ""},
		{"credentials", `{"redmine":{"base_url":"https://fixture:synthetic@redmine.example"}}`, ""},
		{"query", `{"redmine":{"base_url":"https://redmine.example/?next=elsewhere"}}`, ""},
		{"fragment", `{"redmine":{"base_url":"https://redmine.example/#part"}}`, ""},
		{"not a string", `{"redmine":{"base_url":13}}`, ""},
		{"no redmine", `{"backlog":{"base_url":"https://space.example/api/v2"}}`, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			directory := filepath.Join(root, "jobs", "12")
			if err := os.MkdirAll(filepath.Join(directory, "run"), 0700); err != nil {
				t.Fatal(err)
			}
			started := time.Date(2026, 10, 20, 5, 0, 0, 0, time.UTC)
			history, _ := json.Marshal(chain.State{Request: "Original issue: 12\nTitle: Visible request\n\nBODY", Step: "implement",
				History: []chain.Result{{Role: "elicit", Speaker: "elicit-process", Output: "OUTPUT", Instruction: "INSTRUCTION", StartedAt: started, FinishedAt: started.Add(time.Minute)}}})
			if err := os.WriteFile(filepath.Join(directory, "run", "history.json"), history, 0600); err != nil {
				t.Fatal(err)
			}
			record, err := json.Marshal(map[string]any{"id": 12, "subject": "Visible request", "project": map[string]any{"id": 1, "name": "Requests"},
				"author": map[string]any{"id": 6, "name": "Requester Check"}, "created_on": "2026-10-20T04:56:08Z",
				"html_url": "https://elsewhere.example/issues/12"})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, "issue.json"), record, 0600); err != nil {
				t.Fatal(err)
			}
			config := filepath.Join(root, "operator.json")
			if err := os.WriteFile(config, []byte(test.settings), 0600); err != nil {
				t.Fatal(err)
			}
			ts := serve(t, root, config, "", "")
			_, body := get(t, ts, "/jobs/12")
			expectAll(t, body, "<title>#12 status</title>", "Visible request", "Requester Check", "as written in Redmine")
			if strings.Contains(body, "elsewhere.example") {
				t.Fatal("the stored record chose the link")
			}
			linked := strings.Contains(body, `href="`+test.link+`">open the issue</a>`)
			if test.link != "" && !linked || test.link == "" && strings.Contains(body, ">open the issue</a>") {
				t.Fatalf("link: want %q in\n%s", test.link, body)
			}
			_, overview := get(t, ts, "/")
			expectAll(t, overview, `data-key="#12"`, "Visible request")
			request, _ := http.NewRequest("GET", ts.URL+"/jobs/12", nil)
			request.AddCookie(&http.Cookie{Name: "lang", Value: "ja"})
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			japanese, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if !strings.Contains(string(japanese), "あなたが Redmine のチケットに書いた本文") || strings.Contains(string(japanese), "Backlog") {
				t.Fatal("the Redmine issue's page does not name Redmine, or names Backlog")
			}
		})
	}
}

// A Backlog record keeps its own reading: it has an issue key, and an id,
// a subject-like summary and a project id of its own.
func TestABacklogRecordIsNotReadAsARedmineOne(t *testing.T) {
	for _, record := range []map[string]any{
		{"id": 12, "issueKey": "EXAMPLE-12", "summary": "Backlog request", "subject": "odd", "project": map[string]any{"id": 1}},
		{"id": 12, "issueKey": "EXAMPLE-12", "summary": "Backlog request", "projectId": 1},
	} {
		if _, redmine := redmineIssue(record); redmine {
			t.Fatalf("read as Redmine: %v", record)
		}
	}
	for _, record := range []map[string]any{
		{"id": 12.5, "subject": "x", "project": map[string]any{}}, {"id": 0, "subject": "x", "project": map[string]any{}},
		{"id": 12, "project": map[string]any{}}, {"id": 12, "subject": "x"},
	} {
		if _, redmine := redmineIssue(record); redmine {
			t.Fatalf("read as Redmine: %v", record)
		}
	}
}
