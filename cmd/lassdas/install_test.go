package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	// Every Markdown document beside the instruction travels with it (a
	// directory and a non-Markdown file do not).
	linked := []string{"SETUP.md", "PRODUCT_DIRECTION.md", "INIT_DECISIONS.md", "RUNTIME_POD.md"}
	for _, name := range linked {
		if err := os.WriteFile(filepath.Join(engine, "docs", name), []byte("# "+name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(engine, "docs", "mockups"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(engine, "docs", "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	skills := filepath.Join(home, ".claude", "skills")
	note := initwizard.Distribution{EngineRepository: "example/engine", Image: "registry.example/engine@sha256:" + strings.Repeat("a", 64), EngineSHA: strings.Repeat("b", 40), BuildRecord: "https://example/build/1", RegistryLogin: "docker login registry.example", CLI: filepath.Join(home, ".lassdas", "bin", "lassdas")}
	if err := installFiles(home, engine, skills, note); err != nil {
		t.Fatal(err)
	}
	for _, name := range linked {
		if raw, err := os.ReadFile(filepath.Join(home, ".lassdas", name)); err != nil || string(raw) != "# "+name+"\n" {
			t.Fatalf("installed %s: %q %v", name, raw, err)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".lassdas", "notes.txt")); !os.IsNotExist(err) {
		t.Fatal("only Markdown documents are installed")
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
	if err := setupInstall(context.Background(), engine, t.TempDir(), installOptions{image: digest, buildRecord: "u", engineRepository: "e/a", skillsDir: t.TempDir()}, &out); err == nil || !strings.Contains(err.Error(), "engine-sha") {
		t.Fatalf("engine-sha must be given when the checkout has no note: %v", err)
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

// Every link between the body's own documents resolves after install:
// the set installed is the whole docs directory, so a new link never
// dangles where the AI reads it.
func TestInstalledDocumentsLinkOnlyToEachOther(t *testing.T) {
	names, err := installedDocs("../..")
	if err != nil {
		t.Fatal(err)
	}
	present := map[string]bool{}
	for _, name := range names {
		present[name] = true
	}
	link := regexp.MustCompile(`\]\(([A-Za-z0-9_.-]+\.md)(?:#[^)]*)?\)`)
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join("../../docs", name))
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range link.FindAllStringSubmatch(string(raw), -1) {
			if !present[match[1]] {
				t.Errorf("%s links to %s, which is not installed beside it", name, match[1])
			}
		}
	}
}

// The checkout's own docs/DISTRIBUTION.json is the note an install uses
// with no arguments; a flag overrides its field; a missing note with no
// flags says where the note is expected. `setup note` writes that file.
func TestInstallReadsTheRepositorysNoteAndFlagsOverrideIt(t *testing.T) {
	engine := t.TempDir()
	digest := "registry/engine@sha256:" + strings.Repeat("a", 64)
	var out bytes.Buffer
	// The note is never written into a repository that is not the body's.
	if err := setupNote(context.Background(), engine, installOptions{engineRepository: "e/a", image: digest, engineSHA: strings.Repeat("b", 40), buildRecord: "u"}, &out); err == nil || !strings.Contains(err.Error(), "本体 repo の中で") {
		t.Fatalf("setup note outside the body: %v", err)
	}
	if err := os.WriteFile(filepath.Join(engine, "go.mod"), []byte("module automation.internal/ticket-ingress\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setupNote(context.Background(), engine, installOptions{engineRepository: "e/a", image: digest, engineSHA: strings.Repeat("b", 40), buildRecord: "https://example/build/1", registryLogin: "docker login --password-stdin registry"}, &out); err != nil {
		t.Fatal(err)
	}
	// A release rewrites image, sha and record and keeps the login it was
	// not given.
	next := "registry/engine@sha256:" + strings.Repeat("d", 64)
	if err := setupNote(context.Background(), engine, installOptions{image: next, engineSHA: strings.Repeat("e", 40), buildRecord: "https://example/build/2"}, &out); err != nil {
		t.Fatal(err)
	}
	if kept, err := initwizard.ReadDistributionFile(filepath.Join(engine, "docs", "DISTRIBUTION.json")); err != nil || kept.Image != next || kept.RegistryLogin != "docker login --password-stdin registry" || kept.EngineRepository != "e/a" {
		t.Fatalf("a later note keeps the login and the repository: %+v %v", kept, err)
	}
	if err := setupNote(context.Background(), engine, installOptions{image: digest, engineSHA: strings.Repeat("b", 40), buildRecord: "https://example/build/1"}, &out); err != nil {
		t.Fatal(err)
	}
	// Written elsewhere, it still starts from the checkout's note.
	elsewhere := filepath.Join(t.TempDir(), "note.json")
	if err := setupNote(context.Background(), engine, installOptions{out: elsewhere, image: next, engineSHA: strings.Repeat("e", 40), buildRecord: "https://example/build/2"}, &out); err != nil {
		t.Fatal(err)
	}
	if copied, err := initwizard.ReadDistributionFile(elsewhere); err != nil || copied.RegistryLogin != "docker login --password-stdin registry" {
		t.Fatalf("--out keeps the checkout's login: %+v %v", copied, err)
	}
	// A note that exists but cannot be read is not replaced silently.
	if err := os.WriteFile(filepath.Join(engine, "docs", "DISTRIBUTION.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setupNote(context.Background(), engine, installOptions{image: digest, engineSHA: strings.Repeat("b", 40), buildRecord: "u"}, &out); err == nil || !strings.Contains(err.Error(), "上書きしません") {
		t.Fatalf("a broken note must not be overwritten silently: %v", err)
	}
	if _, err := noteFor(engine, installOptions{image: digest}); err == nil || !strings.Contains(err.Error(), "読めません") {
		t.Fatalf("install with a broken note and a flag must name the file: %v", err)
	}
	if whole, err := noteFor(engine, installOptions{image: digest, engineSHA: strings.Repeat("b", 40), buildRecord: "u", engineRepository: "e/a"}); err != nil || whole.Image != digest {
		t.Fatalf("all four flags carry a whole note past a broken file: %+v %v", whole, err)
	}
	if err := setupNote(context.Background(), engine, installOptions{engineRepository: "e/a", image: digest, engineSHA: strings.Repeat("b", 40), buildRecord: "https://example/build/1", registryLogin: "docker login --password-stdin registry"}, &out); err == nil {
		t.Fatal("still broken: the person removes it first")
	}
	if err := os.Remove(filepath.Join(engine, "docs", "DISTRIBUTION.json")); err != nil {
		t.Fatal(err)
	}
	if err := setupNote(context.Background(), engine, installOptions{engineRepository: "e/a", image: digest, engineSHA: strings.Repeat("b", 40), buildRecord: "https://example/build/1", registryLogin: "docker login --password-stdin registry"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "docs/DISTRIBUTION.json") {
		t.Fatalf("note output: %q", out.String())
	}
	note, err := noteFor(engine, installOptions{})
	if err != nil || note.Image != digest || note.EngineSHA != strings.Repeat("b", 40) || note.RegistryLogin != "docker login --password-stdin registry" {
		t.Fatalf("note from the checkout: %+v %v", note, err)
	}
	other := "registry/engine@sha256:" + strings.Repeat("c", 64)
	note, err = noteFor(engine, installOptions{image: other})
	if err != nil || note.Image != other || note.EngineSHA != strings.Repeat("b", 40) {
		t.Fatalf("a flag overrides one field: %+v %v", note, err)
	}
	if _, err := noteFor(t.TempDir(), installOptions{}); err == nil || !strings.Contains(err.Error(), "DISTRIBUTION.json") {
		t.Fatalf("no note and no flags must name the file: %v", err)
	}
	if _, err := noteFor(t.TempDir(), installOptions{note: filepath.Join(t.TempDir(), "missing.json")}); err == nil {
		t.Fatal("an explicit --note that cannot be read is an error")
	}
	if err := setupNote(context.Background(), engine, installOptions{engineRepository: "e/a", image: "registry/engine:latest", engineSHA: strings.Repeat("b", 40), buildRecord: "u"}, &out); err == nil {
		t.Fatal("a tag is refused by setup note too")
	}
	// One bad field in the checkout's note is corrected by its flag: the
	// rest of the file still counts.
	broken := filepath.Join(engine, "docs", "DISTRIBUTION.json")
	if err := os.WriteFile(broken, []byte(`{"engine_repository":"e/a","image":"registry/engine:latest","engine_sha":"`+strings.Repeat("b", 40)+`","build_record":"u","registry_login":"docker login --password-stdin registry"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	fixed, err := noteFor(engine, installOptions{image: digest})
	if err != nil || fixed.Image != digest || fixed.EngineSHA != strings.Repeat("b", 40) || fixed.RegistryLogin == "" {
		t.Fatalf("a flag corrects one field of a broken note: %+v %v", fixed, err)
	}
	if err := fixed.Validate(); err != nil {
		t.Fatalf("the corrected note is whole: %v", err)
	}
}

// The README's build step leaves a binary the tree must ignore, and the
// clone step says what to do when the directory exists.
func TestTheReadmeStepsLeaveNothingBehind(t *testing.T) {
	ignore, err := os.ReadFile("../../.gitignore")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ignore), "\n/lassdas\n") {
		t.Fatal(".gitignore must ignore the CLI the README builds")
	}
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), "git -C /tmp/lassdas-src pull") {
		t.Fatal("the README must say what to do when the clone exists")
	}
}

// The note the repository ships is valid, and the README leads an AI
// handed only the URL to it.
func TestTheRepositorysNoteAndReadmeLeadTheWay(t *testing.T) {
	note, err := initwizard.ReadDistributionFile("../../docs/DISTRIBUTION.json")
	if err != nil {
		t.Fatal(err)
	}
	if note.EngineRepository == "" || !strings.Contains(note.Image, "@sha256:") {
		t.Fatalf("note: %+v", note)
	}
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(readme)
	for _, want := range []string{"URL だけを渡された開発 AI へ", "をセットアップして", "go build -o lassdas ./cmd/lassdas", "./lassdas setup install", "docs/DISTRIBUTION.json", "~/.lassdas/SETUP.md", "registry_login"} {
		if !strings.Contains(text, want) {
			t.Errorf("README lacks %q", want)
		}
	}
	if strings.Index(text, "URL だけを渡された開発 AI へ") > strings.Index(text, "## 理念") {
		t.Error("the entry for the AI must come before everything else")
	}
}
