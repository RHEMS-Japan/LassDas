package ticketview

import (
	"encoding/json"
	"fmt"
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
	write("history/readiness/check-1.json", `{"verdict":"pass","reasons":["前提が票と一致"],"model":"m","checked_at":"2026-09-15T01:02:00Z","invocation":{"requested_model":"m","request_id":"r3","stop_reason":"end_turn","input_tokens":10,"output_tokens":5,"cost_usd":0.30}}`)
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
	write("measurements.jsonl", `{"id":"p1","probe":"read","args":{"path":"app/login.tsx"},"started_at":"2026-09-15T01:04:00Z","output_bytes":120}`+"\n"+
		`{"id":"p2","probe":"http","args":{"url":"https://example.test"},"started_at":"2026-09-15T01:04:30Z","refused":true,"reason":"host not allowed"}`+"\n")
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
	var investigate *Event
	for i := range view.Timeline {
		if view.Timeline[i].Step == "investigate" {
			investigate = &view.Timeline[i]
		}
	}
	if investigate == nil || !strings.Contains(investigate.Title, "2") || !strings.Contains(fmt.Sprint(investigate.Evidence), "host not allowed") {
		t.Fatalf("measurements should show as one investigate event with the refusal: %+v", investigate)
	}
	for _, want := range []string{"intake", "readiness-assessment-1", "readiness-check-1", "stage-1-review-a", "stage-1-decision", "stage-2-review-a-run", "deliver-staging-report", "failed-step", "trail"} {
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
		"readiness-assessment-2":              filepath.Join("history", "readiness", "assessment-2.json"),
		"readiness-check-4":                   "",
		"stage-1-applier-run":                 filepath.Join("history", "stage-1", "applier-run.json"),
		"stage-2-my-reviewer-run":             filepath.Join("history", "stage-2", "my-reviewer-run.json"),
		"design-1-investigation":              filepath.Join("history", "design-1", "investigation.json"),
		"design-3-review-b-design-review-run": filepath.Join("history", "design-3", "review-b-design-review-run.json"),
		"design-0-design":                     "",
		"stage-1-Candidate":                   "",
		"stage-1-a.b":                         "",
		"intake":                              "intake.json",
		"stage-3-review-b-run":                filepath.Join("history", "stage-3", "review-b-run.json"),
		"stage-12-candidate":                  filepath.Join("history", "stage-12", "candidate.json"),
		"../ticket":                           "",
		"stage-1-../../etc/passwd":            "",
		"stage-x-candidate":                   "",
		"history/stage-1/candidate.json":      "",
		"":                                    "",
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

func TestBuildShowsEveryReadinessAttempt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "delivery_attempts")
	readiness := filepath.Join(dir, "history", "readiness")
	if err := os.MkdirAll(readiness, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"assessment-1.json": `{"decision":"ready","assumptions":[{"statement":"OLD ASSUMPTION"}],"needs_design":false,"design_reason":"trigger_word_absent","model":"m","invocation":{"cost_usd":0.2}}`,
		"check-1.json":      `{"verdict":"fail","reasons":["前提が票と食い違う"],"model":"m","checked_at":"2026-09-15T01:02:00Z","invocation":{"cost_usd":0.3}}`,
		"assessment-2.json": `{"decision":"ready","assumptions":[{"statement":"NEW ASSUMPTION"}],"needs_design":false,"design_reason":"trigger_word_absent","model":"m","invocation":{"cost_usd":0.2}}`,
		"check-2.json":      `{"verdict":"pass","reasons":["前提が票と一致"],"model":"m","checked_at":"2026-09-15T01:04:00Z","invocation":{"cost_usd":0.3}}`,
		"decision.json":     `{"request_kind":"change","needs_design":false,"design_reason":"trigger_word_absent","outcome":"ready"}`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(readiness, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	view, err := Build(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Timeline) != 2 {
		t.Fatalf("want the sent-back attempt and the decision, got %+v", titles(view.Timeline))
	}
	sentBack, final := view.Timeline[0], view.Timeline[1]
	if !strings.Contains(sentBack.Title, "1 回目") || sentBack.Tone != "warn" || !strings.Contains(sentBack.Why, "食い違う") || sentBack.Record != "readiness-check-1" {
		t.Fatalf("first attempt should show as sent back with the checker's reasons: %+v", sentBack)
	}
	raw, _ := json.Marshal(final)
	if !strings.Contains(final.Title, "実装へ") || !strings.Contains(final.Title, "2 回目で確定") || !strings.Contains(string(raw), "NEW ASSUMPTION") || strings.Contains(string(raw), "OLD ASSUMPTION") || !strings.Contains(string(raw), "判定: pass") {
		t.Fatalf("the decision must rest on the last attempt: %s", raw)
	}
	if got, want := view.Cost.TotalUSD, 1.0; got < want-1e-9 || got > want+1e-9 {
		t.Fatalf("both attempts are paid for: %v", view.Cost.Lines)
	}
	for _, want := range []string{"readiness-assessment-1", "readiness-check-1", "readiness-assessment-2", "readiness-check-2"} {
		if !contains(view.Records, want) {
			t.Fatalf("records should list %q: %v", want, view.Records)
		}
	}
}

func TestBuildMasksBeforeTakingATail(t *testing.T) {
	// A key that straddles the tail cut: cut first and its prefix is gone,
	// so the remainder no longer looks like a key and passes the scan.
	key := "sk-" + strings.Repeat("ABCDEFGHIJ", 4)
	transcript := strings.Repeat("x", 999) + " " + key + "\ntail text " + strings.Repeat("y", 760)
	dir := filepath.Join(t.TempDir(), "delivery_tail")
	stage := filepath.Join(dir, "history", "stage-1")
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	run, _ := json.Marshal(map[string]any{"ran_at": "2026-09-15T01:12:00Z", "transcript": transcript})
	if err := os.WriteFile(filepath.Join(stage, "review-a-run.json"), run, 0o644); err != nil {
		t.Fatal(err)
	}
	// The trail's tail is longer (1200): straddle that cut too.
	trail := strings.Repeat("x", 1000) + " " + key + "\ntail " + strings.Repeat("y", 1170)
	if err := os.WriteFile(filepath.Join(dir, "m1-trail.txt"), []byte(trail), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "failed-step.txt"), []byte("review\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	view, err := Build(dir)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(view)
	if strings.Contains(string(raw), key[10:]) {
		t.Fatalf("a key fragment survived the tail: %s", raw)
	}
	if strings.Count(string(raw), "[masked:") < 2 {
		t.Fatalf("both the review tail and the failure detail should carry the marker: %s", raw)
	}
}

func TestBuildReadsDesignRoundsAndApplierRuns(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "delivery_design")
	design := filepath.Join(dir, "history", "design-1")
	stage := filepath.Join(dir, "history", "stage-1")
	for _, d := range []string{design, stage} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join(design, "investigation.json"):              `{"round":1,"probes_used":7,"elapsed_seconds":95,"questions":["どこで落ちるか"],"findings":[{"claim":"config.go の既定値が空","evidence":["p3"],"confidence":"high"}],"unknowns":["本番の値"],"next":"既定値を入れる"}`,
		filepath.Join(design, "design.json"):                     `{"round":1,"cause":"既定値が空","cause_evidence":["p3"],"approach":"既定値を 30 にする","alternatives":["起動時に検査"],"files":[{"path":"internal/x/config.go","changes":["既定値を追加"]}],"verification":{"form":"file_text","path":"internal/x/config.go","expected_text":"30"},"blast_radius":["x の利用者"],"not_doing":["設定画面"]}`,
		filepath.Join(design, "review-a-design-review.json"):     `{"reviewer_id":"review-a","model":"m","lens":"safety","verdict":"revise","findings":[{"code":"D01","section":"approach","message":"既定値の根拠が無い"}],"reviewed_at":"2026-09-15T02:00:00Z","invocation":{"cost_usd":0.7}}`,
		filepath.Join(design, "review-b-design-review-run.json"): `{"ran_at":"2026-09-15T02:01:00Z","transcript":"thinking... TOKEN=sk-ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789abcd no answer"}`,
		filepath.Join(design, "decision.json"):                   `{"round":1,"outcome":"revise"}`,
		filepath.Join(design, "objection.json"):                  `{"reason":"files に無いファイルを触る必要がある","section":"files","raised_at":"2026-09-15T02:05:00Z"}`,
		filepath.Join(stage, "applier-run.json"):                 `{"agent_id":"applier","ran_at":"2026-09-15T02:03:00Z","duration_ms":5000,"exit_code":0,"changed_files":[],"transcript":"could not follow the design"}`,
	}
	for path, body := range files {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	view, err := Build(dir)
	if err != nil {
		t.Fatal(err)
	}
	steps := map[string]int{}
	for _, e := range view.Timeline {
		steps[e.Step]++
	}
	if steps["investigate"] != 1 || steps["design"] != 1 || steps["design-review"] != 2 || steps["objection"] != 1 || steps["implement"] != 1 {
		t.Fatalf("design round not read in full: %v (%v)", steps, titles(view.Timeline))
	}
	raw, _ := json.Marshal(view)
	for _, want := range []string{"既定値を 30 にする", "D01 (approach)", "判定を返せなかった", "この巡の結論: 差し戻し", "実装役の異議 (設計 1 巡目の files)", "設計に沿った実装 1 巡目: 変更が封緘されなかった", "発見 1 (high)", "[masked:"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("view should carry %q: %s", want, raw)
		}
	}
	if strings.Contains(string(raw), "KLMNOPQRSTUVWXYZ0123456789abcd") {
		t.Fatalf("token leaked: %s", raw)
	}
	if view.Cost.TotalUSD < 0.7-1e-9 {
		t.Fatalf("design review price missing: %+v", view.Cost)
	}
	for _, want := range []string{"design-1-investigation", "design-1-design", "design-1-review-a-design-review", "design-1-review-b-design-review-run", "design-1-decision", "design-1-objection", "stage-1-applier-run"} {
		if !contains(view.Records, want) {
			t.Fatalf("records should list %q: %v", want, view.Records)
		}
	}
}

func TestBuildTakesTheRecordTimeWhenAnAttemptHasNoCheck(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "delivery_nocheck")
	readiness := filepath.Join(dir, "history", "readiness")
	if err := os.MkdirAll(readiness, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(readiness, "assessment-1.json")
	if err := os.WriteFile(path, []byte(`{"decision":"ready","model":"m"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2026, 9, 15, 3, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	view, err := Build(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Timeline) != 1 || !view.Timeline[0].At.Equal(stamp) || view.Timeline[0].Record != "readiness-assessment-1" {
		t.Fatalf("an attempt that died before its check keeps the record's own time: %+v", view.Timeline)
	}
}

func TestDeliverTitleTreatsAFailedPromotionAsAFailure(t *testing.T) {
	if _, tone := deliverTitle("本番反映", "promotion_failed"); tone != "bad" {
		t.Fatalf("promotion_failed tone = %q", tone)
	}
}
