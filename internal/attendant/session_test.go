package attendant

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
)

func sessionTestConsumerConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "m1-consumer.json")
	raw, err := json.Marshal(map[string]any{"consumers": []any{
		map[string]any{"repository": "example/console", "staging_origin": "https://stg.example.invalid", "staging_login_url": "https://api-stg.example.invalid/login?returnTo=/console", "observation_language": "ja"},
		map[string]any{"repository": "example/public", "staging_origin": "https://public-stg.example.invalid"},
		map[string]any{"repository": "example/edge", "staging_origin": "https://edge-stg.example.invalid", "staging_login_url": "https://edge-stg.example.invalid/admin/login", "observation_language": "ja;q=0.9"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSessionProbesListOnlyDestinationsWithALogin(t *testing.T) {
	probes, err := sessionProbes(sessionTestConsumerConfig(t))
	if err != nil || len(probes) != 2 || probes[0].Repository != "example/console" || probes[1].Repository != "example/edge" {
		t.Fatalf("probes = %+v (%v)", probes, err)
	}
	entry := probes[0].entry()
	if entry.LoginURL != "https://api-stg.example.invalid/login?returnTo=/console" || entry.LandedPrefix != "https://stg.example.invalid" || entry.Language != "ja" {
		t.Fatalf("entry = %+v", entry)
	}
	if probes[1].entry().Language != "" {
		t.Fatalf("a destination whose language is not a tag asks for none: %+v", probes[1])
	}
	if _, err := sessionProbes(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Fatal("an unreadable config must be an error, not an empty list")
	}
}

// A destination that refuses the jar holds the run once, throttles the
// retry, and clears when a login lands again. A login that merely could
// not be reached never holds: the observation will say what it sees.
func TestCheckSessionsHoldsOnRefusalOnlyThrottlesAndClears(t *testing.T) {
	forgetSessionLandings()
	t.Cleanup(forgetSessionLandings)
	runDir := t.TempDir()
	config := runtime.Config{ConsumerConfigPath: sessionTestConsumerConfig(t)}
	run := state.RunOverview{RunID: "TKT-7", DeliveryID: "d7", IssueID: 70}
	source := &fakeConfirmationSource{}
	outcome := map[string]string{"https://stg.example.invalid": "refused"}
	var attempts []string
	renew := func(_ context.Context, probe sessionProbe) (bool, error) {
		attempts = append(attempts, probe.Repository)
		switch outcome[probe.Origin] {
		case "refused":
			return true, errors.New("browser sign-in was refused (browser at https://idp.example.invalid/sign-in)")
		case "unreachable":
			return false, errors.New("browser sign-in failed")
		}
		return false, nil
	}

	if !checkSessions(context.Background(), config, source, run, runDir, 70, renew, resolutionTestLogger{}) {
		t.Fatal("a refused jar must hold the run")
	}
	if len(attempts) != 2 {
		t.Fatalf("every destination with a login is tried: %v", attempts)
	}
	if len(source.added) != 1 || hook.ExtractCommentMarker(source.added[0]) != hook.CommentMarker(string(hook.RunCommentSessionHold), run.RunID) {
		t.Fatalf("notices = %d", len(source.added))
	}
	if !strings.Contains(source.added[0], "https://stg.example.invalid") || strings.Contains(source.added[0], "edge-stg") {
		t.Fatalf("the notice names the refused destination only:\n%s", source.added[0])
	}
	hold, ok := readSessionHold(runDir)
	if !ok || len(hold.Destinations) != 1 || hold.Destinations[0] != "https://stg.example.invalid" {
		t.Fatalf("hold = %+v (%v)", hold, ok)
	}
	if !sessionHeldRecently(runDir, time.Now()) || sessionHeldRecently(runDir, time.Now().Add(sessionRetryInterval+time.Second)) {
		t.Fatal("the hold must throttle the retry for exactly the interval")
	}

	// Still refused at the next attempt: held again, no second notice. The
	// destination that landed is not signed in to again this tick.
	source.comments = append(source.comments, hook.BacklogComment{CommentID: 1, UserID: 1, Body: source.added[0]})
	attempts = nil
	if !checkSessions(context.Background(), config, source, run, runDir, 70, renew, resolutionTestLogger{}) || len(source.added) != 1 {
		t.Fatalf("second attempt: notices = %d", len(source.added))
	}
	if strings.Join(attempts, ",") != "example/console" {
		t.Fatalf("a landing is remembered for the tick; attempts = %v", attempts)
	}

	// The login is unreachable now (an outage): not the jar's fault, the
	// hold clears and the run proceeds.
	outcome["https://stg.example.invalid"] = "unreachable"
	if checkSessions(context.Background(), config, source, run, runDir, 70, renew, resolutionTestLogger{}) {
		t.Fatal("an unreachable login must not hold the run")
	}
	if _, ok := readSessionHold(runDir); ok {
		t.Fatal("the hold file must be removed once nothing refuses")
	}
	if len(source.added) != 1 {
		t.Fatalf("no notice for an outage: %d", len(source.added))
	}

	// The operator logged in again: lands, and the run proceeds.
	outcome["https://stg.example.invalid"] = "landed"
	if checkSessions(context.Background(), config, source, run, runDir, 70, renew, resolutionTestLogger{}) {
		t.Fatal("a login that lands must not hold the run")
	}
}

// A config nobody can read holds nothing, and a hold left from an earlier
// tick does not outlive the decision to proceed.
func TestCheckSessionsClearsAStaleHoldWhenTheConfigIsUnreadable(t *testing.T) {
	forgetSessionLandings()
	t.Cleanup(forgetSessionLandings)
	runDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(runDir, sessionHoldFile), []byte(`{"destinations":["https://stg.example.invalid"],"at":"2026-09-03T00:00:00Z"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	run := state.RunOverview{RunID: "TKT-8", DeliveryID: "d8", IssueID: 80}
	renew := func(context.Context, sessionProbe) (bool, error) { t.Fatal("nothing to probe"); return false, nil }
	if checkSessions(context.Background(), runtime.Config{ConsumerConfigPath: filepath.Join(t.TempDir(), "absent.json")}, &fakeConfirmationSource{}, run, runDir, 80, renew, resolutionTestLogger{}) {
		t.Fatal("an unreadable config must not hold the run")
	}
	if _, ok := readSessionHold(runDir); ok {
		t.Fatal("the stale hold must be removed")
	}
}

func TestLiveSessionRenewerTreatsAMissingJarAsARefusal(t *testing.T) {
	renew := liveSessionRenewer(func(string) string { return "" })
	rejected, err := renew(context.Background(), sessionProbe{Repository: "example/console", Origin: "https://stg.example.invalid", LoginURL: "https://api-stg.example.invalid/login"})
	if err == nil || !rejected || !strings.Contains(err.Error(), "設定されていません") {
		t.Fatalf("without a jar there is nothing to be let in with: rejected=%v err=%v", rejected, err)
	}
}

func TestSessionHoldShowsAtIntakeAsAttention(t *testing.T) {
	root := t.TempDir()
	config := runtime.Config{}
	config.Chain.RunsRoot = root
	const delivery = "TKT-903:1"
	if err := os.MkdirAll(filepath.Join(root, delivery), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, delivery, "session-hold.json"), []byte(`{"destinations":["https://stg.example.invalid"],"at":"2026-09-03T00:00:00Z"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, runState := range []string{"queued", "claimed"} {
		got := classifyRun(config, state.RunOverview{State: runState, DeliveryID: delivery}, nil)
		if got.Step != "attention" || got.Stage != "intake" || !strings.Contains(got.Detail, "https://stg.example.invalid") || !strings.Contains(got.StepTitle, "ログイン") {
			t.Fatalf("%s with a session hold = %q at %q (%q / %q)", runState, got.Step, got.Stage, got.StepTitle, got.Detail)
		}
	}
}

// A screen the browser never reached is an attention state, not a failed
// delivery: the board lights the stage, and an operator's 「確認済み」 can
// close it.
func TestObserveBlockedIsAnAttentionState(t *testing.T) {
	if !attentionVerdict("observe_blocked") || attentionVerdict("observe_failed") {
		t.Fatal("observe_blocked waits for an operator; observe_failed does not")
	}
	var staging RunStatus
	placeStagingOutcome(&staging, "observe_blocked", "")
	if staging.Step != "attention" || staging.Stage != "confirm" || !strings.Contains(staging.StepTitle, "画面確認ができず") {
		t.Fatalf("staging = %+v", staging)
	}
	var release RunStatus
	placeReleaseOutcome(&release, "observe_blocked")
	if release.Step != "attention" || release.Stage != "production" || !strings.Contains(release.StepTitle, "画面確認ができず") {
		t.Fatalf("release = %+v", release)
	}
	var failed RunStatus
	placeStagingOutcome(&failed, "observe_failed", "")
	if failed.Step != "failed" {
		t.Fatalf("a judged failure stays a failure: %+v", failed)
	}
}
