package runner

import (
	"context"
	"encoding/json"
	"errors"
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
	return t.readSpendWith(ctx, config, &http.Client{Timeout: spendReadTimeout}, since)
}

// readSpendWith is the whole reading with the transport handed in: which
// readings are asked for, in which order, and what is kept. Everything above
// it is the configuration file and the real client.
func (t *Terminal) readSpendWith(ctx context.Context, config worker.Config, client *http.Client, since time.Time) string {
	reader, err := worker.NewGatewaySpendReader(client)
	if err != nil {
		return ""
	}
	return t.readAndRecordSpend(ctx, config, t.withUsageDelta(reader, client), since)
}

// withUsageDelta adds the difference-of-running-totals reading behind the
// per-window one, when this run recorded a baseline at its start. The
// per-window figure is the better answer and is asked for first; the
// difference is what a provider that has no such endpoint can still say.
func (t *Terminal) withUsageDelta(reader worker.SpendReader, client *http.Client) worker.SpendReader {
	baseline, found := loadSpendBaseline(t.workspace)
	if !found {
		return reader
	}
	usage, err := worker.NewGatewayUsageReader(client)
	if err != nil {
		return reader
	}
	delta, err := worker.NewUsageDeltaReader(usage, baseline)
	if err != nil {
		return reader
	}
	chained, err := worker.NewFallbackSpendReader(reader, delta)
	if err != nil {
		return reader
	}
	return chained
}

// readAndRecordSpend reads every configured key's figure since the run
// began, keeps the reading beside the run, and renders it for the requester.
func (t *Terminal) readAndRecordSpend(ctx context.Context, config worker.Config, reader worker.SpendReader, since time.Time) string {
	readCtx, cancel := context.WithTimeout(ctx, spendReadTimeout)
	defer cancel()
	spend := worker.ReadRunSpend(readCtx, reader, config, since)
	roles := worker.RolesByKeyEnv(config)
	text := worker.ComposeSpendText(spend, roles)
	t.recordSpend(spend, roles, since, text)
	return text
}

// SpendRecordFile keeps the billing reading beside the run: the same
// figures the terminal comment carries, per key with the roles that key
// serves, so the ticket page shows what a run cost - a failed one too -
// without asking the gateway again. Written on every report, so a report
// posted again carries the latest reading.
const SpendRecordFile = "spend.json"

type spendRecord struct {
	ReadAt   time.Time        `json:"read_at"`
	Since    time.Time        `json:"since"`
	Complete bool             `json:"complete"`
	TotalUSD float64          `json:"total_usd"`
	Keys     []spendRecordKey `json:"keys"`
	Text     string           `json:"text"`
	// Approximate says the figures are differences of running totals, so a
	// reader of the record knows what kind of number it holds.
	Approximate bool `json:"approximate,omitempty"`
}

type spendRecordKey struct {
	// KeyEnv names the environment variable, never the key; KeyName is
	// what the gateway calls the key.
	KeyEnv   string   `json:"key_env"`
	KeyName  string   `json:"key_name,omitempty"`
	Roles    []string `json:"roles,omitempty"`
	SpendUSD float64  `json:"spend_usd"`
	Unpriced int      `json:"unpriced_requests,omitempty"`
}

// recordSpend writes the reading, or removes a stale record when there was
// none. Best-effort: the report never waits on it. The file is removed
// before the write so a link left at the path cannot carry the record
// outside the workspace.
func (t *Terminal) recordSpend(spend worker.RunSpend, roles map[string][]string, since time.Time, text string) {
	path := t.workspace + "/" + SpendRecordFile
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return
	}
	if len(spend.Keys) == 0 {
		return
	}
	record := spendRecord{ReadAt: time.Now().UTC(), Since: since, Complete: spend.Complete,
		TotalUSD: spend.TotalUSD, Text: text, Approximate: spend.Approximate}
	for _, key := range spend.Keys {
		entry := spendRecordKey{KeyEnv: key.KeyEnv, KeyName: key.KeyName, SpendUSD: key.SpendUSD, Unpriced: key.Unpriced}
		seen := map[string]bool{}
		for _, env := range append([]string{key.KeyEnv}, key.AlsoKeyEnvs...) {
			for _, role := range roles[env] {
				if !seen[role] {
					seen[role] = true
					entry.Roles = append(entry.Roles, role)
				}
			}
		}
		record.Keys = append(record.Keys, entry)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return
	}
	_ = writeRecordAtomically(path, encoded)
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
