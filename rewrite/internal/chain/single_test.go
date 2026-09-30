package chain

import (
	"context"
	"testing"
)

func TestOneRoleRouterRunsTheRoleUntilItReturnsAndAsksNoModel(t *testing.T) {
	router := OneRoleRouter{Role: "report"}
	next := func(history ...Result) string {
		assignment, err := router.Next(context.Background(), State{History: history})
		if err != nil {
			t.Fatal(err)
		}
		return assignment.Role
	}
	failed := Result{Role: "report", Speaker: "reporter", Error: "exit status 1"}
	note := Result{Role: "report", Speaker: "runtime", Output: "Process reporter did not exit 0."}
	returned := Result{Role: "report", Speaker: "reporter", Output: "posted and read back"}
	for name, got := range map[string]string{
		"an empty record starts the role":                     next(),
		"a failed launch runs it again":                       next(failed, note),
		"a runtime note alone does not end it":                next(Result{Role: "report", Speaker: "runtime", Output: "taken up again"}),
		"a routing note with an error runs it again":          next(returned, Result{Role: "router", Speaker: "runtime", Error: "routing unavailable"}),
		"a launch that returned ends the run (with its note)": next(failed, note, returned, Result{Role: "report", Speaker: "runtime", Output: "Process reporter exited 0."}),
	} {
		want := "report"
		if name == "a launch that returned ends the run (with its note)" {
			want = "done"
		}
		if got != want {
			t.Errorf("%s: got %q, want %q", name, got, want)
		}
	}
	if got := next(returned); got != "done" {
		t.Errorf("a launch that returned without a note: got %q, want done", got)
	}
	// A launch is every process of the role: one failing, in either order,
	// runs the role again.
	other := Result{Role: "report", Speaker: "readback", Output: "read back"}
	if got := next(failed, other); got != "report" {
		t.Errorf("a launch with a failed process recorded first: got %q, want report", got)
	}
	if got := next(other, failed); got != "report" {
		t.Errorf("a launch with a failed process recorded last: got %q, want report", got)
	}
	if got := next(returned, other, Result{Role: "report", Speaker: "runtime", Output: "Process reporter exited 0."}); got != "done" {
		t.Errorf("a launch whose processes all returned: got %q, want done", got)
	}
}
