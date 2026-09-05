package probe

import (
	"strings"
	"testing"
)

func testCatalog(t *testing.T) Catalog {
	t.Helper()
	catalog, err := NewCatalog([]Spec{
		{ID: "k8s.logs", Kind: KindExec,
			Argv: []string{"kubectl", "--context", "staging", "-n", "app", "logs", "{{pod}}", "--since", "{{since}}"},
			Args: map[string]string{"pod": `[a-z0-9-]{1,63}`, "since": `[1-9][0-9]{0,3}[smh]`}},
		{ID: "sql.read", Kind: KindSQL, DSNEnv: "PROBE_DB_DSN"},
		{ID: "http.timing", Kind: KindHTTP, Hosts: []string{"app-stg.example.invalid"}, Methods: []string{"GET", "HEAD"},
			Returns: []string{"status", "time_total", "bytes"}, Args: map[string]string{"path": `/[a-z0-9/_-]{0,80}`}, Cookies: CookiesObservationJar},
	})
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func TestCatalogRefusesOutOfShapeRequests(t *testing.T) {
	catalog := testCatalog(t)
	refused := []struct {
		name    string
		request Request
		reason  string
	}{
		{"unknown probe", Request{Probe: "k8s.exec", Args: map[string]string{"pod": "web-1"}}, "not in the catalogue"},
		{"undeclared slot", Request{Probe: "k8s.logs", Args: map[string]string{"pod": "web-1", "since": "5m", "tail": "10"}}, "has no slot"},
		{"missing slot", Request{Probe: "k8s.logs", Args: map[string]string{"pod": "web-1"}}, "needs slot"},
		{"pattern mismatch", Request{Probe: "k8s.logs", Args: map[string]string{"pod": "web-1", "since": "5 minutes"}}, "whitespace"},
		{"statement separator", Request{Probe: "k8s.logs", Args: map[string]string{"pod": "web-1;rm", "since": "5m"}}, "statement separator"},
		{"newline", Request{Probe: "k8s.logs", Args: map[string]string{"pod": "web-1\n", "since": "5m"}}, "whitespace"},
		{"control character", Request{Probe: "k8s.logs", Args: map[string]string{"pod": "web\x001", "since": "5m"}}, "control character"},
		{"uppercase pod", Request{Probe: "k8s.logs", Args: map[string]string{"pod": "Web-1", "since": "5m"}}, "declared shape"},
		{"empty value", Request{Probe: "k8s.logs", Args: map[string]string{"pod": "", "since": "5m"}}, "empty"},
		{"too long", Request{Probe: "k8s.logs", Args: map[string]string{"pod": strings.Repeat("a", MaxSlotValueLength+1), "since": "5m"}}, "too long"},
		{"sql update", Request{Probe: "sql.read", Args: map[string]string{"query": "UPDATE runs SET state = 'x'"}}, "declared shape"},
		{"sql two statements", Request{Probe: "sql.read", Args: map[string]string{"query": "SELECT 1; UPDATE runs SET state = 'x'"}}, "statement separator"},
		{"sql explain analyze", Request{Probe: "sql.read", Args: map[string]string{"query": "EXPLAIN ANALYZE DELETE FROM runs"}}, "declared shape"},
		{"http post", Request{Probe: "http.timing", Args: map[string]string{"path": "/console", "method": "POST"}}, "does not allow method"},
		{"http path outside shape", Request{Probe: "http.timing", Args: map[string]string{"path": "/console?x=1"}}, "declared shape"},
		{"http traversal", Request{Probe: "http.timing", Args: map[string]string{"path": "/../admin"}}, "declared shape"},
		{"leading dash", Request{Probe: "k8s.logs", Args: map[string]string{"pod": "-web", "since": "5m"}}, "dash"},
	}
	for _, tc := range refused {
		plan, refusal := catalog.Resolve(tc.request)
		if refusal == nil {
			t.Errorf("%s: accepted %+v as %+v", tc.name, tc.request, plan)
			continue
		}
		if !strings.Contains(refusal.Reason, tc.reason) {
			t.Errorf("%s: reason %q does not mention %q", tc.name, refusal.Reason, tc.reason)
		}
	}

	plan, refusal := catalog.Resolve(Request{Probe: "k8s.logs", Args: map[string]string{"pod": "web-1", "since": "5m"}})
	if refusal != nil {
		t.Fatalf("well-formed request refused: %v", refusal)
	}
	want := "kubectl --context staging -n app logs web-1 --since 5m"
	if got := strings.Join(plan.Argv, " "); got != want {
		t.Errorf("argv = %q, want %q", got, want)
	}
	plan, refusal = catalog.Resolve(Request{Probe: "http.timing", Args: map[string]string{"path": "/console"}})
	if refusal != nil || plan.Args["method"] != "GET" {
		t.Errorf("http default method: plan %+v refusal %v", plan, refusal)
	}
	plan, refusal = catalog.Resolve(Request{Probe: "sql.read", Args: map[string]string{"query": "SELECT count(*) FROM runs_shape WHERE state = 'queued'"}})
	if refusal != nil || plan.Spec.Kind != KindSQL {
		t.Errorf("sql select: plan %+v refusal %v", plan, refusal)
	}
	plan, refusal = catalog.Resolve(Request{Probe: "repo.list"})
	if refusal != nil || plan.Args["path"] != "" {
		t.Errorf("repo.list without path: plan %+v refusal %v", plan, refusal)
	}
}

func TestCatalogValidation(t *testing.T) {
	invalid := []struct {
		name string
		spec Spec
	}{
		{"bad id", Spec{ID: "K8S", Kind: KindExec, Argv: []string{"kubectl"}}},
		{"repo declared by consumer", Spec{ID: "repo.extra", Kind: KindRepo}},
		{"slot without pattern", Spec{ID: "x", Kind: KindExec, Argv: []string{"kubectl", "{{pod}}"}}},
		{"pattern never used", Spec{ID: "x", Kind: KindExec, Argv: []string{"kubectl"}, Args: map[string]string{"pod": `[a-z]+`}}},
		{"empty argv", Spec{ID: "x", Kind: KindExec}},
		{"sql without dsn", Spec{ID: "x", Kind: KindSQL}},
		{"http without hosts", Spec{ID: "x", Kind: KindHTTP, Methods: []string{"GET"}, Args: map[string]string{"path": "/"}}},
		{"http uppercase host", Spec{ID: "x", Kind: KindHTTP, Hosts: []string{"App.example.invalid"}, Methods: []string{"GET"}, Args: map[string]string{"path": "/"}}},
		{"http ip literal host", Spec{ID: "x", Kind: KindHTTP, Hosts: []string{"10.0.0.1"}, Methods: []string{"GET"}, Args: map[string]string{"path": "/"}}},
		{"http post", Spec{ID: "x", Kind: KindHTTP, Hosts: []string{"a.example.invalid"}, Methods: []string{"POST"}, Args: map[string]string{"path": "/"}}},
		{"http without path", Spec{ID: "x", Kind: KindHTTP, Hosts: []string{"a.example.invalid"}, Methods: []string{"GET"}}},
		{"http body return", Spec{ID: "x", Kind: KindHTTP, Hosts: []string{"a.example.invalid"}, Methods: []string{"GET"}, Returns: []string{"body"}, Args: map[string]string{"path": "/"}}},
		{"http unknown jar", Spec{ID: "x", Kind: KindHTTP, Hosts: []string{"a.example.invalid"}, Methods: []string{"GET"}, Cookies: "browser", Args: map[string]string{"path": "/"}}},
		{"unknown kind", Spec{ID: "x", Kind: Kind("shell")}},
		{"duplicate of a built-in", Spec{ID: "repo.read", Kind: KindExec, Argv: []string{"cat"}}},
	}
	for _, tc := range invalid {
		if _, err := NewCatalog([]Spec{tc.spec}); err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
	}
	catalog := testCatalog(t)
	if err := catalog.ForbidHosts([]string{"login.example.invalid"}); err != nil {
		t.Errorf("unrelated forbidden host: %v", err)
	}
	if err := catalog.ForbidHosts([]string{"app-stg.example.invalid"}); err == nil {
		t.Error("http probe addressing the forbidden host was accepted")
	}
	parsed, err := ParseCatalog([]byte(`[{"id":"k8s.pods","kind":"exec","argv":["kubectl","get","pods","-o","wide"]}]`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := parsed.Lookup("k8s.pods"); !ok {
		t.Error("parsed catalogue lost the declared probe")
	}
	if len(parsed.Specs()) != len(Builtins())+1 {
		t.Errorf("catalogue has %d specs", len(parsed.Specs()))
	}
}

// An http probe with several hosts is addressed with a host argument; a
// host outside the list is refused, and a probe with one host still takes
// its own host by name.
func TestHTTPProbeHostArgumentSelectsAmongDeclaredHosts(t *testing.T) {
	catalog, err := NewCatalog([]Spec{
		{ID: "http.timing", Kind: KindHTTP, Hosts: []string{"console.example.invalid", "api.example.invalid"}, Methods: []string{"GET"},
			Returns: []string{"status", "time_total", "bytes"}, Args: map[string]string{"path": `/[a-z0-9/_-]{0,80}`}},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, refusal := catalog.Resolve(Request{Probe: "http.timing", Args: map[string]string{"host": "api.example.invalid", "path": "/health"}})
	if refusal != nil || plan.Args["host"] != "api.example.invalid" {
		t.Fatalf("second host not selected: plan %+v refusal %v", plan, refusal)
	}
	if _, refusal = catalog.Resolve(Request{Probe: "http.timing", Args: map[string]string{"host": "other.example.invalid", "path": "/"}}); refusal == nil || !strings.Contains(refusal.Reason, "does not address host") {
		t.Fatalf("host outside the list accepted: %v", refusal)
	}
	if plan, refusal = catalog.Resolve(Request{Probe: "http.timing", Args: map[string]string{"path": "/"}}); refusal != nil || plan.Args["host"] != "console.example.invalid" {
		t.Fatalf("host-less request must record the first host: plan %+v refusal %v", plan, refusal)
	}
	if _, refusal = catalog.Resolve(Request{Probe: "http.timing", Args: map[string]string{"host": "api.example.invalid\n", "path": "/"}}); refusal == nil || !strings.Contains(refusal.Reason, "host") {
		t.Fatalf("host with a control character accepted: %v", refusal)
	}
	single := testCatalog(t)
	if _, refusal = single.Resolve(Request{Probe: "http.timing", Args: map[string]string{"host": "app-stg.example.invalid", "path": "/console"}}); refusal != nil {
		t.Fatalf("a probe's own host refused by name: %v", refusal)
	}
}
