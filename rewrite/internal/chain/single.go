package chain

import "context"

// OneRoleRouter runs a single role until a launch of it returns without a
// process error, then ends the run. It asks no model: the fact that ends a
// report-only run is the reporter's own return, and a record of any size
// changes nothing about that, so a request whose stopped work left a long
// record still gets its report. A launch is every process record of the role
// behind the runtime's own notes; all of them must have returned.
type OneRoleRouter struct{ Role string }

func (r OneRoleRouter) Next(_ context.Context, state State) (Assignment, error) {
	history := state.History
	i := len(history)
	for i > 0 && history[i-1].Speaker == "runtime" {
		if history[i-1].Error != "" {
			return Assignment{Role: r.Role}, nil
		}
		i--
	}
	processes := 0
	for ; i > 0 && history[i-1].Speaker != "runtime" && history[i-1].Role == r.Role; i-- {
		if history[i-1].Error != "" {
			return Assignment{Role: r.Role}, nil
		}
		processes++
	}
	if processes > 0 {
		return Assignment{Role: "done"}, nil
	}
	return Assignment{Role: r.Role}, nil
}
