package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The depth moving into the run adds no field to a destination's
// configuration, so no configuration that loads today hashes differently
// tomorrow. The one thing that changed is a value that was previously
// required: a file that omits it now loads with a default rather than being
// refused, and a file that names it is untouched — which is every file that
// loaded before, because an omitted value was refused.
func TestTheDeliveryDefaultLeavesEveryExistingDigestAlone(t *testing.T) {
	write := func(t *testing.T, config Config) string {
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
	named := validTestConfig()
	wanted, err := named.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(write(t, named))
	if err != nil {
		t.Fatal(err)
	}
	got, err := loaded.SHA256()
	if err != nil || got != wanted {
		t.Fatalf("a destination that names pull_request hashes %q, want %q (error %v)", got, wanted, err)
	}

	// The same file with the value taken out: refused before, and now read
	// as the deepest delivery.
	silent := validTestConfig()
	silent.Consumers[0].Delivery = ""
	filled, err := LoadConfig(write(t, silent))
	if err != nil {
		t.Fatalf("a destination that says nothing about its depth did not load: %v", err)
	}
	if filled.Consumers[0].Delivery != DeliverProduction {
		t.Fatalf("delivery = %q, want production", filled.Consumers[0].Delivery)
	}
	silentDigest, err := filled.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	if silentDigest == wanted {
		t.Fatal("a destination that goes to production hashes the same as one that only proposes")
	}

	// A command-line destination has no environment to reach, so its own
	// default is the proposal — the one its validation insists on.
	cli := validTestConfig()
	cli.Consumers[0] = ConsumerConfig{Kind: "cli", Repository: "example/tool", RepositoryID: 102,
		IntegrationBranch: "main", GitHub: ConsumerGitHubContract{DefaultBranch: "main"},
		Mode:              validTestConfig().Consumers[0].Mode}
	loadedCLI, err := LoadConfig(write(t, cli))
	if err != nil {
		t.Fatalf("a command-line destination that says nothing did not load: %v", err)
	}
	if loadedCLI.Consumers[0].Delivery != DeliverPullRequest {
		t.Fatalf("cli delivery = %q, want pull_request", loadedCLI.Consumers[0].Delivery)
	}
}
