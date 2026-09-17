package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"time"

	"automation.internal/ticket-ingress/internal/worker"
)

// SpendBaselineFile keeps every configured key's running total as it was
// when this run began, so the report can subtract it. A provider with no
// per-window billing endpoint - which is most of them - can only say what a
// key has been billed in total, and the difference between two of those
// readings is the only honest answer to "what did this ticket cost".
const SpendBaselineFile = "spend-baseline.json"

// spendBaselineTimeout bounds the readings taken before the run starts
// working. They are worth a few seconds and no more: a slow billing endpoint
// must cost the report its cost line, never the delivery.
const spendBaselineTimeout = 10 * time.Second

// maxSpendBaselineBytes bounds the record on the way back in.
const maxSpendBaselineBytes = 1 << 16

// recordSpendBaseline reads each key's running total and writes it beside the
// run. Called once, after the workspace is cleared and before the first paid
// call, so nothing this run spends is already inside the number.
//
// Best-effort throughout: a reading that cannot be taken leaves no record,
// and a report with no record says the cost is not available.
func (p *Pipeline) recordSpendBaseline(ctx context.Context) {
	config, err := worker.LoadConfig(p.Config.ConsumerConfigPath)
	if err != nil {
		return
	}
	reader, err := worker.NewGatewayUsageReader(&http.Client{
		Timeout: spendBaselineTimeout, Transport: p.usageTransport,
	})
	if err != nil {
		return
	}
	p.writeSpendBaseline(ctx, config, reader)
}

// writeSpendBaseline is the reading and the record without the configuration
// file, so the behaviour can be measured against a server.
func (p *Pipeline) writeSpendBaseline(ctx context.Context, config worker.Config, reader worker.UsageReader) {
	readCtx, cancel := context.WithTimeout(ctx, spendBaselineTimeout)
	defer cancel()
	baseline := worker.ReadUsageBaseline(readCtx, reader, config)
	if !baseline.Usable() {
		return
	}
	encoded, err := json.Marshal(baseline)
	if err != nil {
		return
	}
	_ = writeRecordAtomically(p.path(SpendBaselineFile), encoded)
}

// loadSpendBaseline reads the record back. A missing or malformed one is
// reported as absent, and the reading falls back to whatever the gateway
// itself can answer.
func loadSpendBaseline(runDir string) (worker.UsageBaseline, bool) {
	raw, err := readWorkspaceFile(runDir+string(os.PathSeparator)+SpendBaselineFile, maxSpendBaselineBytes)
	if err != nil {
		return worker.UsageBaseline{}, false
	}
	var baseline worker.UsageBaseline
	if json.Unmarshal(raw, &baseline) != nil || !baseline.Usable() {
		return worker.UsageBaseline{}, false
	}
	return baseline, true
}
