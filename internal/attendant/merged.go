package attendant

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
)

// featureMergeFile records how the delivered pull request ended: merged, or
// closed without merging. Once written it is never read again from GitHub,
// and a delivery that rests on it should not depend on the network to keep
// resting there. A reopening is not watched for — the wait this reading
// stands in for already ends at a closed pull request, and the two readers
// must not disagree about what "closed" means.
const featureMergeFile = "feature-merged.json"

// mergeReadTimeout bounds one reading. The attendant wakes every minute; a
// reading that cannot be had in this long is had on the next wake-up.
const mergeReadTimeout = 30 * time.Second

// mergeUnreadableFile records that this run's own records could not be read,
// so the reason is said once and not once a minute for as long as the run is
// kept.
const mergeUnreadableFile = "feature-merged.unreadable.json"

// mergeReadStderrBytes bounds what is kept of the reader's stderr. Only the
// last line matters — the reader prints its detail first and its fixed code
// last — and the detail may name an endpoint, never a credential.
const mergeReadStderrBytes = 4096

// mergeDeliveryRecordCode is the reason a run that DID publish a pull
// request cannot be asked about at all: the record naming that pull request,
// or the round ticket the reading is held to, is missing or unreadable. It
// never reaches the reader, so it carries no code of its own.
const mergeDeliveryRecordCode = "delivery_record_invalid"

// mergeRecordFailures are the refusals that mean this run's own sealed
// records could not be read at all: nothing about them will be different on
// the next wake-up, and an operator has to look. Every other refusal — the
// destination unreachable, the token rejected, the reading itself failing —
// is transient and stays quiet, because a pull request that is simply not
// merged yet looks the same from here.
var mergeRecordFailures = map[string]bool{
	mergeDeliveryRecordCode:   true,
	"ticket_artifact_invalid": true,
	"config_invalid":          true,
	"config_sha256_invalid":   true,
	"arguments_invalid":       true,
	"output_path_invalid":     true,
	"command_invalid":         true,
}

type featureMerge struct {
	Merged         bool      `json:"merged"`
	State          string    `json:"state"`
	MergeCommitSHA string    `json:"merge_commit_sha"`
	ReadAt         time.Time `json:"read_at"`
}

// readFeatureMerge reports how this delivery's pull request ended, when that
// is settled. "Still open" is no ending: it is never written down, and the
// next wake-up asks again.
func readFeatureMerge(runDir string) (featureMerge, bool) {
	raw, err := os.ReadFile(filepath.Join(runDir, featureMergeFile))
	if err != nil || len(raw) > 1<<16 {
		return featureMerge{}, false
	}
	var merge featureMerge
	if json.Unmarshal(raw, &merge) != nil {
		return featureMerge{}, false
	}
	if !merge.Merged && !closedUnmerged(merge) {
		return featureMerge{}, false
	}
	return merge, true
}

// closedUnmerged says the delivered pull request ended the other way: a
// person closed it, and the change never entered the repository.
func closedUnmerged(merge featureMerge) bool {
	return !merge.Merged && merge.State == "closed"
}

// recordFeatureMerge asks whether the delivered pull request has been merged
// and writes it down when it has.
//
// A delivery that stops at the pull request used to rest at マージ待ち for
// ever: the condition was a successful run, a pull request on disk, and no
// post-merge pipeline, none of which change when someone merges. So the
// board told a requester to merge something they had merged an hour ago, and
// the card never left the running list (live 2026-09-18, five of them at
// once).
//
// The attendant has no GitHub client of its own; the controller binary the
// runner already uses has the verb, so it is asked the same way the runner
// asks it.
func recordFeatureMerge(ctx context.Context, config runtime.Config, run state.RunOverview, runDir string, logger Logger) {
	if _, known := readFeatureMerge(runDir); known {
		return
	}
	// A run that published no pull request has no merge to observe, and
	// most finished runs are that. A run that DID publish one, whose record
	// of it cannot be read, would otherwise rest at waiting-for-merge with
	// nothing said — the very shape this reader exists to close, so it is
	// said once rather than passed over.
	number, recordedConfigSHA, err := featureDelivery(runDir)
	if err != nil {
		if !errors.Is(err, errNoDelivery) {
			noteUnreadableRun(runDir, run, mergeDeliveryRecordCode, logger)
		}
		return
	}
	ticket, err := newestStageTicket(runDir)
	if err != nil {
		// The round history outlives the run — pruning takes the copies of
		// the destination and leaves the sealed records — so a published
		// delivery without one is the same unreadable run.
		noteUnreadableRun(runDir, run, mergeDeliveryRecordCode, logger)
		return
	}
	// Match entrypoint.sh: the destination credential is sealed into an
	// operator-only file before any stage is dispatched, never left in the
	// environment an agent could inherit.
	token, err := readTargetToken(config)
	if err != nil {
		return
	}
	out := filepath.Join(runDir, featureMergeFile+".reading")
	_ = os.Remove(out)
	runCtx, cancel := context.WithTimeout(ctx, mergeReadTimeout)
	defer cancel()
	// The run is over, so its records are held to the digest IT recorded and
	// not to the destination's configuration as it stands now. Without this
	// the reader refuses the ticket of every run that finished before the
	// last configuration change, and the refusal is indistinguishable here
	// from "not merged yet" — which is how merges went unseen for as long as
	// the run was kept (live 2026-09-24).
	command := exec.CommandContext(runCtx, config.ControllerBin, "read-merged",
		"--config", config.ConsumerConfigPath, "--ticket", ticket,
		"--config-sha256", recordedConfigSHA,
		"--number", strconv.FormatInt(number, 10), "--out", out)
	command.Env = append(os.Environ(), "TARGET_GITHUB_TOKEN="+token)
	stderr := &tailWriter{limit: mergeReadStderrBytes}
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		// A pull request that is simply not merged yet reads successfully and
		// says so, so a refusal here is never that. Most refusals are still
		// worth no words every minute — the destination may be unreachable —
		// but one whose own records cannot be read will not heal on its own.
		noteUnreadableRun(runDir, run, endingCode(stderr.String()), logger)
		return
	}
	raw, err := os.ReadFile(out)
	_ = os.Remove(out)
	if err != nil {
		return
	}
	var reading struct {
		Merged         bool   `json:"merged"`
		State          string `json:"state"`
		MergeCommitSHA string `json:"merge_commit_sha"`
	}
	if json.Unmarshal(raw, &reading) != nil {
		return
	}
	// A pull request closed without merging is an ending of its own, and the
	// board could not see it: the conditions for 「マージ待ち」 are a finished
	// run and a published pull request, neither of which changes when a
	// person closes one, so the card went on asking for a merge that was
	// never going to come. Both endings are written down; only "still open"
	// is no ending, and is asked about again next wake-up.
	if !reading.Merged && reading.State != "closed" {
		return
	}
	record := featureMerge{
		Merged: reading.Merged, State: reading.State,
		MergeCommitSHA: reading.MergeCommitSHA, ReadAt: time.Now().UTC(),
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return
	}
	if err := os.WriteFile(filepath.Join(runDir, featureMergeFile), encoded, 0o600); err != nil {
		logger.Error("the end of the delivered pull request could not be written down", "run", run.RunID, "error", err.Error())
		return
	}
	if !record.Merged {
		logger.Info("the delivered pull request was closed without merging", "run", run.RunID)
		return
	}
	logger.Info("the delivered pull request was merged", "run", run.RunID, "merge_commit", reading.MergeCommitSHA)
}

// featureDelivery reads what the delivered pull request artifact says about
// itself: the number to ask about, and the configuration digest the run was
// sealed under. Both come from the same artifact on purpose — the reader
// then has to be handed a ticket that belongs to the very delivery whose
// pull request is being asked about.
func featureDelivery(runDir string) (int64, string, error) {
	raw, err := os.ReadFile(filepath.Join(runDir, "feature-pr.json"))
	if err != nil {
		return 0, "", errNoDelivery
	}
	if len(raw) > 1<<20 {
		return 0, "", errors.New("the delivered pull request record is too large")
	}
	var record struct {
		Binding struct {
			ConfigSHA256 string `json:"config_sha256"`
		} `json:"binding"`
		Payload struct {
			PullRequest struct {
				Number int64 `json:"Number"`
			} `json:"pull_request"`
		} `json:"payload"`
	}
	if json.Unmarshal(raw, &record) != nil || record.Payload.PullRequest.Number <= 0 {
		return 0, "", errors.New("the delivered pull request has no number")
	}
	if !recordedDigestPattern.MatchString(record.Binding.ConfigSHA256) {
		return 0, "", errors.New("the delivered pull request records no configuration digest")
	}
	return record.Payload.PullRequest.Number, record.Binding.ConfigSHA256, nil
}

// errNoDelivery says this run published no pull request, which is an
// ordinary thing for a finished run to be. Every other refusal from
// featureDelivery is a record that IS there and cannot be read.
var errNoDelivery = errors.New("no delivered pull request")

var recordedDigestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// tailWriter keeps the last limit bytes written to it.
type tailWriter struct {
	limit int
	data  []byte
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.data = append(w.data, p...)
	if len(w.data) > w.limit {
		w.data = append([]byte(nil), w.data[len(w.data)-w.limit:]...)
	}
	return len(p), nil
}

func (w *tailWriter) String() string { return string(w.data) }

// endingCode is the fixed failure code the reader ended with. It prints its
// detail first and its code last, so the LAST bare "controller: <code>" line
// is the ending; a code named inside an earlier wrapped error is a detail.
func endingCode(stderr string) string {
	ending := ""
	for _, line := range strings.Split(stderr, "\n") {
		after, found := strings.CutPrefix(strings.TrimSpace(line), "controller: ")
		if found && !strings.Contains(after, ":") {
			ending = after
		}
	}
	return ending
}

// noteUnreadableRun says once, per run and per reason, that a finished run's
// own records could not be read. Saying it every minute would bury it; never
// saying it is how this went unnoticed for half a day.
func noteUnreadableRun(runDir string, run state.RunOverview, code string, logger Logger) {
	if !mergeRecordFailures[code] {
		return
	}
	marker := filepath.Join(runDir, mergeUnreadableFile)
	var noted struct {
		Code    string    `json:"code"`
		NotedAt time.Time `json:"noted_at"`
	}
	if raw, err := os.ReadFile(marker); err == nil && len(raw) <= 1<<16 &&
		json.Unmarshal(raw, &noted) == nil && noted.Code == code {
		return
	}
	noted.Code, noted.NotedAt = code, time.Now().UTC()
	encoded, err := json.Marshal(noted)
	if err != nil {
		return
	}
	// Best effort: an unwritable marker must not cost the operator the one
	// line that says what is wrong, even if it then repeats.
	_ = os.WriteFile(marker, encoded, 0o600)
	logger.Error("a finished run's records could not be read, so its merge cannot be observed",
		"run", run.RunID, "reason", code)
}

// newestStageTicket names the ticket artifact of the highest implementation
// round, which is the one the delivery was published from.
func newestStageTicket(runDir string) (string, error) {
	entries, err := os.ReadDir(filepath.Join(runDir, "history"))
	if err != nil {
		return "", err
	}
	stages := []string{}
	for _, entry := range entries {
		if !entry.IsDir() || len(entry.Name()) < 7 || entry.Name()[:6] != "stage-" {
			continue
		}
		candidate := filepath.Join(runDir, "history", entry.Name(), "ticket.json")
		if _, err := os.Stat(candidate); err == nil {
			stages = append(stages, candidate)
		}
	}
	if len(stages) == 0 {
		return "", errors.New("no stage ticket")
	}
	sort.Strings(stages)
	return stages[len(stages)-1], nil
}
