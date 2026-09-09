package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/hook"
)

// receptionWorker stands in for the worker across a whole reception: every
// verb writes a usable record to its --out and succeeds, except the one
// named, which fails the way the real worker does.
func receptionWorker(t *testing.T, failing, stderr string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "stand-in-worker")
	body := "#!/bin/sh\n" +
		"verb=\"$1\"\n" +
		"out=\"\"; prev=\"\"\nfor a in \"$@\"; do [ \"$prev\" = \"--out\" ] && out=\"$a\"; prev=\"$a\"; done\n" +
		"if [ \"$verb\" = \"" + failing + "\" ]; then printf '%s\\n' '" + stderr + "' >&2; exit 1; fi\n" +
		"check='{\"verdict\":\"pass\"}'; decide='{\"outcome\":\"ready\"}'\n" +
		"case \"" + failing + "\" in\n" +
		"  verdict-unreadable) check='not json at all' ;;\n" +
		"  verdict-unknown) check='{\"verdict\":\"maybe\"}' ;;\n" +
		"  outcome-unknown) decide='{\"outcome\":\"sideways\"}' ;;\n" +
		"esac\n" +
		"case \"$verb\" in\n" +
		"  check-readiness) [ -n \"$out\" ] && printf '%s' \"$check\" > \"$out\" ;;\n" +
		"  decide-readiness) [ -n \"$out\" ] && printf '%s' \"$decide\" > \"$out\" ;;\n" +
		"  *) [ -n \"$out\" ] && printf '%s' '{}' > \"$out\" ;;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return script
}

// Reception generates and reviews nothing, so the sentence about "the work
// being generated or reviewed" is not merely vague there — it is false. Each
// reception exit names its own step, and the ones no model ran say so.
func TestEveryReceptionExitNamesItsOwnStep(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failing string
		stderr  string
		want    string
	}{
		{"the assessment could not be asked", "assess-readiness",
			"worker: readiness assessment failed: model invocation failed with status 502", "AI による受付の判定"},
		{"the check could not be asked", "check-readiness",
			"worker: readiness check failed: model invocation failed with status 502", "AI による受付の確認"},
		{"the summary could not be asked", "decide-readiness",
			"worker: readiness decision failed: model invocation failed with status 502", "AI による受付の判定のまとめ"},
		{"the check's verdict cannot be read", "verdict-unreadable",
			"", "受付の確認の記録の読み取り"},
		{"the check's verdict is not one of the two", "verdict-unknown",
			"", "受付の確認の記録の読み取り"},
		{"the summary's outcome is not one it knows", "outcome-unknown",
			"", "受付の判定のまとめの記録の読み取り"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pipeline := receptionPipeline(t, receptionWorker(t, tc.failing, tc.stderr))
			// Some of these exits hand the reading error back alongside the
			// outcome; the code is what decides the report either way.
			outcome, err := pipeline.readinessGate(context.Background())
			if outcome.Code != hook.TerminalModelFailed {
				t.Fatalf("readinessGate() = %+v, %v; want model_failed", outcome, err)
			}
			if got := outcome.Evidence["failed_step"]; got != tc.want {
				t.Fatalf("step = %q, want %q", got, tc.want)
			}
			// The sentence the requester actually reads.
			comment := hook.TerminalCommentContent(hook.TerminalReportRequest{
				Code: hook.TerminalModelFailed, AutomationRunID: "run-1", FailedStep: tc.want,
			}, strings.Repeat("0", 64))
			if !strings.Contains(comment, tc.want+"を完了できなかったため、本番環境には反映していません。") {
				t.Fatalf("the sentence for %q is wrong:\n%s", tc.want, comment)
			}
			if strings.Contains(comment, "成果物の生成またはレビュー") {
				t.Fatalf("reception still reports as if it generated or reviewed the work:\n%s", comment)
			}
		})
	}
}

// A record the reception could not read is not a model failing: no model was
// asked anything on that path, and the step says so.
func TestAReceptionRecordThatCannotBeReadIsNotToldAsTheAIFailing(t *testing.T) {
	// Every verb succeeds, but the summary's record is not the shape the
	// reception can read, so the run ends on the record rather than on an
	// answer — and no model failed at all.
	pipeline := receptionPipeline(t, receptionWorkerWritingUnreadableDecision(t))
	outcome, err := pipeline.readinessGate(context.Background())
	if outcome.Code != hook.TerminalModelFailed {
		t.Fatalf("readinessGate() = %+v, %v; want model_failed", outcome, err)
	}
	step := outcome.Evidence["failed_step"]
	if !strings.Contains(step, "記録の読み取り") {
		t.Fatalf("step = %q, want a step about reading a record", step)
	}
	if strings.Contains(step, "AI") {
		t.Fatalf("step = %q claims a model ran where none was asked", step)
	}
}

// receptionWorkerWritingUnreadableDecision succeeds at every verb but leaves
// a summary the reception cannot read.
func receptionWorkerWritingUnreadableDecision(t *testing.T) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "stand-in-worker")
	body := "#!/bin/sh\n" +
		"verb=\"$1\"\n" +
		"out=\"\"; prev=\"\"\nfor a in \"$@\"; do [ \"$prev\" = \"--out\" ] && out=\"$a\"; prev=\"$a\"; done\n" +
		"case \"$verb\" in\n" +
		"  check-readiness) [ -n \"$out\" ] && printf '%s' '{\"verdict\":\"pass\"}' > \"$out\" ;;\n" +
		"  decide-readiness) [ -n \"$out\" ] && printf '%s' 'not json at all' > \"$out\" ;;\n" +
		"  *) [ -n \"$out\" ] && printf '%s' '{}' > \"$out\" ;;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return script
}
