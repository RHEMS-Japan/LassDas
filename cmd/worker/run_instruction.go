package main

import (
	"automation.internal/ticket-ingress/internal/worker/investigate"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/worker"
)

// run-instruction runs the implementer or the applier on the instruction the
// attendant rendered (INSTRUCTION.md) in the cards orchestration. The card
// is a direct command (`runner chain-stage --stage implement|apply`), so the
// agent starts here — through the launcher, under the agent user — like
// every reviewing agent, instead of as the kanban's native worker under the
// engine's user (issue #23). The seal of what it left stays with the first
// review card, as before.
func runRunInstruction(ctx context.Context, args []string) error {
	flags := commandFlags("run-instruction")
	configPath := flags.String("config", "", "")
	toolSHA := flags.String("tool-sha", "", "")
	draftPath := flags.String("draft", "", "")
	role := flags.String("role", "", "")
	instructionPath := flags.String("instruction", "", "")
	repoRoot := flags.String("repo-root", "", "")
	baseSHA := flags.String("base-sha", "", "")
	stage := flags.Int("stage", 0, "")
	knowledgeRoot := flags.String("knowledge-root", "", "")
	runOutPath := flags.String("out", "", "")
	designPath := flags.String("design", "", "")
	objectionOutPath := flags.String("objection-out", "", "")
	if !parseFlags(flags, args) || (*designPath == "") != (*objectionOutPath == "") ||
		!allPresent(*configPath, *toolSHA, *draftPath, *role, *instructionPath, *repoRoot, *baseSHA, *runOutPath) ||
		!worker.ValidToolSHA(*toolSHA) || *stage < 1 {
		return errors.New("run-instruction arguments are invalid")
	}
	config, err := readConfig(*configPath)
	if err != nil {
		return err
	}
	var draft worker.TicketDraft
	if err := worker.ReadJSONFile(*draftPath, worker.MaxTicketJSONBytes, &draft); err != nil {
		return errors.New("ticket draft could not be read")
	}
	configSHA, err := config.SHA256()
	if err != nil || draft.ConfigSHA256 != configSHA || draft.ToolSHA != *toolSHA {
		return errors.New("ticket draft is not bound to this run")
	}
	consumer, err := config.ConsumerFor(draft.Repository)
	if err != nil {
		return errors.New("ticket draft repository is not a configured consumer")
	}
	var agent worker.AgentConfig
	switch *role {
	case "implementer":
		agent = config.Agents.Implementer
	case "applier":
		if config.Agents.Applier != nil {
			agent = *config.Agents.Applier
		}
	default:
		return errors.New("run-instruction role is invalid")
	}
	if agent.Command == "" {
		return fmt.Errorf("the %s agent is not configured (agents.%s)", *role, *role)
	}
	instruction, err := os.ReadFile(*instructionPath)
	if err != nil || len(instruction) == 0 || len(instruction) > worker.MaxAgentPromptBytes {
		return errors.New("the instruction could not be read")
	}
	if err := placeAgentKnowledge(agent, *knowledgeRoot, *repoRoot); err != nil {
		return err
	}
	// A design-backed apply card: the applier may stop instead of editing by
	// leaving revise-design.json at the root of its working copy — the only
	// place it can write — and this command turns that into the design
	// round's objection record (docs/INVESTIGATING_DESIGNER.md §7, issue
	// #103). The design is read the way the seal reads it, so the record is
	// bound to the design the objection is against.
	var design *investigate.Design
	haltFile := ""
	if *designPath != "" {
		if *role != "applier" {
			return errors.New("only the applier's card carries a design")
		}
		loaded, err := investigate.ReadDesign(*designPath)
		if err != nil || !loaded.DigestMatches() {
			return errors.New("the approved design could not be read or is not intact")
		}
		if loaded.DeliveryID != draft.DeliveryID || loaded.InputSHA256 != draft.InputSHA256 || loaded.ConfigSHA256 != draft.ConfigSHA256 ||
			loaded.ToolSHA != draft.ToolSHA || loaded.BaseSHA != *baseSHA {
			return errors.New("the design belongs to another run")
		}
		design = &loaded
		haltFile = objectionFileName
	}
	if haltFile != "" {
		// The card is re-dispatched once, in the same working copy, and a
		// run that was killed outright (the pod's own death, a stop past
		// the grace period) runs none of the cleanup below. Whatever is
		// here now is a leftover: a run that finished honestly had its
		// objection moved into the design round. The repository clears the
		// previous attempt's leavings at the start elsewhere too (the run
		// record and the seal's partial outputs).
		if err := os.RemoveAll(filepath.Join(*repoRoot, haltFile)); err != nil {
			return errors.New("a leftover objection could not be cleared before the applier ran")
		}
	}
	prompt := string(instruction)
	outcome, halted, runErr := worker.RunAgentUnlessHalted(ctx, agent, *repoRoot, prompt, consumer.Mode.AllowedFilePrefixes, consumer.Mode.IgnoredByproducts, haltFile)
	emptyAttempts := 0
	if runErr == nil && !halted && len(outcome.ChangedFiles) == 0 {
		// The agent finished, wrote nothing, and said it was done: on the
		// tenth live run the applier described the file it had created in
		// detail, and the working copy was untouched (2026-09-09). The
		// working tree is what counts, so it is asked once more with that
		// fact in front of it — but only when there is room for the note
		// and time for another launch, and the first attempt's record is
		// written first, so a wall that fires during the second attempt
		// leaves the first one's evidence behind.
		retry := prompt + emptyResultRetryNote(haltFile != "")
		if len(retry) <= worker.MaxAgentPromptBytes && firstAttemptWasQuick(outcome, agent) {
			// The attempt that reported work it had not done is kept beside
			// the final record, not in its place: a wall or a failure
			// during the second launch leaves this behind, and what the
			// model actually claimed stays readable (the count alone says
			// it happened, not what was said).
			first, sealErr := worker.SealAgentRun(agentRunOf(outcome, draft, *baseSHA, *stage, len(prompt), 0))
			if sealErr == nil {
				_ = worker.WriteJSONFileExclusive(emptyAttemptRecordPath(*runOutPath), first, worker.MaxArtifactJSONBytes)
			}
			emptyAttempts = 1
			second, secondHalted, secondErr := worker.RunAgentUnlessHalted(ctx, agent, *repoRoot, retry,
				consumer.Mode.AllowedFilePrefixes, consumer.Mode.IgnoredByproducts, haltFile)
			if secondErr == nil || second.Command != "" {
				// The second launch ran: its record replaces the first,
				// which stays on disk until the write below succeeds.
				outcome, halted, runErr = second, secondHalted, secondErr
				prompt = retry
			}
		}
	}
	run, sealErr := worker.SealAgentRun(agentRunOf(outcome, draft, *baseSHA, *stage, len(prompt), emptyAttempts))
	if sealErr == nil {
		// The run record is evidence of what happened, written even when
		// the run failed.
		_ = worker.WriteJSONFileExclusive(*runOutPath, run, worker.MaxArtifactJSONBytes)
	}
	if runErr != nil {
		if haltFile != "" {
			// The run did not finish, so nothing it left is a finished
			// statement — and the card is re-dispatched once. A halt file
			// left here would be read by that attempt: as an objection
			// beside the edits it made (losing a round that applied the
			// design), or, if it changed nothing, as this round's objection
			// carrying the previous attempt's reason (#103 review).
			_ = os.RemoveAll(filepath.Join(*repoRoot, haltFile))
		}
		return errors.New("the " + *role + " did not finish: " + runErr.Error())
	}
	if halted {
		return sealAppliersHalt(*repoRoot, *objectionOutPath, consumer, draft, *baseSHA, *stage, design)
	}
	return nil
}

// agentRunOf is the run record for one launch. Keeping it in one place is
// what lets the first attempt be sealed before the second one starts.
func agentRunOf(outcome worker.AgentOutcome, draft worker.TicketDraft, baseSHA string, stage, promptBytes, emptyAttempts int) worker.AgentRun {
	return worker.AgentRun{
		SchemaVersion: worker.ArtifactSchemaVersion, Stage: stage,
		DeliveryID: draft.DeliveryID, InputSHA256: draft.InputSHA256,
		ConfigSHA256: draft.ConfigSHA256, ToolSHA: draft.ToolSHA, BaseSHA: baseSHA,
		AgentID: outcome.AgentID, Command: outcome.Command, PromptBytes: promptBytes, ExitCode: outcome.ExitCode,
		DurationMs: outcome.Duration.Milliseconds(), ChangedFiles: outcome.ChangedFiles,
		Transcript: outcome.Transcript, EmptyAttempts: emptyAttempts, RanAt: time.Now().UTC(),
	}
}

// emptyAttemptRecordPath is where the attempt that changed nothing is kept:
// beside the run record, under a name nothing else writes.
func emptyAttemptRecordPath(runOut string) string {
	extension := filepath.Ext(runOut)
	return strings.TrimSuffix(runOut, extension) + "-empty-attempt" + extension
}

// retryTimeShare is the part of an agent's own timeout a first attempt may
// spend and still leave room for a second one inside the card's wall. A
// variable so a test can move the cutoff without sleeping through a third
// of the smallest timeout the configuration allows. The guard keeps two
// launches inside the card's wall only where the wall is at least four
// thirds of the agent's timeout, which both shipped configurations satisfy
// (apply: 1200 against 900; implement: 5400 against 3600).
var retryTimeShare = 3

// firstAttemptWasQuick reports whether the launch that changed nothing
// finished fast enough that another one fits. The card's wall is enforced
// by the board, which kills this process and passes it no deadline, so the
// only measured budget here is the agent's own timeout: an attempt that
// reported work without doing any is fast by nature (twenty-seven seconds
// against a nine-hundred-second timeout, live 2026-09-09), while one that
// spent most of its time and produced nothing would push a second launch
// into the wall.
func firstAttemptWasQuick(outcome worker.AgentOutcome, agent worker.AgentConfig) bool {
	if agent.TimeoutSeconds <= 0 || retryTimeShare <= 0 {
		return false
	}
	return outcome.Duration < time.Duration(agent.TimeoutSeconds)*time.Second/time.Duration(retryTimeShare)
}

// emptyResultRetryNote is appended when an agent reported success without
// touching the working copy. It states the measurement, not a scolding: the
// engine read the tree and found nothing, so whatever the previous message
// said, the work is still to do. The design and the objection are named
// only for the role that has them.
func emptyResultRetryNote(withDesign bool) string {
	note := `

---

## The working copy is unchanged

Your previous answer reported the work as done. The engine then read the
working copy and found no change at all — no new file, no edited file.
Nothing you described exists.

Only the working copy counts. A message describing edits is not an edit;
the seal reads the tree. Make the changes now with your tools, one file at
a time.

Write with the absolute paths under "Where the working copy is" above. A
relative path lands in your own home, not in the working copy, and that
is the most common reason a change reported as done is not there.
`
	if withDesign {
		note += `
Start with the first file the design lists. If you cannot make the
changes, write the objection file the rules above describe instead of
reporting success.
`
	}
	return note
}

// sealAppliersHalt reads the objection the applier left at the root of its
// working copy and seals it as the design round's record, then fails the
// card so the attendant reads the record and reopens the design. The file
// is gone from the tree either way: a refused objection travels in the
// error, and a stale halt would otherwise stop the next round before it
// began. An objection next to other edits is refused — the contract is to
// stop without editing anything else, and a tree that was edited anyway is
// not the design's author's to answer.
func sealAppliersHalt(repoRoot, objectionOut string, consumer worker.ConsumerConfig, draft worker.TicketDraft, baseSHA string, stage int, design *investigate.Design) error {
	halt := filepath.Join(repoRoot, objectionFileName)
	changed, scanErr := worker.ChangedFilesUnderExcept(repoRoot, consumer.Mode.AllowedFilePrefixes, consumer.Mode.IgnoredByproducts, objectionFileName)
	if scanErr != nil || len(changed) > 0 {
		_ = os.Remove(halt)
		if scanErr != nil {
			return errors.New("the applier objected to the design but also changed the tree; an objection leaves it untouched (" + scanErr.Error() + ")")
		}
		return fmt.Errorf("the applier objected to the design but also changed %d file(s); an objection leaves the tree untouched", len(changed))
	}
	objected, err := sealDesignObjection(halt, objectionOut, draft, baseSHA, stage, design)
	if err != nil {
		_ = os.Remove(halt)
		return errors.New("the applier's objection was refused: " + err.Error())
	}
	if !objected {
		return errors.New("the applier's objection vanished before it was sealed")
	}
	return errors.New("the applier objected to the design; no candidate was sealed and the design goes back to its author")
}
