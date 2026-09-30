package chain

import "context"

// OneRoleRouter runs a single role until a launch of it returns without a
// process error, then ends the run. It asks no model: the fact that ends a
// report-only run is the reporter's own return, and a record of any size
// changes nothing about that, so a request whose stopped work left a long
// record still gets its report.
type OneRoleRouter struct{ Role string }

func (r OneRoleRouter) Next(_ context.Context, state State) (Assignment, error) {
	for i := len(state.History) - 1; i >= 0; i-- {
		record := state.History[i]
		if record.Speaker == "runtime" && record.Error == "" {
			continue
		}
		if record.Role == r.Role && record.Speaker != "runtime" && record.Error == "" {
			return Assignment{Role: "done"}, nil
		}
		break
	}
	return Assignment{Role: r.Role}, nil
}
