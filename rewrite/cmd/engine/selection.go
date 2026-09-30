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
	Judge        chain.Jev  `json:"judge"`
	Fallback     *chain.Jev `json:"fallback,omitempty"`
	Authors      []string   `json:"authors"`
	Instructions string     `json:"instructions,omitempty"`
	// Fixed names one model for every launch instead of choosing one. It is an
	// experiment switch for comparing a single strong model against per-launch
	// selection, not a recommendation and not a statement about either result.
	Fixed string `json:"fixed,omitempty"`
	// Gateway is optional. Without it, selection and invocation are unchanged.
	Gateway *gatewayConfig `json:"gateway,omitempty"`
	observe func(string)
}

// An OpenAI-compatible gateway can serve the same catalog models under
// prefixed ids while billing a different account. Its list publishes ids
// without capabilities or prices, so it narrows the eligible set and never
// replaces the public catalog the judge reads. The prefix is a route to the
// same model, not a different model and not a judgement about its answer.
type gatewayConfig struct {
	ModelsURL string `json:"models_url"`
	KeyEnv    string `json:"key_env"`
	Prefix    string `json:"prefix"`
}

// fixedModel is the operator's named model, or empty when none is configured.
// Whitespace alone is not a model id, so it reads as no fixed model at all.
func (s selectionConfig) fixedModel() string {
	return strings.TrimSpace(s.Fixed)
}

// invocationPrefix is prepended when reaching the chosen model, and nowhere
// else: eligibility, the judge's choices and the recorded model keep the
// catalog id. An empty prefix leaves invocation exactly as configured.
func (s selectionConfig) invocationPrefix() string {
	if s.Gateway == nil {
		return ""
	}
	return s.Gateway.Prefix
}

// A half-configured gateway would silently invoke the bare id on the account
// the operator is moving away from, and an unusable fixed id would only fail
// at launch. Refuse both before any request is accepted.
func (s *selectionConfig) validate() error {
	if s == nil {
		return nil
	}
	if fixed := s.fixedModel(); fixed != "" && !strings.Contains(fixed, "/") {
		return errors.New("model_selection.fixed must be a full publisher/model endpoint id")
	}
	if s.Gateway == nil {
		return nil
	}
	switch {
	case strings.TrimSpace(s.Gateway.ModelsURL) == "":
		return errors.New("model_selection.gateway.models_url must name the gateway's model list endpoint")
	case strings.TrimSpace(s.Gateway.KeyEnv) == "":
		return errors.New("model_selection.gateway.key_env must name the environment variable holding the gateway credential")
	case strings.TrimSpace(s.Gateway.Prefix) == "":
		return errors.New("model_selection.gateway.prefix must give the prefix the gateway lists catalog models under")
	}
	return nil
}

// Routing is also a model invocation. Use the same fresh selection policy as
// working roles, including when chat is the decision service's alternative.
// Selection failure returns to the existing recovery loop; never use an old
// endpoint to conceal it. Neither selection nor this wrapper judges prose.
type selectedChatRouter struct {
	chat      chain.ChatRouter
	selection selectionConfig
}

func (r selectedChatRouter) Next(ctx context.Context, state chain.State) (chain.Assignment, error) {
	model, err := r.selection.choose(ctx,
		chain.Role{Name: "router", Purpose: "Choose the next responsible role from the original request and independent reports; resolve remaining work before delivery or completion"},
		chain.Process{Name: "routing"}, state, nil)
	if err != nil {
		return chain.Assignment{}, err
	}
	chat := r.chat
	// Routing is an invocation too: it reaches the same model by the same
	// route as the working roles. The chosen id itself is unchanged.
	endpoint := r.selection.invocationPrefix() + model
	chat.Service.Model = endpoint
	if r.selection.observe != nil {
		notice := "routing with freshly selected model: " + model
		if r.selection.fixedModel() != "" {
			notice = "routing with the configured fixed model: " + model
		}
		if endpoint != model {
			notice += "; invoked through the configured gateway as " + endpoint
		}
		r.selection.observe(notice)
	}
	next, err := chat.Next(ctx, state)
	if err != nil {
		return chain.Assignment{}, fmt.Errorf("routing model %s: %w", model, err)
	}
	return next, nil
}

func (s selectionConfig) choose(ctx context.Context, role chain.Role, process chain.Process, state chain.State, selected []string) (string, error) {
	// The operator named the model, so there is nothing to look up or judge:
	// no catalog, no gateway list and no selector call. Peer separation cannot
	// exclude a fixed model, so a parallel review group runs the same one.
	if fixed := s.fixedModel(); fixed != "" {
		return fixed, nil
	}
	model, err := s.selectWith(ctx, s.Judge, role, process, state, selected)
	if err == nil || ctx.Err() != nil || s.Fallback == nil {
		return model, err
	}
	if s.observe != nil {
		s.observe("model selector unavailable; using configured chat alternative: " + err.Error())
	}
	// This is a new selection attempt, so selectWith fetches a new catalog
	// rather than letting the alternative decide from an earlier snapshot.
	model, alternativeError := s.selectWith(ctx, chain.ChatJudge{Service: *s.Fallback}, role, process, state, selected)
	if alternativeError != nil {
		return "", errors.Join(err, fmt.Errorf("alternative model selector: %w", alternativeError))
	}
	return model, nil
}

func (s selectionConfig) selectWith(ctx context.Context, judge chain.Judge, role chain.Role, process chain.Process, state chain.State, selected []string) (string, error) {
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
	if s.Gateway != nil {
		// Keep only ids this gateway actually serves, using a list fetched for
		// this attempt. A failure here leaves the attempt unavailable: falling
		// back to the bare id would invoke the account being moved away from,
		// and an earlier list would claim availability nobody observed.
		served, err := gatewayModelIDs(ctx, *s.Gateway)
		if err != nil {
			return "", err
		}
		for id := range choices {
			if !served[s.Gateway.Prefix+id] {
				delete(choices, id)
			}
		}
	}
	if len(choices) == 0 {
		return "", errors.New("fresh catalog has no eligible independent model; no earlier selection was reused")
	}
	// Runtime failures inform recovery without copying every previous work
	// report into the model-selection query. Those reports remain unchanged
	// in the actual role and routing prompts.
	input := selectionInput(state)
	instructions := "Select one current frontier/value tool-using model for the assigned responsibility. Restrict the choice to a publisher's latest frontier generation suitable for the task. Being available in today's catalog does not make an old generation current. Do not choose a superseded generation merely because it is cheap or has a coding-specific name. Use the fresh catalog's descriptions, listed_at dates, canonical versions and prices, not a memorized version shortlist. Within the current frontier generation, choose strong task-completion value. A newly listed accelerated SKU is not automatically more capable: throughput alone does not justify its premium. Prices are USD per token. Earlier runtime failures are observations for choosing a useful recovery, not permission to weaken the request or declare completion. Only the listed endpoints can be invoked. This chooses a worker, not a certificate of the eventual answer.\n" +
		"Responsibility: " + role.Purpose + "\nProcess: " + process.Name + "\nCatalog observation: " + snapshot.FetchedAt.String() + "\n" + s.Instructions
	model, err := judge.Choose(ctx, input, instructions, choices)
	if err != nil {
		return "", err
	}
	if _, exists := choices[model]; !exists {
		return "", errors.New("model selector did not choose an eligible endpoint from the current catalog")
	}
	return model, nil
}

// selectionFailures and selectionErrorLimit bound what the judge reads of
// earlier failures: the most recent ones, each cut to its head. A run that
// failed to launch many times in a row must not grow the query until the
// decision service refuses it.
const selectionFailures, selectionErrorLimit = 12, 300

// selectionRequestLimit bounds the request text the judge reads. Choosing a
// model needs the shape of the work, not every observation a report-only
// request carries with it, and the judge's own input is small.
const selectionRequestLimit = 3000

func selectionInput(state chain.State) chain.State {
	input := chain.State{Request: state.Request}
	if len(input.Request) > selectionRequestLimit {
		input.Request = input.Request[:selectionRequestLimit] + "…"
	}
	var failures []chain.Result
	for _, result := range state.History {
		if result.Error != "" && (result.Model != "" || (result.Role == "router" && result.Speaker == "runtime")) {
			text := result.Error
			if len(text) > selectionErrorLimit {
				text = text[:selectionErrorLimit] + "…"
			}
			failures = append(failures, chain.Result{Role: result.Role, Speaker: result.Speaker, Model: result.Model, Error: text})
		}
	}
	if len(failures) > selectionFailures {
		omitted := len(failures) - selectionFailures
		failures = append([]chain.Result{{Role: "runtime", Speaker: "runtime",
			Error: fmt.Sprintf("%d earlier failures are in the record and not repeated here", omitted)}}, failures[omitted:]...)
	}
	input.History = failures
	return input
}
