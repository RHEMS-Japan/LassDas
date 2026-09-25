package ticketview

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The page says how far this delivery was meant to travel, and — when the
// engine could not take it that far — what stopped it.
//
// Without this the page shows a pull request and nothing else, and a reader
// looking at a destination configured for production cannot tell whether
// the delivery is still going, finished early on purpose, or stopped
// because a setting is missing.
func TestThePageSaysHowFarTheDeliveryWasMeantToGo(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		dir := t.TempDir()
		if body != "" {
			if err := os.WriteFile(filepath.Join(dir, "delivery-depth.json"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}

	// Reached what it was configured for: the depth is on the page and
	// there is nothing to explain.
	var reached View
	reached.readDelivery(write(t, `{"schema_version":1,"repository":"a/b","configured":"production","reached":"production","decided_at":"2026-09-20T00:00:00Z"}`))
	if reached.DeliveryConfigured != "production" || reached.DeliveryReached != "production" {
		t.Fatalf("depth = %q / %q", reached.DeliveryConfigured, reached.DeliveryReached)
	}
	if len(reached.Timeline) != 0 {
		t.Fatalf("a delivery that went the whole way explained itself: %+v", reached.Timeline)
	}

	// Stopped short: the page says both depths and names the settings.
	var short View
	short.readDelivery(write(t, `{"schema_version":1,"repository":"a/b","configured":"production","reached":"pull_request",`+
		`"missing":["chain.deliver.checks_profile","chain.deliver.promote_profile"],"decided_at":"2026-09-20T00:00:00Z"}`))
	if short.DeliveryConfigured != "production" || short.DeliveryReached != "pull_request" {
		t.Fatalf("depth = %q / %q", short.DeliveryConfigured, short.DeliveryReached)
	}
	if len(short.Timeline) != 1 {
		t.Fatalf("timeline = %+v, want one entry saying where it stopped", short.Timeline)
	}
	event := short.Timeline[0]
	if !strings.Contains(event.Title, "production") || !strings.Contains(event.Title, "pull_request") {
		t.Fatalf("the entry does not name both depths: %q", event.Title)
	}
	if len(event.Evidence) != 1 || !strings.Contains(event.Evidence[0].Text, "chain.deliver.checks_profile") {
		t.Fatalf("the entry does not name the settings: %+v", event.Evidence)
	}

	// Nothing recorded, and a half-written record: the page says nothing
	// rather than reading a missing depth as "this one only proposes".
	for name, body := range map[string]string{
		"no record":     "",
		"no depth":      `{"schema_version":1,"repository":"a/b"}`,
		"not ours":      `{"schema_version":99,"configured":"production","reached":"production"}`,
		"not even json": `{`,
	} {
		var silent View
		silent.readDelivery(write(t, body))
		if silent.DeliveryConfigured != "" || silent.DeliveryReached != "" || len(silent.Timeline) != 0 {
			t.Errorf("%s: the page invented a depth: %+v", name, silent)
		}
	}
}
