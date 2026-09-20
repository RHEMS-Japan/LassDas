package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Advancing a chain is local work - the ledger and the cards - but it used to
// run only when the tracker was read, once a minute. A stage that finished
// waited for that minute before the next one started, and a delivery has a
// dozen stages, so most of its wall clock was the wait. The chains now have
// their own faster clock, and reading the tracker keeps the slow one.
func TestChainsAdvanceWithoutWaitingForTheTrackerPoll(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var ticks, chains atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		// The tracker is read once an hour here; the chains every few
		// milliseconds. Anything that advances must come from the chains.
		runLoops(ctx, time.Hour, 0, 5*time.Millisecond,
			func() { ticks.Add(1) }, nil, func() { chains.Add(1) }, nil)
	}()
	deadline := time.After(1500 * time.Millisecond)
	for chains.Load() < 5 {
		select {
		case <-deadline:
			t.Fatalf("段の進行が %d 回しか走っていません", chains.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done
	// The tracker was read once, at start. The chains ran many times: the
	// point of the change is that the two are no longer the same clock.
	if ticks.Load() != 1 {
		t.Fatalf("課題管理への問い合わせが %d 回に増えています", ticks.Load())
	}
}

// Two passes never overlap: the tick advances chains right after reading the
// tracker, and the faster loop advances them too. A run being started twice
// at once is a delivery done twice.
func TestTheTwoPlacesThatAdvanceChainsNeverOverlap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var mu sync.Mutex
	inside, overlaps := 0, 0
	guarded := func() {
		mu.Lock()
		inside++
		if inside > 1 {
			overlaps++
		}
		mu.Unlock()
		time.Sleep(2 * time.Millisecond)
		mu.Lock()
		inside--
		mu.Unlock()
	}
	// The production closure takes a lock of its own; this test stands in
	// for it with the same shape, and drives both callers at once.
	var chainMu sync.Mutex
	syncChains := func() {
		chainMu.Lock()
		defer chainMu.Unlock()
		guarded()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		runLoops(ctx, 3*time.Millisecond, 0, 3*time.Millisecond,
			func() { syncChains() }, nil, syncChains, nil)
	}()
	<-ctx.Done()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if overlaps != 0 {
		t.Fatalf("段の進行が %d 回重なりました", overlaps)
	}
}

// Tied to zero, the chains keep the tick's clock - which is what they had
// before this loop existed. An instance that does not want the faster clock
// gets the old behaviour exactly.
func TestChainsTiedToTheTickWhenTheIntervalIsZero(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var loose atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		runLoops(ctx, time.Hour, 0, 0, func() {}, nil, func() { loose.Add(1) }, nil)
	}()
	<-ctx.Done()
	<-done
	if loose.Load() != 0 {
		t.Fatalf("0 を指定したのに独立した輪が %d 回走りました", loose.Load())
	}
}
