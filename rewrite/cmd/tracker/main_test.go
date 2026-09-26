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
	"strings"
	"sync"
	"testing"
	"time"
)

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
