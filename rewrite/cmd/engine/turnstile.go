package main

import (
	"context"
	"sync"
	"time"
)

// A turnstile hands out execution slots in the order requests were filed.
// Every accepted request has its own watcher asking for a slot on every tick,
// and a free slot would otherwise go to whichever watcher asked first, so a
// request filed later could run before an earlier one and, with one slot,
// keep doing so. A request now takes a slot only when no earlier request is
// waiting for one; a request that stops wanting a slot, because it waits on
// the requester or has ended, steps out of the line.
type turnstile struct {
	mu      sync.Mutex
	waiting map[int64]bool
	slots   chan struct{}
}

func newTurnstile(capacity int) *turnstile {
	return &turnstile{waiting: map[int64]bool{}, slots: make(chan struct{}, capacity)}
}

// try puts the request in line and takes a slot when it is the earliest
// request in line and a slot is free.
func (t *turnstile) try(id int64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.waiting[id] = true
	for other := range t.waiting {
		if other < id {
			return false
		}
	}
	select {
	case t.slots <- struct{}{}:
		delete(t.waiting, id)
		return true
	default:
		return false
	}
}

// leave takes the request out of the line without a slot.
func (t *turnstile) leave(id int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.waiting, id)
}

// acquire waits in line for a slot, or until the context ends.
func (t *turnstile) acquire(ctx context.Context, id int64) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if t.try(id) {
			return nil
		}
		select {
		case <-ctx.Done():
			t.leave(id)
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// release gives a slot back.
func (t *turnstile) release() { <-t.slots }
