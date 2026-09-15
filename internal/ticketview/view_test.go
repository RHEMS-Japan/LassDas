package ticketview

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixtureRun writes a run directory shaped like the ones the pod records:
// a ticket that was read, judged "no design", implemented in two stages
// (the second review returned no verdict), validated, opened as a PR,
// merged, checked on staging, and then failed on a budget-exhausted key.
func fixtureRun(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "delivery_fixture")
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("readiness-ticket.json", `{"issue_key":"PROJ-42","run_id":"delivery_fixture","repository":"example/repo","summary":"ログイン画面の文言修正","request":"ボタンの文言を「送信」に変える","target_files":["app/login.tsx"]}`)
	write("intake.json", `{"model":"m","read_at":"2026-09-15T01:00:00Z","rationale":"文言変更のみ","request":"ボタンの文言を「送信」に変える","invocation":{"requested_model":"m","request_id":"r1","stop_reason":"end_turn","input_tokens":10,"output_tokens":5,"cost_usd":0.10}}`)
	write("history/readiness/assessment-1.json", `{"decision":"ready","questions":[],"assumptions":[{"kind":"scope","statement":"対象は 1 ファイル","evidence":"target_files"}],"request_kind":"change","approach_in_ticket":true,"approach_excerpt":"文言を置換する","needs_design":false,"design_reason":"trigger_word_absent","model":"m","invocation":{"requested_model":"m","request_id":"r2","stop_reason":"end_turn","input_tokens":10,"output_tokens":5,"cost_usd":0.20}}`)
	write("history/readiness/check-1.json", `{"verdict":"agree","reasons":["前提が票と一致"],"model":"m","checked_at":"2026-09-15T01:02:00Z","invocation":{"requested_model":"m","request_id":"r3","stop_reason":"end_turn","input_tokens":10,"output_tokens":5,"cost_usd":0.30}}`)
	write("history/readiness/decision.json", `{"request_kind":"change","needs_design":false,"design_reason":"trigger_word_absent","outcome":"ready"}`)
	write("history/stage-1/candidate.json", `{"generated_at":"2026-09-15T01:05:00Z","files":[{"path":"app/login.tsx","before_sha256":"abc","content":"x"}],"rationale":"置換","invocation":{"requested_model":"m","request_id":"r4","stop_reason":"end_turn","input_tokens":10,"output_tokens":5,"cost_usd":0.40},"implementer":{"model":"m"}}`)
	write("history/stage-1/implement-run.json", `{"agent_id":"impl","ran_at":"2026-09-15T01:06:00Z","duration_ms":1200,"exit_code":0,"changed_files":["app/login.tsx"],"transcript":"changed the label"}`)
	write("history/stage-1/review-a.json", `{"reviewer_id":"a","model":"m","verdict":"revise","findings":[{"code":"F01","path":"app/login.tsx","line":12,"message":"テストが無い"}],"reviewed_at":"2026-09-15T01:08:00Z","invocation":{"requested_model":"m","request_id":"r5","stop_reason":"end_turn","input_tokens":10,"output_tokens":5}}`)
	write("history/stage-1/decision.json", `{"outcome":"revise"}`)
	write("history/stage-2/candidate.json", `{"generated_at":"2026-09-15T01:10:00Z","files":[{"path":"app/login.tsx","before_sha256":"abd","content":"y"}],"rationale":"テスト追加","invocation":{"requested_model":"m","request_id":"r6","stop_reason":"end_turn","input_tokens":10,"output_tokens":5,"cost_usd":0.50},"implementer":{"model":"m"}}`)
	write("history/stage-2/review-a-run.json", `{"transcript":"reading config... DATABASE_URL=postgres://app:hunter2secret@db.example/app\nthen the budget ran out","ran_at":"2026-09-15T01:12:00Z"}`)
	write("validation.json", `{"started_at":"2026-09-15T01:13:00Z","completed_at":"2026-09-15T01:14:00Z","tools":[{"binary":"node","version":"22"}],"commands":["npm test"],"files":[{"path":"app/login.tsx","sha256":"abe"}]}`)
	write("feature-pr.json", `{"payload":{"pull_request":{"Number":7,"HTMLURL":"https://github.com/example/repo/pull/7","CreatedAt":"2026-09-15T01:15:00Z","BaseRef":"stg","HeadSHA":"deadbeef"}}}`)
	write("feature-checks.json", `{"payload":{"checks":{"WorkflowRunIDs":[1,2]}}}`)
	write("feature-merge.json", `{"payload":{"merge":{"MergeSHA":"0123456789abcdef","BaseBranch":"stg"}}}`)
	write("deliver-staging-report.json", `{"phase":"staging","verdict":"deploy_not_applicable","detail":"配布対象の path に触れていない","merged_sha":"0123456789abcdef","observed_at":"2026-09-15T01:20:00Z","target_url":""}`)
	write("board-outcome.json", `{"phase":"staging","verdict":"failed","note":"review-a の鍵の利用枠が上限","at":"2026-09-15T01:21:00Z"}`)
	write("failed-step.txt", "review\n")
	write("model-failure.json", `{"failed_step":"review","model_failure_reason":"budget_exhausted"}`)
	write("m1-trail.txt", "受付\n設計なし\nレビュー a: 鍵の利用枠が上限に達した\n")
	// Records without a timestamp of their own carry the time they were
	// written, as on the pod.
	for rel, at := range map[string]string{"feature-merge.json": "2026-09-15T01:16:00Z", "failed-step.txt": "2026-09-15T01:22:00Z"} {
		stamp, _ := time.Parse(time.RFC3339, at)
		if err := os.Chtimes(filepath.Join(dir, rel), stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	write("measurements.jsonl", `{"id":"p1","probe":"read","args":["app/login.tsx"],"started_at":"2026-09-15T01:04:00Z","refused":false,"masked":false,"output_bytes":120}`+"\n")
	return dir
}

func TestBuildAssemblesTheRunInOrderWithCostsAndMaskedSecrets(t *testing.T) {
	view, err := Build(fixtureRun(t))
	if err != nil {
		t.Fatal(err)
	}
	if view.IssueKey != "PROJ-42" || view.Summary != "ログイン画面の文言修正" || view.PullRequestURL != "https://github.com/example/repo/pull/7" || view.MergeSHA != "0123456789abcdef" {
		t.Fatalf("header not read from the records: %+v", view)
	}
	if len(view.Timeline) < 8 {
		t.Fatalf("timeline too short: %d events", len(view.Timeline))
	}
	for i := 1; i < len(view.Timeline); i++ {
		if view.Timeline[i].At.Before(view.Timeline[i-1].At) {
			t.Fatalf("timeline out of order at %d: %v before %v", i, view.Timeline[i].At, view.Timeline[i-1].At)
		}
	}
	// Costs: every recorded price, and nothing invented for the review
	// (which records no price).
	if got, want := view.Cost.TotalUSD, 0.10+0.20+0.30+0.40+0.50; got < want-1e-9 || got > want+1e-9 {
		t.Fatalf("cost total = %v, want %v (%+v)", got, want, view.Cost.Lines)
	}
	if view.Cost.Note == "" {
		t.Fatal("the page must say that review calls carry no price in the records")
	}
	// The review that returned no verdict is shown as such, with its output
	// masked: the connection string's password never reaches the page.
	var noVerdict *Event
	for i := range view.Timeline {
		if strings.Contains(view.Timeline[i].Title, "判定を返せなかった") {
			noVerdict = &view.Timeline[i]
		}
	}
	if noVerdict == nil {
		t.Fatalf("missing the no-verdict review event: %+v", titles(view.Timeline))
	}
	raw, _ := json.Marshal(view)
	if strings.Contains(string(raw), "hunter2secret") {
		t.Fatalf("secret value leaked into the view: %s", raw)
	}
	if !strings.Contains(string(raw), "[masked:") {
		t.Fatalf("masked review output should keep a marker: %s", raw)
	}
	if view.Failure == nil || view.Failure.Step != "review" || view.Failure.Reason == "" {
		t.Fatalf("failure not read: %+v", view.Failure)
	}
	if !strings.Contains(view.Failure.Detail, "鍵の利用枠") {
		t.Fatalf("failure detail should carry the trail tail: %q", view.Failure.Detail)
	}
	for _, want := range []string{"intake", "readiness-assessment-1", "stage-1-review-a", "stage-2-review-a-run", "deliver-staging-report", "failed-step", "trail"} {
		if !contains(view.Records, want) {
			t.Fatalf("records should list %q: %v", want, view.Records)
		}
	}
	if contains(view.Records, "deliver-production-report") {
		t.Fatalf("records must list only files that exist: %v", view.Records)
	}
}

func TestBuildShowsWhatEarlyRunsRecordedSoFar(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "delivery_early")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "intake.json"), []byte(`{"model":"m","read_at":"2026-09-15T01:00:00Z","rationale":"読んだ","request":"x","invocation":{"cost_usd":0.1}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	view, err := Build(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Timeline) != 1 || view.Timeline[0].Step != "intake" || view.Failure != nil {
		t.Fatalf("early run should show only the intake: %+v", view)
	}
	if _, err := Build(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("a missing run directory must be an error, not an empty page")
	}
}

func TestRecordPathServesOnlyKnownNames(t *testing.T) {
	for name, want := range map[string]string{
		"intake":                         "intake.json",
		"stage-3-review-b-run":           filepath.Join("history", "stage-3", "review-b-run.json"),
		"stage-12-candidate":             filepath.Join("history", "stage-12", "candidate.json"),
		"../ticket":                      "",
		"stage-1-../../etc/passwd":       "",
		"stage-x-candidate":              "",
		"history/stage-1/candidate.json": "",
		"":                               "",
	} {
		if got := RecordPath(name); got != want {
			t.Errorf("RecordPath(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestShownRefusesWholeSecretsAndBoundsLength(t *testing.T) {
	if got := shown("-----BEGIN RSA PRIVATE KEY-----\nabc\n-----END RSA PRIVATE KEY-----"); !strings.HasPrefix(got, "[秘密の形") {
		t.Fatalf("a private key must be refused whole: %q", got)
	}
	long := strings.Repeat("あ", maxText)
	if got := shown(long); !strings.HasSuffix(got, "…(以下略)") || len(got) > maxText+len(" …(以下略)") || !strings.HasPrefix(got, "あ") {
		t.Fatalf("long text must be cut on a rune boundary: len=%d", len(got))
	}
}

func titles(events []Event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.Title)
	}
	return out
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
