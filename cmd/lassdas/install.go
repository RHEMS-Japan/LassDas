package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/initwizard"
)

//go:embed skills/lassdas-setup/SKILL.md
var skillTemplate string

// installOptions is the distributor's note plus where the harness keeps
// its skills.
type installOptions struct {
	engineRepository, image, engineSHA, buildRecord, registryLogin, skillsDir, note, out string
}

// noteFor is the distributor's note an install uses: the checkout's own
// docs/DISTRIBUTION.json (or --note PATH), with any flag given on the
// command line overriding that field. A checkout without a note and no
// flags is told where the note is expected.
func noteFor(engineRoot string, options installOptions) (initwizard.Distribution, error) {
	var note initwizard.Distribution
	path := options.note
	if path == "" {
		path = filepath.Join(engineRoot, filepath.FromSlash(initwizard.RepoDistributionFile))
	}
	// Decoded, not judged: a flag may be the correction to one bad field,
	// and the whole is validated once the flags are laid over it.
	if read, err := initwizard.DecodeDistributionFile(path); err == nil {
		note = read
	} else if options.note != "" || (options.image == "" && options.engineSHA == "" && options.buildRecord == "") {
		return initwizard.Distribution{}, err
	} else if _, statErr := os.Stat(path); statErr == nil && (options.image == "" || options.engineSHA == "" || options.buildRecord == "" || options.engineRepository == "") {
		// The file is there but unreadable, and the flags do not carry a
		// whole note of their own: the message says why and the ways out.
		return initwizard.Distribution{}, fmt.Errorf("%v。直すか消すか、--note PATH で別の案内を渡すか、--image / --engine-sha / --build-record / --engine-repository の 4 つを全部渡してください", err)
	}
	if options.engineRepository != "" {
		note.EngineRepository = options.engineRepository
	}
	if options.image != "" {
		note.Image = options.image
	}
	if options.engineSHA != "" {
		note.EngineSHA = options.engineSHA
	}
	if options.buildRecord != "" {
		note.BuildRecord = options.buildRecord
	}
	if options.registryLogin != "" {
		note.RegistryLogin = options.registryLogin
	}
	return note, nil
}

// setupNote is the distributor's turn at a release: it writes the
// repository's note from the release's image, sha and build record,
// keeping what the previous note said where a flag says nothing (the
// registry login, the body repository). It runs only in the body's own
// repository: the note is never written into a delivery repository.
func setupNote(ctx context.Context, engineRoot string, options installOptions, output io.Writer) error {
	if err := bodyRepository(engineRoot); err != nil {
		return err
	}
	repoNote := filepath.Join(engineRoot, filepath.FromSlash(initwizard.RepoDistributionFile))
	out := options.out
	if out == "" {
		out = repoNote
	}
	// The starting point is always the checkout's note, so a release that
	// restates only the image, sha and record keeps the login and the
	// repository. A note that exists but cannot be read is not silently
	// replaced: it is named, and the person decides.
	var note initwizard.Distribution
	if _, err := os.Stat(repoNote); err == nil {
		decoded, err := initwizard.DecodeDistributionFile(repoNote)
		if err != nil {
			return fmt.Errorf("既存の案内を読めないので上書きしません: %v。直すか消してから再実行してください", err)
		}
		note = decoded
	}
	note.CLI, note.InstalledAt = "", time.Time{}
	if options.engineRepository != "" {
		note.EngineRepository = options.engineRepository
	}
	if note.EngineRepository == "" {
		note.EngineRepository = originRepository(ctx, engineRoot)
	}
	if options.image != "" {
		note.Image = options.image
	}
	if options.engineSHA != "" {
		note.EngineSHA = options.engineSHA
	}
	if options.buildRecord != "" {
		note.BuildRecord = options.buildRecord
	}
	if options.registryLogin != "" {
		note.RegistryLogin = options.registryLogin
	}
	if err := initwizard.WriteDistributionFile(out, note); err != nil {
		return fmt.Errorf("配布者の案内を書けません: %v (--image / --engine-sha / --build-record / --engine-repository)", err)
	}
	_, err := fmt.Fprintf(output, "配布者の案内を %s に書きました (image %s / engine-sha %s)。commit して push してください\n", out, note.Image, note.EngineSHA)
	return err
}

// bodyRepository refuses any checkout that is not the body's own module.
func bodyRepository(engineRoot string) error {
	module, err := os.ReadFile(filepath.Join(engineRoot, "go.mod"))
	if err != nil || !strings.HasPrefix(string(module), "module automation.internal/ticket-ingress\n") {
		return errors.New("本体 repo の中で実行してください (go.mod が見つからないか、別の module です)")
	}
	return nil
}

// setupInstall is run once per machine, from a checkout of the body's
// repository: it builds the CLI under ~/.lassdas/bin, copies the setup
// instruction, stores the distributor's note, and installs the skill the
// person's development AI picks up when asked to set LassDas up - so
// "LassDas をこのプロジェクトに導入して" is the whole request.
func setupInstall(ctx context.Context, engineRoot, home string, options installOptions, output io.Writer) error {
	if err := bodyRepository(engineRoot); err != nil {
		return err
	}
	// The note is the checkout's own docs/DISTRIBUTION.json unless flags
	// say otherwise. The source sha is the distributor's word, taken from
	// the build record, never this checkout's HEAD: what runs is the image,
	// and the checkout may be newer or older than what it was built from.
	distribution, err := noteFor(engineRoot, options)
	if err != nil {
		return err
	}
	if distribution.EngineRepository == "" {
		distribution.EngineRepository = originRepository(ctx, engineRoot)
	}
	base := filepath.Join(home, ".lassdas")
	cli := filepath.Join(base, "bin", "lassdas")
	distribution.CLI, distribution.InstalledAt = cli, time.Now().UTC()
	if err := distribution.Validate(); err != nil {
		return fmt.Errorf("配布者の案内が足りません: %v (repo の %s か、--image / --engine-sha / --build-record / --engine-repository)", err, initwizard.RepoDistributionFile)
	}
	previous, hadPrevious, _ := initwizard.LoadDistribution(home)
	if hadPrevious && (previous.Image != distribution.Image || previous.EngineSHA != distribution.EngineSHA) {
		// Said before anything is written, so a build that fails below
		// does not hide that the note changed.
		_, _ = fmt.Fprintf(output, "配布者の案内を置き換えます (前: image %s / engine-sha %s)\n", previous.Image, previous.EngineSHA)
	}
	// The instruction, the note and the skill first, the CLI last: a build
	// that fails leaves the pointers consistent, and a re-run repairs.
	if err := installFiles(home, engineRoot, options.skillsDir, distribution); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(cli), 0o700); err != nil {
		return err
	}
	build := exec.CommandContext(ctx, "go", "build", "-o", cli, "./cmd/lassdas")
	build.Dir = engineRoot
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("CLI を組み立てられませんでした: %v\n%s", err, strings.TrimSpace(string(out)))
	}
	skill := filepath.Join(options.skillsDir, "lassdas-setup", "SKILL.md")
	built := headSHA(ctx, engineRoot)
	if dirty(ctx, engineRoot) {
		built += " (未コミットの変更あり)"
	}
	_, err = fmt.Fprintf(output, "入れました:\n- CLI: %s (この checkout %s から組み立て)\n- 導入指示と参照文書: %s\n- 配布者の案内: %s (image %s / engine-sha %s)\n- skill: %s\n\n以後は、どの repo でも新しい会話で「LassDas をこのプロジェクトに導入して」と頼むだけで始まります。\n",
		cli, built, filepath.Join(base, "SETUP.md"), filepath.Join(home, filepath.FromSlash(initwizard.DistributionFile)), distribution.Image, distribution.EngineSHA, skill)
	return err
}

// dirty reports uncommitted changes in the checkout the CLI was built from.
func dirty(ctx context.Context, root string) bool {
	command := exec.CommandContext(ctx, "git", "status", "--porcelain")
	command.Dir = root
	out, err := command.Output()
	return err == nil && strings.TrimSpace(string(out)) != ""
}

// installFiles places what the skill points at: the instruction, the
// note, and the skill itself (paths substituted). Separate from the build
// so it can be tested without a toolchain.
func installFiles(home, engineRoot, skillsDir string, distribution initwizard.Distribution) error {
	base := filepath.Join(home, ".lassdas")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return err
	}
	// The instruction and every document beside it, so the links between
	// them still resolve where the AI reads them.
	names, err := installedDocs(engineRoot)
	if err != nil {
		return err
	}
	for _, name := range names {
		content, err := os.ReadFile(filepath.Join(engineRoot, "docs", name))
		if err != nil {
			return fmt.Errorf("docs/%s を読めません: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(base, name), content, 0o644); err != nil {
			return err
		}
	}
	if err := initwizard.WriteDistribution(home, distribution); err != nil {
		return err
	}
	skillDir := filepath.Join(skillsDir, "lassdas-setup")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(strings.ReplaceAll(skillTemplate, "{{HOME}}", home)), 0o644)
}

// installedDocs lists the documents installed beside the instruction:
// every Markdown file at the top of the body's docs directory, so no
// link between them is left dangling. The instruction itself must be
// among them.
func installedDocs(engineRoot string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(engineRoot, "docs"))
	if err != nil {
		return nil, errors.New("docs/ が見つかりません (本体 repo の checkout が古いか、場所が違います)")
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".md") {
			names = append(names, entry.Name())
		}
	}
	for _, name := range names {
		if name == "SETUP.md" {
			return names, nil
		}
	}
	return nil, errors.New("docs/SETUP.md が見つかりません (本体 repo の checkout が古いか、場所が違います)")
}

func originRepository(ctx context.Context, root string) string {
	command := exec.CommandContext(ctx, "git", "remote", "get-url", "origin")
	command.Dir = root
	out, err := command.Output()
	if err != nil {
		return ""
	}
	url := strings.TrimSpace(string(out))
	url = strings.TrimSuffix(url, ".git")
	for _, prefix := range []string{"git@github.com:", "https://github.com/", "ssh://git@github.com/"} {
		if strings.HasPrefix(url, prefix) {
			return strings.TrimPrefix(url, prefix)
		}
	}
	return ""
}

func headSHA(ctx context.Context, root string) string {
	command := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	command.Dir = root
	out, err := command.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
