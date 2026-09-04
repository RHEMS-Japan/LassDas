package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"time"

	"automation.internal/ticket-ingress/internal/worker"
)

// spendReadTimeout bounds the billing readings. They run after the work is
// delivered, so a slow gateway must cost the report its cost line rather than
// hold the run open.
const spendReadTimeout = 20 * time.Second

// maxIntakeBytes bounds the intake record the run start is read from.
const maxIntakeBytes = 1 << 20

// loadRunSpendText reads what this run was billed and renders it for the
// requester. Every failure path returns an empty string: a missing cost line is
// a smaller problem than a wrong one, and the run's outcome never depends on
// whether a billing endpoint answered.
//
// The window starts at the intake record's read_at — the moment this run began
// working, which precedes its first paid call. The ticket's own created_at is
// deliberately not used: a ticket filed days before it is processed would make
// the window swallow every other run's spend in between.
func (t *Terminal) loadRunSpendText(ctx context.Context) string {
	since, ok := t.loadRunStart()
	if !ok {
		return ""
	}
	config, err := worker.LoadConfig(t.config.ConsumerConfigPath)
	if err != nil {
		return ""
	}
	reader, err := worker.NewGatewaySpendReader(&http.Client{Timeout: spendReadTimeout})
	if err != nil {
		return ""
	}
	readCtx, cancel := context.WithTimeout(ctx, spendReadTimeout)
	defer cancel()
	spend := worker.ReadRunSpend(readCtx, reader, config, since)
	return worker.ComposeSpendText(spend, worker.RolesByKeyEnv(config))
}

// loadRunStart reads the moment this run started working, as recorded when it
// read the ticket.
func (t *Terminal) loadRunStart() (time.Time, bool) {
	encoded, err := os.ReadFile(t.workspace + "/intake.json")
	if err != nil || len(encoded) > maxIntakeBytes {
		return time.Time{}, false
	}
	var intake struct {
		ReadAt time.Time `json:"read_at"`
	}
	if err := json.Unmarshal(encoded, &intake); err != nil || intake.ReadAt.IsZero() {
		return time.Time{}, false
	}
	return intake.ReadAt, true
}
