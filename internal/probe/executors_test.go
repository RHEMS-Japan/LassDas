package probe

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeSQLConn struct {
	sent    []string
	queried []string
	closed  bool
}

func (c *fakeSQLConn) exec(_ context.Context, statement string) error {
	c.sent = append(c.sent, statement)
	return nil
}

func (c *fakeSQLConn) query(_ context.Context, statement string) ([]string, [][]string, error) {
	c.queried = append(c.queried, statement)
	return []string{"count"}, [][]string{{"42"}}, nil
}

func (c *fakeSQLConn) close(context.Context) error { c.closed = true; return nil }

type fakeConnector struct {
	conns []*fakeSQLConn
	dsns  []string
}

func (f *fakeConnector) connect(_ context.Context, dsn string) (sqlConn, error) {
	f.dsns = append(f.dsns, dsn)
	conn := &fakeSQLConn{}
	f.conns = append(f.conns, conn)
	return conn, nil
}

func TestSQLProbeSendsOneReadStatement(t *testing.T) {
	catalog := testCatalog(t)
	recorder, err := OpenRecorder(filepath.Join(t.TempDir(), "measurements.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	connector := &fakeConnector{}
	session := &Session{Catalog: catalog, Recorder: recorder, connector: connector,
		DSN: func(name string) string {
			return map[string]string{"PROBE_DB_DSN": "postgres://reader:pw@db.example.invalid/app"}[name]
		}}

	outcome, err := session.Run(context.Background(), Request{Probe: "sql.read", Args: map[string]string{"query": "SELECT count(*) FROM runs_shape"}})
	if err != nil || outcome.Measurement.Refused {
		t.Fatalf("plain select: %+v %v", outcome.Measurement, err)
	}
	if len(connector.conns) != 1 {
		t.Fatalf("connections opened: %d", len(connector.conns))
	}
	conn := connector.conns[0]
	wantFence := []string{"BEGIN READ ONLY", "SET LOCAL statement_timeout = 10000", "SET LOCAL lock_timeout = 2000", "SET LOCAL transaction_read_only = on", "ROLLBACK"}
	if strings.Join(conn.sent, "|") != strings.Join(wantFence, "|") {
		t.Errorf("fence = %q", conn.sent)
	}
	if len(conn.queried) != 1 || conn.queried[0] != "SELECT count(*) FROM runs_shape" || !conn.closed {
		t.Errorf("queried %q closed %v", conn.queried, conn.closed)
	}
	if outcome.Excerpt != "count\n42\n" {
		t.Errorf("excerpt = %q", outcome.Excerpt)
	}

	// Every shape that a SELECT-only grant would let through is refused
	// before a connection is opened; the refusal is recorded.
	refusedStatements := map[string]string{
		"SELECT 1; UPDATE runs_shape SET state = 'x'":                              "statement separator",
		"EXPLAIN ANALYZE DELETE FROM runs_shape":                                   "declared shape",
		"SELECT set_config('transaction_read_only', 'off', false)":                 "set_config",
		"SELECT pg_advisory_lock(1)":                                               "pg_advisory_lock",
		"SELECT dblink_exec('dbname=app', 'UPDATE t SET x = 1')":                   "dblink_exec",
		"SELECT pg_terminate_backend(123)":                                         "pg_terminate_backend",
		"SELECT * INTO runs_copy FROM runs_shape":                                  "INTO",
		"SELECT id FROM runs_shape FOR UPDATE":                                     "FOR UPDATE",
		"SELECT lo_import('/etc/passwd')":                                          "lo_import",
		"SELECT pg_read_file('/etc/passwd')":                                       "pg_read_file",
		"SELECT 'unbalanced FROM runs_shape":                                       "unbalanced quote",
		"WITH RECURSIVE r AS (SELECT 1 UNION ALL SELECT 1 FROM r) SELECT * FROM r": "declared shape",
	}
	for statement, reason := range map[string]string{
		"SELECT \"pg_terminate_backend\"(1)":     "quoted identifiers",
		"SELECT pg_terminate_backend/**/(1)":     "comments",
		"SELECT U&\"set_config\"('a','b',false)": "quoted identifiers",
		"SELECT $$x$$":                           "dollar quoting",
		"SELECT 1 -- and more":                   "comments",
		"SELECT E'\\x41'":                        "escape strings",
	} {
		refusedStatements[statement] = reason
	}
	before := len(connector.conns)
	for statement, reason := range refusedStatements {
		outcome, err := session.Run(context.Background(), Request{Probe: "sql.read", Args: map[string]string{"query": statement}})
		if err != nil {
			t.Fatalf("%q: %v", statement, err)
		}
		if !outcome.Measurement.Refused || !strings.Contains(outcome.Measurement.Reason, reason) {
			t.Errorf("%q: refused %v reason %q (want %q)", statement, outcome.Measurement.Refused, outcome.Measurement.Reason, reason)
		}
	}
	if len(connector.conns) != before {
		t.Errorf("a refused statement opened a connection (%d -> %d)", before, len(connector.conns))
	}
	for _, statement := range []string{
		"SELECT count(*) FROM runs_shape WHERE note = 'a--b'",
		"SELECT count(*) FROM runs_shape WHERE state LIKE'done%'",
		"SELECT count(*) FROM runs_shape WHERE created_at > date'2026-09-01'",
		"SELECT count(*) FROM runs_shape WHERE tag = 'AU&B' AND price = '$5'",
	} {
		if reason := sqlStatementProblem(statement); reason != "" {
			t.Errorf("legitimate read %q refused: %s", statement, reason)
		}
	}
	if !strings.Contains(safeDBError(errors.New("connect to postgres://u:p@h/db failed")), "withheld") {
		t.Error("safeDBError leaked a connection string")
	}
}

func staticResolver(answers map[string]string) resolver {
	return func(_ context.Context, host string) ([]net.IPAddr, error) {
		if ip, ok := answers[host]; ok {
			return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil
		}
		return nil, errors.New("no such host")
	}
}

func TestHTTPProbeRefusesPrivateResolution(t *testing.T) {
	catalog, err := NewCatalog([]Spec{{ID: "http.timing", Kind: KindHTTP, Hosts: []string{"app.example.invalid", "meta.example.invalid", "inner.example.invalid"},
		Methods: []string{"GET"}, Args: map[string]string{"path": `/[a-z/]{0,20}`, "host": `[a-z.]{1,40}`}}})
	if err == nil {
		t.Fatal("http probe accepted a host slot; the design allows path and method only")
	}
	catalog = testCatalog(t)
	recorder, err := OpenRecorder(filepath.Join(t.TempDir(), "measurements.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for name, ip := range map[string]string{"link-local": "169.254.169.254", "private": "10.0.0.7", "loopback": "127.0.0.1", "unspecified": "0.0.0.0", "unique-local v6": "fd00::1", "link-local v6": "fe80::1", "cgnat": "100.64.0.1"} {
		session := &Session{Catalog: catalog, Recorder: recorder, lookup: staticResolver(map[string]string{"app-stg.example.invalid": ip})}
		outcome, err := session.Run(context.Background(), Request{Probe: "http.timing", Args: map[string]string{"path": "/console"}})
		if err != nil {
			t.Fatal(err)
		}
		if !outcome.Measurement.Refused || !strings.Contains(outcome.Measurement.Reason, "private, loopback or link-local") {
			t.Errorf("%s (%s): refused %v reason %q", name, ip, outcome.Measurement.Refused, outcome.Measurement.Reason)
		}
	}
	for _, ip := range []string{"203.0.113.10", "2001:db8::10"} {
		if !addressAllowed(net.ParseIP(ip)) {
			t.Errorf("public address %s refused", ip)
		}
	}
}

func TestHTTPProbeNeverWritesJar(t *testing.T) {
	var seenCookie string
	var requests int
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if c, err := r.Cookie("console_session"); err == nil {
			seenCookie = c.Value
		}
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "console_session", Value: "rotated-value-0002", Path: "/"})
		_, _ = w.Write([]byte(strings.Repeat("x", 1000)))
	}))
	defer server.Close()
	serverURL, _ := url.Parse(server.URL)
	_, port, _ := net.SplitHostPort(serverURL.Host)
	catalog, err := NewCatalog([]Spec{{ID: "http.timing", Kind: KindHTTP, Hosts: []string{"app-stg.example.invalid"}, Methods: []string{"GET", "HEAD"},
		Returns: []string{"status", "time_total", "bytes"}, Args: map[string]string{"path": `/[a-z]{0,20}`}, Cookies: CookiesObservationJar}})
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := OpenRecorder(filepath.Join(t.TempDir(), "measurements.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	jar := []Cookie{{Name: "console_session", Value: "original-value-0001", Domain: "app-stg.example.invalid", Path: "/"},
		{Name: "other_site", Value: "elsewhere-value-000", Domain: "other.example.invalid", Path: "/"}}
	session := &Session{Catalog: catalog, Recorder: recorder, Jar: jar,
		lookup:    staticResolver(map[string]string{"app-stg.example.invalid": "127.0.0.1"}),
		httpHooks: httpTestHooks{allowLoopback: true, tlsConfig: &tls.Config{InsecureSkipVerify: true}}} // #nosec G402 -- test server certificate
	// Point the probe at the test server's port by rewriting the path? The
	// path shape forbids ports, so the dialer is asked for :443; substitute
	// the test port through the resolver-side address instead.
	session.lookup = func(_ context.Context, host string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}
	session.httpHooks.port = port

	outcome, err := session.Run(context.Background(), Request{Probe: "http.timing", Args: map[string]string{"path": "/console"}})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Measurement.Refused {
		t.Fatalf("refused: %s", outcome.Measurement.Reason)
	}
	if seenCookie != "original-value-0001" {
		t.Errorf("server saw cookie %q", seenCookie)
	}
	if !outcome.Measurement.Rotated || !strings.Contains(outcome.Excerpt, "rotated=true") {
		t.Errorf("rotation not recorded: %+v excerpt %q", outcome.Measurement, outcome.Excerpt)
	}
	if !strings.Contains(outcome.Excerpt, "status=200") || !strings.Contains(outcome.Excerpt, "bytes=1000") || strings.Contains(outcome.Excerpt, "xxxx") {
		t.Errorf("excerpt = %q", outcome.Excerpt)
	}
	if jar[0].Value != "original-value-0001" {
		t.Error("jar was written")
	}
	// The jar's values never appear in stored output even if a page echoes them.
	if kind, found := SecretShaped("session=original-value-0001", session.forbiddenLiterals()); !found || kind != "known secret value" {
		t.Errorf("jar value not refused: %q %v", kind, found)
	}
	// After a rotation the host is closed for the rest of the request.
	outcome, err = session.Run(context.Background(), Request{Probe: "http.timing", Args: map[string]string{"path": "/console"}})
	if err != nil || !outcome.Measurement.Refused || !strings.Contains(outcome.Measurement.Reason, "rotated") {
		t.Errorf("second request to a rotated host: %+v %v", outcome.Measurement, err)
	}
	if requests != 1 {
		t.Errorf("server saw %d requests; the rotated host must not be addressed again", requests)
	}
	// Redirects are not followed: the 302 is the recorded answer.
	session.rotatedHosts = nil
	outcome, err = session.Run(context.Background(), Request{Probe: "http.timing", Args: map[string]string{"path": "/redirect"}})
	if err != nil || outcome.Measurement.Refused || !strings.Contains(outcome.Excerpt, "status=302") {
		t.Errorf("redirect: %+v %q %v", outcome.Measurement, outcome.Excerpt, err)
	}
	if requests != 2 {
		t.Errorf("server saw %d requests; a followed redirect would make 3", requests)
	}
}

func TestExecProbeCapsOutputAndTime(t *testing.T) {
	catalog, err := NewCatalog([]Spec{
		{ID: "noise", Kind: KindExec, Argv: []string{"sh", "-c", "head -c 300000 /dev/zero | tr '\\0' x"}, MaxOutputBytes: 1024},
		{ID: "slow", Kind: KindExec, Argv: []string{"sleep", "{{seconds}}"}, Args: map[string]string{"seconds": `[0-9]{1,2}`}, TimeoutSeconds: 1},
		{ID: "fail", Kind: KindExec, Argv: []string{"sh", "-c", "echo boom >&2; exit 3"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := OpenRecorder(filepath.Join(t.TempDir(), "measurements.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{Catalog: catalog, Recorder: recorder, Env: []string{"PATH=" + os.Getenv("PATH")}, Limits: Limits{MaxProbes: 10, MaxTotalBytes: 1 << 20, ExcerptBytes: 100}}
	outcome, err := session.Run(context.Background(), Request{Probe: "noise"})
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Measurement.Truncated || outcome.Measurement.OutputBytes != 300000 || len(outcome.Excerpt) != 100 || outcome.Measurement.ExcerptBytes != 100 {
		t.Errorf("noise: %+v excerpt %d", outcome.Measurement, len(outcome.Excerpt))
	}
	stored, err := ReadPrefix(recorder.path, 1)
	if err != nil || len(stored[0].Output) != 1024 {
		t.Errorf("stored output %d bytes, %v", len(stored[0].Output), err)
	}
	started := time.Now()
	outcome, err = session.Run(context.Background(), Request{Probe: "slow", Args: map[string]string{"seconds": "30"}})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > 5*time.Second || outcome.Measurement.Reason != "timed out" || outcome.Measurement.ExitCode != -1 {
		t.Errorf("slow: %+v after %s", outcome.Measurement, time.Since(started))
	}
	outcome, err = session.Run(context.Background(), Request{Probe: "fail"})
	if err != nil || outcome.Measurement.ExitCode != 3 || outcome.Excerpt != "boom\n" || outcome.Measurement.Refused {
		t.Errorf("fail: %+v %q %v", outcome.Measurement, outcome.Excerpt, err)
	}
}

func TestRepoProbesStayInsideTheRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "internal", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "internal", "x", "a.go"), []byte("package x\n\nfunc Alpha() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	catalog, _ := NewCatalog(nil)
	recorder, err := OpenRecorder(filepath.Join(t.TempDir(), "measurements.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{Catalog: catalog, Recorder: recorder, RepoRoot: root}
	run := func(probe string, args map[string]string) Outcome {
		outcome, err := session.Run(context.Background(), Request{Probe: probe, Args: args})
		if err != nil {
			t.Fatal(err)
		}
		return outcome
	}
	if out := run("repo.list", nil); out.Measurement.Refused || !strings.Contains(out.Excerpt, "internal/x/a.go\n") {
		t.Errorf("list: %+v %q", out.Measurement, out.Excerpt)
	}
	if out := run("repo.read", map[string]string{"path": "internal/x/a.go"}); out.Excerpt != "package x\n\nfunc Alpha() {}\n" {
		t.Errorf("read: %q", out.Excerpt)
	}
	if out := run("repo.grep", map[string]string{"pattern": "func Alpha", "path": "internal"}); !strings.Contains(out.Excerpt, "internal/x/a.go:3: func Alpha() {}") {
		t.Errorf("grep: %q", out.Excerpt)
	}
	if out := run("repo.read", map[string]string{"path": "link.txt"}); out.Excerpt != "" || out.Measurement.ExitCode == 0 {
		t.Errorf("symlink outside the root was read: %+v %q", out.Measurement, out.Excerpt)
	}
	if out := run("repo.read", map[string]string{"path": "../secret.txt"}); out.Excerpt != "" || out.Measurement.ExitCode == 0 {
		t.Errorf("parent path was read: %+v %q", out.Measurement, out.Excerpt)
	}
}

func TestSessionRecordsRefusalsAndBudget(t *testing.T) {
	catalog := testCatalog(t)
	recorder, err := OpenRecorder(filepath.Join(t.TempDir(), "measurements.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{Catalog: catalog, Recorder: recorder, RepoRoot: t.TempDir(), Limits: Limits{MaxProbes: 2, MaxTotalBytes: 1 << 20, ExcerptBytes: 1 << 10}}
	outcome, err := session.Run(context.Background(), Request{Probe: "nope"})
	if err != nil || !outcome.Measurement.Refused || outcome.Measurement.ID != "m-0001" {
		t.Errorf("refusal not recorded: %+v %v", outcome.Measurement, err)
	}
	if _, err := session.Run(context.Background(), Request{Probe: "repo.list"}); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Run(context.Background(), Request{Probe: "repo.list"}); !errors.Is(err, ErrBudgetExhausted) {
		t.Errorf("third request: %v", err)
	}
	if recorder.Count() != 2 || session.Used != 2 {
		t.Errorf("count %d used %d", recorder.Count(), session.Used)
	}
	// Secret-shaped output is not stored; the attempt is.
	if err := os.WriteFile(filepath.Join(session.RepoRoot, "leak.txt"), []byte("AKIAABCDEFGHIJKLMNOP\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	session.Limits.MaxProbes = 10
	outcome, err = session.Run(context.Background(), Request{Probe: "repo.read", Args: map[string]string{"path": "leak.txt"}})
	if err != nil || !outcome.Measurement.Refused || outcome.Excerpt != "" || !strings.Contains(outcome.Measurement.Reason, "aws access key id") {
		t.Errorf("leak: %+v %q %v", outcome.Measurement, outcome.Excerpt, err)
	}
	stored, _ := ReadPrefix(recorder.path, 3)
	if stored[2].Output != "" || stored[2].OutputBytes != 0 {
		t.Errorf("secret output stored: %+v", stored[2])
	}
}

// A probe that lists two hosts is addressed with a host argument; a rotation
// on one host closes that host alone, the other is still measured, and the
// dialer is asked for the host the request named.
func TestHTTPProbeRotationClosesOnlyTheRotatedHost(t *testing.T) {
	var requests int
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.SetCookie(w, &http.Cookie{Name: "console_session", Value: "rotated-value-0002", Path: "/"})
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()
	serverURL, _ := url.Parse(server.URL)
	_, port, _ := net.SplitHostPort(serverURL.Host)
	catalog, err := NewCatalog([]Spec{{ID: "http.timing", Kind: KindHTTP, Hosts: []string{"console.example.invalid", "api.example.invalid"}, Methods: []string{"GET"},
		Returns: []string{"status", "time_total", "bytes"}, Args: map[string]string{"path": `/[a-z]{0,20}`}, Cookies: CookiesObservationJar}})
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := OpenRecorder(filepath.Join(t.TempDir(), "measurements.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var asked []string
	session := &Session{Catalog: catalog, Recorder: recorder,
		Jar:       []Cookie{{Name: "console_session", Value: "original-value-0001", Domain: "console.example.invalid", Path: "/"}},
		httpHooks: httpTestHooks{allowLoopback: true, port: port, tlsConfig: &tls.Config{InsecureSkipVerify: true}}} // #nosec G402 -- test server certificate
	session.lookup = func(_ context.Context, host string) ([]net.IPAddr, error) {
		asked = append(asked, host)
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}

	first, err := session.Run(context.Background(), Request{Probe: "http.timing", Args: map[string]string{"path": "/console"}})
	if err != nil || first.Measurement.Refused || !first.Measurement.Rotated || first.Measurement.Args["host"] != "console.example.invalid" {
		t.Fatalf("first host: %+v %v", first.Measurement, err)
	}
	again, err := session.Run(context.Background(), Request{Probe: "http.timing", Args: map[string]string{"host": "console.example.invalid", "path": "/console"}})
	if err != nil || !again.Measurement.Refused || !strings.Contains(again.Measurement.Reason, "rotated") {
		t.Fatalf("rotated host addressed again: %+v %v", again.Measurement, err)
	}
	other, err := session.Run(context.Background(), Request{Probe: "http.timing", Args: map[string]string{"host": "api.example.invalid", "path": "/health"}})
	if err != nil || other.Measurement.Refused || other.Measurement.Args["host"] != "api.example.invalid" {
		t.Fatalf("second host after the first rotated: %+v %v", other.Measurement, err)
	}
	if requests != 2 {
		t.Errorf("server saw %d requests; the rotated host must not be addressed again, the other must", requests)
	}
	if len(asked) != 2 || asked[0] != "console.example.invalid" || asked[1] != "api.example.invalid" {
		t.Errorf("resolver asked for %v; the request's host must reach the dialer", asked)
	}
}
