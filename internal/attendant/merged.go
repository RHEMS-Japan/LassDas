package attendant

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
)

// featureMergeFile records that the delivered pull request was merged. Once
// written it is never read again from GitHub: a merge does not come undone,
// and a delivery that rests on it should not depend on the network to keep
// resting there.
const featureMergeFile = "feature-merged.json"

// mergeReadTimeout bounds one reading. The attendant wakes every minute; a
// reading that cannot be had in this long is had on the next wake-up.
const mergeReadTimeout = 30 * time.Second

// SyncRunnerMerges observes the PRs left by finished runners. It neither
// reopens runs nor starts cards/delivery stages. The ordinary reception
// tick calls it; the fast, read-only display loop must not poll GitHub.
func SyncRunnerMerges(ctx context.Context, config runtime.Config, services *runtime.Services, hermes *runtime.Hermes, logger Logger) error {
	if config.OrchestrationCards() {
		return nil // SyncChains already owns merge observation in this mode.
	}
	runs, err := services.Store.ScanRuns(ctx)
	if err != nil {
		return err
	}
	var tasks []runtime.BoardTask
	loaded := false
	for _, run := range runs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if run.State != "terminal" || run.TerminalCode != string(hook.TerminalSuccess) {
			continue
		}
		if !loaded {
			tasks, err = hermes.ListBoardTasks(ctx)
			if err != nil {
				return err
			}
			loaded = true
		}
		dir, _ := runnerWorkspace(tasks, run.DeliveryID)
		if dir == "" {
			dir = runDirectory(config, run.DeliveryID)
		}
		recordFeatureMerge(ctx, config, run, dir, logger)
	}
	return nil
}

type featureMerge struct {
	Merged         bool      `json:"merged"`
	State          string    `json:"state"`
	MergeCommitSHA string    `json:"merge_commit_sha"`
	ReadAt         time.Time `json:"read_at"`
}

// readFeatureMerge reports whether this delivery's pull request is known to
// have been merged.
func readFeatureMerge(runDir string) (featureMerge, bool) {
	raw, err := os.ReadFile(filepath.Join(runDir, featureMergeFile))
	if err != nil || len(raw) > 1<<16 {
		return featureMerge{}, false
	}
	var merge featureMerge
	if json.Unmarshal(raw, &merge) != nil || !merge.Merged {
		return featureMerge{}, false
	}
	return merge, true
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
	number, err := featurePullNumber(runDir)
	if err != nil {
		return
	}
	ticket, err := newestStageTicket(runDir)
	if err != nil {
		return
	}
	token, err := readTargetToken(config)
	if err != nil {
		return
	}
	out := filepath.Join(runDir, featureMergeFile+".reading")
	_ = os.Remove(out)
	runCtx, cancel := context.WithTimeout(ctx, mergeReadTimeout)
	defer cancel()
	command := exec.CommandContext(runCtx, config.ControllerBin, "read-merged",
		"--config", config.ConsumerConfigPath, "--ticket", ticket,
		"--number", strconv.FormatInt(number, 10), "--out", out)
	command.Env = append(os.Environ(), "TARGET_GITHUB_TOKEN="+token)
	if err := command.Run(); err != nil {
		// Nothing to say every minute: the pull request is simply not known
		// to be merged yet, which is also what "not merged" looks like.
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
	if json.Unmarshal(raw, &reading) != nil || !reading.Merged {
		return
	}
	encoded, err := json.Marshal(featureMerge{
		Merged: true, State: reading.State, MergeCommitSHA: reading.MergeCommitSHA, ReadAt: time.Now().UTC(),
	})
	if err != nil {
		return
	}
	if err := os.WriteFile(filepath.Join(runDir, featureMergeFile), encoded, 0o600); err != nil {
		logger.Error("the merge could not be written down", "run", run.RunID, "error", err.Error())
		return
	}
	logger.Info("the delivered pull request was merged", "run", run.RunID, "merge_commit", reading.MergeCommitSHA)
}

// featurePullNumber reads the delivered pull request's number.
func featurePullNumber(runDir string) (int64, error) {
	raw, err := os.ReadFile(filepath.Join(runDir, "feature-pr.json"))
	if err != nil || len(raw) > 1<<20 {
		return 0, errors.New("no delivered pull request")
	}
	var record struct {
		Payload struct {
			PullRequest struct {
				Number int64 `json:"Number"`
			} `json:"pull_request"`
		} `json:"payload"`
	}
	if json.Unmarshal(raw, &record) != nil || record.Payload.PullRequest.Number <= 0 {
		return 0, errors.New("the delivered pull request has no number")
	}
	return record.Payload.PullRequest.Number, nil
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
