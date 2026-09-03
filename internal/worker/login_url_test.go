package worker

import (
	"path/filepath"
	"testing"
)

func TestValidLoginURL(t *testing.T) {
	for _, value := range []string{
		"https://api-stg.example.invalid/console/auth/login",
		"https://api-stg.example.invalid/console/auth/login?returnTo=/console/new",
		"https://edge-stg.example.invalid/admin/login?next=%2Fadmin%2F",
	} {
		if !ValidLoginURL(value) {
			t.Errorf("ValidLoginURL(%q) = false", value)
		}
	}
	for _, value := range []string{
		"", "http://api-stg.example.invalid/login", "https://API-STG.example.invalid/login",
		"https://user:secret@api-stg.example.invalid/login", "https://api-stg.example.invalid:8443/login",
		"https://api-stg.example.invalid", "https://api-stg.example.invalid/login#top",
		"https://api-stg.example.invalid/login?x=1 2", "https://api-stg.example.invalid/login\n",
		"https://例え.example.invalid/login",
	} {
		if ValidLoginURL(value) {
			t.Errorf("ValidLoginURL(%q) = true", value)
		}
	}
}

func TestConsumerLoginURLsAreValidatedAndResolvedPerEnvironment(t *testing.T) {
	config, err := LoadConfig(filepath.Join("..", "..", "config", "m1-consumer.json"))
	if err != nil {
		t.Fatal(err)
	}
	consumer := config.Consumers[0]
	if consumer.LoginURL("staging") != consumer.StagingLoginURL || consumer.LoginURL("production") != consumer.ProductionLoginURL ||
		consumer.Origin("staging") != consumer.StagingOrigin || consumer.Origin("production") != consumer.ProductionOrigin {
		t.Fatalf("per-environment accessors disagree with the fields: %+v", consumer)
	}
	config.Consumers[0].StagingLoginURL = "http://api-stg.example.invalid/login"
	if err := config.Validate(); err == nil {
		t.Fatal("a plain-http staging login url was accepted")
	}
	config.Consumers[0].StagingLoginURL = "https://api-stg.example.invalid/login?returnTo=/console"
	config.Consumers[0].ProductionLoginURL = "https://api.example.invalid/login#fragment"
	if err := config.Validate(); err == nil {
		t.Fatal("a production login url with a fragment was accepted")
	}
	config.Consumers[0].ProductionLoginURL = config.Consumers[0].StagingLoginURL
	if err := config.Validate(); err == nil {
		t.Fatal("one login entry for both environments was accepted")
	}
	config.Consumers[0].ProductionLoginURL = ""
	if err := config.Validate(); err != nil {
		t.Fatalf("a valid staging login url alone must pass: %v", err)
	}
	for _, language := range []string{"ja", "en-US", "zh-Hant-TW"} {
		config.Consumers[0].ObservationLanguage = language
		if err := config.Validate(); err != nil {
			t.Fatalf("observation language %q must pass: %v", language, err)
		}
	}
	for _, language := range []string{"JA", "ja_JP", "ja;q=0.9", "ja ", "j"} {
		config.Consumers[0].ObservationLanguage = language
		if err := config.Validate(); err == nil {
			t.Fatalf("observation language %q was accepted", language)
		}
	}
}
