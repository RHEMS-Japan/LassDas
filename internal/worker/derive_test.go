package worker

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func deriveTestTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{
		"client/src/components/Settings.tsx",
		"client/src/components/Example.tsx",
		"client/src/index.tsx",
		"server/main.go",
		"README.md",
	} {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("export const label = 'Old label';\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func deriveTestDraft(t *testing.T) TicketDraft {
	t.Helper()
	draft, _, err := ParseTicketDraft(validTicketEnvelope(t, draftTicketDescription()), validTestConfig(), unboundDevelopmentToolSHA)
	if err != nil {
		t.Fatalf("ParseTicketDraft() error = %v", err)
	}
	return draft
}

func TestCandidateListingOffersOnlyTheWritableScope(t *testing.T) {
	config := validTestConfig()
	listing, err := ReadCandidateListing(deriveTestTree(t), strings.Repeat("a", 40), config.Consumers[0], config)
	if err != nil {
		t.Fatalf("ReadCandidateListing() error = %v", err)
	}
	if err := listing.Validate(config.Consumers[0], config); err != nil {
		t.Fatalf("listing does not validate: %v", err)
	}
	want := []string{"client/src/components/Example.tsx", "client/src/components/Settings.tsx", "client/src/index.tsx"}
	if len(listing.Paths) != len(want) {
		t.Fatalf("paths = %v, want %v", listing.Paths, want)
	}
	for index, path := range want {
		if listing.Paths[index] != path {
			t.Fatalf("paths = %v, want %v (sorted)", listing.Paths, want)
		}
	}
	// Anything the mode does not allow to be written is never offered, so it
	// cannot be chosen no matter what the model answers.
	for _, path := range listing.Paths {
		if !strings.HasPrefix(path, "client/src/") {
			t.Fatalf("listing offered %q outside the writable scope", path)
		}
	}
	// A tampered listing fails its own digest.
	tampered := listing
	tampered.Paths = append([]string{"server/main.go"}, listing.Paths...)
	if err := tampered.Validate(config.Consumers[0], config); err == nil {
		t.Fatal("a tampered listing was accepted")
	}
}

// scriptedDeriveAPI returns one canned model answer and captures the prompt.
func newDeriveInvoker(t *testing.T, response string) (*ModelInvoker, *scriptedChatAPI) {
	t.Helper()
	api := &scriptedChatAPI{responses: []scriptedResponse{{text: response, requestID: "request-derive"}}}
	invoker, err := NewModelInvoker(api)
	if err != nil {
		t.Fatal(err)
	}
	return invoker, api
}

func TestDeriveTargetFilesSealsTheChoiceAndCompletesTheContract(t *testing.T) {
	config := validTestConfig()
	draft := deriveTestDraft(t)
	listing, err := ReadCandidateListing(deriveTestTree(t), strings.Repeat("a", 40), config.Consumers[0], config)
	if err != nil {
		t.Fatalf("ReadCandidateListing() error = %v", err)
	}
	invoker, api := newDeriveInvoker(t, `{"files":["client/src/components/Settings.tsx"],"rationale":"The settings screen renders this label."}`)
	derivation, _, err := invoker.DeriveTargetFiles(context.Background(), draft, listing, config)
	if err != nil {
		t.Fatalf("DeriveTargetFiles() error = %v", err)
	}
	if err := derivation.Validate(draft, listing, config); err != nil {
		t.Fatalf("derivation does not validate: %v", err)
	}
	if len(derivation.TargetFiles) != 1 || derivation.TargetFiles[0] != "client/src/components/Settings.tsx" {
		t.Fatalf("target files = %v", derivation.TargetFiles)
	}
	if derivation.ListingSHA256 != listing.ListingSHA256 || derivation.DeliveryID != draft.DeliveryID {
		t.Fatal("derivation is not bound to the draft and the listing it was shown")
	}

	// The model must see the requester's words and the offered paths, and
	// nothing outside the writable scope.
	if len(api.prompts) != 1 {
		t.Fatalf("captured prompts = %d", len(api.prompts))
	}
	for _, required := range []string{"USER_DATA_JSON", "Updated label", "Old label", "/settings", "client/src/components/Settings.tsx"} {
		if !strings.Contains(api.prompts[0], required) {
			t.Fatalf("prompt lacks %q:\n%s", required, api.prompts[0])
		}
	}
	if strings.Contains(api.prompts[0], "server/main.go") {
		t.Fatal("the prompt offered a path outside the writable scope")
	}

	// The completed contract is the same shape a hand-written ticket produces.
	request, err := draft.WithTargetFiles(derivation.TargetFiles, config)
	if err != nil {
		t.Fatalf("WithTargetFiles() error = %v", err)
	}
	if err := request.Validate(config); err != nil {
		t.Fatalf("completed contract is invalid: %v", err)
	}
}

func TestDeriveTargetFilesRefusesAnythingNotOffered(t *testing.T) {
	config := validTestConfig()
	draft := deriveTestDraft(t)
	listing, err := ReadCandidateListing(deriveTestTree(t), strings.Repeat("a", 40), config.Consumers[0], config)
	if err != nil {
		t.Fatalf("ReadCandidateListing() error = %v", err)
	}
	for _, run := range []struct {
		name     string
		response string
	}{
		{name: "path outside the writable scope", response: `{"files":["server/main.go"],"rationale":"x"}`},
		{name: "path that does not exist", response: `{"files":["client/src/components/Ghost.tsx"],"rationale":"x"}`},
		{name: "traversal", response: `{"files":["client/src/../../etc/passwd"],"rationale":"x"}`},
		{name: "absolute path", response: `{"files":["/etc/passwd"],"rationale":"x"}`},
		{name: "no files", response: `{"files":[],"rationale":"x"}`},
		{name: "duplicated", response: `{"files":["client/src/index.tsx","client/src/index.tsx"],"rationale":"x"}`},
		{name: "beyond the file budget", response: `{"files":["client/src/a.tsx","client/src/b.tsx","client/src/c.tsx","client/src/d.tsx"],"rationale":"x"}`},
		{name: "unknown field", response: `{"files":["client/src/index.tsx"],"rationale":"x","command":"rm -rf /"}`},
	} {
		t.Run(run.name, func(t *testing.T) {
			invoker, _ := newDeriveInvoker(t, run.response)
			if _, _, err := invoker.DeriveTargetFiles(context.Background(), draft, listing, config); err == nil {
				t.Fatal("an unoffered choice was accepted")
			}
		})
	}
}

func TestDerivationDigestDetectsTampering(t *testing.T) {
	config := validTestConfig()
	draft := deriveTestDraft(t)
	listing, err := ReadCandidateListing(deriveTestTree(t), strings.Repeat("a", 40), config.Consumers[0], config)
	if err != nil {
		t.Fatalf("ReadCandidateListing() error = %v", err)
	}
	invoker, _ := newDeriveInvoker(t, `{"files":["client/src/index.tsx"],"rationale":"root entry renders it"}`)
	derivation, _, err := invoker.DeriveTargetFiles(context.Background(), draft, listing, config)
	if err != nil {
		t.Fatalf("DeriveTargetFiles() error = %v", err)
	}
	swapped := derivation
	swapped.TargetFiles = []string{"client/src/components/Example.tsx"}
	if err := swapped.Validate(draft, listing, config); err == nil {
		t.Fatal("a swapped file survived the digest")
	}
	foreign := derivation
	foreign.DeliveryID = "delivery_" + strings.Repeat("0", 32)
	if err := foreign.Validate(draft, listing, config); err == nil {
		t.Fatal("a derivation for another ticket was accepted")
	}
}

func TestCandidateListingFailsClosedOnABrokenTree(t *testing.T) {
	config := validTestConfig()
	if _, err := ReadCandidateListing(t.TempDir(), strings.Repeat("a", 40), config.Consumers[0], config); err == nil {
		t.Fatal("an empty tree produced a listing")
	}
	if _, err := ReadCandidateListing(filepath.Join(t.TempDir(), "missing"), strings.Repeat("a", 40), config.Consumers[0], config); err == nil {
		t.Fatal("a missing root produced a listing")
	}
	root := deriveTestTree(t)
	link := filepath.Join(root, "client", "src", "linked.tsx")
	if err := os.Symlink(filepath.Join(root, "server", "main.go"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	listing, err := ReadCandidateListing(root, strings.Repeat("a", 40), config.Consumers[0], config)
	if err != nil {
		t.Fatalf("ReadCandidateListing() error = %v", err)
	}
	for _, path := range listing.Paths {
		if strings.HasSuffix(path, "linked.tsx") {
			t.Fatal("a symlink was offered as a candidate")
		}
	}
}

// A listing reaches the derivation as a file, so its contents are only as
// trustworthy as whoever wrote them. Every offered path must therefore be
// re-checked against the mode, not merely against the listing's own digest.
func TestSuppliedListingCannotWidenTheWritableScope(t *testing.T) {
	config := validTestConfig()
	draft := deriveTestDraft(t)
	honest, err := ReadCandidateListing(deriveTestTree(t), strings.Repeat("a", 40), config.Consumers[0], config)
	if err != nil {
		t.Fatalf("ReadCandidateListing() error = %v", err)
	}
	for _, run := range []struct {
		name  string
		paths []string
	}{
		{name: "outside the allowed prefix", paths: []string{"client/src/index.tsx", "server/main.go"}},
		{name: "repository machinery", paths: []string{"client/src/.git/config", "client/src/index.tsx"}},
		{name: "secrets", paths: []string{"client/src/.env", "client/src/index.tsx"}},
		{name: "traversal", paths: []string{"client/src/../../etc/passwd"}},
	} {
		t.Run(run.name, func(t *testing.T) {
			forged := CandidateListing{SchemaVersion: honest.SchemaVersion, BaseSHA: honest.BaseSHA}
			forged.Paths = append([]string(nil), run.paths...)
			sort.Strings(forged.Paths)
			// Reseal so only the scope check can reject it: a digest mismatch
			// would hide whether the scope is actually enforced.
			unsealed := forged
			digest, err := sealedDigest(unsealed)
			if err != nil {
				t.Fatal(err)
			}
			forged.ListingSHA256 = digest
			if err := forged.Validate(config.Consumers[0], config); err == nil {
				t.Fatalf("a resealed listing widened the scope to %v", forged.Paths)
			}
			invoker, _ := newDeriveInvoker(t, `{"files":["`+run.paths[0]+`"],"rationale":"x"}`)
			if _, _, err := invoker.DeriveTargetFiles(context.Background(), draft, forged, config); err == nil {
				t.Fatal("a derivation ran against a widened listing")
			}
		})
	}
}

// Repository machinery and secrets under the writable prefix must be invisible
// to both the file choice and the wording search.
func TestHiddenFilesAreNeitherOfferedNorSearched(t *testing.T) {
	config := validTestConfig()
	root := t.TempDir()
	for name, content := range map[string]string{
		"client/src/index.tsx":   "export const heading = 'Old label';\n",
		"client/src/.env":        "SECRET=Old label\n",
		"client/src/.git/config": "[core]\n\thooksPath = Old label\n",
	} {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	listing, err := ReadCandidateListing(root, strings.Repeat("a", 40), config.Consumers[0], config)
	if err != nil {
		t.Fatalf("ReadCandidateListing() error = %v", err)
	}
	if len(listing.Paths) != 1 || listing.Paths[0] != "client/src/index.tsx" {
		t.Fatalf("paths = %v, want only the ordinary file", listing.Paths)
	}
	location, err := LocateTargetFiles(root, deriveTestDraft(t), config)
	if err != nil {
		t.Fatalf("LocateTargetFiles() error = %v", err)
	}
	if len(location.Matches) != 1 || location.Matches[0] != "client/src/index.tsx" {
		t.Fatalf("matches = %v, want the search to skip dotted paths", location.Matches)
	}
}
