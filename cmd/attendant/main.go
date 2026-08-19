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
	"syscall"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runtime"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "attendant:", err)
		os.Exit(1)
	}
}

func run() error {
	flags := flag.NewFlagSet("attendant", flag.ContinueOnError)
	configPath := flags.String("config", os.Getenv("LASSDAS_RUNTIME_CONFIG"), "runtime.json path")
	interval := flags.Duration("interval", time.Minute, "tick interval")
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
	cards, err := runtime.LoadCardLedger(config.LedgerPath)
	if err != nil {
		return err
	}
	hermes := runtime.NewHermes(config)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

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
		if err := runtime.SyncCards(ctx, services, cards, hermes, logger); err != nil {
			logger.Error("card sync failed", "error", err.Error())
		}
	}

	tick()
	if *once {
		return nil
	}
	timer := time.NewTicker(*interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			// The context is signal-only, so Done always means a clean stop.
			logger.Info("attendant stopping")
			return nil
		case <-timer.C:
			tick()
		}
	}
}
