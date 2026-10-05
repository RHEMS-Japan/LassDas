package main

// Offline operational support, not a sandbox for arbitrary candidate source.
// The caller supplies the remote-read assumptions; no live service is queried.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"ticket-runner/internal/chain"
)

type rehearsalRead struct {
	URL      string          `json:"url"`
	Body     json.RawMessage `json:"body"`
	Link     string          `json:"link,omitempty"`
	MinReads int             `json:"min_reads"`
}

type rehearsalTransport struct {
	mu       sync.Mutex
	reads    map[string]rehearsalRead
	counts   map[string]int
	writes   int
	unknown  int
	attempts []rehearsalAttempt
}

type rehearsalAttempt struct {
	Kind   string `json:"kind"`
	Method string `json:"method"`
	URL    string `json:"url"`
}

// Only URLs are retained, never request bodies or headers. The configured
// trackers use apiKey or Authorization; userinfo and other credential-shaped
// query values are removed as well before diagnostics are written.
func rehearsalURL(address *url.URL) string {
	clean := *address
	clean.User, clean.Fragment = nil, ""
	query := clean.Query()
	for name := range query {
		lower := strings.ToLower(name)
		for _, part := range []string{"key", "token", "password", "secret", "credential", "authorization", "signature"} {
			if strings.Contains(lower, part) {
				query.Del(name)
				break
			}
		}
	}
	clean.RawQuery = query.Encode()
	return clean.String()
}

func newRehearsalTransport(data []byte) (*rehearsalTransport, error) {
	var snapshot struct {
		Reads []rehearsalRead `json:"reads"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&snapshot) != nil || len(snapshot.Reads) == 0 {
		return nil, errors.New("offline read snapshot is missing or unreadable")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("offline read snapshot has trailing data")
	}
	transport := &rehearsalTransport{reads: map[string]rehearsalRead{}, counts: map[string]int{}}
	for _, read := range snapshot.Reads {
		address, err := url.Parse(read.URL)
		if err != nil || address.Scheme != "https" || address.Host == "" || address.User != nil || address.Fragment != "" || read.MinReads < 1 || !json.Valid(read.Body) {
			return nil, errors.New("offline read entry is unreadable")
		}
		query, err := url.ParseQuery(address.RawQuery)
		if err != nil || query.Has("apiKey") {
			return nil, errors.New("offline read URLs must omit credentials")
		}
		address.RawQuery = query.Encode()
		key := address.String()
		if _, exists := transport.reads[key]; exists {
			return nil, errors.New("offline read entries repeat a URL")
		}
		transport.reads[key] = read
	}
	return transport, nil
}

func (transport *rehearsalTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if request.Method != http.MethodGet {
		transport.writes++
		transport.attempts = append(transport.attempts, rehearsalAttempt{"write", request.Method, rehearsalURL(request.URL)})
		return nil, errors.New("offline rehearsal refused a write attempt")
	}
	address := *request.URL
	query := address.Query()
	query.Del("apiKey")
	address.RawQuery = query.Encode()
	key := address.String()
	read, exists := transport.reads[key]
	if !exists {
		transport.unknown++
		transport.attempts = append(transport.attempts, rehearsalAttempt{"missing_read", request.Method, rehearsalURL(request.URL)})
		return nil, errors.New("offline rehearsal has no supplied read for this request")
	}
	transport.counts[key]++
	header := make(http.Header)
	header.Set("Content-Type", "application/json")
	if read.Link != "" {
		header.Set("Link", read.Link)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: header, Request: request,
		Body: io.NopCloser(bytes.NewReader(read.Body))}, nil
}

func (transport *rehearsalTransport) verifiedReads() (int, error) {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if transport.writes != 0 || transport.unknown != 0 {
		return 0, errors.New("offline rehearsal observed writes or unknown reads")
	}
	total := 0
	for key, read := range transport.reads {
		if transport.counts[key] < read.MinReads {
			return 0, errors.New("offline rehearsal did not cover every required read")
		}
		total += transport.counts[key]
	}
	return total, nil
}

// Remove commands independently of recorded work state. If a regression
// reaches a role anyway, Process.run refuses an empty command before exec.
func disarmRehearsal(cfg config) config {
	cfg.Roles = append([]chain.Role(nil), cfg.Roles...)
	for index := range cfg.Roles {
		cfg.Roles[index].Processes = append([]chain.Process(nil), cfg.Roles[index].Processes...)
		for process := range cfg.Roles[index].Processes {
			cfg.Roles[index].Processes[process].Command = nil
		}
	}
	return cfg
}

func rehearsalDisarmed(cfg config) bool {
	for _, role := range cfg.Roles {
		for _, process := range role.Processes {
			if len(process.Command) != 0 {
				return false
			}
		}
	}
	return true
}

func rehearsalState(path string) (chain.State, error) {
	var state chain.State
	data, err := os.ReadFile(path)
	var required map[string]json.RawMessage
	if err != nil || json.Unmarshal(data, &required) != nil || required["done"] == nil || bytes.Equal(required["done"], []byte("null")) || required["request"] == nil || json.Unmarshal(data, &state) != nil || state.Request == "" {
		return state, errors.New("a required history is missing or unreadable")
	}
	if state.Done && (state.Pending != nil || state.Waiting || state.Recovering) {
		return state, errors.New("a history has conflicting states")
	}
	return state, nil
}

func rehearsalRecords(cfg config, root string) (map[string][32]byte, error) {
	entries, err := os.ReadDir(filepath.Join(root, "jobs"))
	if err != nil {
		return nil, errors.New("the copied queue has no readable jobs")
	}
	checked := map[string][32]byte{}
	for _, entry := range entries {
		if entry.Name() == "notice-kinds.json" {
			continue
		}
		id, err := strconv.ParseInt(entry.Name(), 10, 64)
		if err != nil || id <= 0 || !entry.IsDir() {
			return nil, errors.New("the copied queue has an unknown job entry")
		}
		directory := filepath.Join(root, "jobs", entry.Name())
		raw, err := os.ReadFile(filepath.Join(directory, "issue.json"))
		issue, issueErr := cfg.source().ReadIssue(raw)
		if err != nil || issueErr != nil || issue.ID != id {
			return nil, errors.New("an accepted issue cannot be read for its source")
		}
		state, err := rehearsalState(filepath.Join(directory, "run", "history.json"))
		if err != nil {
			return nil, err
		}
		stopped, err := savedStop(cfg.source(), directory, issue, cfg.Intake.StopUserIDs)
		if err != nil {
			return nil, errors.New("a saved stop cannot be verified")
		}
		reportPath := filepath.Join(directory, "stop-report", "history.json")
		_, reportErr := os.Stat(reportPath)
		reportDone := false
		if reportErr == nil || !errors.Is(reportErr, os.ErrNotExist) {
			report, err := rehearsalState(reportPath)
			if err != nil {
				return nil, errors.New("stopped reporting is unreadable")
			}
			reportDone = report.Done
		}
		for _, relative := range []string{"issue.json", "run/history.json", "stop-request.json", "stop-report/history.json"} {
			if relative == "run/history.json" && !state.Done && !stopped || relative == "stop-report/history.json" && !reportDone {
				continue
			}
			data, err := os.ReadFile(filepath.Join(directory, relative))
			if errors.Is(err, os.ErrNotExist) && (relative == "stop-request.json" || relative == "stop-report/history.json") {
				continue
			}
			if err != nil {
				return nil, errors.New("a terminal record cannot be read")
			}
			checked[entry.Name()+"/"+relative] = sha256.Sum256(data)
		}
	}
	if len(checked) == 0 {
		return nil, errors.New("an empty queue is not rehearsal coverage")
	}
	return checked, nil
}

// Copy into a new private directory before the queue loop sees anything.
// os.Root contains path resolution; links and special files are rejected.
func copyRehearsalQueue(source, destination string) error {
	absolute, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	for path := absolute; ; path = filepath.Dir(path) {
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("queue ancestors must be real directories")
		}
		if filepath.Dir(path) == path {
			break
		}
	}
	root, err := os.OpenRoot(absolute)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := os.Mkdir(destination, 0700); err != nil {
		return err
	}
	return fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path == "." {
				return nil
			}
			return os.Mkdir(filepath.Join(destination, path), 0700)
		}
		if !entry.Type().IsRegular() {
			return errors.New("queue copies cannot contain links or special files")
		}
		input, err := root.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		defer input.Close()
		before, err := input.Stat()
		if err != nil || !before.Mode().IsRegular() || before.Sys().(*syscall.Stat_t).Nlink != 1 {
			return errors.New("queue records must be ordinary unlinked files")
		}
		target := filepath.Join(destination, path)
		output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(output, input)
		closeErr := output.Close()
		after, statErr := input.Stat()
		if copyErr != nil || closeErr != nil || statErr != nil || before.Size() != after.Size() || before.ModTime() != after.ModTime() {
			return errors.New("a queue record could not be copied consistently")
		}
		return os.Chtimes(target, before.ModTime(), before.ModTime())
	})
}

type rehearsalResult struct {
	Records          int   `json:"records"`
	Reads            int   `json:"reads"`
	RequiredReads    int   `json:"required_reads"`
	ObservedMS       int64 `json:"observed_ms"`
	NormalExit       bool  `json:"normal_exit"`
	FullTickCoverage bool  `json:"full_tick_coverage"`
	log              string
}

func observeRehearsalLoop(duration time.Duration, run func(context.Context) error) (int64, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	outcome := make(chan error, 1)
	started := time.Now()
	go func() { outcome <- run(ctx) }()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-outcome:
		return 0, errors.New("queue observation ended before its requested duration")
	case <-timer.C:
	}
	cancel()
	// pollRequests joins its workers on cancellation. The enclosing go test
	// timeout bounds a regression that cannot stop; it cannot produce success.
	if err := <-outcome; !errors.Is(err, context.Canceled) {
		return 0, errors.New("queue observation did not exit normally on cancellation")
	}
	return time.Since(started).Milliseconds(), nil
}

func observeRehearsal(cfg config, root string, transport *rehearsalTransport, duration time.Duration) (result rehearsalResult, observedErr error) {
	var log bytes.Buffer
	defer func() { result.log = log.String() }()
	if !rehearsalDisarmed(cfg) {
		return result, errors.New("rehearsal commands were not all removed")
	}
	_, since, _, capacity, err := watchSettings(&cfg, root)
	if err != nil {
		return result, errors.New("rehearsal configuration is not accepted")
	}
	before, err := rehearsalRecords(cfg, root)
	if err != nil {
		return result, err
	}
	previous := http.DefaultTransport
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = previous }()
	result.ObservedMS, err = observeRehearsalLoop(duration, func(ctx context.Context) error {
		return pollRequests(ctx, cfg, filepath.Join(root, "jobs"), since, 25*time.Millisecond, capacity, &serialLog{writer: &log})
	})
	if err != nil {
		return result, err
	}
	result.NormalExit = true
	reads, err := transport.verifiedReads()
	if err != nil {
		return result, err
	}
	after, err := rehearsalRecords(cfg, root)
	if err != nil {
		return result, errors.New("terminal records changed during observation")
	}
	for path, digest := range before {
		if after[path] != digest {
			return result, errors.New("terminal records changed during observation")
		}
	}
	result.Records, result.Reads, result.RequiredReads = len(before), reads, len(transport.reads)
	return result, nil
}

func retainRehearsalObservation(directory string, transport *rehearsalTransport, result rehearsalResult) error {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	missing := map[string]int{}
	for key, read := range transport.reads {
		if count := transport.counts[key]; count < read.MinReads {
			address, _ := url.Parse(key)
			missing[rehearsalURL(address)] = read.MinReads - count
		}
	}
	data, err := json.Marshal(map[string]any{"attempts": append([]rehearsalAttempt{}, transport.attempts...), "unmet_reads": missing})
	if err != nil {
		return err
	}
	for name, body := range map[string][]byte{"observations.json": append(data, '\n'), "collector.log": []byte(result.log)} {
		file, err := os.OpenFile(filepath.Join(directory, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, writeErr := file.Write(body)
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			return errors.New("could not retain the private rehearsal observations")
		}
	}
	return nil
}

func TestRehearsalOfflineQueue(t *testing.T) {
	queue, configPath, snapshotPath, output := os.Getenv("REHEARSAL_QUEUE"), os.Getenv("REHEARSAL_CONFIG"), os.Getenv("REHEARSAL_READS"), os.Getenv("REHEARSAL_RESULT")
	if queue == "" && configPath == "" && snapshotPath == "" && output == "" {
		t.Skip("opt-in offline operational check; use the rehearsal helper")
	}
	if queue == "" || configPath == "" || snapshotPath == "" || output == "" {
		t.Fatal("all rehearsal inputs are required")
	}
	duration, err := strconv.Atoi(os.Getenv("REHEARSAL_MS"))
	if err != nil || duration < 1 || duration > 60000 {
		t.Fatal("rehearsal duration must be between one and sixty thousand milliseconds")
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal("cannot read rehearsal configuration")
	}
	cfg, err := readConfig(data)
	if err != nil {
		t.Fatal("cannot parse rehearsal configuration")
	}
	// Use only synthetic values for configuration-named API credentials.
	var tree any
	if json.Unmarshal(data, &tree) != nil {
		t.Fatal("cannot read rehearsal configuration")
	}
	var keys func(any)
	keys = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			for key, item := range value {
				if name, ok := item.(string); key == "key_env" && ok && name != "" {
					t.Setenv(name, "synthetic-rehearsal-credential")
				}
				keys(item)
			}
		case []any:
			for _, item := range value {
				keys(item)
			}
		}
	}
	keys(tree)
	data, err = os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatal("cannot read offline read snapshot")
	}
	transport, err := newRehearsalTransport(data)
	if err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(t.TempDir(), "queue")
	if err := copyRehearsalQueue(queue, copied); err != nil {
		t.Fatal("cannot make a new private queue copy")
	}
	result, err := observeRehearsal(disarmRehearsal(cfg), copied, transport, time.Duration(duration)*time.Millisecond)
	if writeErr := retainRehearsalObservation(filepath.Dir(output), transport, result); writeErr != nil {
		t.Fatal("cannot retain private observation details")
	}
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal("cannot create a new rehearsal result")
	}
	writeErr := json.NewEncoder(file).Encode(result)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatal("cannot retain the rehearsal result")
	}
}

const rehearsalDiscovery = "https://watch-tracker.example/api/v2/issues?count=100&offset=0&order=asc&projectId%5B%5D=17&sort=created"

func rehearsalFixtureReads(t *testing.T, reads ...rehearsalRead) *rehearsalTransport {
	t.Helper()
	data, err := json.Marshal(map[string]any{"reads": reads})
	if err != nil {
		t.Fatal(err)
	}
	transport, err := newRehearsalTransport(data)
	if err != nil {
		t.Fatal(err)
	}
	return transport
}

func TestRehearsalTerminalObservationRequiresReadCoverageAndNormalExit(t *testing.T) {
	for _, minimum := range []int{2, 1000} {
		t.Run(fmt.Sprint(minimum), func(t *testing.T) {
			cfg := disarmRehearsal(watchConfiguration(t))
			root, _ := noticeJob(t, chain.State{Done: true})
			transport := rehearsalFixtureReads(t, rehearsalRead{URL: rehearsalDiscovery, Body: json.RawMessage(`[]`), MinReads: minimum})
			result, err := observeRehearsal(cfg, root, transport, 100*time.Millisecond)
			if minimum == 2 {
				if err != nil || !result.NormalExit || result.FullTickCoverage || result.ObservedMS < 100 || result.Records != 2 || result.Reads < 2 {
					t.Fatalf("observation=%+v error=%v", result, err)
				}
			} else if err == nil {
				t.Fatal("unobserved required reads produced success")
			}
		})
	}
}

func TestRehearsalLoopRejectsEarlyAndAbnormalExit(t *testing.T) {
	for _, kind := range []string{"early", "abnormal", "normal"} {
		elapsed, err := observeRehearsalLoop(5*time.Millisecond, func(ctx context.Context) error {
			if kind == "early" {
				return nil
			}
			<-ctx.Done()
			if kind == "abnormal" {
				return errors.New("fixture failure")
			}
			return ctx.Err()
		})
		if kind == "normal" {
			if err != nil || elapsed < 5 {
				t.Fatal("normal observation was not confirmed")
			}
		} else if err == nil {
			t.Fatal("unobserved or abnormal run was accepted")
		}
	}
}

func TestRehearsalUsesNativeGitHubRecordsAndExplicitReads(t *testing.T) {
	cfg := githubConfiguration(t)
	cfg, err := readConfig(configurationJSON(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	root, job := noticeJob(t, chain.State{Done: true})
	raw, err := json.Marshal(githubIssueRow(cfg, 51, []string{"automation"}))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(job, "issue.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	request, err := cfg.source().RequestText(raw)
	if err != nil {
		t.Fatal(err)
	}
	history, err := json.Marshal(chain.State{Request: request, Done: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(job, "run", "history.json"), history, 0600); err != nil {
		t.Fatal(err)
	}
	address := cfg.GitHub.APIURL + "/repos/example/project/issues?direction=asc&labels=automation&per_page=100&sort=created&state=open"
	transport := rehearsalFixtureReads(t, rehearsalRead{URL: address, Body: json.RawMessage(`[]`), MinReads: 2})
	if _, err := observeRehearsal(disarmRehearsal(cfg), root, transport, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
}

func TestRehearsalUnknownReadsAndEmptySnapshotsCannotPass(t *testing.T) {
	for _, input := range []string{`{}`, `{"reads":[]}`, `{"reads":[{"url":"https://tracker.example/a?apiKey=fixture","body":[],"min_reads":1}]}`, `{"reads":[{"url":"https://tracker.example/a","body":[],"min_reads":0}]}`} {
		if _, err := newRehearsalTransport([]byte(input)); err == nil {
			t.Fatal("unusable snapshot accepted")
		}
	}
	cfg := disarmRehearsal(watchConfiguration(t))
	root, _ := noticeJob(t, chain.State{Done: true})
	transport := rehearsalFixtureReads(t, rehearsalRead{URL: rehearsalDiscovery + "&unexpected=1", Body: json.RawMessage(`[]`), MinReads: 1})
	if _, err := observeRehearsal(cfg, root, transport, 60*time.Millisecond); err == nil || transport.unknown == 0 {
		t.Fatal("an unknown read was treated as observed coverage")
	}
}

func TestRehearsalDetectsCommentStatusAndAssignmentWrites(t *testing.T) {
	for _, kind := range []string{"comment", "status", "assignment"} {
		t.Run(kind, func(t *testing.T) {
			cfg := watchConfiguration(t)
			transport := rehearsalFixtureReads(t, rehearsalRead{URL: rehearsalDiscovery, Body: json.RawMessage(`[]`), MinReads: 1})
			cfg.Backlog.Client = &http.Client{Transport: transport}
			request, err := http.NewRequest(http.MethodGet, rehearsalDiscovery, nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := transport.RoundTrip(request)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			switch kind {
			case "comment":
				_, err = cfg.Backlog.AddComment(context.Background(), "EXAMPLE-51", "private fixture request")
			case "status":
				err = cfg.Backlog.SetStatus(context.Background(), "EXAMPLE-51", 3)
			case "assignment":
				err = cfg.Backlog.SetAssignee(context.Background(), "EXAMPLE-51", 55)
			}
			if err == nil || transport.writes != 1 {
				t.Fatal("write attempt was not refused and counted")
			}
			if _, err := transport.verifiedReads(); err == nil {
				t.Fatal("write attempt passed rehearsal")
			}
		})
	}
}

func TestRehearsalDisarmingIsIndependentOfRecordedState(t *testing.T) {
	cfg := watchConfiguration(t)
	marker := filepath.Join(t.TempDir(), "role-ran")
	cfg.Roles[0].Processes[0].Command = []string{"/bin/sh", "-c", `printf 'fixture' > "$1"`, "fixture", marker}
	blocked := disarmRehearsal(cfg)
	if len(cfg.Roles[0].Processes[0].Command) == 0 || !rehearsalDisarmed(blocked) {
		t.Fatal("disarming altered the original or left a command")
	}
	executor := chain.Processes{Roles: map[string]chain.Role{"implement": blocked.Roles[0]}}
	results := executor.Execute(context.Background(), chain.Assignment{Role: "implement"}, chain.State{Request: "fixture"})
	if len(results) != 1 || results[0].Error != "No command configured." {
		t.Fatal("a role reached execution despite disarming")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("configured role command ran")
	}
	root, _ := noticeJob(t, chain.State{Done: true})
	transport := rehearsalFixtureReads(t, rehearsalRead{URL: rehearsalDiscovery, Body: json.RawMessage(`[]`), MinReads: 1})
	if _, err := observeRehearsal(cfg, root, transport, 100*time.Millisecond); err == nil || err.Error() != "rehearsal commands were not all removed" || len(transport.counts) != 0 {
		t.Fatal("observation did not check disarming before it ran")
	}
	// Positive control: the same harmless fixture command really would leave
	// a marker if the rehearsal passed the original command to the executor.
	executor.Roles["implement"] = cfg.Roles[0]
	executor.Execute(context.Background(), chain.Assignment{Role: "implement"}, chain.State{Request: "fixture"})
	if data, err := os.ReadFile(marker); err != nil || string(data) != "fixture" {
		t.Fatal("role-marker positive control did not execute")
	}
}

func TestRehearsalAdmitsReadableWorkAndRejectsUnreadableRecords(t *testing.T) {
	for _, kind := range []string{"unfinished", "waiting", "missing", "broken", "live", "missing-report", "pending-report", "bad-stop", "empty"} {
		t.Run(kind, func(t *testing.T) {
			cfg := watchConfiguration(t)
			root, job := noticeJob(t, chain.State{Done: true})
			switch kind {
			case "unfinished":
				writeJobHistory(t, job, chain.State{})
			case "waiting":
				writeJobHistory(t, job, chain.State{Waiting: true})
			case "missing":
				if err := os.Remove(filepath.Join(job, "run", "history.json")); err != nil {
					t.Fatal(err)
				}
			case "broken":
				if err := os.WriteFile(filepath.Join(job, "run", "history.json"), []byte(`{"done":null}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "live":
				if err := os.Mkdir(filepath.Join(job, "live"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(job, "live", "active"), []byte("fixture"), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing-report", "pending-report":
				cfg.Intake.StopReportRole = "implement"
				if err := os.WriteFile(filepath.Join(job, "stop-request.json"), []byte(`{"id":7,"issueId":51,"projectId":17,"content":"停止","createdUser":{"id":55}}`), 0600); err != nil {
					t.Fatal(err)
				}
				if kind == "pending-report" {
					if err := os.Mkdir(filepath.Join(job, "stop-report"), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(job, "stop-report", "history.json"), []byte(`{"request":"fixture stop report","done":false}`), 0600); err != nil {
						t.Fatal(err)
					}
				}
			case "bad-stop":
				if err := os.WriteFile(filepath.Join(job, "stop-request.json"), []byte(`{}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "empty":
				root = t.TempDir()
				if err := os.Mkdir(filepath.Join(root, "jobs"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			_, err := rehearsalRecords(cfg, root)
			unreadable := kind == "missing" || kind == "broken" || kind == "bad-stop" || kind == "empty"
			if (err != nil) != unreadable {
				t.Fatalf("readable=%t error=%v", !unreadable, err)
			}
		})
	}
}

func TestRehearsalKeepsFinishedStopReportingUnchanged(t *testing.T) {
	cfg := watchConfiguration(t)
	cfg.Intake.StopReportRole = "implement"
	root, job := noticeJob(t, chain.State{Pending: &chain.Assignment{Role: "implement"}})
	if err := os.WriteFile(filepath.Join(job, "stop-request.json"), []byte(`{"id":7,"issueId":51,"projectId":17,"content":"停止","createdUser":{"id":55}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(job, "stop-report"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(job, "stop-report", "history.json"), []byte(`{"request":"fixture stop report","done":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	protected, err := rehearsalRecords(cfg, root)
	if _, exists := protected["51/run/history.json"]; err != nil || !exists {
		t.Fatal("the stopped request's unfinished run lost its unchanged-record check")
	}
	transport := rehearsalFixtureReads(t, rehearsalRead{URL: rehearsalDiscovery, Body: json.RawMessage(`[]`), MinReads: 2})
	if _, err := observeRehearsal(disarmRehearsal(cfg), root, transport, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
}

func TestRehearsalOptInEntryUsesOnlySyntheticInputsAndKeepsSource(t *testing.T) {
	for _, kind := range []string{"done", "status-write", "missing-read", "waiting", "active"} {
		t.Run(kind, func(t *testing.T) {
			cfg := watchConfiguration(t)
			state := chain.State{Done: true}
			if kind == "status-write" {
				cfg.Intake.Statuses = &statusConfig{Delivered: 3}
			}
			if kind == "waiting" {
				state = chain.State{Waiting: true, Pending: &chain.Assignment{Role: "implement"}}
			}
			if kind == "active" {
				state = chain.State{Pending: &chain.Assignment{Role: "implement"}}
			}
			root, _ := noticeJob(t, state)
			root, err := filepath.EvalSymlinks(root)
			if err != nil {
				t.Fatal(err)
			}
			history := filepath.Join(root, "jobs", "51", "run", "history.json")
			before, err := os.ReadFile(history)
			if err != nil {
				t.Fatal(err)
			}
			private := t.TempDir()
			marker := filepath.Join(private, "role-command-ran")
			cfg.Roles[0].Processes[0].Command = []string{"/bin/sh", "-c", `printf 'fixture' > "$1"`, "fixture", marker}
			configPath, readsPath, resultPath := filepath.Join(private, "config.json"), filepath.Join(private, "reads.json"), filepath.Join(private, "result.json")
			if err := os.WriteFile(configPath, configurationJSON(t, cfg), 0600); err != nil {
				t.Fatal(err)
			}
			reads := []rehearsalRead{{URL: rehearsalDiscovery, Body: json.RawMessage(`[]`), MinReads: 2}}
			if kind == "missing-read" {
				reads[0].URL += "&missing=1"
			}
			if kind == "waiting" || kind == "active" {
				reads = append(reads, rehearsalRead{URL: cfg.Backlog.BaseURL + "/issues/EXAMPLE-51/comments?count=100&minId=0&order=asc", Body: json.RawMessage(`[]`), MinReads: 1})
			}
			data, err := json.Marshal(map[string]any{"reads": reads})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(readsPath, data, 0600); err != nil {
				t.Fatal(err)
			}
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, binary, "-test.run=^TestRehearsalOfflineQueue$", "-test.timeout=5s")
			command.Env = []string{"REHEARSAL_QUEUE=" + root, "REHEARSAL_CONFIG=" + configPath, "REHEARSAL_READS=" + readsPath, "REHEARSAL_RESULT=" + resultPath, "REHEARSAL_MS=100", "TMPDIR=" + private}
			output, runErr := command.CombinedOutput()
			failed := kind == "status-write" || kind == "missing-read" || kind == "active"
			observed, err := os.ReadFile(filepath.Join(private, "observations.json"))
			if err != nil {
				t.Fatalf("observation details were discarded: %v: %s", err, output)
			}
			log, err := os.ReadFile(filepath.Join(private, "collector.log"))
			if err != nil {
				t.Fatal("collector diagnostics were discarded")
			}
			if (runErr != nil) != failed {
				t.Fatalf("synthetic entry result: %v: %s\nobservations: %s\ncollector: %s", runErr, output, observed, log)
			}
			if !failed && !strings.Contains(string(observed), `"attempts":[]`) {
				t.Fatalf("successful observation needs an empty attempt list: %s", observed)
			}
			for _, name := range []string{"observations.json", "collector.log"} {
				info, err := os.Stat(filepath.Join(private, name))
				if err != nil || info.Mode().Perm() != 0600 {
					t.Fatal("observation file is not private")
				}
			}
			if strings.Contains(string(observed)+string(log), "synthetic-rehearsal-credential") || strings.Contains(string(observed), "apiKey=") {
				t.Fatal("credential appeared in private diagnostics")
			}
			if kind == "status-write" && (!strings.Contains(string(observed), `"method":"PATCH"`) || !strings.Contains(string(observed), "/issues/EXAMPLE-51") || !strings.Contains(string(log), "status")) {
				t.Fatalf("actual collector write is not named: %s %s", observed, log)
			}
			if kind == "missing-read" && (!strings.Contains(string(observed), `"kind":"missing_read"`) || !strings.Contains(string(observed), "/api/v2/issues?")) {
				t.Fatalf("missing GET is not distinguished: %s", observed)
			}
			if kind == "active" && !strings.Contains(string(log), "starting accepted request 51") {
				t.Fatalf("unfinished work was not observed: %s", log)
			}
			if kind == "active" && (!strings.Contains(string(observed), `"method":"POST"`) || !strings.Contains(string(observed), "/issues/EXAMPLE-51/comments")) {
				t.Fatalf("unfinished work's attempted restart notice was not retained: %s", observed)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("a configured role command ran during observation")
			}
			if !failed {
				data, err = os.ReadFile(resultPath)
				var result rehearsalResult
				if err != nil || json.Unmarshal(data, &result) != nil || result.Records < 1 || !result.NormalExit || result.FullTickCoverage {
					t.Fatal("missing scoped result")
				}
			}
			after, err := os.ReadFile(history)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("original queue changed")
			}
			t.Logf("kind=%s nonzero=%t private_observations=true collector_log=true original_unchanged=true role_commands_ran=false", kind, runErr != nil)
		})
	}
}

func TestRehearsalQueueCopyPreservesInputAndRefusesLinks(t *testing.T) {
	root, job := noticeJob(t, chain.State{Done: true})
	root, _ = filepath.EvalSymlinks(root)
	job = filepath.Join(root, "jobs", "51")
	history := filepath.Join(job, "run", "history.json")
	before, err := os.ReadFile(history)
	if err != nil {
		t.Fatal(err)
	}
	when := time.Unix(1234, 0)
	if err := os.Chtimes(history, when, when); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "copy")
	if err := copyRehearsalQueue(root, destination); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{history, filepath.Join(destination, "jobs", "51", "run", "history.json")} {
		data, err := os.ReadFile(path)
		info, statErr := os.Stat(path)
		if err != nil || statErr != nil || !bytes.Equal(data, before) || !info.ModTime().Equal(when) {
			t.Fatal("input or copied record changed")
		}
	}
	if err := copyRehearsalQueue(root, destination); err == nil {
		t.Fatal("existing copy overwritten")
	}
	for _, kind := range []string{"symlink", "hardlink", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			unsafe := filepath.Join(root, "unsafe")
			switch kind {
			case "symlink":
				err = os.Symlink(history, unsafe)
			case "hardlink":
				err = os.Link(history, unsafe)
			case "fifo":
				err = syscall.Mkfifo(unsafe, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Remove(unsafe) })
			if err := copyRehearsalQueue(root, filepath.Join(t.TempDir(), "copy")); err == nil {
				t.Fatal("unsafe input was copied")
			}
		})
	}
}
