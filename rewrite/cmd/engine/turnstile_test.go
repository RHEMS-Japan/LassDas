package main

import (
	"context"
	"testing"
	"time"
)

func TestTheTurnstileHandsSlotsOutInFilingOrder(t *testing.T) {
	turns := newTurnstile(1)
	if !turns.try(30) {
		t.Fatal("a free slot with nobody earlier in line was not taken")
	}
	turns.release()
	if !turns.try(20) {
		t.Fatal("a free slot was not taken")
	}
	// While 20 holds the only slot, 30 and then 15 ask and wait in line.
	if turns.try(30) || turns.try(15) {
		t.Fatal("a request got a slot while another held the only one")
	}
	turns.release()
	// The slot is free again: 30 asks first, but 15 is earlier in line.
	if turns.try(30) {
		t.Fatal("a later request took the slot ahead of the earlier one in line")
	}
	if !turns.try(15) {
		t.Fatal("the earliest request in line did not get the slot")
	}
	turns.release()
	// 30 is still in line; a request filed earlier than it, 25, wins.
	if !turns.try(25) {
		t.Fatal("an earlier request was held back by a later one in line")
	}
	turns.release()
	// A request that leaves the line no longer holds later ones back.
	if turns.try(40) {
		t.Fatal("a later request took the slot while an earlier one was in line")
	}
	turns.leave(30)
	if !turns.try(40) {
		t.Fatal("the last request in line did not get the slot after the earlier one left")
	}
	turns.release()
	// A request entered at discovery holds its place before its watcher asks.
	turns.enter(7)
	if turns.try(8) {
		t.Fatal("a later request took the slot ahead of one entered at discovery")
	}
	if !turns.try(7) {
		t.Fatal("the request entered at discovery did not get the slot")
	}
	turns.release()
	turns.leave(8)
	// acquire waits its turn and gives up with the context.
	if !turns.try(5) {
		t.Fatal("a free slot was not taken")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := turns.acquire(ctx, 6); err == nil {
		t.Fatal("acquire returned while the only slot was held")
	}
	turns.release()
	if err := turns.acquire(context.Background(), 6); err != nil {
		t.Fatal(err)
	}
	turns.release()
}
