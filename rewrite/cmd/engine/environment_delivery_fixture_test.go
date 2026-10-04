package main

// These commands and this provider belong only to the composition test. They
// are not a deployment adapter, a model answer format, or an engine contract.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ticket-runner/internal/chain"
)

const environmentIssue = "EXAMPLE-41"
const environmentSource = "the requested application\n"

type environmentIntegration struct {
	Issue, Repository, Base, Commit, Environment string
	Merged                                       bool
}

type environmentArtifact struct {
	Commit, Digest, Content string
	Verified                bool
}

type environmentLive struct {
	Environment, Digest, Content string
	Healthy                      bool
}

type environmentEvent struct{ Role, Prompt string }

type environmentProvider struct {
	mu                                                       sync.Mutex
	integration                                              environmentIntegration
	artifact                                                 environmentArtifact
	live                                                     environmentLive
	mode, runDir                                             string
	events                                                   []environmentEvent
	comments                                                 []map[string]any
	deployAttempts, accepted, builds, confirms, observations int
	confirmFailure, prematureDone                            bool
	cancel                                                   context.CancelFunc
	advance                                                  func()
}

func environmentDigest(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func (p *environmentProvider) serve(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	write := func(value any) { _ = json.NewEncoder(w).Encode(value) }
	switch r.Method + " " + r.URL.Path {
	case "POST /event":
		var event environmentEvent
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		p.events = append(p.events, event)
		if event.Role == "confirm" {
			p.confirms++
		}
		if event.Role == "observe" {
			p.observations++
		}
		var state chain.State
		data, _ := os.ReadFile(filepath.Join(p.runDir, "history.json"))
		_ = json.Unmarshal(data, &state)
		p.prematureDone = p.prematureDone || state.Done
		if event.Role == "elicit" && len(p.events) > 1 && p.cancel != nil {
			p.cancel()
		}
		write(map[string]bool{"ok": true})
	case "GET /integration":
		if p.mode == "malformed" {
			_, _ = w.Write([]byte("not JSON"))
			return
		}
		write(p.integration)
	case "POST /artifact":
		if err := json.NewDecoder(r.Body).Decode(&p.artifact); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		p.builds++
		switch p.mode {
		case "old-artifact":
			p.artifact.Content = "older application\n"
		case "other-commit":
			p.artifact.Commit = "another-commit"
		case "unverified":
			p.artifact.Verified = false
		}
		if p.advance != nil {
			p.advance()
			p.advance = nil
		}
		write(map[string]bool{"ok": true})
	case "GET /artifact":
		if p.mode == "missing-artifact" {
			http.NotFound(w, r)
			return
		}
		write(p.artifact)
	case "GET /environment":
		live := p.live
		// Only the readback stage sees this faulty observation; application
		// retries can still identify the operation that actually succeeded.
		if len(p.events) > 0 && p.events[len(p.events)-1].Role == "observe" {
			switch p.mode {
			case "stale-healthy":
				live.Digest, live.Content = "old-digest", "older application\n"
			case "readback-fails":
				http.Error(w, "unavailable", 503)
				return
			}
		}
		write(live)
	case "POST /environment":
		p.deployAttempts++ // Count attempts even if the provider rejects one.
		var live environmentLive
		if err := json.NewDecoder(r.Body).Decode(&live); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		p.live = live
		p.accepted++
		if p.mode == "response-lost" && p.accepted == 1 {
			http.Error(w, "response unavailable after applying", 503)
			return
		}
		write(live)
	case "GET /api/v2/issues/" + environmentIssue:
		write(map[string]any{"id": 41, "issueKey": environmentIssue, "projectId": 17, "summary": "Environment composition", "description": "Verify the agreed artifact in the agreed environment."})
	case "GET /api/v2/issues/" + environmentIssue + "/comments":
		if p.mode == "confirm-fails-once" && p.confirms == 1 && !p.confirmFailure {
			p.confirmFailure = true
			http.Error(w, "readback unavailable", 503)
			return
		}
		write(p.comments)
	case "POST /api/v2/issues/" + environmentIssue + "/comments":
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		row := map[string]any{"id": len(p.comments) + 1, "content": r.Form.Get("content"), "createdUser": map[string]any{"id": 99}}
		p.comments = append(p.comments, row)
		w.WriteHeader(201)
		write(row)
	default:
		http.Error(w, "unexpected fixture operation: "+r.Method+" "+r.URL.Path, 400)
	}
}

func environmentCall(method, path string, input, output any) error {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, os.Getenv("ENVIRONMENT_FIXTURE_URL")+path, body)
	if err != nil {
		return err
	}
	client := http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return fmt.Errorf("%s %s returned %d", method, path, response.StatusCode)
	}
	if output != nil {
		return json.NewDecoder(response.Body).Decode(output)
	}
	_, err = io.Copy(io.Discard, response.Body)
	return err
}

func environmentInspect() (environmentIntegration, error) {
	var integration environmentIntegration
	if err := environmentCall("GET", "/integration", nil, &integration); err != nil {
		return integration, err
	}
	if !integration.Merged {
		return integration, fmt.Errorf("the pull request is not integrated")
	}
	if integration.Issue != environmentIssue {
		return integration, fmt.Errorf("different request")
	}
	if integration.Repository != "fixture/application" {
		return integration, fmt.Errorf("different repository")
	}
	if integration.Base != "integration" {
		return integration, fmt.Errorf("different base")
	}
	if integration.Commit != os.Getenv("ENVIRONMENT_COMMIT") {
		return integration, fmt.Errorf("unconfirmed commit")
	}
	return integration, nil
}

func environmentCheckedArtifact() (environmentIntegration, environmentArtifact, error) {
	integration, err := environmentInspect()
	if err != nil {
		return integration, environmentArtifact{}, err
	}
	var artifact environmentArtifact
	if err := environmentCall("GET", "/artifact", nil, &artifact); err != nil {
		return integration, artifact, err
	}
	if !artifact.Verified || artifact.Commit != integration.Commit || artifact.Digest != environmentDigest(artifact.Content) {
		return integration, artifact, fmt.Errorf("artifact differs from the verified commit and bytes")
	}
	if integration.Environment != os.Getenv("ENVIRONMENT_TARGET") {
		return integration, artifact, fmt.Errorf("different environment")
	}
	return integration, artifact, nil
}

func environmentCommand(action string) error {
	switch action {
	case "elicit":
		fmt.Println("Model stand-in: all work is done. This sentence cannot finish a stage run.")
	case "inspect":
		_, err := environmentInspect()
		return err
	case "build":
		if os.Getenv("ENVIRONMENT_APPLY_KEY") != "" || os.Getenv("GITHUB_TOKEN") != "" {
			return fmt.Errorf("build inherited a deployment credential")
		}
		cmd := exec.Command("git", "rev-parse", "HEAD")
		head, err := cmd.Output()
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(head)) != os.Getenv("ENVIRONMENT_COMMIT") {
			return fmt.Errorf("fetched source changed before verification")
		}
		content, err := os.ReadFile("application.txt")
		if err != nil {
			return err
		}
		if string(content) != environmentSource {
			return fmt.Errorf("application check failed")
		}
		artifact := environmentArtifact{Commit: strings.TrimSpace(string(head)), Content: string(content), Digest: environmentDigest(string(content)), Verified: true}
		return environmentCall("POST", "/artifact", artifact, nil)
	case "apply":
		integration, artifact, err := environmentCheckedArtifact()
		if err != nil {
			return err
		}
		var live environmentLive
		if err := environmentCall("GET", "/environment", nil, &live); err != nil {
			return err
		}
		if live.Environment == integration.Environment && live.Digest == artifact.Digest {
			return nil
		}
		if os.Getenv("ENVIRONMENT_APPLY_KEY") == "" {
			return fmt.Errorf("apply credential unavailable")
		}
		return environmentCall("POST", "/environment", environmentLive{Environment: integration.Environment, Digest: artifact.Digest, Content: artifact.Content, Healthy: true}, nil)
	case "observe":
		integration, artifact, err := environmentCheckedArtifact()
		if err != nil {
			return err
		}
		var live environmentLive
		if err := environmentCall("GET", "/environment", nil, &live); err != nil {
			return err
		}
		if live.Environment != integration.Environment || live.Digest != artifact.Digest || live.Content != environmentSource || !live.Healthy {
			return fmt.Errorf("running artifact or agreed check does not match")
		}
	case "report":
		integration, artifact, err := environmentCheckedArtifact()
		if err != nil {
			return err
		}
		text := fmt.Sprintf("Observed %s at commit %s, artifact %s. Synthetic application check passed. Real infrastructure and model judgment were not checked.", integration.Environment, artifact.Commit, artifact.Digest)
		path := filepath.Join(os.Getenv("TASK_WORKSPACE"), "report", "result.md")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			return err
		}
		rows, err := environmentTracker("comments", "")
		if err != nil {
			return err
		}
		var comments []struct{ Content string }
		if err := json.Unmarshal(rows, &comments); err != nil {
			return err
		}
		for _, comment := range comments {
			if comment.Content == text {
				fmt.Println(text)
				return nil
			}
		}
		if _, err := environmentTracker("post", text); err != nil {
			return err
		}
		fmt.Println(text)
	default:
		return fmt.Errorf("unknown fixture action %q", action)
	}
	return nil
}

func environmentTracker(action, body string) ([]byte, error) {
	cmd := exec.Command(os.Getenv("TRACKER_TOOL"), "--base-url", os.Getenv("TASK_TRACKER_URL"), "--key-env", "TASK_TRACKER_KEY", "--cert-env", "TASK_TRACKER_CERT", "--issue", os.Getenv("TASK_TRACKER_ISSUE"), action)
	cmd.Stdin = strings.NewReader(body)
	return cmd.Output()
}

func TestEnvironmentDeliveryHelper(t *testing.T) {
	action := os.Getenv("ENVIRONMENT_ACTION")
	if action == "" {
		return
	}
	prompt, err := io.ReadAll(os.Stdin)
	if err == nil && os.Getenv("ENVIRONMENT_PARENT_ONLY") != "" {
		err = fmt.Errorf("child inherited an unrelated parent environment value")
	}
	if err == nil && action != "build" {
		err = environmentCall("POST", "/event", environmentEvent{action, string(prompt)}, nil)
	}
	if err == nil && action != "apply" && os.Getenv("ENVIRONMENT_APPLY_KEY") != "" {
		err = fmt.Errorf("unrelated role inherited apply credential")
	}
	if err == nil {
		if action == "verify" || action == "confirm" {
			script := os.Getenv("ENVIRONMENT_VERIFY")
			if action == "confirm" {
				script = os.Getenv("ENVIRONMENT_CONFIRM")
			}
			cmd := exec.Command("python3", "-B", script)
			cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
			err = cmd.Run()
		} else {
			err = environmentCommand(action)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func environmentGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = directory
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture"}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func environmentWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func environmentJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	environmentWrite(t, path, data)
}

func environmentTools(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	trackerTool := filepath.Join(root, "ticket-tracker")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-p", "1", "-o", trackerTool, "../tracker")
	cmd.Env = append(os.Environ(), "GOMAXPROCS=2")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build tracker: %v\n%s", err, output)
	}
	data, err := os.ReadFile("../../../deploy/ticket-engine/operator-scripts-configmap.yaml.example")
	if err != nil {
		t.Fatal(err)
	}
	_, script, ok := strings.Cut(string(data), "  confirm-report: |\n")
	if !ok {
		t.Fatal("missing shipped confirm-report")
	}
	var plain strings.Builder
	for _, line := range strings.SplitAfter(script, "\n") {
		if strings.TrimSpace(line) != "" && !strings.HasPrefix(line, "    ") {
			break
		}
		plain.WriteString(strings.TrimPrefix(line, "    "))
	}
	confirm := filepath.Join(root, "confirm-report.py")
	environmentWrite(t, confirm, []byte(plain.String()))
	return trackerTool, confirm
}

func environmentServer(t *testing.T, p *environmentProvider) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(server.Close)
	local, _ := url.Parse(server.URL)
	transport := http.DefaultTransport
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "tracker.example.invalid" {
			return nil, fmt.Errorf("external network forbidden: %s", r.URL.Host)
		}
		copy := r.Clone(r.Context())
		address := *r.URL
		address.Scheme, address.Host = local.Scheme, local.Host
		copy.URL = &address
		return transport.RoundTrip(copy)
	})
	return server.URL
}
