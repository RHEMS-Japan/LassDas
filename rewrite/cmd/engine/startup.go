package main

import (
	"context"
	"time"

	"ticket-runner/internal/chain"
)

// Acquisition has not dispatched a role or performed a delivery side effect.
// A transient read/storage failure must not discard the assigned work. Once
// read, keep the same original request while waiting for its history store.
// Command-line syntax and role/router wiring are checked before this loop.
func acquireRequest(ctx context.Context, directory string, read func(context.Context) (string, error), delay time.Duration, observe func(string)) (*chain.FileStore, error) {
	var request string
	loaded := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var err error
		if !loaded {
			request, err = read(ctx)
			loaded = err == nil
		}
		if err == nil && ctx.Err() == nil {
			var store *chain.FileStore
			store, err = chain.Open(directory, request)
			if err == nil {
				return store, nil
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		observe("waiting to receive or resume request: " + err.Error())
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
