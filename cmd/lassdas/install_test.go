package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
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
	for _, name := range installedDocs {
		if err := os.WriteFile(filepath.Join(engine, "docs", name), []byte("# "+name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	skills := filepath.Join(home, ".claude", "skills")
	note := initwizard.Distribution{EngineRepository: "example/engine", Image: "registry.example/engine@sha256:" + strings.Repeat("a", 64), EngineSHA: strings.Repeat("b", 40), BuildRecord: "https://example/build/1", RegistryLogin: "docker login registry.example", CLI: filepath.Join(home, ".lassdas", "bin", "lassdas")}
	if err := installFiles(home, engine, skills, note); err != nil {
		t.Fatal(err)
	}
	for _, name := range installedDocs {
		if raw, err := os.ReadFile(filepath.Join(home, ".lassdas", name)); err != nil || string(raw) != "# "+name+"\n" {
			t.Fatalf("installed %s: %q %v", name, raw, err)
		}
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

// A note with a tag instead of a digest, a short or uppercase digest, a
// short sha, no build record, a bad repository or a login carrying a
// password is refused before anything is written; the sha is never taken
// from the checkout's HEAD.
func TestInstallRefusesAnIncompleteNote(t *testing.T) {
	digest := "registry/engine@sha256:" + strings.Repeat("a", 64)
	for name, note := range map[string]initwizard.Distribution{
		"tag":            {EngineRepository: "e/a", Image: "registry/engine:latest", EngineSHA: strings.Repeat("a", 40), BuildRecord: "u"},
		"short digest":   {EngineRepository: "e/a", Image: "registry/engine@sha256:" + strings.Repeat("a", 63), EngineSHA: strings.Repeat("a", 40), BuildRecord: "u"},
		"upper digest":   {EngineRepository: "e/a", Image: "registry/engine@sha256:" + strings.Repeat("A", 64), EngineSHA: strings.Repeat("a", 40), BuildRecord: "u"},
		"short sha":      {EngineRepository: "e/a", Image: digest, EngineSHA: "abc", BuildRecord: "u"},
		"no record":      {EngineRepository: "e/a", Image: digest, EngineSHA: strings.Repeat("a", 40)},
		"bad repo":       {EngineRepository: "engine", Image: digest, EngineSHA: strings.Repeat("a", 40), BuildRecord: "u"},
		"password login": {EngineRepository: "e/a", Image: digest, EngineSHA: strings.Repeat("a", 40), BuildRecord: "u", RegistryLogin: "docker login -u me -p hunter2 registry"},
	} {
		if err := note.Validate(); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
	if err := (initwizard.Distribution{EngineRepository: "e/a", Image: digest, EngineSHA: strings.Repeat("a", 40), BuildRecord: "u", RegistryLogin: "aws ecr get-login-password | docker login --username AWS --password-stdin registry"}).Validate(); err != nil {
		t.Fatalf("a stdin login is fine: %v", err)
	}
	engine := t.TempDir()
	var out bytes.Buffer
	if err := setupInstall(context.Background(), engine, t.TempDir(), installOptions{skillsDir: t.TempDir()}, &out); err == nil || !strings.Contains(err.Error(), "本体 repo の中で") {
		t.Fatalf("outside the engine repo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(engine, "go.mod"), []byte("module automation.internal/ticket-ingress\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setupInstall(context.Background(), engine, t.TempDir(), installOptions{image: digest, buildRecord: "u", engineRepository: "e/a", skillsDir: t.TempDir()}, &out); err == nil || !strings.Contains(err.Error(), "--engine-sha が必要") {
		t.Fatalf("engine-sha must be given: %v", err)
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

// `setup apply` reads the note too: a file that leaves the four answers
// out is not "回答が足りません" - the run gets as far as the first thing
// only a person gives (the GitHub token), before any network call.
func TestSetupApplyTakesTheDistributorsNote(t *testing.T) {
	for _, tool := range []string{"git", "go", "docker"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not available; the wizard requires it before it starts", tool)
		}
	}
	root := gitRepo(t)
	home := t.TempDir()
	note := initwizard.Distribution{EngineRepository: "example/engine", Image: "registry.example/engine@sha256:" + strings.Repeat("a", 64), EngineSHA: strings.Repeat("b", 40), BuildRecord: "https://example/build/1"}
	if err := initwizard.WriteDistribution(home, note); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".lassdas"), 0o755); err != nil {
		t.Fatal(err)
	}
	var fields []string
	models := map[string]string{"implementer-model": "deepseek/a", "review-a-model": "anthropic/b", "review-b-model": "openai/c", "readiness-assessor-model": "google/d", "readiness-checker-model": "openai/e", "designer-model": "anthropic/f", "applier-model": "deepseek/g", "tracker-origin": "https://example.backlog.com", "creator-id": "7"}
	for _, requirement := range initwizard.RequiredAnswers() {
		switch requirement.ID {
		case "image", "engine-sha", "build-record", "engine-repository":
			continue
		}
		value := "x"
		if m, ok := models[requirement.ID]; ok {
			value = m
		}
		fields = append(fields, `"`+requirement.ID+`":"`+value+`"`)
	}
	// The temporary repository has no go.mod or package.json, so the four
	// proposals the wizard cannot make are written too.
	fields = append(fields, `"scope":["docs/"]`, `"toolchain":[]`, `"install":[]`, `"verify":[["true"]]`)
	if err := os.WriteFile(filepath.Join(root, ".lassdas", "setup.json"), []byte(`{"answers":{`+strings.Join(fields, ",")+`}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := runSetup(context.Background(), "setup apply", "sample", root, home, "", localrun.Manager{}, &out)
	var secret *initwizard.MissingSecret
	if !errors.As(err, &secret) || secret.Name != "TARGET_GITHUB_TOKEN" {
		t.Fatalf("apply should reach the person's first turn, got: %v", err)
	}
}
