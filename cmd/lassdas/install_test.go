package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/initwizard"
	"automation.internal/ticket-ingress/internal/localrun"
)

// One install puts the instruction, the distributor's note and the skill
// where the person's development AI finds them, with the home substituted
// into the skill; the note is read back and fills the four answers.
func TestInstallFilesPlaceTheInstructionTheNoteAndTheSkill(t *testing.T) {
	home := t.TempDir()
	engine := t.TempDir()
	if err := os.MkdirAll(filepath.Join(engine, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(engine, "docs", "SETUP.md"), []byte("# 導入\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	skills := filepath.Join(home, ".claude", "skills")
	note := initwizard.Distribution{EngineRepository: "example/engine", Image: "registry.example/engine@sha256:" + strings.Repeat("a", 64), EngineSHA: strings.Repeat("b", 40), BuildRecord: "https://example/build/1", RegistryLogin: "docker login registry.example", CLI: filepath.Join(home, ".lassdas", "bin", "lassdas")}
	if err := installFiles(home, engine, skills, note); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(filepath.Join(home, ".lassdas", "SETUP.md")); err != nil || string(raw) != "# 導入\n" {
		t.Fatalf("instruction: %q %v", raw, err)
	}
	skill, err := os.ReadFile(filepath.Join(skills, "lassdas-setup", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(skill)
	for _, want := range []string{"name: lassdas-setup", "description:", filepath.Join(home, ".lassdas", "SETUP.md"), filepath.Join(home, ".lassdas", "bin", "lassdas"), filepath.Join(home, ".lassdas", "distribution.json"), "鍵の値を読まない"} {
		if !strings.Contains(text, want) {
			t.Fatalf("skill lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "{{HOME}}") {
		t.Fatal("the home must be substituted")
	}
	loaded, found, err := initwizard.LoadDistribution(home)
	if err != nil || !found || loaded.Image != note.Image || loaded.RegistryLogin != note.RegistryLogin {
		t.Fatalf("note: %+v %v %v", loaded, found, err)
	}
	// The note answers the four questions the file leaves out; the file
	// wins where it answers.
	answers := initwizard.Answers{}
	if err := os.MkdirAll(filepath.Join(engine, ".lassdas"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(engine, ".lassdas", "setup.json"), []byte(`{"answers":{"image":"other/engine@sha256:c"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	answers, err = initwizard.LoadAnswers(engine)
	if err != nil {
		t.Fatal(err)
	}
	merged := answers.WithDistribution(loaded)
	if v, _ := merged.Value("engine-sha"); v != note.EngineSHA {
		t.Fatalf("engine-sha from the note: %q", v)
	}
	if v, _ := merged.Value("image"); !strings.HasPrefix(v, "other/engine") {
		t.Fatalf("the file's own answer wins: %q", v)
	}
	if v, _ := merged.Value("build-record"); v != "https://example/build/1" {
		t.Fatalf("build-record: %q", v)
	}
}

// A note with a tag instead of a digest, or a short sha, is refused before
// anything is written.
func TestInstallRefusesAnIncompleteNote(t *testing.T) {
	for name, note := range map[string]initwizard.Distribution{
		"tag":       {EngineRepository: "e/a", Image: "registry/engine:latest", EngineSHA: strings.Repeat("a", 40), BuildRecord: "u"},
		"short sha": {EngineRepository: "e/a", Image: "registry/engine@sha256:x", EngineSHA: "abc", BuildRecord: "u"},
		"no record": {EngineRepository: "e/a", Image: "registry/engine@sha256:x", EngineSHA: strings.Repeat("a", 40)},
		"bad repo":  {EngineRepository: "engine", Image: "registry/engine@sha256:x", EngineSHA: strings.Repeat("a", 40), BuildRecord: "u"},
	} {
		if err := note.Validate(); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
	engine := t.TempDir()
	var out bytes.Buffer
	if err := setupInstall(context.Background(), engine, t.TempDir(), installOptions{skillsDir: t.TempDir()}, &out); err == nil || !strings.Contains(err.Error(), "本体 repo の中で") {
		t.Fatalf("outside the engine repo: %v", err)
	}
}

// `setup check` fills the four answers from the note, so a file without
// them passes that part.
func TestSetupCheckTakesTheDistributorsNote(t *testing.T) {
	root := gitRepo(t)
	home := t.TempDir()
	note := initwizard.Distribution{EngineRepository: "example/engine", Image: "registry.example/engine@sha256:" + strings.Repeat("a", 64), EngineSHA: strings.Repeat("b", 40), BuildRecord: "https://example/build/1"}
	if err := initwizard.WriteDistribution(home, note); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".lassdas"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".lassdas", "setup.json"), []byte(`{"answers":{"repository":"example/app"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	_ = runSetup(context.Background(), "setup check", "", root, home, "", localrun.Manager{}, &out)
	for _, id := range []string{"image", "engine-sha", "build-record", "engine-repository"} {
		if strings.Contains(out.String(), "回答がありません: "+id) {
			t.Fatalf("%s should come from the note: %s", id, out.String())
		}
	}
	if !strings.Contains(out.String(), "回答がありません: branch") {
		t.Fatalf("other gaps are still named: %s", out.String())
	}
}
