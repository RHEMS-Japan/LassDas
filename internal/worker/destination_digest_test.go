package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The destination configuration's digest is folded from every exported
// field, and every sealed record of a delivery in flight carries the digest
// it was sealed under. So one key added to the destination — or one whose
// zero value stops being omitted — makes every record of every flying
// delivery unreadable at once, and each of those deliveries fails a card on
// "not bound to this run" until something notices.
//
// These three are the shipped destination as it is, one asking for
// production, and one asking for production with no observation entry at
// all — which loads today, and is exactly the state a destination with no
// release path is in. Pinned so that widening what a destination may say is
// a deliberate act with a visible cost, rather than a field added in
// passing.
func TestTheDestinationDigestsAreUnmoved(t *testing.T) {
	const shipped = "../../config/m1-consumer.json"
	raw, err := os.ReadFile(shipped)
	if err != nil {
		t.Fatal(err)
	}
	digest := func(path string) string {
		t.Helper()
		config, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		sum, err := config.SHA256()
		if err != nil {
			t.Fatal(err)
		}
		return sum
	}
	variant := func(mutate func(map[string]any)) string {
		t.Helper()
		var parsed map[string]any
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Fatal(err)
		}
		mutate(parsed["consumers"].([]any)[0].(map[string]any))
		encoded, err := json.Marshal(parsed)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "m1-consumer.json")
		if err := os.WriteFile(path, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
		return digest(path)
	}
	for _, testCase := range []struct {
		name, want string
		mutate     func(map[string]any)
	}{
		{
			name:   "as shipped",
			want:   "c03470efb08d9662e97d6972c6976be418b738348b313717b863fe99fe041cd7",
			mutate: nil,
		},
		{
			name: "asking for production",
			want: "57fbde78a645429d7c0afbda5ede51cd351ad16f4051a6eef99d1aae0ea2e58a",
			mutate: func(consumer map[string]any) {
				consumer["delivery"] = "production"
			},
		},
		{
			name: "asking for production with no observation entry",
			want: "380d803071c8fb9b4fce1d28d262cb6f639465d79ad2a07fc6eb0bd8fbba7084",
			mutate: func(consumer map[string]any) {
				consumer["delivery"] = "production"
				delete(consumer, "staging_login_url")
				delete(consumer, "production_login_url")
				delete(consumer, "observation_language")
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := digest(shipped)
			if testCase.mutate != nil {
				got = variant(testCase.mutate)
			}
			if got != testCase.want {
				t.Fatalf("digest = %s, want %s: a destination key moved, and every record of every "+
					"delivery in flight is now unreadable", got, testCase.want)
			}
		})
	}
}
