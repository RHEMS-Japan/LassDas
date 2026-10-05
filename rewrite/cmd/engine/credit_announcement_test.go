package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ticket-runner/internal/chain"
)

func TestAcceptanceAndReplyDoNotAnnounceWorkWhileCreditIsLow(t *testing.T) {
	for _, answered := range []bool{false, true} {
		name := "accepted"
		if answered {
			name = "answered"
		}
		t.Run(name, func(t *testing.T) {
			cfg := watchConfiguration(t)
			cfg.Intake.Announce = true
			cfg.Intake.MinModelCredit = 5
			holdingWorker(&cfg)
			var mu sync.Mutex
			remaining := "3"
			server := creditServer(t, func() string {
				mu.Lock()
				defer mu.Unlock()
				return `{"data":{"limit_remaining":` + remaining + `}}`
			})
			cfg.Intake.ModelCreditURL, cfg.Intake.Client = server.URL, server.Client()
			fixture := &noticeTracker{}
			fixture.install(t, alwaysChoose("implement"))
			state := chain.State{Waiting: answered, Step: "implement", History: []chain.Result{}}
			root, directory := noticeJob(t, state)
			queueRanSince(t, root, cfg, time.Now().Add(-time.Hour))
			if answered {
				if err := writeRuntimeFile(filepath.Join(directory, "question.json"), []byte(`{"after":899}`)); err != nil {
					t.Fatal(err)
				}
				fixture.rows = []json.RawMessage{issueComment(900, 55, requesterAnswer)}
				data, err := json.Marshal(noticeLog{Notices: []noticeRecord{{Kind: acceptedNotice, Predates: true}}})
				if err != nil {
					t.Fatal(err)
				}
				if err := writeRuntimeFile(filepath.Join(directory, "notices.json"), data); err != nil {
					t.Fatal(err)
				}
			}
			finish := startStopQueue(t, cfg, root, 20*time.Millisecond, io.Discard)
			waitFor(t, func() bool { return len(fixture.all()) > 0 })
			time.Sleep(120 * time.Millisecond)
			finish()
			words := fixture.all()
			if len(words) != 1 || !strings.Contains(words[0], "待機") || strings.Contains(words[0], "再開しました") || strings.Contains(words[0], "すぐに自動処理") {
				t.Fatalf("expected one truthful waiting notice, got %q", words)
			}
			if _, err := os.Stat(filepath.Join(directory, "workspace", "child-pid")); err == nil {
				t.Fatal("work started below the configured minimum")
			}
			// Restarting while credit remains low must not repeat the notice.
			finish = startStopQueue(t, cfg, root, 20*time.Millisecond, io.Discard)
			time.Sleep(120 * time.Millisecond)
			if got := fixture.all(); len(got) != 1 {
				t.Fatalf("restart repeated waiting: %q", got)
			}
			mu.Lock()
			remaining = "20"
			mu.Unlock()
			waitFor(t, func() bool { _, err := os.Stat(filepath.Join(directory, "workspace", "child-pid")); return err == nil })
			waitFor(t, func() bool { return fixture.count(restoredNoticeText) > 0 })
			time.Sleep(120 * time.Millisecond)
			finish()
			if got := fixture.all(); len(got) != 2 || got[1] != restoredNoticeText {
				t.Fatalf("recovery did not say once that work began: %q", got)
			}
		})
	}
}

func TestCreditRecoveryWaitsForAnActualExecutionSlot(t *testing.T) {
	cfg := watchConfiguration(t)
	cfg.Intake.Announce = true
	cfg.Intake.MinModelCredit = 5
	holdingWorker(&cfg)
	server := creditServer(t, func() string { return `{"data":{"limit_remaining":20}}` })
	cfg.Intake.ModelCreditURL, cfg.Intake.Client = server.URL, server.Client()
	fixture := &noticeTracker{}
	fixture.install(t, alwaysChoose("implement"))
	root, directory := noticeJob(t, chain.State{})
	queueRanSince(t, root, cfg, time.Now().Add(-time.Hour))
	issue := announcedIssue()
	issue.Creator.ID = 55
	acceptTurn(context.Background(), cfg, issue, directory, 1, true, func(message string) { t.Log(message) })
	if got := fixture.all(); len(got) != 1 || !strings.Contains(got[0], "前に 1 件") || !strings.Contains(got[0], budgetWaitingText) {
		t.Errorf("low-allowance acceptance lost its queue position: %q", got)
	}
	bound, err := bindRequestConfig(cfg, directory, issue.Key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(directory, "workspace"), 0700); err != nil {
		t.Fatal(err)
	}
	configPath, requestPath := filepath.Join(directory, "engine.json"), filepath.Join(directory, "request.txt")
	if err := writeRuntimeFile(configPath, configurationJSON(t, bound)); err != nil {
		t.Fatal(err)
	}
	if err := writeRuntimeFile(requestPath, []byte(noticeRequest)); err != nil {
		t.Fatal(err)
	}
	slots := newTurnstile(1)
	slots.enter(41)
	if !slots.try(41) {
		t.Fatal("fixture did not reserve the first slot")
	}
	slots.enter(issue.ID)
	held := true
	t.Cleanup(func() {
		if held {
			slots.release()
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	ended := make(chan error, 1)
	log := &lockedLog{}
	go func() {
		ended <- runWatchedRequest(ctx, bound, issue, directory, configPath, requestPath, 20*time.Millisecond, slots, log)
	}()
	t.Cleanup(func() {
		cancel()
		err := <-ended
		if t.Failed() {
			t.Logf("run ended: %v\n%s", err, log.text.String())
		}
	})
	before := fixture.readings()
	waitFor(t, func() bool { return fixture.readings() >= before+5 })
	if fixture.count(restoredNoticeText) != 0 {
		t.Fatalf("said work resumed while another request still owned the slot: %q", fixture.all())
	}
	slots.release()
	held = false
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(directory, "workspace", "child-pid")); return err == nil })
	waitFor(t, func() bool { return fixture.count(restoredNoticeText) == 1 })
	before = fixture.readings()
	waitFor(t, func() bool { return fixture.readings() >= before+5 })
	if got := fixture.all(); len(got) != 2 {
		t.Fatalf("repeated the recovery notice: %q", got)
	}
	if fixture.count(startedNoticeText) != 0 {
		t.Fatal("credit recovery was followed by an ordinary start announcement")
	}
}

func TestAStatusPageCannotImplyABudgetHold(t *testing.T) {
	now := time.Now()
	log := noticeLog{Notices: []noticeRecord{{Kind: acceptedNotice, Text: acceptedNoticeText(0, "https://status.example/"+budgetWaitingText), WrittenAt: now, PostedAt: &now}}}
	if noticeDue(log, restoredNotice, now) || !noticeDue(log, pausedNotice, now) {
		t.Fatal("the operator's status page was interpreted as a credit hold")
	}
}
