// Command attendant is the resident receptionist of the single-pod
// constitution (docs: HERMES_AS_LASSDAS_RUNTIME). Every interval it
//
//  1. runs one question tick — which is the whole reception protocol:
//     pulling new tracker activity into the ledger, adopting answers,
//     posting renotifications and shortfall replies, expiring deadlines,
//     recovering half-posted questions, and projecting the board; and
//  2. aligns Hermes cards with ledger states — creating the card for a
//     newly queued run (idempotent by delivery id), unblocking it when an
//     adopted answer returned the run to the queue, re-blocking it if a
//     waiting run's card somehow runs free, and retiring bookkeeping for
//     finished runs.
//
// It replaces the Lambda entirely: no webhook endpoint exists in this
// constitution — the attendant reads the tracker, the tracker never calls
// in. It holds no state beyond the ledger and a local delivery→card map.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"automation.internal/ticket-ingress/internal/attendant"
	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runtime"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "attendant:", err)
		os.Exit(1)
	}
}

func statusDir() string {
	if dir := os.Getenv("LASSDAS_STATUS_DIR"); dir != "" {
		return dir
	}
	return "/data/status"
}

func run() error {
	flags := flag.NewFlagSet("attendant", flag.ContinueOnError)
	configPath := flags.String("config", os.Getenv("LASSDAS_RUNTIME_CONFIG"), "runtime.json path")
	interval := flags.Duration("interval", time.Minute, "tick interval")
	observeInterval := flags.Duration("observe-interval", 5*time.Second, "status-board snapshot interval (0 disables the fast loop)")
	once := flags.Bool("once", false, "run a single tick and exit (for tests and cron)")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	config, err := runtime.Load(*configPath)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	services, err := runtime.BuildServices(config, logger)
	if err != nil {
		return err
	}
	defer func() { _ = services.Close() }()
	hermes := runtime.NewHermes(config)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// observe writes the status-board snapshot. Observation only; a failed
	// snapshot must never disturb the tick that feeds it. The mutex
	// serializes the fast loop against the tick's own call — events.jsonl
	// appends and the board.json rename must not interleave.
	var observeMu sync.Mutex
	observe := func() {
		observeMu.Lock()
		defer observeMu.Unlock()
		// A hung `hermes kanban list` must not hold this mutex forever —
		// the tick also observes, and a stuck observation would freeze the
		// whole reception. The timeout kills the CLI via CommandContext.
		observeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		if snapshot, err := attendant.SnapshotStatus(observeCtx, config, services, hermes); err != nil {
			logger.Error("status snapshot failed", "error", err.Error())
		} else if err := attendant.WriteBoardStatus(statusDir(), snapshot); err != nil {
			logger.Error("status write failed", "error", err.Error())
		}
	}

	tick := func() {
		// The attendant is the only reception mechanism (no webhook exists
		// in this constitution); one poisoned tick must not crash-loop it.
		defer func() {
			if recovered := recover(); recovered != nil {
				logger.Error("tick panicked", "panic", fmt.Sprint(recovered))
			}
		}()
		result := services.Tick.ProcessQuestionTick(ctx, hook.QuestionTickRequest{
			Protocol: hook.QuestionTickProtocol, AutomationRunID: config.AutomationRunID, IssuedAt: time.Now().UTC(),
		})
		logger.Info("tick", "decision", result.Decision, "code", result.Code)
		if config.OrchestrationCards() {
			// The cards orchestration: the attendant claims, prepares,
			// aligns chains and owns every report; no runner process exists.
			if err := attendant.SyncChains(ctx, config, services, hermes, logger); err != nil {
				logger.Error("chain sync failed", "error", err.Error())
			}
			observe()
		} else if err := runtime.SyncCards(ctx, services, hermes, logger); err != nil {
			logger.Error("card sync failed", "error", err.Error())
		}
	}

	if *once {
		tick()
		return nil
	}
	// The bell: the board server touches this file when the tracker's
	// webhook rings (body unread — the bell only means "worth looking").
	// A moved mtime makes the NEXT fast-loop pass request a full tick,
	// so tracker events reach the pipeline within seconds
	// instead of a minute. Forged rings cost one rate-limited look.
	bellPath := filepath.Join(statusDir(), "wakeup")
	lastBell := time.Time{}
	if info, err := os.Stat(bellPath); err == nil {
		lastBell = info.ModTime() // rings from before this boot are stale
	}
	bellRang := func() bool {
		info, err := os.Stat(bellPath)
		if err != nil || !info.ModTime().After(lastBell) {
			return false
		}
		lastBell = info.ModTime()
		return true
	}
	// The fast observation loop: the board's data sources (ledger, cards,
	// artifacts) change mid-tick as cards execute, and a minute-old picture
	// reads as a frozen board. This loop re-snapshots every few seconds —
	// it never touches the tracker unless the bell rang, so the extra rate
	// costs nothing external. Only the main loop runs ticks; the observation
	// loop signals a bell without waiting for the reception to finish.
	snapshotInterval := *observeInterval
	if !config.OrchestrationCards() {
		snapshotInterval = 0
	}
	runLoops(ctx, *interval, snapshotInterval, tick, observe, bellRang)
	logger.Info("attendant stopping")
	return nil
}

// A slow reception must not suspend its own progress display. Bells are
// coalesced while one tick runs; they never spawn concurrent reception work.
func runLoops(ctx context.Context, interval, observeInterval time.Duration, tick, observe func(), bellRang func() bool) {
	wakeup := make(chan struct{}, 1)
	if observeInterval > 0 {
		observeCtx, stopObserving := context.WithCancel(ctx)
		observing := make(chan struct{})
		defer func() { stopObserving(); <-observing }()
		go func() {
			defer close(observing)
			fast := time.NewTicker(observeInterval)
			defer fast.Stop()
			for {
				select {
				case <-observeCtx.Done():
					return
				case <-fast.C:
					if bellRang() {
						select {
						case wakeup <- struct{}{}:
						default:
						}
					}
					observe()
				}
			}
		}()
	}
	// Observation is already running when the first tick prepares a request.
	tick()
	timer := time.NewTicker(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			tick()
		case <-wakeup:
			tick()
		}
	}
}
