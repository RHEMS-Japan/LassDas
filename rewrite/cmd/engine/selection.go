package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"ticket-runner/internal/chain"
)

// The operator limits eligible publishers; model versions come only from a
// fresh catalog. Publisher names are not a nationality classifier. No model
// ids, prices, benchmark cutoffs or successful-work answers are baked in.
type selectionConfig struct {
	Judge        chain.Jev `json:"judge"`
	Authors      []string  `json:"authors"`
	Instructions string    `json:"instructions,omitempty"`
}

func (s selectionConfig) choose(ctx context.Context, role chain.Role, process chain.Process, state chain.State, selected []string) (string, error) {
	if len(s.Authors) == 0 {
		return "", errors.New("configure eligible model publishers; none were supplied")
	}
	snapshot, err := currentModelList(ctx)
	if err != nil {
		return "", err
	}
	excluded := map[string]bool{}
	for _, model := range selected {
		author, _, _ := strings.Cut(model, "/")
		excluded[author] = true
	}
	choices := map[string]string{}
	for _, raw := range snapshot.Data {
		var model struct {
			ID           string   `json:"id"`
			Supported    []string `json:"supported_parameters"`
			Architecture struct {
				Output []string `json:"output_modalities"`
			} `json:"architecture"`
		}
		if err := json.Unmarshal(raw, &model); err != nil {
			return "", fmt.Errorf("read catalog capabilities: %w", err)
		}
		author, _, ok := strings.Cut(model.ID, "/")
		if !ok || excluded[author] || !slices.Contains(s.Authors, author) || !slices.Contains(model.Supported, "tools") || !slices.Contains(model.Architecture.Output, "text") {
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return "", err
		}
		// Only the selection input is summarized. --list-models still exposes
		// every provider field, and no working report is trimmed or decoded.
		metadata := map[string]json.RawMessage{}
		for _, key := range []string{"name", "canonical_slug", "created", "description", "pricing", "context_length", "reasoning"} {
			if value, exists := fields[key]; exists {
				metadata[key] = value
			}
		}
		// The provider's Unix timestamp remains available; a readable date
		// makes recency explicit without inventing a release-age cutoff.
		var created int64
		if json.Unmarshal(fields["created"], &created) == nil && created > 0 {
			metadata["listed_at"], _ = json.Marshal(time.Unix(created, 0).UTC().Format(time.RFC3339))
		}
		data, err := json.Marshal(metadata)
		if err != nil {
			return "", err
		}
		choices[model.ID] = string(data)
	}
	if len(choices) == 0 {
		return "", errors.New("fresh catalog has no eligible independent model; no earlier selection was reused")
	}
	// Runtime failures inform recovery without copying every previous work
	// report into the model-selection query. Those reports remain unchanged
	// in the actual role and routing prompts.
	input := chain.State{Request: state.Request}
	for _, result := range state.History {
		if result.Model != "" && result.Error != "" {
			input.History = append(input.History, chain.Result{Role: result.Role, Speaker: result.Speaker, Model: result.Model, Error: result.Error})
		}
	}
	instructions := "Select one current frontier/value tool-using model for the assigned responsibility. Restrict the choice to a publisher's latest frontier generation suitable for the task. Being available in today's catalog does not make an old generation current. Do not choose a superseded generation merely because it is cheap or has a coding-specific name. Use the fresh catalog's descriptions, listed_at dates, canonical versions and prices, not a memorized version shortlist. Within the current frontier generation, choose strong task-completion value. A newly listed accelerated SKU is not automatically more capable: throughput alone does not justify its premium. Prices are USD per token. Earlier runtime failures are observations for choosing a useful recovery, not permission to weaken the request or declare completion. Only the listed endpoints can be invoked. This chooses a worker, not a certificate of the eventual answer.\n" +
		"Responsibility: " + role.Purpose + "\nProcess: " + process.Name + "\nCatalog observation: " + snapshot.FetchedAt.String() + "\n" + s.Instructions
	model, err := s.Judge.Choose(ctx, input, instructions, choices)
	if err != nil {
		return "", err
	}
	if _, exists := choices[model]; !exists {
		return "", errors.New("model selector did not choose an eligible endpoint from the current catalog")
	}
	return model, nil
}
