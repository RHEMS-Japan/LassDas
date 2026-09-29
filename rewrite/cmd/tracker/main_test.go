package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ticket-runner/internal/tracker"
)

func TestScopedTrackerCLIHelper(t *testing.T) {
	if os.Getenv("SCOPED_CLI_TEST_CHILD") != "1" {
		return
	}
	if os.Getenv("SCOPED_CLI_TEST_ACCOUNT") != "" {
		t.Fatal("account credential inherited")
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"tracker"}, os.Args[i+1:]...)
			main()
			os.Exit(0)
		}
	}
	os.Exit(2)
}

func TestCLIPostsThroughScopedAccessWithExplicitCertificate(t *testing.T) {
	t.Setenv("SCOPED_CLI_TEST_ACCOUNT", "synthetic-controller-account")
	const prose = "ordinary 日本語\n{\"unrecognized\":null} $(literal)\n"
	var mu sync.Mutex
	posts := 0
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Query().Get("apiKey") != "synthetic-controller-account" {
			t.Error("wrong upstream authority")
		}
		switch r.Method + " " + r.URL.Path {
		case "POST /issues/EXAMPLE-1/comments":
			posts++
			if err := r.ParseForm(); err != nil || r.PostForm.Get("content") != prose {
				t.Error("prose changed")
			}
			w.WriteHeader(201)
		case "GET /issues/EXAMPLE-1/comments/7":
		default:
			t.Errorf("unexpected operation: %s %s", r.Method, r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{"id": 7, "content": prose})
	}))
	defer upstream.Close()
	access, err := tracker.ServeIssue(context.Background(), tracker.Backlog{BaseURL: upstream.URL, KeyEnv: "SCOPED_CLI_TEST_ACCOUNT", Client: upstream.Client()}, "EXAMPLE-1", true)
	if err != nil {
		t.Fatal(err)
	}
	defer access.Close()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	call := func(issue, certificate, input string, tail ...string) ([]byte, error) {
		t.Helper()
		args := []string{"-test.run=^TestScopedTrackerCLIHelper$", "--", "--base-url", access.URL, "--key-env", "SCOPED_CLI_KEY", "--cert-env", "SCOPED_CLI_CERT", "--issue", issue}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, append(args, tail...)...)
		cmd.Env = []string{"SCOPED_CLI_TEST_CHILD=1", "SCOPED_CLI_KEY=" + access.Key, "SCOPED_CLI_CERT=" + certificate}
		cmd.Stdin = strings.NewReader(input)
		return cmd.CombinedOutput()
	}
	posted, err := call("EXAMPLE-1", access.Certificate, prose, "post")
	if err != nil {
		t.Fatalf("post: %s %v", posted, err)
	}
	read, err := call("EXAMPLE-1", access.Certificate, "", "--comment-id", "7", "comment")
	if err != nil || string(posted) != string(read) {
		t.Fatalf("readback: %s %v", read, err)
	}
	for _, args := range []struct{ issue, cert string }{{"EXAMPLE-2", access.Certificate}, {"EXAMPLE-1", ""}, {"EXAMPLE-1", "not a PEM"}} {
		out, err := call(args.issue, args.cert, prose, "post")
		if err == nil || strings.Contains(string(out), access.Key) {
			t.Fatalf("unauthorized request succeeded/leaked access: %s %v", out, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if posts != 1 {
		t.Fatalf("submitted %d times", posts)
	}
}

func TestTrackerCLIHelper(t *testing.T) {
	if os.Getenv("TRACKER_CLI_TEST_CHILD") != "1" {
		return
	}
	// macOS Go does not use SSL_CERT_FILE for system trust. Trust only this
	// local fixture certificate in the test child; production TLS is unchanged.
	certificate, err := os.ReadFile(os.Getenv("TRACKER_CLI_TEST_CERT"))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certificate) {
		t.Fatal("invalid test certificate")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	http.DefaultTransport = transport
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"tracker"}, os.Args[i+1:]...)
			main()
			os.Exit(0)
		}
	}
	os.Exit(2)
}

// Run the actual CLI entry point in a real subprocess against a local TLS
// tracker. This checks publication/readback, not real service permissions.
func TestCLIProseToCommentAndReadback(t *testing.T) {
	const prose = "\n報告は自由文。\n{\"new_field\":null} and `code`, $(literal), & + %\n"
	var mu sync.Mutex
	var posted string
	posts := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Query().Get("apiKey") != "synthetic-CLI-key" {
			t.Error("wrong named key")
			w.WriteHeader(401)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v2/issues/EXAMPLE-1":
			json.NewEncoder(w).Encode(map[string]any{"issueKey": "EXAMPLE-1", "summary": "Original request", "description": "Keep this wording."})
			return
		case "POST /api/v2/issues/EXAMPLE-1/comments":
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			posted = r.PostForm.Get("content")
			posts++
			w.WriteHeader(201)
		case "GET /api/v2/issues/EXAMPLE-1/comments":
			json.NewEncoder(w).Encode([]any{map[string]any{"id": 42, "content": posted}})
			return
		case "GET /api/v2/issues/EXAMPLE-1/comments/42":
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": 42, "content": posted, "futureMetadata": true})
	}))
	defer server.Close()
	certificate := filepath.Join(t.TempDir(), "tracker-cert.pem")
	if err := os.WriteFile(certificate, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	call := func(input string, tail ...string) string {
		t.Helper()
		args := []string{"-test.run=^TestTrackerCLIHelper$", "--", "--base-url", server.URL + "/api/v2", "--key-env", "TRACKER_TEST_KEY", "--issue", "EXAMPLE-1"}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, binary, append(args, tail...)...)
		command.Env = []string{"TRACKER_CLI_TEST_CHILD=1", "TRACKER_TEST_KEY=synthetic-CLI-key", "TRACKER_CLI_TEST_CERT=" + certificate}
		command.Stdin = strings.NewReader(input)
		var out, log bytes.Buffer
		command.Stdout = &out
		command.Stderr = &log
		if err := command.Run(); err != nil || log.Len() != 0 {
			t.Fatalf("CLI: %v stderr=%s", err, &log)
		}
		return out.String()
	}
	if original := call("", "read"); !strings.HasSuffix(original, "Keep this wording.") {
		t.Fatalf("request changed: %s", original)
	}
	for _, raw := range []string{call(prose, "post"), call("", "--comment-id", "42", "comment")} {
		var row struct {
			ID             int
			Content        string
			FutureMetadata bool
		}
		if err := json.Unmarshal([]byte(raw), &row); err != nil || row.ID != 42 || row.Content != prose || !row.FutureMetadata {
			t.Fatalf("report/receipt altered: %s %v", raw, err)
		}
	}
	var comments []struct {
		ID      int
		Content string
	}
	if err := json.Unmarshal([]byte(call("", "comments")), &comments); err != nil || len(comments) != 1 || comments[0].Content != prose {
		t.Fatalf("cannot observe posted report: %#v %v", comments, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if posts != 1 {
		t.Fatalf("post repeated %d times", posts)
	}
}

type failingInput struct{}

func (failingInput) Read([]byte) (int, error) { return 0, errors.New("stdin unavailable") }

func TestCLIInvalidSelectionOrUnreadableReportDoesNotSubmit(t *testing.T) {
	// A nonexistent credential prevents any network call if a regression gets
	// past argument/input handling; it cannot authorize a real submission.
	base := []string{"--base-url", "https://tracker.invalid/api/v2", "--key-env", "MISSING_TRACKER_TEST_KEY", "--issue", "EXAMPLE-1"}
	for _, tail := range [][]string{{"post", "unexpected"}, {"--after-id", "7", "post"}, {"--comment-id", "4", "post"}, {"--after-id", "-1", "comments"}, {"comment"}, {"unknown"}} {
		var out bytes.Buffer
		if err := run(context.Background(), append(base, tail...), strings.NewReader("body"), &out, io.Discard); err == nil || out.Len() != 0 {
			t.Fatalf("invalid action accepted: %v %v", tail, err)
		}
	}
	var out bytes.Buffer
	err := run(context.Background(), append(base, "post"), failingInput{}, &out, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "stdin unavailable") || out.Len() != 0 {
		t.Fatalf("unreadable report sent: %v", err)
	}
}

func TestCLIProjectIssuesReadsAllPagesAndPrintsNothingOnPartialFailure(t *testing.T) {
	var mu sync.Mutex
	failSecond := false
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		query := r.URL.Query()
		if r.Method != "GET" || r.URL.Path != "/api/v2/issues" || query.Get("projectId[]") != "17" || query.Get("apiKey") != "synthetic-CLI-key" {
			t.Error("issue discovery widened scope or performed a write")
			w.WriteHeader(400)
			return
		}
		offset, err := strconv.Atoi(query.Get("offset"))
		if err != nil {
			t.Error(err)
		}
		if failSecond && offset == 100 {
			w.WriteHeader(503)
			io.WriteString(w, "later page unavailable: synthetic-CLI-key")
			return
		}
		rows := []any{}
		for id := offset + 1; id <= 105 && len(rows) < 100; id++ {
			rows = append(rows, map[string]any{"id": id, "projectId": 17, "issueKey": "EXAMPLE-" + strconv.Itoa(id), "description": "日本語\nKeep `literal` $(text)", "futureField": true})
		}
		json.NewEncoder(w).Encode(rows)
	}))
	defer server.Close()
	certificate := filepath.Join(t.TempDir(), "tracker-cert.pem")
	if err := os.WriteFile(certificate, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, fail := range []bool{false, true} {
		mu.Lock()
		failSecond, calls = fail, 0
		mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		command := exec.CommandContext(ctx, binary, "-test.run=^TestTrackerCLIHelper$", "--", "--base-url", server.URL+"/api/v2", "--key-env", "TRACKER_TEST_KEY", "--project-id", "17", "issues")
		command.Env = []string{"TRACKER_CLI_TEST_CHILD=1", "TRACKER_TEST_KEY=synthetic-CLI-key", "TRACKER_CLI_TEST_CERT=" + certificate}
		var out, log bytes.Buffer
		command.Stdout, command.Stderr = &out, &log
		err := command.Run()
		cancel()
		mu.Lock()
		gotCalls := calls
		mu.Unlock()
		if gotCalls != 2 {
			t.Fatalf("pagination calls=%d", gotCalls)
		}
		if fail {
			if err == nil || out.Len() != 0 || !strings.Contains(log.String(), "later page unavailable") || strings.Contains(log.String(), "synthetic-CLI-key") {
				t.Fatalf("partial output or lost/leaked cause: error=%v stdout=%s stderr=%s", err, &out, &log)
			}
		} else {
			var rows []struct {
				ID          int
				Description string
				FutureField bool
			}
			if err != nil || log.Len() != 0 || json.Unmarshal(out.Bytes(), &rows) != nil || len(rows) != 105 {
				t.Fatalf("issue discovery failed: %v stdout=%s stderr=%s", err, &out, &log)
			}
			for i, row := range rows {
				if row.ID != i+1 || !row.FutureField || row.Description != "日本語\nKeep `literal` $(text)" {
					t.Fatalf("issue changed: %#v", row)
				}
			}
		}
	}
}

func TestCLIRejectsUnscopedAndMixedIssueDiscoveryArguments(t *testing.T) {
	base := []string{"--base-url", "https://tracker.invalid/api/v2", "--key-env", "MISSING_TRACKER_TEST_KEY"}
	for _, tail := range [][]string{
		{"issues"}, {"--project-id", "-1", "issues"}, {"--project-id", "17", "--issue", "EXAMPLE-1", "issues"},
		{"--project-id", "17", "--after-id", "2", "issues"}, {"--project-id", "17", "--comment-id", "3", "issues"},
		{"--project-id", "17", "--issue", "EXAMPLE-1", "post"},
	} {
		var out bytes.Buffer
		if err := run(context.Background(), append(base, tail...), strings.NewReader("body"), &out, io.Discard); err == nil || out.Len() != 0 || strings.Contains(err.Error(), "credential") {
			t.Fatalf("invalid arguments reached service access: %v error=%v", tail, err)
		}
	}
}
