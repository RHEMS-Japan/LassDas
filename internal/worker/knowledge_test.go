package worker

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func knowledgeSource(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeAgentFile(t, root, "knowledge/rules.md", "# 規範\n\n憶測で実装するな。\n")
	writeAgentFile(t, root, "knowledge/library/past-decision.md", "EFS は片道移行。戻すな。\n")
	writeAgentFile(t, root, "knowledge/library/nested/another.md", "本番 DB は Aurora。\n")
	return root
}

func TestPlaceKnowledgePutsRulesWhereTheAgentLoadsThem(t *testing.T) {
	source := knowledgeSource(t)
	home := t.TempDir()
	workspace, _ := buildAgentRepository(t)

	placed, err := PlaceKnowledge(KnowledgeConfig{
		Rules: []KnowledgePlacement{{From: "knowledge/rules.md", To: ".claude/CLAUDE.md"}},
	}, source, home, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(placed) != 1 || placed[0] != ".claude/CLAUDE.md" {
		t.Fatalf("placed = %v", placed)
	}
	content, err := os.ReadFile(filepath.Join(home, ".claude", "CLAUDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "憶測で実装するな") {
		t.Fatalf("the rules were not placed: %q", content)
	}
}

// This is the measurement that makes the library safe: knowledge placed in the
// working copy must never be reported as something the agent changed, or every
// run would be thrown away for writing outside its scope.
func TestPlacedKnowledgeIsInvisibleToChangeDetection(t *testing.T) {
	source := knowledgeSource(t)
	home := t.TempDir()
	workspace, _ := buildAgentRepository(t)

	if _, err := PlaceKnowledge(KnowledgeConfig{
		Library: &KnowledgePlacement{From: "knowledge/library", To: "automation-knowledge"},
	}, source, home, workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "automation-knowledge", "past-decision.md")); err != nil {
		t.Fatalf("the library was not placed: %v", err)
	}
	changed, err := ChangedFilesUnder(workspace, []string{"client/src/"})
	if err != nil {
		t.Fatalf("placed knowledge was reported as a change: %v", err)
	}
	if len(changed) != 0 {
		t.Fatalf("placed knowledge was reported as a change: %v", changed)
	}
}

func TestPlaceKnowledgePlacesEveryFileInTheLibrary(t *testing.T) {
	source := knowledgeSource(t)
	workspace, _ := buildAgentRepository(t)

	placed, err := PlaceKnowledge(KnowledgeConfig{
		Library: &KnowledgePlacement{From: "knowledge/library", To: "automation-knowledge"},
	}, source, t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"automation-knowledge/nested/another.md", "automation-knowledge/past-decision.md"}
	if len(placed) != len(want) {
		t.Fatalf("placed = %v, want %v", placed, want)
	}
	for index, path := range want {
		if placed[index] != path {
			t.Fatalf("placed = %v, want %v", placed, want)
		}
	}
}

// Placing the same library twice is what a second stage does, and it must not
// keep appending to the exclusion.
func TestPlaceKnowledgeIsRepeatable(t *testing.T) {
	source := knowledgeSource(t)
	home := t.TempDir()
	workspace, _ := buildAgentRepository(t)
	knowledge := KnowledgeConfig{
		Rules:   []KnowledgePlacement{{From: "knowledge/rules.md", To: ".claude/CLAUDE.md"}},
		Library: &KnowledgePlacement{From: "knowledge/library", To: "automation-knowledge"},
	}
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := PlaceKnowledge(knowledge, source, home, workspace); err != nil {
			t.Fatalf("attempt %d: %v", attempt+1, err)
		}
	}
	exclude, err := os.ReadFile(filepath.Join(workspace, ".git", "info", "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(exclude), "/automation-knowledge/") != 1 {
		t.Fatalf("the exclusion was written more than once: %q", exclude)
	}
	changed, err := ChangedFilesUnder(workspace, []string{"client/src/"})
	if err != nil || len(changed) != 0 {
		t.Fatalf("changed = %v, err = %v", changed, err)
	}
}

// An existing exclusion must be kept: it is the destination's, not ours.
func TestPlaceKnowledgeKeepsAnExistingExclusion(t *testing.T) {
	source := knowledgeSource(t)
	workspace, _ := buildAgentRepository(t)
	exclude := filepath.Join(workspace, ".git", "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(exclude), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exclude, []byte("/scratch/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PlaceKnowledge(KnowledgeConfig{
		Library: &KnowledgePlacement{From: "knowledge/library", To: "automation-knowledge"},
	}, source, t.TempDir(), workspace); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(exclude)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "/scratch/") {
		t.Fatalf("an existing exclusion was lost: %q", content)
	}
}

func TestPlaceKnowledgeRejectsWhatItCannotPlace(t *testing.T) {
	source := knowledgeSource(t)
	home := t.TempDir()
	workspace, _ := buildAgentRepository(t)

	for name, knowledge := range map[string]KnowledgeConfig{
		"a rule that does not exist": {
			Rules: []KnowledgePlacement{{From: "knowledge/missing.md", To: ".claude/CLAUDE.md"}},
		},
		"a path that climbs out": {
			Rules: []KnowledgePlacement{{From: "../secrets.md", To: ".claude/CLAUDE.md"}},
		},
		"a destination that climbs out": {
			Rules: []KnowledgePlacement{{From: "knowledge/rules.md", To: "../../CLAUDE.md"}},
		},
		"two rules at the same place": {
			Rules: []KnowledgePlacement{
				{From: "knowledge/rules.md", To: ".claude/CLAUDE.md"},
				{From: "knowledge/rules.md", To: ".claude/CLAUDE.md"},
			},
		},
		"a library that does not exist": {
			Library: &KnowledgePlacement{From: "knowledge/absent", To: "automation-knowledge"},
		},
		"a hidden library the agent would not see": {
			Library: &KnowledgePlacement{From: "knowledge/library", To: ".hidden-knowledge"},
		},
	} {
		if _, err := PlaceKnowledge(knowledge, source, home, workspace); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

// A link in the library could point anywhere; following one would hand the
// agent something nobody configured.
func TestPlaceKnowledgeDoesNotFollowLinksOutOfTheLibrary(t *testing.T) {
	source := knowledgeSource(t)
	outside := filepath.Join(source, "not-knowledge.md")
	if err := os.WriteFile(outside, []byte("a credential\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(source, "knowledge", "library", "sneaky.md")); err != nil {
		t.Skipf("links are unavailable here: %v", err)
	}
	workspace, _ := buildAgentRepository(t)

	placed, err := PlaceKnowledge(KnowledgeConfig{
		Library: &KnowledgePlacement{From: "knowledge/library", To: "automation-knowledge"},
	}, source, t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range placed {
		if strings.HasSuffix(path, "sneaky.md") {
			t.Fatal("a link out of the library was followed")
		}
	}
	if _, err := os.Stat(filepath.Join(workspace, "automation-knowledge", "sneaky.md")); err == nil {
		t.Fatal("a link out of the library was placed")
	}
}

// TestShippedKnowledgeCarriesNoConfidentialCodename fixes, in CI, what the
// sync script checks at the boundary: the project codename must never travel
// into a destination working copy, where an agent could quote it into a pull
// request. The name is assembled here so this file cannot trip its own check.
func TestShippedKnowledgeCarriesNoConfidentialCodename(t *testing.T) {
	codename := strings.ToLower("Lass" + "Das")
	root := filepath.Join("..", "..", "knowledge")
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		content, err := os.ReadFile(name) // #nosec G304 -- repository fixture walk.
		if err != nil {
			return err
		}
		if strings.Contains(strings.ToLower(string(content)), codename) {
			t.Errorf("%s carries the confidential codename", name)
		}
		return nil
	})
	if err != nil {
		t.Skipf("no shipped knowledge to check: %v", err)
	}
}
