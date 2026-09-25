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
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/worker"
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

	outcome := composeOutcomeText(runDir, hook.TerminalSuccess, evidence, &outcomeNotes{})
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
	unlooked := composeOutcomeText(runDir, hook.TerminalSuccess, evidence, &outcomeNotes{})
	if !strings.Contains(unlooked, "表示の照合は行っていません") {
		t.Errorf("a pass with no screen check claims one:\n%s", unlooked)
	}

	// Nothing deployed anywhere: the outcome says so rather than naming a
	// place the change never reached.
	proposal := composeOutcomeText(t.TempDir(), hook.TerminalSuccess, map[string]string{
		"reached_delivery": "pull_request",
	}, &outcomeNotes{})
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

	decided := composeAssumptionsText(runDir, "", &outcomeNotes{})
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
	if empty := composeAssumptionsText(t.TempDir(), "", &outcomeNotes{}); empty != "" {
		t.Errorf("a run that decided nothing still wrote %q", empty)
	}

	// The record the arbitrating role writes is not merged here yet, so a
	// ruling with no assumption of its own still has to reach the ticket.
	bare := t.TempDir()
	writeOutcomeRecord(t, bare, "history/stage-3/ruling.json", map[string]any{
		"ruling": "instruct_implementer", "instruction": "一覧に合計件数を出すこと",
	})
	if text := composeAssumptionsText(bare, "", &outcomeNotes{}); !strings.Contains(text, "一覧に合計件数を出すこと") {
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

	outcome := composeOutcomeText(runDir, hook.TerminalModelFailed, evidence, &outcomeNotes{})
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
	}, &outcomeNotes{})
	if !strings.Contains(account, "利用枠を使い切りました") {
		t.Errorf("a spent allowance is not named:\n%s", account)
	}

	// A delivery that finished says nothing about failures.
	if finished := composeOutcomeText(runDir, hook.TerminalSuccess, evidence, &outcomeNotes{}); strings.Contains(finished, "何が起きたか") {
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
	}, &outcomeNotes{})
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
	nowhere := composeOutcomeText(runDir, hook.TerminalCancelled, map[string]string{}, &outcomeNotes{})
	if strings.Contains(nowhere, "どこで見られるか") || strings.Contains(nowhere, "Pull Request") {
		t.Errorf("a stop that reached nowhere points somewhere:\n%s", nowhere)
	}
	if strings.Contains(nowhere, "できるようになったこと") {
		t.Errorf("a stop that reached nowhere opens as fulfilled:\n%s", nowhere)
	}
}

// The heading over the request is the whole difference between a report and
// a false completion. A run whose implementation card was killed rendered
// 「この依頼でできるようになったこと」 above the request and 「完了しませんでした」
// three lines below it; nothing that did not get somewhere may say the first.
func TestAFailedRunNeverOpensAsIfTheRequestWereFulfilled(t *testing.T) {
	const fulfilled = "できるようになったこと"
	request := map[string]string{"request": "注文履歴を月ごとに絞り込めるようにする"}

	failed := t.TempDir()
	writeOutcomeRecord(t, failed, "readiness-ticket.json", request)
	writeOutcomeRecord(t, failed, "history/stage-2/implement-failure.json", StageFailure{
		SchemaVersion: StageFailureSchemaVersion, Stage: "implement", Round: 2,
		Class: FailureClassModel, Interrupted: true, Error: "stopped part-way", FailedAt: time.Now().UTC(),
	})
	account := composeOutcomeText(failed, hook.TerminalModelFailed, map[string]string{
		"failed_step": "AI による変更の作成",
	}, &outcomeNotes{})
	if strings.Contains(account, fulfilled) {
		t.Errorf("a run that never got anywhere opens as fulfilled:\n%s", account)
	}
	for _, want := range []string{"## お預かりした依頼", "注文履歴を月ごとに絞り込めるようにする", "完了しませんでした"} {
		if !strings.Contains(account, want) {
			t.Errorf("%q is missing from the account:\n%s", want, account)
		}
	}

	// Every ending that reached no environment reads the same way.
	for _, code := range []hook.TerminalCode{
		hook.TerminalInternalFailed, hook.TerminalValidationFailed, hook.TerminalNonconverged,
		hook.TerminalModelFailed, hook.TerminalReleaseFailed, hook.TerminalInputRejected,
		hook.TerminalClarificationRequired, hook.TerminalClarificationExpired,
		hook.TerminalImplementationReturned, hook.TerminalInvestigated, hook.TerminalCancelled,
	} {
		text := composeOutcomeText(failed, code, map[string]string{}, &outcomeNotes{})
		if strings.Contains(text, fulfilled) {
			t.Errorf("%s opens as fulfilled:\n%s", code, text)
		}
	}
}

// Only a change that is running somewhere made anything possible. A pull
// request asks for the change to be made, so a delivery that stopped at its
// proposal — and a stop that arrived with only a proposal behind it — have
// changed nothing a requester can go and use, however successfully they
// stopped there.
func TestOnlyAChangeThatIsRunningSaysWhatBecamePossible(t *testing.T) {
	const (
		fulfilled = "## この依頼でできるようになったこと"
		asked     = "## お預かりした依頼"
	)
	runDir := t.TempDir()
	writeOutcomeRecord(t, runDir, "readiness-ticket.json", map[string]string{
		"request": "注文履歴を月ごとに絞り込めるようにする",
	})
	proposal := map[string]string{
		"reached_delivery": "pull_request",
		// The depth a proposal-only delivery names is deliberately not the
		// test: this one names one too.
		"pull_request_url": "https://github.com/example/target/pull/42",
	}
	staging := map[string]string{
		"reached_delivery":     "integration",
		"pull_request_url":     "https://github.com/example/target/pull/42",
		"staging_evidence_url": "https://staging.example.com/orders",
	}
	production := map[string]string{
		"reached_delivery":        "production",
		"pull_request_url":        "https://github.com/example/target/pull/42",
		"staging_evidence_url":    "https://staging.example.com/orders",
		"production_evidence_url": "https://www.example.com/orders",
	}

	for _, shape := range []struct {
		name     string
		code     hook.TerminalCode
		evidence map[string]string
		running  bool
	}{
		{"a delivery confirmed in production", hook.TerminalSuccess, production, true},
		{"a delivery confirmed on staging", hook.TerminalSuccess, staging, true},
		{"a delivery that only proposed the change", hook.TerminalSuccess, proposal, false},
		{"a stop after production landed", hook.TerminalCancelled, production, true},
		{"a stop after staging landed", hook.TerminalCancelled, staging, true},
		{"a stop with only a pull request behind it", hook.TerminalCancelled, proposal, false},
		{"a stop that reached nowhere", hook.TerminalCancelled, map[string]string{}, false},
		{"an investigation", hook.TerminalInvestigated, map[string]string{}, false},
	} {
		text := composeOutcomeText(runDir, shape.code, shape.evidence, &outcomeNotes{})
		if shape.running && (!strings.Contains(text, fulfilled) || strings.Contains(text, asked)) {
			t.Errorf("%s does not say what became possible:\n%s", shape.name, text)
		}
		if !shape.running && (strings.Contains(text, fulfilled) || !strings.Contains(text, asked)) {
			t.Errorf("%s says something became possible:\n%s", shape.name, text)
		}
		if !strings.Contains(text, "注文履歴を月ごとに絞り込めるようにする") {
			t.Errorf("%s lost the request itself:\n%s", shape.name, text)
		}
	}

	// A proposal-only delivery still says where to look, and what it names
	// is the pull request rather than an environment it never reached.
	text := composeOutcomeText(runDir, hook.TerminalSuccess, proposal, &outcomeNotes{})
	if !strings.Contains(text, "まだ動いている場所はありません") || !strings.Contains(text, "Pull Request") {
		t.Errorf("a proposal-only delivery does not name the pull request as where to look:\n%s", text)
	}
}

// Every decision lands where a person can read it. The pull request
// description is that place, so it holds the whole list rather than the
// comment's share of it.
func TestThePullRequestDescriptionHoldsEveryDecision(t *testing.T) {
	runDir := t.TempDir()
	writeOutcomeRecord(t, runDir, "readiness-ticket.json", map[string]string{"request": "一覧を直す"})
	assumptions := make([]runAssumption, 0, 20)
	for index := 1; index <= 20; index++ {
		assumptions = append(assumptions, runAssumption{
			Kind:      assumptionDefensibleDefault,
			Statement: fmt.Sprintf("決めたこと %02d 番", index),
			Evidence:  "ほかに擁護できる既定が立たないため",
		})
	}
	writeOutcomeRecord(t, runDir, "history/readiness/assessment-1.json", map[string]any{"assumptions": assumptions})

	preamble := composeDeliveryPreamble(runDir)
	for index := 1; index <= 20; index++ {
		if want := fmt.Sprintf("決めたこと %02d 番", index); !strings.Contains(preamble, want) {
			t.Errorf("%q never reached the description:\n%s", want, preamble)
		}
	}
	if strings.Contains(preamble, "ほか ") {
		t.Errorf("the description counted decisions away instead of carrying them:\n%s", preamble)
	}
	// Whatever the description cannot carry, it never sends a reader to
	// itself: it is the page they are already reading. A list past even
	// the description's own reach is what proves it, so one is built.
	beyond := t.TempDir()
	past := make([]runAssumption, 0, deliveryListItems+50)
	for index := 1; index <= deliveryListItems+50; index++ {
		past = append(past, runAssumption{
			Kind:      assumptionDefensibleDefault,
			Statement: fmt.Sprintf("決めたこと %03d 番", index),
			Evidence:  "ほかに擁護できる既定が立たないため",
		})
	}
	writeOutcomeRecord(t, beyond, "history/readiness/assessment-1.json", map[string]any{"assumptions": past})
	overflowed := composeDeliveryPreamble(beyond)
	if !strings.Contains(overflowed, "ほか 50 件") {
		t.Fatalf("the fixture did not exceed the description's own reach:\n%s", overflowed)
	}
	for _, page := range []string{preamble, overflowed} {
		if strings.Contains(page, "Pull Request の説明") {
			t.Errorf("the description points at itself:\n%s", page)
		}
	}
	if !strings.Contains(overflowed, "運用担当者が保管しているこの実行の記録") {
		t.Errorf("the description cut its list without naming where the rest is:\n%s", overflowed)
	}
	// And a comment for that same run is not sent to a description that
	// does not hold the whole list either.
	if beyondComment := composeAssumptionsText(beyond, "https://github.com/example/target/pull/7", &outcomeNotes{}); strings.Contains(beyondComment, "Pull Request の説明") {
		t.Errorf("the comment names a description that does not hold the rest:\n%s", beyondComment)
	}

	// The comment carries what one comment holds. A list too long for it is
	// cut, and the cut names the description by its address, because that
	// is where the rest of it actually is.
	many := t.TempDir()
	const crowdedCount = 150
	crowded := make([]runAssumption, 0, crowdedCount)
	for index := 1; index <= crowdedCount; index++ {
		crowded = append(crowded, runAssumption{
			Kind:      assumptionDefensibleDefault,
			Statement: fmt.Sprintf("決めたこと %03d 番、理由をそえて長めに書いたもの", index),
			Evidence:  "ほかに擁護できる既定が立たないため",
		})
	}
	writeOutcomeRecord(t, many, "history/readiness/assessment-1.json", map[string]any{"assumptions": crowded})
	const url = "https://github.com/example/target/pull/42"
	comment := composeAssumptionsText(many, url, &outcomeNotes{})
	if len(comment) > hook.MaxAssumptionsTextBytes {
		t.Fatalf("the comment's list is %d bytes, over its %d", len(comment), hook.MaxAssumptionsTextBytes)
	}
	if !strings.Contains(comment, "…（") {
		t.Fatalf("four hundred decisions did not exceed the comment's share; the fixture proves nothing")
	}
	if !strings.Contains(comment, url) {
		t.Errorf("the comment was cut without naming where the rest is:\n%s", comment)
	}
	// And the whole four hundred are in the description, which is what the
	// comment just sent the reader to.
	whole := composeDeliveryPreamble(many)
	for _, index := range []int{1, crowdedCount / 2, crowdedCount} {
		if want := fmt.Sprintf("決めたこと %03d 番", index); !strings.Contains(whole, want) {
			t.Errorf("%q is not in the description the comment points at", want)
		}
	}

	// With no pull request there is no description, and the honest answer
	// is the run's own records rather than a page that does not exist.
	orphan := composeAssumptionsText(many, "", &outcomeNotes{})
	if !strings.Contains(orphan, "運用担当者が保管しているこの実行の記録") {
		t.Errorf("a cut list with no pull request names no place that holds it:\n%s", orphan)
	}
	if strings.Contains(orphan, "Pull Request の説明") {
		t.Errorf("a delivery with no pull request points at its description:\n%s", orphan)
	}
}

// A declaration the destination refused is not something this delivery
// created, and listing it under what was created would tell a requester the
// engine did a thing it was configured not to do.
func TestARefusedResourceIsNotListedAsCreated(t *testing.T) {
	runDir := t.TempDir()
	writeOutcomeRecord(t, runDir, "history/resources.jsonl",
		`{"kind":"queue","identifier":"orders-export","provider":"aws","stage":"implement"}`+"\n"+
			`{"kind":"database","identifier":"orders-db","provider":"aws","stage":"implement","refused":true}`+"\n")
	decided := composeAssumptionsText(runDir, "", &outcomeNotes{})
	if !strings.Contains(decided, "orders-export") {
		t.Errorf("the resource that was created is missing:\n%s", decided)
	}
	if strings.Contains(decided, "orders-db") {
		t.Errorf("a refused declaration is listed as created:\n%s", decided)
	}
}

// A run that died in a delivery card has its cause written down in a
// directory of its own, because a delivery card belongs to no round of the
// implementation. Reading only the implementation's directories left every
// one of those runs with a step name and no cause.
func TestADeliveryCardsFailureIsReadBackForTheReport(t *testing.T) {
	runDir := t.TempDir()
	writeOutcomeRecord(t, runDir,
		fmt.Sprintf("history/deliver-%d/%s-failure.json", runtime.DeliverRound, runtime.DeliverStagePromote),
		StageFailure{
			SchemaVersion: StageFailureSchemaVersion, Stage: runtime.DeliverStagePromote,
			Round: runtime.DeliverRound, Class: FailureClassDisk,
			Error: "no space left on device", FailedAt: time.Now().UTC(),
		})
	writeOutcomeRecord(t, runDir, fmt.Sprintf("retry/%s-r%d.json", runtime.DeliverStagePromote, runtime.DeliverRound),
		map[string]any{
			"schema_version": 1, "stage": runtime.DeliverStagePromote, "round": runtime.DeliverRound,
			"attempts": 2, "ladder_step": 0, "tried": []string{"reclaim:finished-runs"},
		})
	account := composeOutcomeText(runDir, hook.TerminalReleaseFailed, map[string]string{
		"failed_step": "本番への反映",
	}, &outcomeNotes{})
	for name, want := range map[string]string{
		"the cause":      "保存領域が足りなくなりました",
		"what was tried": "保存領域を空けてから、やり直しました",
	} {
		if !strings.Contains(account, want) {
			t.Errorf("%s is missing from a delivery card's account:\n%s", name, account)
		}
	}
}

// A record that is there and will not read is named. An absent section
// otherwise reads as "nothing of that kind happened", which is a different
// claim and not one this report has the evidence for.
func TestARecordThatCannotBeReadIsNamedRatherThanSilentlyMissing(t *testing.T) {
	runDir := t.TempDir()
	writeOutcomeRecord(t, runDir, "readiness-ticket.json", map[string]string{"request": "一覧を直す"})
	writeOutcomeRecord(t, runDir, "history/readiness/assessment-1.json", "{ this is not json")

	notes := &outcomeNotes{}
	_ = composeAssumptionsText(runDir, "", notes)
	outcome := composeOutcomeText(runDir, hook.TerminalSuccess, map[string]string{}, notes)
	if !strings.Contains(outcome, "読み取れなかった記録") {
		t.Fatalf("an unreadable record is not mentioned at all:\n%s", outcome)
	}
	if !strings.Contains(outcome, recordReception) {
		t.Errorf("the unreadable record is not named:\n%s", outcome)
	}

	// A record that is simply not there says nothing: most rounds are never
	// ruled on and most deliveries create nothing.
	quiet := composeOutcomeText(t.TempDir(), hook.TerminalSuccess, map[string]string{}, &outcomeNotes{})
	if strings.Contains(quiet, "読み取れなかった記録") {
		t.Errorf("an ordinary absence is reported as a fault:\n%s", quiet)
	}
}

// An investigation's whole product is a document, and the report used to say
// nothing about where to read it.
func TestAnInvestigationSaysWhereItsReportIs(t *testing.T) {
	text := composeOutcomeText(t.TempDir(), hook.TerminalInvestigated, map[string]string{}, &outcomeNotes{})
	if !strings.Contains(text, "## どこで見られるか") || !strings.Contains(text, "調査の報告はこのチケットに掲示しました") {
		t.Errorf("an investigation does not say where its report is:\n%s", text)
	}
}

// A Latin-script name takes a space before a Japanese particle. Without it
// the same comment read 「stagingの画面」 in one line and 「staging の画面」
// in the next.
func TestTheEnvironmentsNameIsSpacedTheSameWayEverywhere(t *testing.T) {
	runDir := t.TempDir()
	writeOutcomeRecord(t, runDir, DeliverStagingReportFile, DeliverReport{
		SchemaVersion: 1, Phase: "staging", Verdict: "pass", TargetURL: "https://staging.example.com/orders",
		ExpectedText: "月で絞り込む", ScreenChecked: true, ObservedAt: time.Now().UTC(),
	})
	writeOutcomeRecord(t, runDir, DeliverProductionReportFile, DeliverReport{
		SchemaVersion: 1, Phase: "production", Verdict: "pass", TargetURL: "https://www.example.com/orders",
		ExpectedText: "月で絞り込む", ScreenChecked: true, ObservedAt: time.Now().UTC(),
	})
	text := composeOutcomeText(runDir, hook.TerminalSuccess, map[string]string{
		"reached_delivery":        "production",
		"staging_evidence_url":    "https://staging.example.com/orders",
		"production_evidence_url": "https://www.example.com/orders",
	}, &outcomeNotes{})
	if strings.Contains(text, "stagingの") {
		t.Errorf("the environment's name runs into the particle after it:\n%s", text)
	}
	if !strings.Contains(text, "staging の画面") || !strings.Contains(text, "本番の画面") {
		t.Errorf("one of the environments is not named as expected:\n%s", text)
	}
}

// The reception's decisions reach the report under the heading for points
// the requester may want back, and this reader is pinned to the reception's
// own vocabulary rather than to a copy of it: the reception settles a point
// it would otherwise have asked about and seals it under a kind, and if that
// kind is renamed there this stops compiling here.
func TestTheReceptionsOwnDecisionsReachTheReport(t *testing.T) {
	runDir := t.TempDir()
	writeOutcomeRecord(t, runDir, "history/readiness/assessment-1.json", map[string]any{
		"assumptions": []worker.ReadinessAssumption{
			{Kind: worker.AssumptionDefensibleDefault, Statement: "絞り込みの初期値は今月とした", Evidence: "一覧が今月から始まるため"},
			{Kind: worker.AssumptionRepositoryConvention, Statement: "日付の書式は既存の一覧に合わせた", Evidence: "同じ画面の既存表示"},
			{Kind: worker.AssumptionImplementationDetail, Statement: "内部の名前を変えた", Evidence: "利用者には見えない"},
		},
	})
	if worker.AssumptionDefensibleDefault != assumptionDefensibleDefault {
		t.Fatalf("this reader spells the kind %q and the reception seals %q",
			assumptionDefensibleDefault, worker.AssumptionDefensibleDefault)
	}

	text := composeAssumptionsText(runDir, "", &outcomeNotes{})
	decided, _, split := strings.Cut(text, "## 前提とした解釈")
	if !split {
		t.Fatalf("the two headings did not both appear:\n%s", text)
	}
	if !strings.Contains(decided, "絞り込みの初期値は今月とした") {
		t.Errorf("a point the reception decided instead of asking is not under what was decided:\n%s", text)
	}
	for _, settled := range []string{"日付の書式は既存の一覧に合わせた", "内部の名前を変えた"} {
		if strings.Contains(decided, settled) {
			t.Errorf("%q was filed as a decision the requester may want back:\n%s", settled, text)
		}
		if !strings.Contains(text, settled) {
			t.Errorf("%q never reached the report at all:\n%s", settled, text)
		}
	}

	// The same split governs the stream the run appends to while it works,
	// so the plan notice and this comment cannot disagree about one decision.
	stream := t.TempDir()
	writeOutcomeRecord(t, stream, "history/assumptions.jsonl",
		`{"kind":"`+assumptionArbiterRuling+`","statement":"範囲を越えた指摘は退けた","evidence":"検収条件に無い"}`+"\n"+
			`{"kind":"seat_moved","statement":"レビュー役を別の提供元に移した","evidence":"26 分無応答"}`+"\n")
	loaded := LoadRecordedDecisions(stream)
	if len(loaded) != 2 {
		t.Fatalf("the stream read back %d decisions, want 2", len(loaded))
	}
	if !loaded[0].Decided {
		t.Errorf("a ruling is not counted as decided in place of asking")
	}
	if loaded[1].Decided {
		t.Errorf("a seat that moved is counted as a decision the requester may want back")
	}
}
