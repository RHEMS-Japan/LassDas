package worker

import (
	"regexp"
	"slices"
)

// What one run may write outside the path vocabulary, and nothing else.
//
// The dotted-path floor is not a rule about tidiness. A file under a dotted
// directory is the platform's own machinery: it runs on push, with the
// repository's secrets, before a person has read it. The floor is enforced
// in eleven places and it stays in all eleven — what this type adds is a
// hole exactly the shape of the file one sealed plan says it is going to
// build, open only while that plan is the one being carried.
//
// It is threaded rather than configured on purpose. A destination cannot
// declare the workflow directory writable — validFilePrefix still refuses
// it — and a handed policy on its own opens nothing: the policy says what
// MAY be built, and the plan says what IS being built, and a path needs
// both. That is what keeps the hole from outliving the round it was opened
// for.

// workflowFileShape is what a plan-named path must look like before it is
// admitted anywhere. The plan is a sealed record and the seal is the
// engine's own, but a record read back from a volume is still a record: a
// plan that had been edited to say ".github/workflows/../../.ssh/config"
// would otherwise be admitted by every gate that trusts the plan.
var workflowFileShape = regexp.MustCompile(`^\.github/workflows/[A-Za-z0-9][A-Za-z0-9._-]{0,63}\.(?:yml|yaml)$`)

// WorkflowAllowance is the set of workflow files one run may create. The
// zero value admits nothing, which is what every path in the engine that
// does not carry a plan uses.
type WorkflowAllowance struct {
	files []string
}

// NewWorkflowAllowance admits the given repository paths for one run.
// Anything that is not a plain workflow file is dropped here rather than
// refused, because the caller's own record has already been read and the
// answer this type owes is which paths are admitted, not whether a record
// was well formed.
func NewWorkflowAllowance(paths []string) WorkflowAllowance {
	admitted := make([]string, 0, len(paths))
	for _, candidate := range paths {
		if workflowFileShape.MatchString(candidate) && !slices.Contains(admitted, candidate) {
			admitted = append(admitted, candidate)
		}
	}
	if len(admitted) == 0 {
		return WorkflowAllowance{}
	}
	return WorkflowAllowance{files: admitted}
}

// Admits reports whether this exact path is one the run may write.
func (a WorkflowAllowance) Admits(candidate string) bool {
	return slices.Contains(a.files, candidate)
}

// Empty reports whether nothing is admitted, which is the ordinary state of
// every run.
func (a WorkflowAllowance) Empty() bool { return len(a.files) == 0 }

// Files are the admitted paths, for a caller that has to name them.
func (a WorkflowAllowance) Files() []string { return slices.Clone(a.files) }

// WorkflowAllowance is the ticket's own: the files the sealed plan named
// for this delivery, carried in the contract so every later reader of the
// contract admits exactly what the delivery was allowed to build.
func (r TicketRequest) WorkflowAllowance() WorkflowAllowance {
	return NewWorkflowAllowance(r.ReleaseWorkflows)
}

// validRelativePathWithin is the path pattern, widened by an allowance.
// Gate one of five: every other caller of validRelativePath is unchanged,
// so a dotted path is still unaddressable anywhere a plan is not being
// carried.
func validRelativePathWithin(value string, allowance WorkflowAllowance) bool {
	return allowance.Admits(value) || validRelativePath(value)
}

// allowedPathWithin measures a path against the destination's writable
// declaration, widened by an allowance. Gate two of five: the declaration
// itself does not move — validFilePrefix still refuses a dotted prefix, so
// a destination cannot opt into this — and what changes is only that the
// files one plan names are also inside the scope for that run.
func allowedPathWithin(filename string, prefixes []string, allowance WorkflowAllowance) bool {
	return allowance.Admits(filename) || allowedPath(filename, prefixes)
}
