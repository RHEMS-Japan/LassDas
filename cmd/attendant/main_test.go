package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestObservationContinuesDuringInitialAndBellDrivenReception(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan int, 8)
	release := make(chan struct{})
	observed := make(chan int, 128)
	observedBells := make(chan int, 128)
	done := make(chan struct{})
	var calls, active, overlap atomic.Int32
	var bell atomic.Bool
	bells := 0 // Owned by the observation loop.
	tick := func() {
		if active.Add(1) != 1 {
			overlap.Add(1)
		}
		defer active.Add(-1)
		started <- int(calls.Add(1))
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	go func() {
		defer close(done)
		runLoops(ctx, time.Hour, 5*time.Millisecond, tick, func() {
			select {
			case observed <- int(calls.Load()):
			default:
			}
			select {
			case observedBells <- bells:
			default:
			}
		}, func() bool {
			if !bell.Swap(false) {
				return false
			}
			bells++
			return true
		})
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("loops did not stop")
		}
	}()
	await := func(ch <-chan int, want int) {
		t.Helper()
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		for {
			select {
			case got := <-ch:
				if got == want {
					return
				}
			case <-timer.C:
				t.Fatalf("no progress for reception %d", want)
			}
		}
	}
	await(started, 1)
	await(observed, 1) // Initial reception has not finished.
	for i := range 4 {
		bell.Store(true)
		await(observedBells, i+1) // This ring was queued before observation.
	}
	if calls.Load() != 1 || overlap.Load() != 0 {
		t.Fatal("bells started concurrent reception work")
	}
	release <- struct{}{}
	await(started, 2)
	await(observed, 2) // The bell-driven reception is also blocked.
	release <- struct{}{}
	await(observed, 2)
	select {
	case n := <-started:
		t.Fatalf("bells were not coalesced: reception %d", n)
	case <-time.After(30 * time.Millisecond):
	}
}
