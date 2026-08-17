package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Getenv, http.DefaultTransport); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "controller:", err)
		os.Exit(1)
	}
}
