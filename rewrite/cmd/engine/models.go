package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// This is provider catalog data, not a schema for a working model's answer.
// Raw entries retain new pricing/capability fields without a code update.
type modelList struct {
	FetchedAt time.Time         `json:"fetched_at"`
	Data      []json.RawMessage `json:"data"`
}

func writeModelList(ctx context.Context, output io.Writer) error {
	snapshot, err := currentModelList(ctx)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(snapshot)
}

func currentModelList(ctx context.Context) (modelList, error) {
	// Omitting offset and limit requests the complete catalog. The default
	// modality is text only; all includes the decision models used in the PoC.
	// The public catalog needs no credential. No operator key is read here.
	entries, err := fetchModelCatalog(ctx, "current model catalog", "https://openrouter.ai/api/v1/models?output_modalities=all", "")
	if err != nil {
		return modelList{}, err
	}
	snapshot := modelList{FetchedAt: time.Now().UTC(), Data: entries}
	// No process cache, saved-list fallback, model default or curated shortlist.
	return snapshot, nil
}

// gatewayModelIDs reports which endpoint ids an invocation gateway serves.
// This is provider catalog data like the public list; nothing here decodes or
// judges a role's answer. The gateway publishes ids without capabilities or
// prices, so eligibility still comes from the public catalog.
func gatewayModelIDs(ctx context.Context, gateway gatewayConfig) (map[string]bool, error) {
	// This list is the one catalog request that carries a credential, so the
	// endpoint must be one that cannot expose it in transit.
	address, err := url.Parse(gateway.ModelsURL)
	if err != nil || address.Scheme != "https" || address.Host == "" || address.User != nil {
		return nil, errors.New("gateway model list must be an HTTPS URL without credentials in it")
	}
	key := os.Getenv(gateway.KeyEnv)
	if key == "" || strings.ContainsAny(key, "\r\n") {
		return nil, errors.New("gateway credential is unavailable: " + gateway.KeyEnv)
	}
	entries, err := fetchModelCatalog(ctx, "gateway model list", gateway.ModelsURL, key)
	if err != nil {
		return nil, err
	}
	served := make(map[string]bool, len(entries))
	for _, entry := range entries {
		var model struct{ ID string }
		if err := json.Unmarshal(entry, &model); err != nil {
			return nil, err
		}
		served[model.ID] = true
	}
	return served, nil
}

// One bounded request per call: no redirect, no partial body, no saved
// snapshot and no merge with an earlier list. A list that needs a credential
// receives it as a header, and any reported reply has it removed first.
func fetchModelCatalog(ctx context.Context, name, address, key string) ([]json.RawMessage, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Cache-Control", "no-cache")
	if key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("model catalog redirect refused")
	}}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", name, err)
	}
	defer response.Body.Close()
	// Bound the administrative query's memory, never truncate it into a list
	// that would falsely look complete. This is not a role-output size limit.
	const maxCatalogBytes = 16 << 20
	body, err := io.ReadAll(io.LimitReader(response.Body, maxCatalogBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	if response.StatusCode != http.StatusOK {
		// Scrub before truncating: a credential straddling the cut would
		// otherwise survive in part and travel into the history and the
		// tracker as an operator-facing error.
		detail := string(body)
		if key != "" {
			detail = strings.ReplaceAll(detail, key, "[credential]")
		}
		detail = strings.TrimSpace(detail[:min(len(detail), 1024)])
		return nil, fmt.Errorf("%s HTTP %d: %s", name, response.StatusCode, detail)
	}
	if len(body) > maxCatalogBytes {
		return nil, errors.New(name + " exceeds 16 MiB; no partial list returned")
	}
	var payload struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("read %s JSON: %w", name, err)
	}
	if len(payload.Data) == 0 {
		return nil, errors.New(name + " returned no models")
	}
	for i, entry := range payload.Data {
		var model struct{ ID string }
		if err := json.Unmarshal(entry, &model); err != nil || strings.TrimSpace(model.ID) == "" {
			return nil, fmt.Errorf("%s entry %d has no readable model id", name, i)
		}
	}
	return payload.Data, nil
}
