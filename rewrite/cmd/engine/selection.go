package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

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
// The list may name a model with its publisher or without it; routes reads
// both, so the operator writes the prefix and nothing about the shape.
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

// invocationPrefix is prepended when reaching the operator's fixed model,
// which is named as the catalog names it, and nowhere else: a selected model
// is reached by the route its selection found. An empty prefix leaves
// invocation exactly as configured.
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
	if s.fixedModel() == "" && strings.TrimSpace(s.Judge.URL) == "" && s.Fallback == nil {
		return errors.New("model_selection needs a judge (decision service), a fallback chat service to choose with, or a fixed model")
	}
	// A judge left half configured would quietly hand every choice to the
	// chat service; say so instead.
	if strings.TrimSpace(s.Judge.URL) == "" && (strings.TrimSpace(s.Judge.Model) != "" || strings.TrimSpace(s.Judge.KeyEnv) != "") {
		return errors.New("model_selection.judge needs url when model or key_env is given; omit the judge entirely to choose with the fallback chat service")
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
	model, prefix, err := r.selection.choose(ctx,
		chain.Role{Name: "router", Purpose: "Choose the next responsible role from the original request and independent reports; resolve remaining work before delivery or completion"},
		chain.Process{Name: "routing"}, state, nil)
	if err != nil {
		return chain.Assignment{}, err
	}
	chat := r.chat
	// Routing is an invocation too: it reaches the same model by the same
	// route as the working roles. The chosen id itself is unchanged.
	endpoint := prefix + model
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

// choose names the model one launch records, and the prefix that reaches it
// through the configured gateway: empty when the model is reached as named.
func (s selectionConfig) choose(ctx context.Context, role chain.Role, process chain.Process, state chain.State, selected []string) (model, prefix string, err error) {
	// The operator named the model, so there is nothing to look up or judge:
	// no catalog, no gateway list and no selector call. Peer separation cannot
	// exclude a fixed model, so a parallel review group runs the same one.
	if fixed := s.fixedModel(); fixed != "" {
		return fixed, s.invocationPrefix(), nil
	}
	// With no decision service named, the chat service configured as the
	// alternative makes every choice by itself: nothing is tried first and
	// nothing is logged as a failure. That keeps every model call on one
	// route, for an operator whose gateway answers chat but not decisions.
	if strings.TrimSpace(s.Judge.URL) == "" && s.Fallback != nil {
		return s.selectWith(ctx, chain.ChatJudge{Service: *s.Fallback}, role, process, state, selected)
	}
	model, prefix, err = s.selectWith(ctx, s.Judge, role, process, state, selected)
	if err == nil || ctx.Err() != nil || s.Fallback == nil {
		return model, prefix, err
	}
	if s.observe != nil {
		s.observe("model selector unavailable; using configured chat alternative: " + err.Error())
	}
	// This is a new selection attempt, so selectWith fetches a new catalog
	// rather than letting the alternative decide from an earlier snapshot.
	model, prefix, alternativeError := s.selectWith(ctx, chain.ChatJudge{Service: *s.Fallback}, role, process, state, selected)
	if alternativeError != nil {
		return "", "", errors.Join(err, fmt.Errorf("alternative model selector: %w", alternativeError))
	}
	return model, prefix, nil
}

func (s selectionConfig) selectWith(ctx context.Context, judge chain.Judge, role chain.Role, process chain.Process, state chain.State, selected []string) (string, string, error) {
	if len(s.Authors) == 0 {
		return "", "", errors.New("configure eligible model publishers; none were supplied")
	}
	snapshot, err := currentModelList(ctx)
	if err != nil {
		return "", "", err
	}
	choices := map[string]string{}
	// The catalog's order is kept: of two publishers' ids that a gateway
	// serves by one id, the first listed is the one offered.
	var eligible []string
	for _, raw := range snapshot.Data {
		var model struct {
			ID           string   `json:"id"`
			Supported    []string `json:"supported_parameters"`
			Architecture struct {
				Output []string `json:"output_modalities"`
			} `json:"architecture"`
		}
		if err := json.Unmarshal(raw, &model); err != nil {
			return "", "", fmt.Errorf("read catalog capabilities: %w", err)
		}
		author, _, ok := strings.Cut(model.ID, "/")
		if !ok || !slices.Contains(s.Authors, author) || !slices.Contains(model.Supported, "tools") || !slices.Contains(model.Architecture.Output, "text") {
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return "", "", err
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
			return "", "", err
		}
		if _, listed := choices[model.ID]; !listed {
			eligible = append(eligible, model.ID)
		}
		choices[model.ID] = string(data)
	}
	routes, err := s.routes(ctx, eligible)
	if err != nil {
		return "", "", err
	}
	// Peers are separated by the catalog's publishers. An earlier choice made
	// by an id a gateway lists without the publisher is read back to the
	// catalog id it stood for in this attempt; any other is read as it is.
	standsFor := make(map[string]string, len(routes))
	for _, id := range eligible {
		if route, served := routes[id]; served {
			if _, taken := standsFor[route.model]; !taken {
				standsFor[route.model] = id
			}
		}
	}
	excluded := map[string]bool{}
	for _, model := range selected {
		if id, known := standsFor[model]; known {
			model = id
		}
		author, _, _ := strings.Cut(model, "/")
		excluded[author] = true
	}
	for id := range choices {
		author, _, _ := strings.Cut(id, "/")
		if _, served := routes[id]; !served || excluded[author] {
			delete(choices, id)
		}
	}
	if len(choices) == 0 {
		return "", "", errors.New("fresh catalog has no eligible independent model; no earlier selection was reused")
	}
	// Runtime failures inform recovery without copying every previous work
	// report into the model-selection query. Those reports remain unchanged
	// in the actual role and routing prompts.
	input := selectionInput(state)
	instructions := "Select one current frontier/value tool-using model for the assigned responsibility. Restrict the choice to a publisher's latest frontier generation suitable for the task. Being available in today's catalog does not make an old generation current. Do not choose a superseded generation merely because it is cheap or has a coding-specific name. Use the fresh catalog's descriptions, listed_at dates, canonical versions and prices, not a memorized version shortlist. Within the current frontier generation, choose strong task-completion value. A newly listed accelerated SKU is not automatically more capable: throughput alone does not justify its premium. Prices are USD per token. Earlier runtime failures are observations for choosing a useful recovery, not permission to weaken the request or declare completion. Only the listed endpoints can be invoked. This chooses a worker, not a certificate of the eventual answer.\n" +
		"Responsibility: " + role.Purpose + "\nProcess: " + process.Name + "\nCatalog observation: " + snapshot.FetchedAt.String() + "\n" + s.Instructions
	model, err := judge.Choose(ctx, input, instructions, choices)
	if err != nil {
		return "", "", err
	}
	if _, exists := choices[model]; !exists {
		return "", "", errors.New("model selector did not choose an eligible endpoint from the current catalog")
	}
	route := routes[model]
	return route.model, route.prefix, nil
}

// modelRoute is how a launch reaches one catalog model: the id it records,
// and the prefix added to that id when it is invoked.
type modelRoute struct{ model, prefix string }

// routes says how each eligible catalog id is reached, and leaves out every
// id the configured gateway does not serve. A gateway lists a model either
// with its publisher, prefix + "z-ai/glm-5.3", or without it, prefix +
// "glm-5.3". The first shape is reached as it always was: the catalog id is
// recorded and the prefix added when invoking. The second is recorded and
// invoked as the gateway lists it, and serves only the first of the eligible
// ids that share its name, so the judge is not offered one model twice.
func (s selectionConfig) routes(ctx context.Context, eligible []string) (map[string]modelRoute, error) {
	routes := make(map[string]modelRoute, len(eligible))
	if s.Gateway == nil {
		for _, id := range eligible {
			routes[id] = modelRoute{model: id}
		}
		return routes, nil
	}
	// Keep only ids this gateway actually serves, using a list fetched for
	// this attempt. A failure here leaves the attempt unavailable: falling
	// back to the bare id would invoke the account being moved away from,
	// and an earlier list would claim availability nobody observed.
	served, err := gatewayModelIDs(ctx, *s.Gateway)
	if err != nil {
		return nil, err
	}
	prefix := s.Gateway.Prefix
	taken := map[string]bool{}
	for _, id := range eligible {
		_, name, _ := strings.Cut(id, "/")
		switch {
		case served[prefix+id]:
			routes[id] = modelRoute{model: id, prefix: prefix}
		case served[prefix+name] && !taken[prefix+name]:
			taken[prefix+name] = true
			routes[id] = modelRoute{model: prefix + name}
		}
	}
	return routes, nil
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
	if utf8.RuneCountInString(input.Request) > selectionRequestLimit {
		input.Request = limitRunes(input.Request, selectionRequestLimit) + "…"
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
