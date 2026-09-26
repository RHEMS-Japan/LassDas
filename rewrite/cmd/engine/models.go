package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://openrouter.ai/api/v1/models?output_modalities=all", nil)
	if err != nil {
		return modelList{}, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Cache-Control", "no-cache")
	// The public catalog needs no credential. No operator key is read here.
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("model catalog redirect refused")
	}}
	response, err := client.Do(request)
	if err != nil {
		return modelList{}, fmt.Errorf("fetch current model catalog: %w", err)
	}
	defer response.Body.Close()
	// Bound the administrative query's memory, never truncate it into a list
	// that would falsely look complete. This is not a role-output size limit.
	const maxCatalogBytes = 16 << 20
	body, err := io.ReadAll(io.LimitReader(response.Body, maxCatalogBytes+1))
	if err != nil {
		return modelList{}, fmt.Errorf("read current model catalog: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return modelList{}, fmt.Errorf("current model catalog HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body[:min(len(body), 1024)])))
	}
	if len(body) > maxCatalogBytes {
		return modelList{}, errors.New("current model catalog exceeds 16 MiB; no partial list returned")
	}
	var payload struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return modelList{}, fmt.Errorf("read current model catalog JSON: %w", err)
	}
	if len(payload.Data) == 0 {
		return modelList{}, errors.New("current model catalog returned no models")
	}
	for i, entry := range payload.Data {
		var model struct{ ID string }
		if err := json.Unmarshal(entry, &model); err != nil || strings.TrimSpace(model.ID) == "" {
			return modelList{}, fmt.Errorf("current model catalog entry %d has no readable model id", i)
		}
	}
	snapshot := modelList{FetchedAt: time.Now().UTC(), Data: payload.Data}
	// No process cache, saved-list fallback, model default or curated shortlist.
	return snapshot, nil
}
