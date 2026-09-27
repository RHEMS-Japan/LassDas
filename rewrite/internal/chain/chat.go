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
	rolesJSON, err := json.Marshal(r.Roles)
	if err != nil {
		return Assignment{}, err
	}
	choices := make(map[string]string, len(r.Roles)+1)
	for name, purpose := range r.Roles {
		choices[name] = purpose
	}
	choices["done"] = "The original request is complete."
	instructions := routingInstructions + "\n" + r.Instructions + "\nAvailable roles: " + string(rolesJSON) + "\nInvoke handoff for the next role. Include useful instructions for that role, or choose done when the request is complete."
	return chatAction(ctx, r.Service, state, instructions, choices, "Invoke one configured role, or finish a completed request.")
}

// ChatJudge uses the same native function-call transport for choosing a model
// endpoint. Its choices contain no completion action. Working prose is still
// neither parsed nor certified here.
type ChatJudge struct{ Service Jev }

func (j ChatJudge) Choose(ctx context.Context, state State, instructions string, choices map[string]string) (string, error) {
	options, err := json.Marshal(choices)
	if err != nil {
		return "", err
	}
	instructions += "\nAvailable model endpoints: " + string(options) + "\nInvoke handoff with the chosen model id as role. Only select an endpoint; do not perform the work or declare the request complete."
	assignment, err := chatAction(ctx, j.Service, state, instructions, choices, "Select one listed model endpoint.")
	return assignment.Role, err
}

func chatAction(ctx context.Context, service Jev, state State, instructions string, choices map[string]string, description string) (Assignment, error) {
	names := make([]string, 0, len(choices))
	for name := range choices {
		names = append(names, name)
	}
	sort.Strings(names)
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return Assignment{}, err
	}
	data, err := service.request(ctx, map[string]any{
		"model": service.Model, "max_tokens": 4000,
		"reasoning": map[string]string{"effort": "low"},
		"messages": []map[string]string{
			{"role": "system", "content": instructions},
			{"role": "user", "content": string(stateJSON)},
		},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name": "handoff", "description": description,
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
			if _, ok := choices[assignment.Role]; !ok {
				return Assignment{}, errors.New("handoff named an unconfigured role")
			}
			return assignment, nil
		}
	}
	return Assignment{}, errors.New("the router did not invoke a role")
}
