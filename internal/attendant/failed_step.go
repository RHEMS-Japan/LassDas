package attendant

import (
	"os"
	"path/filepath"
	"strconv"

	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
)

// requesterStepNames is what each step of the work is called when a
// requester is told it could not be completed. The engine's own stage names
// ("design-review-a") say nothing to the person who filed the ticket, and a
// report that names no step reads the same whether the run stopped before
// anything was written or after the change was made and reviewed.
//
// Each name says whether a model ran, because the sentence is otherwise
// read as a claim about one. Three of these steps ask no model anything:
// design-decide and validate are tallies of records already written, and
// the step that fixes a round's changes into a candidate is a plain read of
// the working copy. Naming them as the AI's work would be a specific false
// claim where the older sentence was only a vague one.
//
// Both reviewers of a pair share one name: which of the two refused is the
// operator's question, and the requester's answer does not change with it.
var requesterStepNames = map[string]string{
	// The investigate card seals the investigation AND the design when the
	// delivery is design-backed, so a failure while writing the design is
	// this step too. Naming only the measuring would tell a requester the
	// measuring failed when it succeeded.
	runtime.StageInvestigate:   "AI による調査と設計",
	runtime.StageDesignReviewA: "AI による設計のレビュー",
	runtime.StageDesignReviewB: "AI による設計のレビュー",
	runtime.StageDesignDecide:  "設計レビューの集計",
	runtime.StageApply:         "AI による設計にもとづく変更の作成",
	runtime.StageImplement:     "AI による変更の作成",
	runtime.StageReviewA:       "AI による変更のレビュー",
	runtime.StageReviewB:       "AI による変更のレビュー",
	runtime.StageValidate:      "変更の検証",
	runtime.StagePublish:       "Pull Request の公開",
}

// candidateSealStep names the step that runs before a round's first review:
// the round's changes are read out of the working copy and fixed as the
// candidate. It fails when the agent changed nothing, which has happened on
// live runs — and reporting it as the review would tell the requester the
// change exists and the judging failed, when nothing was written at all.
const candidateSealStep = "変更の確定"

// failedStepFor names the step a failed card was on, for a requester. It
// takes the round's own artifacts into account where one card runs two
// steps: the first review card seals the candidate before it reviews, and
// the two fail differently.
func failedStepFor(runDir, stageName string, round int) string {
	if stageName == runtime.StageReviewA && round > 0 && !candidateSealed(runDir, round) {
		return candidateSealStep
	}
	name, ok := requesterStepNames[stageName]
	if !ok {
		return ""
	}
	return name
}

// candidateSealed reports whether the round's candidate was written. Its
// absence after the round's first review card failed means the seal is what
// failed, because the review cannot run without it.
//
// Two other things produce that absence: an unreadable run directory, and a
// card that died before the seal started at all. Both then name a step
// earlier than the truth, which is the safe direction — it never claims the
// change exists when it does not.
func candidateSealed(runDir string, round int) bool {
	info, err := os.Stat(filepath.Join(runDir, "history", "stage-"+strconv.Itoa(round), "candidate.json"))
	return err == nil && info.Mode().IsRegular()
}

// failedStepEvidence carries the step's requester-facing name to the report.
// A name the report would refuse carries nothing instead: an over-long or
// unnamed step would fail the report's shape check, and a run whose report
// is refused never ends at all — it retries for ever with no comment. A
// missing name costs the older, vaguer sentence; a refused report costs the
// requester everything.
func failedStepEvidence(runDir, stageName string, round int) map[string]string {
	name := failedStepFor(runDir, stageName, round)
	if !runner.UsableStepName(name) {
		return nil
	}
	return map[string]string{"failed_step": name}
}
