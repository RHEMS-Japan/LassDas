package main

import "automation.internal/ticket-ingress/internal/worker"

// The two things a stage card tells a verb about the ladder's earlier
// attempts at the same round: which occupant of the role's seat is running,
// and whether the instruction it gets has been rebuilt.
//
// Both arrive as flags rather than being read out of the run directory. A
// verb is given what it works on; the card is the one process that knows
// which round and which seat this is, and a second reader of those records
// would be a second place for them to be interpreted.

// validRebuild accepts the rebuild shapes this engine knows, and nothing
// else. An unknown name is refused rather than ignored: a card asking for a
// rebuild that silently did not happen would spend the ladder's one
// different hand on the same instruction as before.
func validRebuild(name string) bool {
	switch name {
	case "", worker.PromptRebuildShorten:
		return true
	default:
		return false
	}
}
