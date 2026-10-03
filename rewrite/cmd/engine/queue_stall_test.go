package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"ticket-runner/internal/chain"
)

// Use real elapsed time: backdating an acceptance alone does not catch a
// watcher that starts its silence clock before it obtains an execution slot.
func TestTheActualWaitForASlotDoesNotAgeANewLaunch(t *testing.T) {
	cfg := watchConfiguration(t)
	window := 1
	cfg.Intake.StallNoticeMinutes = &window
	cfg.Roles[0].Processes[0].Command = []string{"/bin/sh", "-c", `cat > received.txt; printf '%s' "$$" > child-pid; exec sleep 60`}
	root, directory := noticeJob(t, chain.State{})
	if err := os.MkdirAll(filepath.Join(directory, "workspace"), 0700); err != nil {
		t.Fatal(err)
	}
	queueRanSince(t, root, cfg, time.Now().Add(-3*time.Minute))
	bound, err := bindRequestConfig(cfg, directory, "EXAMPLE-51")
	if err != nil {
		t.Fatal(err)
	}
	configPath, requestPath := filepath.Join(directory, "engine.json"), filepath.Join(directory, "request.txt")
	if err := writeRuntimeFile(configPath, configurationJSON(t, bound)); err != nil {
		t.Fatal(err)
	}
	if err := writeRuntimeFile(requestPath, []byte(noticeRequest)); err != nil {
		t.Fatal(err)
	}
	fixture := &noticeTracker{}
	fixture.install(t, alwaysChoose("implement"))
	turns := newTurnstile(1)
	if !turns.try(1) {
		t.Fatal("could not occupy the execution slot")
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(turns.release) }
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 85*time.Second)
	done := make(chan error, 1)
	issue := announcedIssue()
	issue.Creator.ID = 55
	var log bytes.Buffer
	go func() {
		done <- runWatchedRequest(ctx, cfg, issue, directory, configPath, requestPath, 30*time.Millisecond, turns, &serialLog{writer: &log})
	}()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if t.Failed() {
				t.Logf("watcher diagnostics: %s", &log)
			}
			if !errors.Is(err, context.Canceled) {
				t.Errorf("watcher did not stop cleanly: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("watcher did not return after cancellation")
		}
	}()
	waitFor(t, func() bool { return fixture.readings() >= 2 })
	startedWaiting := time.Now()
	<-time.After(61 * time.Second)
	pidFile := filepath.Join(directory, "workspace", "child-pid")
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatalf("a child started before the slot was released: %v", err)
	}
	release()
	waitTestPID(t, pidFile)
	afterStart := fixture.readings()
	waitFor(t, func() bool { return fixture.readings() >= afterStart+5 })
	if posts := fixture.withPrefix("依頼はまだ終わっていませんが"); len(posts) != 0 {
		t.Fatalf("actual queue waiting was counted as work silence: %v", posts)
	}
	t.Logf("waited %s for the occupied slot; a fresh child produced no stall notice", time.Since(startedWaiting))
}
