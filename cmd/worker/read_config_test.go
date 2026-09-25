package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"automation.internal/ticket-ingress/internal/worker"
)

// The card and the engine read one destination configuration, so they have
// to read it the same way.
//
// They did not. The loader fills in a destination that does not say how far
// its changes travel; this command decoded and validated the file by itself
// and did not, so a file the engine planned a production delivery from was
// refused by the first card that opened it — "worker configuration was
// rejected", with nothing saying which line was at fault. The two paths
// have to agree on every file, including the one that says nothing.
func TestTheCardAndTheLoaderReadOneConfiguration(t *testing.T) {
	loaded, err := worker.LoadConfig("../../config/m1-consumer.json")
	if err != nil {
		t.Fatalf("the shipped example did not load: %v", err)
	}
	write := func(t *testing.T, config worker.Config) string {
		t.Helper()
		encoded, err := json.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "consumer.json")
		if err := os.WriteFile(path, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	// As shipped, and then with the one value taken out.
	for name, mutate := range map[string]func(*worker.Config){
		"as shipped": func(*worker.Config) {},
		"with no delivery named": func(c *worker.Config) {
			for index := range c.Consumers {
				c.Consumers[index].Delivery = ""
			}
		},
	} {
		config := loaded
		config.Consumers = append([]worker.ConsumerConfig(nil), loaded.Consumers...)
		mutate(&config)
		path := write(t, config)

		byLoader, loaderErr := worker.LoadConfig(path)
		byCard, cardErr := readConfig(path)
		if (loaderErr == nil) != (cardErr == nil) {
			t.Fatalf("%s: the loader said %v and the card said %v", name, loaderErr, cardErr)
		}
		if loaderErr != nil {
			continue
		}
		loaderDigest, err := byLoader.SHA256()
		if err != nil {
			t.Fatal(err)
		}
		cardDigest, err := byCard.SHA256()
		if err != nil {
			t.Fatal(err)
		}
		if loaderDigest != cardDigest {
			t.Fatalf("%s: the card and the loader read different configurations (%s vs %s)", name, cardDigest, loaderDigest)
		}
		for index, consumer := range byCard.Consumers {
			if consumer.Delivery == "" {
				t.Fatalf("%s: destination %d reached a card with no depth", name, index)
			}
		}
	}
}
