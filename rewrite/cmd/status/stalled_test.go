package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ticket-runner/internal/chain"
)

func TestStalledDisplayUsesLatestHistoryAndKeepsAnError(t *testing.T) {
	now := time.Now().UTC()
	for _, test := range []struct {
		name                string
		age                 time.Duration
		failure             string
		waiting, done, want bool
	}{
		{"recent progress", time.Minute, "", false, false, false},
		{"no progress", 31 * time.Minute, "", false, false, true},
		{"recorded failure", 31 * time.Minute, "no space left on device\nprivate detail", false, false, true},
		{"waiting for requester", time.Hour, "", true, false, false},
		{"finished", time.Hour, "", false, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := fixtureQueue(t)
			state := chain.State{Step: "implement", Waiting: test.waiting, Done: test.done,
				History: []chain.Result{{Role: "implement", Speaker: "worker", FinishedAt: now.Add(-test.age), Error: test.failure}}}
			raw, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "jobs/7/run/history.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			s, err := newServer(root, "", "", "")
			if err != nil {
				t.Fatal(err)
			}
			j := s.loadJob("7", now, false)
			if (j.Status == "stalled") != test.want {
				t.Fatalf("status=%s, want stalled=%t", j.Status, test.want)
			}
			if test.want && j.Lane != "attention" {
				t.Fatal("stall was still counted as running")
			}
			if test.failure != "" && !strings.Contains(j.Failure, "no space left on device") {
				t.Fatal("last error was lost")
			}
			if detail := s.loadJob("7", now, true); detail.Status != j.Status || detail.Failure != j.Failure {
				t.Fatal("overview and detail disagree")
			}
			if strings.Contains(j.Failure, "private detail") {
				t.Fatal("displayed more than the error's first line")
			}
			w := httptest.NewRecorder()
			s.overview(w, httptest.NewRequest("GET", "/", nil))
			if test.want && !strings.Contains(w.Body.String(), "stalled") {
				t.Fatal("stall missing from overview")
			}
			s.stalledAfter = 2 * time.Hour
			if got := s.loadJob("7", now, false); got.Status == "stalled" {
				t.Fatal("configured threshold ignored")
			}
		})
	}
}

func TestStalledExcludesHeldWorkAndClearsOnAnyNewRecord(t *testing.T) {
	now := time.Now()
	s := &server{stalledAfter: 30 * time.Minute}
	for _, held := range []string{"stop", "pause", "budget", "queued", "new record"} {
		j := &job{Lane: "running", State: &chain.State{History: []chain.Result{
			{FinishedAt: now.Add(-time.Hour), Error: "old failure"},
		}}}
		switch held {
		case "stop":
			j.Stopped = true
		case "pause":
			j.WorkPause = "awaiting instruction"
		case "budget":
			j.Notices = []map[string]any{{"kind": "budget-paused", "written_at": now.Add(-time.Minute).Format(time.RFC3339Nano)}}
		case "queued":
			j.Lane = "queued"
		case "new record":
			j.State.History = append(j.State.History, chain.Result{Speaker: "runtime", FinishedAt: now.Add(-time.Minute), Error: "new failure"})
		}
		s.markStalled(j, now)
		if j.Status == "stalled" {
			t.Fatalf("%s was classified as stalled", held)
		}
	}
}
