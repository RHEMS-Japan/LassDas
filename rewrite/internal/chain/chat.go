package chain

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
)

// ChatRouter invokes a configured role using native function calling. It can
// give the next role an instruction, unlike a choice-only decision model.
// Reports from working roles remain arbitrary text, not function arguments.
type ChatRouter struct {
	Service      Jev
	Roles        map[string]string
	Instructions string
}

func (r ChatRouter) Next(ctx context.Context, state State) (Assignment, error) {
	names := []string{"done"}
	for name := range r.Roles {
		names = append(names, name)
	}
	sort.Strings(names)
	stateJSON, err := json.Marshal(routingView(state))
	if err != nil {
		return Assignment{}, err
	}
	rolesJSON, err := json.Marshal(r.Roles)
	if err != nil {
		return Assignment{}, err
	}
	data, err := r.Service.request(ctx, map[string]any{
		"model": r.Service.Model, "max_tokens": 4000,
		"reasoning": map[string]string{"effort": "low"},
		"messages": []map[string]string{
			{"role": "system", "content": routingInstructions + "\n" + r.Instructions + "\nAvailable roles: " + string(rolesJSON) + "\nInvoke handoff for the next role. Include useful instructions for that role, or choose done when the request is complete."},
			{"role": "user", "content": string(stateJSON)},
		},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name": "handoff", "description": "Invoke one configured role, or finish a completed request.",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{
				"role":        map[string]any{"type": "string", "enum": names},
				"instruction": map[string]string{"type": "string"},
			}, "required": []string{"role"}},
		}}},
	})
	if err != nil {
		return Assignment{}, err
	}
	var response struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					Function struct {
						Name      string
						Arguments string
					}
				} `json:"tool_calls"`
			}
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return Assignment{}, err
	}
	for _, choice := range response.Choices {
		for _, call := range choice.Message.ToolCalls {
			if call.Function.Name != "handoff" {
				continue
			}
			var assignment Assignment
			if err := json.Unmarshal([]byte(call.Function.Arguments), &assignment); err != nil {
				return Assignment{}, err
			}
			if _, ok := r.Roles[assignment.Role]; !ok && assignment.Role != "done" {
				return Assignment{}, errors.New("handoff named an unconfigured role")
			}
			return assignment, nil
		}
	}
	return Assignment{}, errors.New("the router did not invoke a role")
}
