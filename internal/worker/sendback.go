package worker

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The implementing agent is told that when it cannot carry out the request,
// it must change nothing, say why, and leave the decision to the requester.
// It did exactly that on a live run (2026-09-25) and the engine threw the
// answer away: the empty working copy went to the first review card, the
// seal refused it as "the agent changed nothing", and the run ended as
// model_failed with a terminal comment that carried neither the refusal nor
// its reason. The requester was told the AI had failed; the AI had in fact
// answered.
//
// Returned work is not a failure of the automation, so it does not end as
// one. The round stops before the review, the run ends on its own terminal
// code, and the report the agent wrote is what the requester reads.

// implementingRunRecords are the run records an implementation round may
// leave, in the order they are looked for. The cards orchestration names
// them for the role that ran; the one-process mode writes the implement
// verb's own record.
var implementingRunRecords = []string{"implementer-run.json", "applier-run.json", "implement-run.json"}

// IsSendBack reports whether an implementing run returned the work instead
// of doing it: it finished, it changed nothing, and it said something. The
// working tree is what decides "changed nothing" — the same measurement the
// empty-result retry already makes — and the transcript is what separates a
// refusal with a reason from an agent that produced nothing at all, which is
// still a model failure.
func IsSendBack(run AgentRun) bool {
	return run.ExitCode == 0 && len(run.ChangedFiles) == 0 && strings.TrimSpace(run.Transcript) != ""
}

// ReadImplementingRun reads a round's implementing run record, whichever
// role wrote it.
//
// The record is deliberately not put through AgentRun.Validate: that refuses
// an implementing run which changed no files, which is the very record this
// function exists to read. Its fields were bounded by the seal that wrote
// them, and nothing here acts on the record beyond reading what the agent
// said and whether it changed anything.
func ReadImplementingRun(historyDir string, round int) (AgentRun, error) {
	stageDir := filepath.Join(historyDir, "stage-"+strconv.Itoa(round))
	for _, name := range implementingRunRecords {
		var run AgentRun
		if err := ReadJSONFile(filepath.Join(stageDir, name), MaxArtifactJSONBytes, &run); err != nil {
			continue
		}
		return run, nil
	}
	return AgentRun{}, errors.New("the round left no implementing run record")
}

// RoundReturnedWork reports whether a round's implementing agent returned
// the work to the requester. A round with no readable record did not — an
// unreadable record is the machinery's own failure and keeps the ending it
// always had — and neither does one that got as far as a candidate, whose
// agent plainly did change something.
func RoundReturnedWork(historyDir string, round int) bool {
	if candidateSealedFor(historyDir, round) {
		return false
	}
	run, err := ReadImplementingRun(historyDir, round)
	return err == nil && IsSendBack(run)
}

// candidateSealedFor reports whether a round fixed its changes into a
// candidate. Its absence is what makes a round's run record the only record
// of what happened.
func candidateSealedFor(historyDir string, round int) bool {
	info, err := os.Stat(filepath.Join(historyDir, "stage-"+strconv.Itoa(round), "candidate.json"))
	return err == nil && info.Mode().IsRegular()
}

// ReportText is an agent's own words, made safe to carry in a
// requester-facing record: the characters a trail may not hold are removed
// and the ends are trimmed. Nothing is shortened here — the trail's own
// bound is the only limit, so the whole report travels as far as that
// allows. It is introduced as the agent's report wherever it is rendered,
// so it is never read as the engine's own statement.
func ReportText(transcript string) string {
	var cleaned strings.Builder
	cleaned.Grow(len(transcript))
	for _, r := range transcript {
		if r == '\n' || r == '\t' {
			cleaned.WriteRune(r)
			continue
		}
		// A carriage return is dropped rather than turned into a newline:
		// the common case is a CRLF line ending, and translating it would
		// double every line break in the report.
		if r < 0x20 || r == 0x7f {
			continue
		}
		cleaned.WriteRune(r)
	}
	return strings.TrimSpace(cleaned.String())
}
