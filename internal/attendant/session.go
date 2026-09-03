package attendant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
	"automation.internal/ticket-ingress/internal/visiblecheck"
	"automation.internal/ticket-ingress/internal/worker"
)

// A delivery whose staging observation cannot sign in ends as a screen
// nobody judged (live, 2026-09-03: the console's day-long session in the
// operator's jar had expired 28 hours before the observation, the browser
// was sent to the portal, and a correct change was reported as a failed
// screen check). The attendant now signs in first, before the reception
// starts, through every destination's login entry: a login that lands
// renews the jar — the console session re-minted, the identity provider's
// session rolled forward — and a login the destination REFUSES holds the
// run, visibly, once, with automatic resumption once an operator has
// logged in again. Only a refusal decides: a login that could not be
// reached (an outage, a slow round trip, an error page on the login host)
// lets the run proceed and its observation say what it sees — the same
// division the budget check draws between "no money" and "no answer".

const (
	sessionHoldFile      = "session-hold.json"
	sessionRetryInterval = budgetRetryInterval
	sessionRenewTimeout  = 2 * time.Minute
	// sessionMemoFreshness is how long a landing counts for the queued
	// runs that follow it: one Chrome round trip per destination per
	// tick, not one per queued ticket.
	sessionMemoFreshness = 5 * time.Minute
)

// sessionProbe is one destination whose staging observation signs in.
type sessionProbe struct {
	Repository string
	Origin     string
	LoginURL   string
	Language   string
}

func (p sessionProbe) entry() visiblecheck.SignIn {
	return visiblecheck.SignIn{LoginURL: p.LoginURL, LandedPrefix: p.Origin, Language: p.Language}
}

type sessionHold struct {
	Destinations []string  `json:"destinations"`
	At           time.Time `json:"at"`
}

// sessionRenewer signs in for one destination with the current jar and
// keeps the result. rejected says the destination refused the jar (the
// browser came to rest on an identity provider or a portal); an error
// without rejection is a login that could not be reached.
type sessionRenewer func(ctx context.Context, probe sessionProbe) (rejected bool, err error)

// sessionMemo remembers, per destination, when a login last landed, so a
// tick with several queued runs signs in once.
var sessionMemo = struct {
	mu     sync.Mutex
	landed map[string]time.Time
}{landed: map[string]time.Time{}}

func sessionLandedRecently(origin string, now time.Time) bool {
	sessionMemo.mu.Lock()
	defer sessionMemo.mu.Unlock()
	at, ok := sessionMemo.landed[origin]
	return ok && now.Before(at.Add(sessionMemoFreshness))
}

func rememberSessionLanded(origin string, now time.Time) {
	sessionMemo.mu.Lock()
	sessionMemo.landed[origin] = now
	sessionMemo.mu.Unlock()
}

func forgetSessionLandings() {
	sessionMemo.mu.Lock()
	sessionMemo.landed = map[string]time.Time{}
	sessionMemo.mu.Unlock()
}

// sessionProbes lists the destinations with a staging login entry. A
// lenient decode on purpose, like the budget probe's: the file's other
// sections are the worker's business and validated there.
func sessionProbes(consumerConfigPath string) ([]sessionProbe, error) {
	raw, err := os.ReadFile(consumerConfigPath)
	if err != nil {
		return nil, err
	}
	var config struct {
		Consumers []struct {
			Repository          string `json:"repository"`
			StagingOrigin       string `json:"staging_origin"`
			StagingLoginURL     string `json:"staging_login_url"`
			ObservationLanguage string `json:"observation_language"`
		} `json:"consumers"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return nil, err
	}
	var probes []sessionProbe
	for _, consumer := range config.Consumers {
		if consumer.StagingLoginURL == "" || consumer.StagingOrigin == "" {
			continue
		}
		language := ""
		if worker.ValidLanguageTag(consumer.ObservationLanguage) {
			language = consumer.ObservationLanguage
		}
		probes = append(probes, sessionProbe{
			Repository: consumer.Repository, Origin: consumer.StagingOrigin,
			LoginURL: consumer.StagingLoginURL, Language: language,
		})
	}
	return probes, nil
}

// liveSessionRenewer drives the real browser: the jar named by the pod
// environment goes in, the jar after the landing comes out and replaces
// the engine's renewed copy, stamped with the seed it grew from. Without
// a state file the login still counts (the observation will sign in again
// on its own) — nothing is kept. A jar nobody provisioned is a refusal:
// there is nothing the login could have been let in with.
func liveSessionRenewer(getenv func(string) string) sessionRenewer {
	return func(ctx context.Context, probe sessionProbe) (bool, error) {
		seedPath, statePath := getenv(visiblecheck.SessionFileEnvironment), getenv(visiblecheck.SessionStateFileEnvironment)
		cookies, note := visiblecheck.LoadSessionCookies(seedPath, statePath)
		if len(cookies) == 0 {
			return true, errors.New(strings.TrimSpace(note))
		}
		ctx, cancel := context.WithTimeout(ctx, sessionRenewTimeout)
		defer cancel()
		entry := probe.entry()
		before := visiblecheck.FileStamp(statePath)
		jar, where, err := visiblecheck.RenewSession(ctx, entry, cookies)
		if err != nil {
			rejected := visiblecheck.JarRejected(err, where, entry)
			if where != "" {
				err = fmt.Errorf("%w (browser at %s)", err, where)
			}
			return rejected, err
		}
		if statePath == "" || visiblecheck.FileStamp(statePath) != before {
			// Nowhere to keep it, or somebody kept a newer jar meanwhile:
			// the login landed either way.
			return false, nil
		}
		if err := visiblecheck.WriteSessionFile(statePath, jar, visiblecheck.SeedDigest(seedPath)); err != nil {
			return false, fmt.Errorf("landed, but the renewed jar could not be kept: %w", err)
		}
		return false, nil
	}
}

func readSessionHold(runDir string) (sessionHold, bool) {
	raw, err := os.ReadFile(filepath.Join(runDir, sessionHoldFile))
	if err != nil || len(raw) > 1<<16 {
		return sessionHold{}, false
	}
	var hold sessionHold
	if json.Unmarshal(raw, &hold) != nil || hold.At.IsZero() {
		return sessionHold{}, false
	}
	return hold, true
}

// sessionHeldRecently throttles the retry exactly like the budget hold.
func sessionHeldRecently(runDir string, now time.Time) bool {
	hold, held := readSessionHold(runDir)
	return held && now.Before(hold.At.Add(sessionRetryInterval))
}

func clearSessionHold(runDir string, run state.RunOverview, logger Logger) {
	if _, held := readSessionHold(runDir); held {
		_ = os.Remove(filepath.Join(runDir, sessionHoldFile))
		logger.Info("session check: hold cleared, run proceeds", "run", run.RunID)
	}
}

// checkSessions signs in for every destination with a login entry and
// records a hold when any destination refuses the jar: the hold file
// (which throttles the retry and drives the board) and one ticket comment
// (marker-idempotent). A landing, or a login that merely could not be
// reached, clears an earlier hold and lets the run proceed. Returns
// whether the run is held.
func checkSessions(ctx context.Context, config runtime.Config, backlog operatorConfirmationSource, run state.RunOverview, runDir string, issueID int64, renew sessionRenewer, logger Logger) bool {
	probes, err := sessionProbes(config.ConsumerConfigPath)
	if err != nil {
		logger.Error("session check: consumer config unreadable; proceeding", "run", run.RunID, "error", err.Error())
		clearSessionHold(runDir, run, logger)
		return false
	}
	now := time.Now()
	var refused []string
	for _, probe := range probes {
		if sessionLandedRecently(probe.Origin, now) {
			continue
		}
		rejected, err := renew(ctx, probe)
		switch {
		case err == nil:
			rememberSessionLanded(probe.Origin, time.Now())
			logger.Info("session check: signed in, jar renewed", "run", run.RunID, "destination", probe.Repository)
		case rejected:
			refused = append(refused, probe.Origin)
			logger.Info("session check: destination refused the jar", "run", run.RunID, "destination", probe.Repository, "error", err.Error())
		default:
			logger.Error("session check: sign-in inconclusive; proceeding for this destination", "run", run.RunID, "destination", probe.Repository, "error", err.Error())
		}
	}
	holdPath := filepath.Join(runDir, sessionHoldFile)
	if len(refused) == 0 {
		clearSessionHold(runDir, run, logger)
		return false
	}
	encoded, err := json.Marshal(sessionHold{Destinations: refused, At: time.Now().UTC()})
	if err == nil {
		err = os.WriteFile(holdPath, encoded, 0o644)
	}
	if err != nil {
		logger.Error("session hold: record failed", "run", run.RunID, "error", err.Error())
	}
	comments, err := backlog.ListComments(ctx, issueID, 0)
	if err != nil {
		logger.Error("session hold: comment listing failed", "run", run.RunID, "error", err.Error())
		return true
	}
	if _, posted := commentIDWithMarker(comments, hook.CommentMarker(string(hook.RunCommentSessionHold), run.RunID)); !posted {
		if _, err := backlog.AddComment(ctx, issueID, hook.SessionHoldContent(run.RunID, refused)); err != nil {
			logger.Error("session hold: notice post failed", "run", run.RunID, "error", err.Error())
		} else {
			logger.Info("session hold: notice posted", "run", run.RunID, "destinations", strings.Join(refused, ","))
		}
	}
	return true
}

// placeSessionHold names the hold on the board: off the rail at intake,
// waiting for an operator's login, resuming by itself.
func placeSessionHold(status *RunStatus, hold sessionHold) {
	status.placeAt("attention", "intake", "確認用のログイン状態が切れていて開始できません",
		"運用担当者が確認用のログインをやり直すと自動で再開します: "+strings.Join(hold.Destinations, "、"))
}
