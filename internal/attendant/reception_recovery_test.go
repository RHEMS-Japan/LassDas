package attendant

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/worker"
)

// Use actual sealed records, not an empty ticket beside a routing fragment.
// Keep the harness's delivery settings while supplying the remaining config.
func writeAcceptedReception(t *testing.T, h *depthHarness, text string) worker.ReadinessDecision {
	t.Helper()
	config, err := worker.LoadConfig("../../config/m1-consumer.json")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(h.config.ConsumerConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var partial struct {
		Consumers []json.RawMessage `json:"consumers"`
	}
	if err := json.Unmarshal(raw, &partial); err != nil || len(partial.Consumers) != 1 {
		t.Fatalf("consumer fixture: %v", err)
	}
	if err := json.Unmarshal(partial.Consumers[0], &config.Consumers[0]); err != nil {
		t.Fatal(err)
	}
	encode := func(path string, value any) {
		t.Helper()
		body, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	encode(h.config.ConsumerConfigPath, config)
	digest, err := config.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	request := worker.TicketRequest{SchemaVersion: 1, DeliveryID: h.deliveryID,
		InputSHA256: strings.Repeat("b", 64), ConfigSHA256: digest, ToolSHA: h.config.Identity.EngineSHA,
		IssueKey: depthRunID, RunID: depthRunID, Summary: "Implement the request",
		Repository: depthRepository, Mode: config.Consumers[0].Mode.ID, Request: text}
	source, err := worker.ReadSourceSnapshot(h.runDir, strings.Repeat("d", 40), request, config)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := worker.FallbackReadinessDecision(source, request, config, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	encode(filepath.Join(h.runDir, "readiness-ticket.json"), request)
	encode(filepath.Join(h.runDir, "readiness-source.json"), source)
	encode(filepath.Join(h.runDir, "history/readiness/decision.json"), decision)
	return decision
}

func TestAnInterruptedAcceptedHandoffDoesNotRepeatReception(t *testing.T) {
	for _, afterCrash := range []bool{false, true} {
		name := "handoff immediately after reception"
		if afterCrash {
			name = "claimed without its first card"
		}
		t.Run(name, func(t *testing.T) {
			h := newDepthHarness(t, "production", true, "")
			want := writeAcceptedReception(t, h, "Implement the requested behavior.")
			if _, err := runner.ChainPlanFromDecision(h.runDir, h.config.ConsumerConfigPath); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(h.boardFile, []byte("[]"), 0o600); err != nil {
				t.Fatal(err)
			}
			h.write("history/readiness/decision.json", "{broken")
			workerLog := filepath.Join(t.TempDir(), "verbs")
			h.config.WorkerBin = filepath.Join(t.TempDir(), "worker")
			if err := os.WriteFile(h.config.WorkerBin, []byte("#!/bin/sh\nprintf '%s\\n' \"$1\" >> "+workerLog+"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			if afterCrash {
				h.tick()
			} else {
				envelope, err := readEnvelope(h.runDir, h.deliveryID)
				if err != nil {
					t.Fatal(err)
				}
				pipeline := &runner.Pipeline{Config: h.config, Services: h.services, Envelope: envelope, Workspace: h.runDir, Logger: h.logger}
				if err := startAcceptedChain(t.Context(), h.config, h.services, h.hermes, h.runRow(), pipeline, h.logger); err != nil {
					t.Fatal(err)
				}
			}
			if row := h.runRow(); row.State != "claimed" || row.TerminalCode != "" {
				t.Fatalf("accepted handoff was restarted or ended: %+v; %v", row, h.logger.lines)
			}
			verbs, err := os.ReadFile(workerLog)
			if err != nil || string(verbs) != "implement-instruction\n" {
				t.Fatalf("handoff did not resume directly at implementation: %q %v; %v", verbs, err, h.logger.lines)
			}
			if !strings.Contains(h.calls(), "|create|") || strings.Contains(h.calls(), "|archive|") || len(*h.posted) != 0 {
				t.Fatalf("handoff did not create cards without questions: %s %v", h.calls(), *h.posted)
			}
			var got worker.ReadinessDecision
			if err := worker.ReadJSONFile(filepath.Join(h.runDir, "history/readiness/decision.json"), worker.MaxReadinessJSONBytes, &got); err != nil || got.DecisionSHA256 != want.DecisionSHA256 {
				t.Fatalf("the resumed cards lost the accepted decision: %v %+v", err, got)
			}
		})
	}
}

func TestUnavailableFirstHandoffKeepsTheClaim(t *testing.T) {
	h := newDepthHarness(t, "production", true, "")
	h.write("history/readiness/decision.json", "{broken")
	envelope, err := readEnvelope(h.runDir, h.deliveryID)
	if err != nil {
		t.Fatal(err)
	}
	pipeline := &runner.Pipeline{Config: h.config, Services: h.services, Envelope: envelope, Workspace: h.runDir, Logger: h.logger}
	for range 3 {
		err := startAcceptedChain(t.Context(), h.config, h.services, h.hermes, h.runRow(), pipeline, h.logger)
		if !errors.Is(err, runner.ErrReadinessDecisionUnreadable) {
			t.Fatalf("recovery reason was lost: %v", err)
		}
	}
	if row := h.runRow(); row.State != "claimed" || len(*h.posted) != 0 {
		t.Fatalf("unavailable first handoff ended the accepted run: %+v %v", row, *h.posted)
	}
}

func TestAStopStillWorksWhileReceptionRecoveryIsUnavailable(t *testing.T) {
	h := newDepthHarness(t, "production", true, "")
	unpublished(t, h)
	runningChainCard(t, h, runtime.StageImplement)
	h.write("history/readiness/decision.json", "{broken")
	h.tick()
	*h.comments = append(*h.comments, ticketComment(stopCommentID, depthRequester, "停止"))
	rewindStopRead(t, h.runDir, 2*tickStopReadInterval)
	h.tick()
	if row := h.runRow(); row.State != "terminal" || row.TerminalCode != "cancelled" {
		t.Fatalf("reception recovery prevented the requested stop: %+v; %v", row, h.logger.lines)
	}
	if got := stopAcknowledgements(*h.posted); got != 1 {
		t.Fatalf("stop acknowledgements = %d", got)
	}
}

func TestDamagedReceptionKeepsTheSameRunningCards(t *testing.T) {
	h := newDepthHarness(t, "production", true, "")
	want := writeAcceptedReception(t, h, "Implement the requested behavior.")
	runningChainCard(t, h, runtime.StageImplement)
	h.tick()
	before := h.calls()
	for _, damaged := range []string{"{broken", "{}", `{"request_kind":"change","needs_design":true}`} {
		h.write("history/readiness/decision.json", damaged)
		h.tick()
		if row := h.runRow(); row.State != "claimed" || row.TerminalCode != "" {
			t.Fatalf("accepted work was restarted or ended: %+v; %v", row, h.logger.lines)
		}
		if len(*h.posted) != 0 {
			t.Fatalf("a question or ending was posted: %v", *h.posted)
		}
		if calls := h.calls()[len(before):]; strings.Contains(calls, "|archive|") || strings.Contains(calls, "|create|") {
			t.Fatalf("the running chain was replaced: %s", calls)
		}
		var got worker.ReadinessDecision
		if err := worker.ReadJSONFile(filepath.Join(h.runDir, "history/readiness/decision.json"), worker.MaxReadinessJSONBytes, &got); err != nil || got.DecisionSHA256 != want.DecisionSHA256 {
			t.Fatalf("accepted decision was not restored: %v %+v", err, got)
		}
	}
	if _, err := os.Stat(filepath.Join(h.runDir, runner.ReceptionAgainFile)); !os.IsNotExist(err) {
		t.Fatalf("recovery re-ran the reception: %v", err)
	}
}
