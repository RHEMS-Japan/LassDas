package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
)

// writeOutcomeRecord seals one record in a make-believe run directory, the
// way the card that owns it would.
func writeOutcomeRecord(t *testing.T, runDir, name string, content any) {
	t.Helper()
	path := filepath.Join(runDir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("directory for %s: %v", name, err)
	}
	var encoded []byte
	switch value := content.(type) {
	case string:
		encoded = []byte(value)
	default:
		marshalled, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("encode %s: %v", name, err)
		}
		encoded = marshalled
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// A delivery that reached production says what was asked for as a thing that
// is done, and then says what was actually seen on the screen it was
// confirmed on. "Delivered and verified" is a claim about a screen, and the
// requester is owed the screen.
func TestTheOutcomeSaysWhatTheObservationSaw(t *testing.T) {
	runDir := t.TempDir()
	writeOutcomeRecord(t, runDir, "readiness-ticket.json", map[string]string{
		"request": "注文履歴を月ごとに絞り込めるようにする",
	})
	writeOutcomeRecord(t, runDir, DeliverProductionReportFile, DeliverReport{
		SchemaVersion: 1, Phase: "production", Verdict: "pass",
		TargetURL: "https://www.example.com/orders", ExpectedText: "月で絞り込む",
		ScreenChecked: true, ObservedAt: time.Now().UTC(),
	})
	evidence := map[string]string{
		"reached_delivery":        "production",
		"production_evidence_url": "https://www.example.com/orders",
	}

	outcome := composeOutcomeText(runDir, hook.TerminalSuccess, evidence)
	for name, want := range map[string]string{
		"what was asked for":      "注文履歴を月ごとに絞り込めるようにする",
		"the heading":             "## どこで見られるか",
		"the screen":              "https://www.example.com/orders",
		"what was seen on it":     "「月で絞り込む」が表示されているのを確認しました",
		"which place it was seen": "本番の画面",
	} {
		if !strings.Contains(outcome, want) {
			t.Errorf("%s is missing from the outcome:\n%s", name, outcome)
		}
	}

	// A phase that passed without looking at a screen must not read as one
	// that did: the older comment's "verified" said the same thing for both.
	writeOutcomeRecord(t, runDir, DeliverProductionReportFile, DeliverReport{
		SchemaVersion: 1, Phase: "production", Verdict: "pass",
		TargetURL: "https://www.example.com/orders", ObservedAt: time.Now().UTC(),
	})
	unlooked := composeOutcomeText(runDir, hook.TerminalSuccess, evidence)
	if !strings.Contains(unlooked, "表示の照合は行っていません") {
		t.Errorf("a pass with no screen check claims one:\n%s", unlooked)
	}

	// Nothing deployed anywhere: the outcome says so rather than naming a
	// place the change never reached.
	proposal := composeOutcomeText(t.TempDir(), hook.TerminalSuccess, map[string]string{
		"reached_delivery": "pull_request",
	})
	if !strings.Contains(proposal, "まだ動いている場所はありません") {
		t.Errorf("a proposal-only delivery points at a screen:\n%s", proposal)
	}
}

// Everything the engine settled without asking reaches the ticket: what the
// reception decided instead of putting to the requester, what was ruled when
// a round stopped agreeing, what was stood in for a key, which roles were
// run somewhere else, and what now exists outside the repository.
func TestEveryDecisionTheEngineMadeAloneReachesTheReport(t *testing.T) {
	runDir := t.TempDir()
	writeOutcomeRecord(t, runDir, "history/readiness/assessment-1.json", map[string]any{
		"assumptions": []runAssumption{
			{Kind: assumptionDefensibleDefault, Statement: "絞り込みの初期値は今月とする", Evidence: "一覧が今月から始まるため"},
			{Kind: "repository_convention", Statement: "日付の書式は既存の一覧に合わせる", Evidence: "同じ画面の既存表示"},
		},
	})
	writeOutcomeRecord(t, runDir, "history/assumptions.jsonl",
		`{"kind":"seat_moved","statement":"レビュー役を別の提供元に移した","evidence":"26 分無応答"}`+"\n")
	writeOutcomeRecord(t, runDir, "history/stage-1/ruling.json", map[string]any{
		"ruling":      "overrule_reviewer",
		"instruction": "",
		"overruled":   []map[string]string{{"reviewer_id": "review-a", "code": "style", "path": "a.go"}},
		"assumption": runAssumption{
			Kind: assumptionArbiterRuling, Statement: "依頼の範囲を越えた指摘は退けた", Evidence: "検収条件に無いため",
		},
	})
	writeOutcomeRecord(t, runDir, "history/stage-1/returns.json", map[string]any{
		"schema_version": 1, "stage": 1,
		"returns": []map[string]any{{
			"supply": []string{"本番の決済 API の鍵"},
			"assumption": runAssumption{
				Kind: assumptionCredentialStandIn, Statement: "決済 API は代役で組んだ", Evidence: "鍵が渡されていないため",
			},
		}},
	})
	writeOutcomeRecord(t, runDir, "history/stage-2/review-a-seat.json", SeatRecord{
		SchemaVersion: SeatRecordSchemaVersion, Seat: "review-a", Stage: "review-a", Round: 2,
		Candidate: 1, MovedFrom: SeatOccupantNote{Vendor: "vendor-one", Model: "model-a"},
		MovedTo: SeatOccupantNote{Vendor: "vendor-two", Model: "model-b"},
		Reason:  "26 分応答がなかったため", At: time.Now().UTC(),
	})
	writeOutcomeRecord(t, runDir, "history/resources.jsonl",
		`{"kind":"queue","identifier":"orders-export","provider":"aws","stage":"implement"}`+"\n")

	decided := composeAssumptionsText(runDir)
	for name, want := range map[string]string{
		"the point decided instead of asked":  "絞り込みの初期値は今月とする",
		"its reason":                          "一覧が今月から始まるため",
		"the point settled from the repo":     "日付の書式は既存の一覧に合わせる",
		"the decision made mid-run":           "レビュー役を別の提供元に移した",
		"the ruling":                          "依頼の範囲を越えた指摘は退けた",
		"the stand-in":                        "決済 API は代役で組んだ",
		"what must be supplied for the real":  "本番の決済 API の鍵",
		"the seat that moved":                 "review-a",
		"where it moved to":                   "vendor-two の model-b",
		"the resource that now exists":        "orders-export",
		"which kind of resource":              "queue",
		"the heading over what was decided":   "## 確認せずに本体が決めたこと",
		"the heading over what was assumed":   "## 前提とした解釈",
		"the heading over the resources made": "## この依頼で作った資源",
	} {
		if !strings.Contains(decided, want) {
			t.Errorf("%s is missing:\n%s", name, decided)
		}
	}
	if len(decided) > hook.MaxAssumptionsTextBytes {
		t.Errorf("the list is %d bytes, over its %d", len(decided), hook.MaxAssumptionsTextBytes)
	}

	// A run that decided nothing writes nothing at all, rather than a
	// heading over an empty list.
	if empty := composeAssumptionsText(t.TempDir()); empty != "" {
		t.Errorf("a run that decided nothing still wrote %q", empty)
	}

	// The record the arbitrating role writes is not merged here yet, so a
	// ruling with no assumption of its own still has to reach the ticket.
	bare := t.TempDir()
	writeOutcomeRecord(t, bare, "history/stage-3/ruling.json", map[string]any{
		"ruling": "instruct_implementer", "instruction": "一覧に合計件数を出すこと",
	})
	if text := composeAssumptionsText(bare); !strings.Contains(text, "一覧に合計件数を出すこと") {
		t.Errorf("a ruling without its own assumption vanished:\n%s", text)
	}
}

// A run whose card died mid-round still says what happened and what the
// engine did about it. Live on 2026-09-25 the whole of such a report was the
// one line saying the record could not be composed, while the failure and
// the climb were both already written down beside it.
func TestARunThatDiedMidCardStillSaysWhatHappened(t *testing.T) {
	runDir := t.TempDir()
	writeOutcomeRecord(t, runDir, "history/stage-2/implement-failure.json", StageFailure{
		SchemaVersion: StageFailureSchemaVersion, Stage: "implement", Round: 2,
		Class: FailureClassModel, Interrupted: true, Error: "the implement step was stopped part-way",
		FailedAt: time.Now().UTC(),
	})
	writeOutcomeRecord(t, runDir, "retry/implement-r2.json", map[string]any{
		"schema_version": 1, "stage": "implement", "round": 2, "attempts": 4,
		"ladder_step": 5, "tried": []string{"seat:1", "prompt:shorten", "seat:2"},
	})
	evidence := map[string]string{"failed_step": "AI による変更の作成"}

	outcome := composeOutcomeText(runDir, hook.TerminalModelFailed, evidence)
	for name, want := range map[string]string{
		"which step stopped":   "AI による変更の作成",
		"which round":          "2 周目",
		"that it was stopped":  "工程が途中で止まりました",
		"the heading":          "## 本体が試したこと",
		"the seat that moved":  "別のモデルと別の提供元に替えて頼み直しました",
		"the rebuilt request":  "指示の出し方を変えて頼み直しました",
		"how often it retried": "4 回やり直しました",
	} {
		if !strings.Contains(outcome, want) {
			t.Errorf("%s is missing from the account:\n%s", name, outcome)
		}
	}
	// The same remedy played twice is one sentence, not two.
	if count := strings.Count(outcome, "別のモデルと別の提供元に替えて頼み直しました"); count != 1 {
		t.Errorf("the same remedy is listed %d times:\n%s", count, outcome)
	}

	// The key's allowance is the one failure no remedy reaches, so it says
	// what a person has to do rather than what the engine tried.
	spent := t.TempDir()
	writeOutcomeRecord(t, spent, "history/stage-1/review-a-failure.json", StageFailure{
		SchemaVersion: StageFailureSchemaVersion, Stage: "review-a", Round: 1,
		Class: FailureClassCredit, Error: "the provider refused", FailedAt: time.Now().UTC(),
	})
	account := composeOutcomeText(spent, hook.TerminalModelFailed, map[string]string{
		"failed_step": "AI による変更のレビュー",
	})
	if !strings.Contains(account, "利用枠を使い切りました") {
		t.Errorf("a spent allowance is not named:\n%s", account)
	}

	// A delivery that finished says nothing about failures.
	if finished := composeOutcomeText(runDir, hook.TerminalSuccess, evidence); strings.Contains(finished, "何が起きたか") {
		t.Errorf("a success carries a failure account:\n%s", finished)
	}
}

// The envelope is the one bound that cannot be given way: a report too large
// to marshal is a delivery that ends saying nothing at all. So the record
// gives way to the two composed sections, and they give way to each other in
// the order the reader values them.
func TestTheRunRecordGivesWaySoTheReportCanBeSent(t *testing.T) {
	report := hook.TerminalReportRequest{
		Protocol:          hook.TerminalReportProtocolVersion,
		DeliveryID:        "delivery_0123456789abcdef0123456789abcdef",
		InputSHA256:       strings.Repeat("1", 64),
		RepositoryID:      42,
		RepositorySHA256:  strings.Repeat("a", 64),
		WorkflowRefSHA256: strings.Repeat("b", 64),
		WorkflowSHA:       strings.Repeat("2", 40),
		WorkflowRunID:     123456789,
		RunAttempt:        1,
		AutomationRunID:   "run_20260802_alpha",
		Code:              hook.TerminalSuccess,
		Repository:        "example/target",
		RunURL:            "https://github.com/example/automation-receiver/actions/runs/123456789/attempts/1",
		PullRequestURL:    "https://github.com/example/target/pull/42",
		OutcomeText:       strings.Repeat("成", hook.MaxOutcomeTextBytes/3-1),
		AssumptionsText:   strings.Repeat("決", hook.MaxAssumptionsTextBytes/3-1),
	}
	fitReportText(&report, strings.Repeat("記録の行\n", hook.MaxTerminalTrailBytes/10))
	report.IssuedAt = time.Now().UTC()
	encoded, err := hook.MarshalTerminalReportRequest(report)
	if err != nil {
		t.Fatalf("the report cannot be sent at all: %v", err)
	}
	if len(encoded) > hook.MaxTerminalReportRequestBytes {
		t.Fatalf("the report is %d bytes, over the envelope's %d", len(encoded), hook.MaxTerminalReportRequestBytes)
	}
	if report.OutcomeText == "" || report.AssumptionsText == "" {
		t.Errorf("the outcome or the decisions gave way before the record did")
	}
	if report.TrailText == "" {
		t.Errorf("the record was dropped when it could have been shortened")
	}

	// A run whose record alone fills the envelope keeps the outcome and
	// drops the record, rather than being unable to report anything.
	crowded := report
	crowded.OutcomeText = strings.Repeat("成", hook.MaxOutcomeTextBytes/3-1)
	crowded.AssumptionsText = strings.Repeat("決", hook.MaxAssumptionsTextBytes/3-1)
	fitReportText(&crowded, strings.Repeat("記", hook.MaxTerminalReportRequestBytes))
	crowded.IssuedAt = time.Now().UTC()
	if _, err := hook.MarshalTerminalReportRequest(crowded); err != nil {
		t.Fatalf("a long record left the report unsendable: %v", err)
	}
	if crowded.OutcomeText == "" {
		t.Errorf("the outcome gave way to the record")
	}
}

// The pull request the engine opens leads with the same answer, and never
// names a screen: nothing has been merged or deployed when it is written.
func TestThePullRequestDescriptionLeadsWithWhatTheChangeIsFor(t *testing.T) {
	runDir := t.TempDir()
	writeOutcomeRecord(t, runDir, "readiness-ticket.json", map[string]string{
		"request": "注文履歴を月ごとに絞り込めるようにする",
	})
	writeOutcomeRecord(t, runDir, "history/readiness/assessment-1.json", map[string]any{
		"assumptions": []runAssumption{
			{Kind: assumptionDefensibleDefault, Statement: "絞り込みの初期値は今月とする", Evidence: "一覧が今月から始まるため"},
		},
	})
	writeOutcomeRecord(t, runDir, DeliverProductionReportFile, DeliverReport{
		SchemaVersion: 1, Phase: "production", Verdict: "pass",
		TargetURL: "https://www.example.com/orders", ObservedAt: time.Now().UTC(),
	})

	preamble := composeDeliveryPreamble(runDir)
	for name, want := range map[string]string{
		"what the change is for": "注文履歴を月ごとに絞り込めるようにする",
		"what was decided":       "絞り込みの初期値は今月とする",
	} {
		if !strings.Contains(preamble, want) {
			t.Errorf("%s is missing from the description:\n%s", name, preamble)
		}
	}
	if strings.Contains(preamble, "どこで見られるか") || strings.Contains(preamble, "example.com/orders") {
		t.Errorf("the description names a screen the change has not reached:\n%s", preamble)
	}
	if !strings.HasSuffix(preamble, "\n\n") {
		t.Errorf("the description's opening does not part from the record below it: %q", preamble)
	}
	if empty := composeDeliveryPreamble(t.TempDir()); empty != "" {
		t.Errorf("a run with nothing to say still wrote %q", empty)
	}
}

// The publish card hands the opening to the verb that writes the pull
// request, so the description a reviewer opens leads with the same answer
// the ticket does.
func TestThePublishCardHandsTheOpeningToThePullRequest(t *testing.T) {
	repo, _ := gitBaseRepo(t)
	pipeline, state := baseAdvancePipeline(t, "", repo)
	writeExecutable(t, pipeline.Config.ControllerBin, fmt.Sprintf(controllerScriptShared+`case "$verb" in
  publish-feature) printf '{}' > "$out"; exit 0 ;;
  create-feature-pr) printf '{"payload":{"pull_request":{"HTMLURL":"https://example.invalid/pr/9"}}}' > "$out"; exit 0 ;;
esac
exit 1
`, state))
	writeOutcomeRecord(t, pipeline.Workspace, "readiness-ticket.json", map[string]string{
		"request": "注文履歴を月ごとに絞り込めるようにする",
	})

	if outcome, err := pipeline.deliveryStage(context.Background(), 1, []string{"review-a", "review-b"}); err != nil || outcome.Code != "" {
		t.Fatalf("deliveryStage() = %+v, %v", outcome, err)
	}
	calls, err := os.ReadFile(filepath.Join(state, "calls.log"))
	if err != nil {
		t.Fatal(err)
	}
	var invocation string
	for _, line := range strings.Split(string(calls), "\n") {
		if strings.HasPrefix(line, "create-feature-pr ") {
			invocation = line
		}
	}
	if invocation == "" {
		t.Fatalf("the pull request was never created:\n%s", calls)
	}
	_, after, found := strings.Cut(invocation, "--outcome ")
	if !found {
		t.Fatalf("the opening was not handed over: %s", invocation)
	}
	path, _, _ := strings.Cut(after, " ")
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the opening was named but not written: %v", err)
	}
	if !strings.HasPrefix(string(written), "## この変更でできるようになること\n注文履歴を月ごとに絞り込めるようにする") {
		t.Errorf("the description does not lead with what the change is for:\n%s", written)
	}
}

// A stop is no longer the same as nothing having happened: a requester can
// stop a delivery whose change is already merged, deployed and looked at.
// The outcome then names the environment they have to go and see, and what
// was seen on it, rather than sending them away.
func TestAStopThatLandedSomewhereStillSaysWhereToLook(t *testing.T) {
	runDir := t.TempDir()
	writeOutcomeRecord(t, runDir, "readiness-ticket.json", map[string]string{
		"request": "注文履歴を月ごとに絞り込めるようにする",
	})
	writeOutcomeRecord(t, runDir, DeliverProductionReportFile, DeliverReport{
		SchemaVersion: 1, Phase: "production", Verdict: "pass",
		TargetURL: "https://www.example.com/orders", ExpectedText: "月で絞り込む",
		ScreenChecked: true, ObservedAt: time.Now().UTC(),
	})
	landed := composeOutcomeText(runDir, hook.TerminalCancelled, map[string]string{
		"reached_delivery":        "production",
		"production_evidence_url": "https://www.example.com/orders",
	})
	for name, want := range map[string]string{
		"the heading":         "## どこで見られるか",
		"the screen":          "https://www.example.com/orders",
		"what was seen on it": "「月で絞り込む」が表示されているのを確認しました",
	} {
		if !strings.Contains(landed, want) {
			t.Errorf("%s is missing from a stop that landed:\n%s", name, landed)
		}
	}
	// A stop is not a failure, so it carries no account of one.
	if strings.Contains(landed, "何が起きたか") {
		t.Errorf("a stop reads as a failure:\n%s", landed)
	}

	// A stop that reached nowhere invents no place to look: the stop's own
	// sentence already says what it left behind, which is nothing.
	nowhere := composeOutcomeText(t.TempDir(), hook.TerminalCancelled, map[string]string{})
	if strings.Contains(nowhere, "どこで見られるか") || strings.Contains(nowhere, "Pull Request") {
		t.Errorf("a stop that reached nowhere points somewhere:\n%s", nowhere)
	}
}
