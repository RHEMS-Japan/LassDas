package worker

import (
	"errors"
	"fmt"
	"strings"
)

// A round that produced no candidate has no sealed chain to render, so the
// ordinary trail cannot be composed from it: LoadTrailStages needs a
// decision, the decision needs reviews, and the reviews need the candidate
// that never existed. On a live run (2026-09-25) that left the requester
// with the fallback line — "the record could not be generated automatically"
// — while the one thing worth reading, the implementing agent's own report
// of why it changed nothing, sat unread in the run directory.
//
// The materials of such a round are the run record and, in the cards
// orchestration, the card that blocked afterwards. This renders those, and
// nothing it cannot read from them. It is deliberately separate from the
// sealed-history composer: the two answer different questions and neither
// should acquire the other's branches.

// UnsealedRound is a round that ran an implementing agent and sealed no
// candidate from it.
type UnsealedRound struct {
	// Round is the implementation round the record belongs to.
	Round int
	// AgentID names the role whose report this is, as the configuration
	// calls it.
	AgentID string
	// Report is the agent's own words about what it did or would not do,
	// whole: only the trail's own bound shortens it.
	Report string
	// ReturnedWork is true when the agent finished and left the working
	// copy untouched, which is what separates work handed back with a
	// reason from a round that died mid-edit.
	ReturnedWork bool
	// EmptyAttempts counts the launches before this one that reported work
	// and left the tree alone.
	EmptyAttempts int
}

// LoadUnsealedRound reads the newest implementation round that ran an agent
// and sealed no candidate. A round with a candidate is not one of these —
// its trail is the ordinary one — and neither is a directory with no run
// record at all.
func LoadUnsealedRound(historyDir string, config Config) (UnsealedRound, error) {
	for number := config.StageCeiling(); number >= 1; number-- {
		if candidateSealedFor(historyDir, number) {
			// This round got as far as a candidate, so the sealed history
			// is what renders it, whatever happened afterwards.
			return UnsealedRound{}, errors.New("the round sealed a candidate")
		}
		run, err := ReadImplementingRun(historyDir, number)
		if err != nil {
			continue
		}
		report := ReportText(run.Transcript)
		if report == "" {
			continue
		}
		return UnsealedRound{
			Round: number, AgentID: run.AgentID, Report: report,
			ReturnedWork: IsSendBack(run), EmptyAttempts: run.EmptyAttempts,
		}, nil
	}
	return UnsealedRound{}, errors.New("no implementing round left a report")
}

// ComposeUnsealedTrail renders the round for its requester: what the
// automation did, what the implementing agent said, and — when the caller
// knows it — which step then blocked. The agent's report goes in whole; the
// same cap as the sealed trail bounds the result, because the same comment
// and pull request body carry it.
func ComposeUnsealedTrail(round UnsealedRound, blocked string) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "### 実装の経過 (%d 周目で停止)\n", round.Round)
	if round.ReturnedWork {
		builder.WriteString("- 実装役は、変更を加えずに理由を報告して作業を返しました。対象リポジトリと本番環境は変更していません。\n")
	} else {
		builder.WriteString("- 変更を確定できなかったため、この周の記録は残っていません。対象リポジトリと本番環境は変更していません。\n")
	}
	if round.EmptyAttempts > 0 {
		fmt.Fprintf(&builder, "- 変更がないまま終わった試行が、この前に %d 回ありました。\n", round.EmptyAttempts)
	}
	if step := strings.TrimSpace(blocked); step != "" {
		builder.WriteString("- 止まった段階: " + trailClip(step, 120) + "\n")
	}
	builder.WriteString("\n### 実装役の報告\n" + round.Report + "\n")
	return trailTruncate(builder.String())
}
