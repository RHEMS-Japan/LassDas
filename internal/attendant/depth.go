package attendant

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
	"automation.internal/ticket-ingress/internal/worker"
)

// The delivery depth.
//
// How far a change travels without a person is the destination's own
// setting, and until now no run read it. The publish stage proposed a pull
// request, the delivery ended there, and a continuation afterwards carried
// every delivery the same distance regardless of what its destination had
// asked for. The depth is decided here instead — once, before the first
// delivery card is issued — from the destination's `delivery` and from
// whether this instance has the cards configured to carry it.
//
// Capability is not completion. If the instance cannot reach the requested
// depth, the record names what is missing while the same delivery remains
// open. Restoring the route may change its capability, never its goal.

// deliveryDepthFile is where the decision is sealed, beside the delivery's
// other records on the volume.
const deliveryDepthFile = "delivery-depth.json"

// depthSchemaVersion is this record's shape.
const depthSchemaVersion = 1

// depthPlan is one delivery's depth: what the destination asked for, how
// far this instance can actually carry it, and what the difference needs.
type depthPlan struct {
	SchemaVersion int    `json:"schema_version"`
	Repository    string `json:"repository"`
	// Configured is the destination's own setting. Reached is the deepest
	// point this instance is equipped to carry the change to, which is the
	// same thing unless something the deeper points need is not configured.
	Configured string `json:"configured"`
	Reached    string `json:"reached"`
	// Missing names the settings a deeper delivery would have needed, by
	// the keys an operator would write. Empty when nothing is missing.
	Missing   []string  `json:"missing,omitempty"`
	DecidedAt time.Time `json:"decided_at"`
}

// short reports whether the available route falls short of the goal.
func (p depthPlan) short() bool { return p.Reached != p.Configured }

// reachesIntegration and reachesProduction say which delivery cards this
// depth calls for.
func (p depthPlan) reachesIntegration() bool {
	return p.Reached == string(worker.DeliverIntegration) || p.Reached == string(worker.DeliverProduction)
}
func (p depthPlan) reachesProduction() bool { return p.Reached == string(worker.DeliverProduction) }

// shortfallText is the one line the ticket is given when the delivery
// stopped before the destination's setting. Requester-facing: it says where
// the change got to and what the rest would have needed, and it names the
// settings rather than describing them, so an operator can act on it.
func (p depthPlan) shortfallText() string {
	if !p.short() {
		return ""
	}
	where := "取り込み用の Pull Request の作成"
	if p.reachesIntegration() {
		where = "staging への反映と確認"
	}
	line := "この納品先は " + p.Configured + " まで届ける設定ですが、" + where + "までで止めています。"
	if len(p.Missing) > 0 {
		line += "先へ進むには次が必要です: " + strings.Join(p.Missing, " / ")
	}
	return line
}

// planDeliveryDepth decides the depth for one delivery.
//
// The destination's configuration is read leniently — the few fields this
// needs, not the whole typed structure. An unreadable depth is an error,
// not permission to replace the goal with a proposal-only delivery.
func planDeliveryDepth(config runtime.Config, run state.RunOverview, runDir string) (depthPlan, error) {
	repository, err := readField(runDir, "ticket-draft.json", "repository")
	if err != nil {
		return depthPlan{}, errors.New("run repository unreadable")
	}
	configured, err := consumerDelivery(config.ConsumerConfigPath, repository)
	if err != nil {
		return depthPlan{}, err
	}
	plan := depthPlan{
		SchemaVersion: depthSchemaVersion, Repository: repository,
		Configured: configured, Reached: configured, DecidedAt: time.Now().UTC(),
	}
	if configured == string(worker.DeliverPullRequest) {
		return plan, nil
	}
	// Everything past the pull request is carried by the delivery cards, so
	// a delivery whose instance has no cards configured to carry it reaches
	// the pull request and says which settings would have taken it further.
	// This is the whole of "the release path is not configured" as far as
	// the engine can see it: the destination's own release settings —
	// branches, origins, workflows — are already required of every web
	// destination before its configuration will load at all.
	if !config.Chain.Deliver.Enabled() {
		plan.Reached = string(worker.DeliverPullRequest)
		plan.Missing = append(plan.Missing,
			"chain.deliver.checks_profile", "chain.deliver.integrate_profile", "chain.deliver.promote_profile")
		return plan, nil
	}
	if !deliverConfigured(config.Chain, run) {
		// The cards are configured but this delivery is not one they may
		// touch: it was claimed before the instant the operator turned them
		// on, and reaching back through deliveries older than that is the
		// one thing the cut-off exists to prevent.
		plan.Reached = string(worker.DeliverPullRequest)
		plan.Missing = append(plan.Missing, "chain.deliver.enabled_after")
	}
	return plan, nil
}

// currentDeliveryPlan preserves the requested repository and depth while
// rechecking whether the configured route can now carry the delivery. It
// never changes operator settings or revives a run before enabled_after.
func currentDeliveryPlan(config runtime.Config, run state.RunOverview, runDir string) (depthPlan, error) {
	previous, sealed := readDepthRecord(runDir)
	if !sealed {
		if _, err := os.Lstat(filepath.Join(runDir, deliveryDepthFile)); !errors.Is(err, os.ErrNotExist) {
			return depthPlan{}, errors.New("the saved delivery goal is unreadable; keeping the delivery and its evidence")
		}
	}
	current, err := planDeliveryDepth(config, run, runDir)
	if err != nil {
		return depthPlan{}, fmt.Errorf("delivery goal recovery is pending: %w", err)
	}
	if sealed {
		if current.Repository != previous.Repository || current.Configured != previous.Configured {
			return depthPlan{}, errors.New("the current configuration does not match this delivery's recorded goal")
		}
		if current.Reached == previous.Reached && slices.Equal(current.Missing, previous.Missing) {
			return previous, nil
		}
		current.DecidedAt = previous.DecidedAt
	}
	if err := writeDepthRecord(runDir, current); err != nil {
		return depthPlan{}, fmt.Errorf("the delivery goal could not be recorded; keeping the delivery open: %w", err)
	}
	return current, nil
}

// consumerDelivery reads one destination's depth out of the destination
// configuration, applying the same default the typed loader applies: a
// command-line destination has no environment to reach and proposes, and
// everything else goes the whole way unless it says otherwise.
func consumerDelivery(consumerConfigPath, repository string) (string, error) {
	raw, err := os.ReadFile(consumerConfigPath)
	if err != nil {
		return "", errors.New("consumer config unreadable")
	}
	var parsed struct {
		Consumers []struct {
			Repository string `json:"repository"`
			Kind       string `json:"kind"`
			Delivery   string `json:"delivery"`
		} `json:"consumers"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", errors.New("consumer config unreadable")
	}
	for _, consumer := range parsed.Consumers {
		if consumer.Repository != repository {
			continue
		}
		switch consumer.Delivery {
		case string(worker.DeliverPullRequest), string(worker.DeliverIntegration), string(worker.DeliverProduction):
			return consumer.Delivery, nil
		case "":
			if consumer.Kind == "cli" {
				return string(worker.DeliverPullRequest), nil
			}
			return string(worker.DeliverProduction), nil
		default:
			return "", errors.New("consumer delivery is invalid")
		}
	}
	return "", errors.New("repository is not a configured consumer")
}

// sealDepthRecord is the best-effort snapshot used by cancellation: a stop
// must not wait for storage to recover. New delivery work instead requires
// currentDeliveryPlan to persist its goal before issuing cards.
func sealDepthRecord(runDir string, plan depthPlan, logger Logger) depthPlan {
	if existing, ok := readDepthRecord(runDir); ok {
		return existing
	}
	if err := writeDepthRecord(runDir, plan); err != nil {
		logger.Error("the delivery depth could not be recorded; the delivery continues",
			"repository", plan.Repository, "error", err.Error())
	}
	return plan
}

func writeDepthRecord(runDir string, plan depthPlan) error {
	encoded, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	if len(encoded) > maxDepthRecordBytes {
		return errors.New("delivery goal record exceeds its size limit")
	}
	file, err := os.CreateTemp(runDir, ".delivery-depth-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if _, err = file.Write(encoded); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(temporary, filepath.Join(runDir, deliveryDepthFile))
}

// maxDepthRecordBytes bounds the read. The record is a handful of short
// fields; anything larger is not one of ours.
const maxDepthRecordBytes = 16 * 1024

// readDepthRecord reads the sealed decision back. A record that does not
// name a depth is no record: a half-written file must not be read as "this
// delivery proposes and nothing else".
func readDepthRecord(runDir string) (depthPlan, bool) {
	encoded, err := worker.ReadBoundedRegularFile(filepath.Join(runDir, deliveryDepthFile), maxDepthRecordBytes)
	if err != nil {
		return depthPlan{}, false
	}
	var plan depthPlan
	if json.Unmarshal(encoded, &plan) != nil || plan.SchemaVersion != depthSchemaVersion {
		return depthPlan{}, false
	}
	switch plan.Configured {
	case string(worker.DeliverPullRequest):
		if plan.Reached != string(worker.DeliverPullRequest) {
			return depthPlan{}, false
		}
	case string(worker.DeliverIntegration):
		if plan.Reached == string(worker.DeliverProduction) {
			return depthPlan{}, false
		}
	case string(worker.DeliverProduction):
	default:
		return depthPlan{}, false
	}
	switch plan.Reached {
	case string(worker.DeliverPullRequest), string(worker.DeliverIntegration), string(worker.DeliverProduction):
		return plan, true
	default:
		return depthPlan{}, false
	}
}
