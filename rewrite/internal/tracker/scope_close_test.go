package tracker

import (
	"sync"
	"testing"
)

// Once Close has begun, no request is counted: one that arrives then is
// refused, and the count Close waits on is never raised from zero while it
// waits. (A WaitGroup raised from zero while another goroutine waits on it is
// a race; at b84f97f the handler counted with no guard against Close.)
func TestAnAccessCountsNoRequestOnceItIsClosing(t *testing.T) {
	var counted answering
	if !counted.start() {
		t.Fatal("a request before Close was refused")
	}
	counted.stop()
	if counted.start() {
		t.Fatal("a request was counted after Close began")
	}
	counted.group.Done()
	counted.group.Wait()
}

// Requests keep arriving while Close stops the count and waits on it. Run
// with -race: counting and waiting never race, and nothing is counted after
// the wait began.
func TestCountingRequestsWhileClosingDoesNotRace(t *testing.T) {
	for range 200 {
		var counted answering
		var arriving sync.WaitGroup
		for range 8 {
			arriving.Add(1)
			go func() {
				defer arriving.Done()
				for range 50 {
					if counted.start() {
						counted.group.Done()
					}
				}
			}()
		}
		counted.stop()
		counted.group.Wait()
		if counted.start() {
			t.Fatal("a request was counted after the wait began")
		}
		arriving.Wait()
	}
}
