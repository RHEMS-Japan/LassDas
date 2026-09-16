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
	engineRepository, image, engineSHA, buildRecord, registryLogin, skillsDir string
}

// setupInstall is run once per machine, from a checkout of the body's
// repository: it builds the CLI under ~/.lassdas/bin, copies the setup
// instruction, stores the distributor's note, and installs the skill the
// person's development AI picks up when asked to set LassDas up - so
// "LassDas をこのプロジェクトに導入して" is the whole request.
func setupInstall(ctx context.Context, engineRoot, home string, options installOptions, output io.Writer) error {
	module, err := os.ReadFile(filepath.Join(engineRoot, "go.mod"))
	if err != nil || !strings.HasPrefix(string(module), "module automation.internal/ticket-ingress\n") {
		return errors.New("本体 repo の中で実行してください (go.mod が見つからないか、別の module です)")
	}
	if options.engineRepository == "" {
		options.engineRepository = originRepository(ctx, engineRoot)
	}
	// The source sha is the distributor's word, taken from the build
	// record, never this checkout's HEAD: what runs is the image, and the
	// checkout may be newer or older than what it was built from.
	if options.engineSHA == "" {
		return errors.New("--engine-sha が必要です: そのイメージを作った本体ソースの 40 桁 SHA を、ビルド記録から写してください (手元の checkout の HEAD とは限りません)")
	}
	base := filepath.Join(home, ".lassdas")
	cli := filepath.Join(base, "bin", "lassdas")
	distribution := initwizard.Distribution{EngineRepository: options.engineRepository, Image: options.image, EngineSHA: options.engineSHA,
		BuildRecord: options.buildRecord, RegistryLogin: options.registryLogin, CLI: cli, InstalledAt: time.Now().UTC()}
	if err := distribution.Validate(); err != nil {
		return fmt.Errorf("配布者の案内が足りません: %v (--image / --engine-sha / --build-record / --engine-repository)", err)
	}
	previous, hadPrevious, _ := initwizard.LoadDistribution(home)
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
	if hadPrevious && (previous.Image != distribution.Image || previous.EngineSHA != distribution.EngineSHA) {
		_, _ = fmt.Fprintf(output, "配布者の案内を置き換えました (前: image %s / engine-sha %s)\n", previous.Image, previous.EngineSHA)
	}
	_, err = fmt.Fprintf(output, "入れました:\n- CLI: %s (この checkout %s から組み立て)\n- 導入指示と参照文書: %s\n- 配布者の案内: %s (image %s / engine-sha %s)\n- skill: %s\n\n以後は、どの repo でも新しい会話で「LassDas をこのプロジェクトに導入して」と頼むだけで始まります。\n",
		cli, headSHA(ctx, engineRoot), filepath.Join(base, "SETUP.md"), filepath.Join(home, filepath.FromSlash(initwizard.DistributionFile)), distribution.Image, distribution.EngineSHA, skill)
	return err
}

// installFiles places what the skill points at: the instruction, the
// note, and the skill itself (paths substituted). Separate from the build
// so it can be tested without a toolchain.
func installFiles(home, engineRoot, skillsDir string, distribution initwizard.Distribution) error {
	base := filepath.Join(home, ".lassdas")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return err
	}
	// The instruction and the documents it links to, side by side, so
	// its relative links still resolve where the AI reads it.
	for _, name := range installedDocs {
		content, err := os.ReadFile(filepath.Join(engineRoot, "docs", name))
		if err != nil {
			return fmt.Errorf("docs/%s が見つかりません (本体 repo の checkout が古いか、場所が違います)", name)
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

// installedDocs are the instruction and every document it links to.
var installedDocs = []string{"SETUP.md", "PRODUCT_DIRECTION.md", "INIT_DECISIONS.md", "RUNTIME_POD.md"}

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
