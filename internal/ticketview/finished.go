package ticketview

// FinishedSteps are the board steps a run rests in for good. A card in one
// of them stays in the running lane, showing when it got there, until a
// person clears it away — so the engine, the board's server and both of its
// pages have to agree on exactly which steps those are. They were written
// out in four places; a page that believed in one more step than the server
// did would have shown a control the server refuses, which is the one thing
// the board must never do. This list is the definition, and a test pins the
// pages' own copies equal to it.
//
// The order is the one the pages carry, so the comparison is a plain one.
var FinishedSteps = []string{"done", "stopped", "failed"}

// IsFinished says this step is one a run rests in for good.
func IsFinished(step string) bool {
	for _, finished := range FinishedSteps {
		if step == finished {
			return true
		}
	}
	return false
}
