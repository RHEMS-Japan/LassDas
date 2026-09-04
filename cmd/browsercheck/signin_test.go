package main

import (
	"testing"

	"automation.internal/ticket-ingress/internal/worker"
)

// The sealed observation signs in through the consumer's entry for the
// environment it observes, landing on that environment's own origin; a
// consumer without an entry signs in nowhere.
func TestSignInForPicksTheEnvironmentEntry(t *testing.T) {
	config := worker.Config{Consumers: []worker.ConsumerConfig{
		{Repository: "example/public", StagingOrigin: "https://public-stg.example.invalid", ProductionOrigin: "https://public.example.invalid"},
		{
			Repository: "example/console", StagingOrigin: "https://stg.example.invalid", ProductionOrigin: "https://console.example.invalid",
			StagingLoginURL: "https://api-stg.example.invalid/login?returnTo=/console", ProductionLoginURL: "https://api.example.invalid/login?returnTo=/console",
			ObservationLanguage: "ja",
		},
	}}
	staging := signInFor(config, "example/console", "staging")
	if staging.LoginURL != "https://api-stg.example.invalid/login?returnTo=/console" || staging.LandedPrefix != "https://stg.example.invalid" || staging.Language != "ja" {
		t.Fatalf("staging = %+v", staging)
	}
	production := signInFor(config, "example/console", "production")
	if production.LoginURL != "https://api.example.invalid/login?returnTo=/console" || production.LandedPrefix != "https://console.example.invalid" || production.Language != "ja" {
		t.Fatalf("production = %+v", production)
	}
	if entry := signInFor(config, "example/public", "staging"); entry.LoginURL != "" || entry.LandedPrefix != "" || entry.Language != "" {
		t.Fatalf("a consumer without an entry = %+v", entry)
	}
	if entry := signInFor(config, "example/absent", "staging"); entry.LoginURL != "" {
		t.Fatalf("an unknown consumer = %+v", entry)
	}
}
