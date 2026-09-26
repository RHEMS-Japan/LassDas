package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/hook"
)

func TestAuditUnchosenDestinationDoesNotClaimAHandoff(t *testing.T) {
	script := filepath.Join(t.TempDir(), "worker")
	body := `#!/bin/sh
verb="$1"
shift
out=""
while [ $# -gt 0 ]; do
 if [ "$1" = "--out" ]; then out="$2"; fi
 shift
done
if [ "$verb" = "read-contract" ]; then
 printf '%s' '{"fallback":true,"gaps":[{"field":"repository","question":"Which repository?"}]}' > "$out"
fi
exit 0
`
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	p := receptionPipeline(t, script)
	_, outcome, err := p.pretrip(context.Background())
	if err != nil || outcome.Code != hook.TerminalClarificationRequired {
		t.Fatalf("pretrip: %+v, %v", outcome, err)
	}
	notes := LoadRecordedDecisions(p.Workspace)
	if len(notes) != 1 {
		t.Fatalf("notes: %+v", notes)
	}
	if strings.Contains(notes[0].Statement, "実装役へ渡し") || strings.Contains(notes[0].Statement, "質問はしていません") {
		t.Fatalf("false handoff: %s", notes[0].Statement)
	}
	if !strings.Contains(notes[0].Statement, "納品先") {
		t.Fatalf("missing actual blocker: %s", notes[0].Statement)
	}
}

func TestAuditFallbackDoesNotPublishDiscardedAssumptions(t *testing.T) {
	p := receptionPipeline(t, fallbackStubWorker(t, "check-readiness", unusableAnswerStderr(t)))
	dir := filepath.Join(p.Workspace, "history", "readiness")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assessment-1.json"), []byte(`{"assumptions":[{"kind":"repository_convention","statement":"discarded assumption"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	outcome, err := p.readinessGate(context.Background())
	if err != nil || outcome.Code != "" {
		t.Fatalf("gate: %+v, %v", outcome, err)
	}
	_, assumed, _ := receptionAssumptions(p.Workspace, &outcomeNotes{})
	if strings.Contains(strings.Join(assumed, "\n"), "discarded assumption") {
		t.Fatalf("discarded reading published: %+v", assumed)
	}
}

func TestAuditIntakeFallbackDoesNotDenyALaterReceptionQuestion(t *testing.T) {
	p, _ := configuredRunner(t, runnerFixtureConfig(t, 2, 1, true), "converged")
	original := p.Config.WorkerBin
	script := filepath.Join(t.TempDir(), "worker")
	body := `#!/bin/sh
verb="$1"
out=""; prev=""
for arg in "$@"; do
 [ "$prev" = "--out" ] && out="$arg"
 prev="$arg"
done
case "$verb" in
 read-contract) printf '%s' '{"fallback":true,"gaps":[]}' > "$out"; exit 0 ;;
 decide-readiness) printf '%s' '{"outcome":"clarification_required"}' > "$out"; exit 0 ;;
esac
exec '` + original + `' "$@"
`
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	p.Config.WorkerBin = script
	_, outcome, err := p.PrepareChainRun(t.Context(), nil)
	if err != nil || outcome.QuestionDecisionPath == "" {
		t.Fatalf("preparation: %+v, %v", outcome, err)
	}
	for _, note := range LoadRecordedDecisions(p.Workspace) {
		if strings.Contains(note.Statement, "質問はしていません") || strings.Contains(note.Statement, "実装役へ渡し") {
			t.Fatalf("the intake note contradicts the later question: %s", note.Statement)
		}
	}
}

func TestAuditReceptionDistinguishesUnreadableInputFromAnOpenQuestion(t *testing.T) {
	for _, tc := range []struct {
		stage string
		want  hook.TerminalCode
	}{
		{"read-ticket", hook.TerminalInputRejected},
		{"build-draft", hook.TerminalClarificationRequired},
	} {
		t.Run(tc.stage, func(t *testing.T) {
			script := filepath.Join(t.TempDir(), "worker")
			body := `#!/bin/sh
if [ "$1" = '` + tc.stage + `' ]; then exit 2; fi
verb="$1"; shift
out=""
while [ $# -gt 0 ]; do
 if [ "$1" = "--out" ]; then out="$2"; fi
 shift
done
if [ "$verb" = "read-contract" ]; then printf '%s' '{"gaps":[]}' > "$out"; fi
exit 0
`
			if err := os.WriteFile(script, []byte(body), 0700); err != nil {
				t.Fatal(err)
			}
			p := receptionPipeline(t, script)
			_, outcome, err := p.pretrip(t.Context())
			if err != nil || outcome.Code != tc.want {
				t.Fatalf("outcome=%+v err=%v", outcome, err)
			}
			comment := hook.TerminalCommentContent(hook.TerminalReportRequest{Code: outcome.Code, AutomationRunID: "run-1"}, strings.Repeat("0", 64))
			if tc.stage == "read-ticket" && (!strings.Contains(comment, "入力の読み取り検査") || !strings.Contains(comment, "起票し直してください")) {
				t.Fatalf("the requester gets no reason and action: %s", comment)
			}
		})
	}
}
