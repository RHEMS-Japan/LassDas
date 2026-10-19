package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ticket-runner/internal/chain"
)

func saveStallState(t *testing.T, root string, state chain.State) {
	t.Helper()
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "jobs/7/run/history.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func stalledBadge(s *server, j *job) bool {
	w := httptest.NewRecorder()
	s.render(w, "job", page{Lang: "en", Job: j})
	return strings.Contains(w.Body.String(), ">stalled</span>")
}

func TestStalledClockAndPreservedStatus(t *testing.T) {
	now := time.Now().UTC()
	row := func(age time.Duration) chain.Result {
		return chain.Result{Role: "implement", Speaker: "worker", FinishedAt: now.Add(-age)}
	}
	for _, tc := range []struct {
		name    string
		rows    []chain.Result
		pending time.Time
		want    bool
	}{
		{"old then recent", []chain.Result{row(2 * time.Hour), row(5 * time.Minute)}, time.Time{}, false},
		{"recent then old", []chain.Result{row(5 * time.Minute), row(2 * time.Hour)}, time.Time{}, true},
		{"unfinished recent", []chain.Result{{StartedAt: now.Add(-5 * time.Minute)}}, time.Time{}, false},
		{"unfinished old", []chain.Result{{StartedAt: now.Add(-2 * time.Hour)}}, time.Time{}, true},
		{"new launch after hold", []chain.Result{row(2 * time.Hour)}, now.Add(-time.Minute), false},
		{"old launch without rows", nil, now.Add(-time.Hour), true},
		{"new launch without rows", nil, now.Add(-time.Minute), false},
		{"unknown clock", nil, time.Time{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := fixtureQueue(t)
			saveStallState(t, root, chain.State{Step: "implement", Pending: &chain.Assignment{Role: "implement"}, PendingSince: tc.pending, History: tc.rows})
			s, err := newServer(root, "", "", "")
			if err != nil {
				t.Fatal(err)
			}
			for _, detail := range []bool{false, true} {
				j := s.loadJob("7", now, detail)
				if j.Status != "running implement" {
					t.Fatalf("existing status lost: %q", j.Status)
				}
				if got := stalledBadge(s, j); got != tc.want {
					t.Fatalf("detail=%t stalled=%t want %t lane=%s", detail, got, tc.want, j.Lane)
				}
				if tc.want && j.Lane != "attention" {
					t.Fatalf("stalled lane=%s", j.Lane)
				}
			}
			s.stalledAfter = 3 * time.Hour
			if stalledBadge(s, s.loadJob("7", now, false)) {
				t.Fatal("configured threshold ignored")
			}
		})
	}
}

func TestStalledHoldsAndRecoveredErrors(t *testing.T) {
	now := time.Now().UTC()
	for _, held := range []string{"active", "slot after answer", "slot after restart", "budget accepted", "budget resumed", "budget restored", "requester", "done", "stop", "stop broken", "pause", "pause broken"} {
		t.Run(held, func(t *testing.T) {
			root := fixtureQueue(t)
			state := chain.State{Step: "implement", Pending: &chain.Assignment{Role: "implement"}, History: []chain.Result{
				{Role: "verify", Speaker: "worker", FinishedAt: now.Add(-3 * time.Hour), Error: "recovered failure"},
				{Role: "verify", Speaker: "worker", FinishedAt: now.Add(-2 * time.Hour)},
				{Role: "implement", Speaker: "runtime", FinishedAt: now.Add(-time.Hour), Error: "interrupted latest", Interrupted: true},
			}}
			switch held {
			case "slot after answer", "slot after restart":
				state.Pending = nil
				state.Recovering = held == "slot after restart"
				if err := os.Remove(filepath.Join(root, "jobs/7/live/implement-implement-process.json")); err != nil {
					t.Fatal(err)
				}
			case "requester":
				state.Waiting = true
			case "done":
				state.Done = true
			}
			saveStallState(t, root, state)
			s, err := newServer(root, "", "", "")
			if err != nil {
				t.Fatal(err)
			}
			j := s.loadJob("7", now, false)
			if held == "active" {
				j.Attention = "an existing observation"
			}
			switch held {
			case "budget accepted", "budget resumed", "budget restored":
				kind, text := "accepted", "受け付けました。"
				if held == "budget resumed" {
					kind, text = "resumed", "返答を受け取りました。"
				}
				j.Notices = []map[string]any{{"kind": kind, "text": text + "モデル利用枠の残りが設定の下限を下回っているため、待機中です。枠が戻り次第、自動で処理を始めます。", "written_at": now.Add(-30 * time.Minute).Format(time.RFC3339Nano)}}
				if held == "budget restored" {
					j.Notices = append(j.Notices, map[string]any{"kind": "budget-restored", "written_at": now.Add(-20 * time.Minute).Format(time.RFC3339Nano)})
				}
			case "stop":
				j.Stopped = true
			case "stop broken":
				j.StopBroken = true
			case "pause":
				j.WorkPause = "awaiting instruction"
			case "pause broken":
				j.PauseBroken = true
			}
			if len(j.Notices) > 0 && strings.HasPrefix(held, "budget") {
				raw, _ := json.Marshal(map[string]any{"notices": j.Notices})
				if err := os.WriteFile(filepath.Join(root, "jobs/7/notices.json"), raw, 0600); err != nil {
					t.Fatal(err)
				}
				j = s.loadJob("7", now, false)
			} else {
				j.derive(now, s.stalledAfter)
			}
			want := held == "active" || held == "budget restored"
			if got := stalledBadge(s, j); got != want {
				t.Fatalf("stalled=%t want=%t status=%s lane=%s", got, want, j.Status, j.Lane)
			}
			if held == "active" && j.Attention != "an existing observation" {
				t.Fatal("stall replaced an existing observation")
			}
			if strings.Contains(j.Failure, "recovered failure") || strings.Contains(j.Failure, "interrupted latest") {
				t.Fatalf("old failure attributed to current stall: %s", j.Failure)
			}
		})
	}
}

func TestStalledEngineLogIsSeparateAndRequestDelimited(t *testing.T) {
	for _, text := range []string{"request 77: wrong\nunattributed error\n", "[12 earlier bytes not shown; the whole file is under files]\nrequest 7: cut mid-line\n"} {
		if got := latestRequestLog(text, "7"); got != "" {
			t.Fatalf("unattributed or cut line was used: %q", got)
		}
	}
	root := fixtureQueue(t)
	now := time.Now().UTC()
	saveStallState(t, root, chain.State{Step: "implement", Pending: &chain.Assignment{Role: "implement"}, History: []chain.Result{{FinishedAt: now.Add(-time.Hour)}}})
	log := "request 7: old failure\nrequest 77: WRONG-REQUEST\nunattributed WRONG-CAUSE\nrequest 7 remains unfinished: no space left on device <script>\nrequest 77: WRONG-LATEST\n"
	if err := os.WriteFile(filepath.Join(root, "engine.log"), []byte(log), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := newServer(root, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/jobs/7"} {
		w := httptest.NewRecorder()
		s.handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		body := w.Body.String()
		if path == "/" {
			start := strings.Index(body, `<article class="card`)
			if start < 0 {
				t.Fatal("no card")
			}
			end := strings.Index(body[start:], "</article>")
			body = body[start : start+end]
		}
		for _, want := range []string{"engine.log", "time unknown", "no space left on device &lt;script&gt;", ">stalled</span>"} {
			if !strings.Contains(body, want) {
				t.Fatalf("%s missing %q: %s", path, want, body)
			}
		}
		for _, bad := range []string{"WRONG-", "old failure", "<script>"} {
			if strings.Contains(body, bad) {
				t.Fatalf("%s attributed or unescaped %q", path, bad)
			}
		}
	}
	if err := os.Remove(filepath.Join(root, "engine.log")); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, httptest.NewRequest("GET", "/jobs/7", nil))
	if !strings.Contains(w.Body.String(), "No attributed line in the available log tail") {
		t.Fatal("missing log evidence was hidden")
	}
	if strings.Contains(translate("ja", "No new record or launch for 31m00s."), "No new") || translate("ja", "stalled") == "stalled" {
		t.Fatal("stall explanation was not translated")
	}
}

type startedStatusLog chan string

func (c startedStatusLog) Write(p []byte) (int, error) { c <- string(p); return len(p), nil }

func TestStalledRunFlag(t *testing.T) {
	root := fixtureQueue(t)
	saveStallState(t, root, chain.State{Step: "implement", Pending: &chain.Assignment{Role: "implement"}, PendingSince: time.Now().Add(-time.Hour)})
	if err := run(context.Background(), []string{"--run-dir", root, "--stalled-after=0"}, io.Discard); err == nil || !strings.Contains(err.Error(), "must be positive") {
		t.Fatalf("zero flag: %v", err)
	}
	for _, duration := range []string{"30m", "2h"} {
		ctx, cancel := context.WithCancel(context.Background())
		started := make(startedStatusLog, 1)
		finished := make(chan error, 1)
		go func() {
			finished <- run(ctx, []string{"--run-dir", root, "--listen=localhost:0", "--stalled-after=" + duration}, started)
		}()
		var line string
		select {
		case line = <-started:
		case err := <-finished:
			cancel()
			t.Fatalf("start: %v", err)
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatal("start timeout")
		}
		var address string
		if _, err := fmt.Sscanf(line, "status page on %s", &address); err != nil {
			cancel()
			t.Fatal(err)
		}
		client := &http.Client{Timeout: 5 * time.Second}
		response, err := client.Get("http://" + address + "/")
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(string(body), ">stalled</span>"); got != (duration == "30m") {
			t.Fatalf("flag %s stalled=%t", duration, got)
		}
		if err := <-finished; err != nil {
			t.Fatal(err)
		}
	}
}
