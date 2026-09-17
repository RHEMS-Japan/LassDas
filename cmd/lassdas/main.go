package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"automation.internal/ticket-ingress/internal/initsmoke"
	"automation.internal/ticket-ingress/internal/initwizard"
	"automation.internal/ticket-ingress/internal/localrun"
)

const help = `使用方法:
  lassdas setup install [--note PATH] [--image IMAGE@sha256:…] [--engine-sha SHA] [--build-record URL] [--engine-repository OWNER/NAME] [--registry-login CMD] [--repo-root PATH] [--skills-dir DIR]
  lassdas setup note --image IMAGE@sha256:… --engine-sha SHA --build-record URL [--engine-repository OWNER/NAME] [--registry-login CMD | --public] [--repo-root PATH] [--out PATH]
  lassdas setup check [--repo-root PATH]
  lassdas setup secrets --project NAME [--repo-root PATH]
  lassdas setup apply --project NAME [--repo-root PATH] [--redo STAGE]
  lassdas setup smoke --project NAME [--repo-root PATH]
  lassdas init [--project NAME] [--repo-root PATH] [--redo STAGE]
  lassdas run start|stop|status|logs --project NAME
setup は、開発 AI が導入指示 (~/.lassdas/SETUP.md) に従って書いた .lassdas/setup.json から
導入を進めます。install は 1 台に 1 回、本体 repo の中で実行し、CLI・導入指示・
配布者の案内 (既定は repo の docs/DISTRIBUTION.json)・開発 AI の skill を利用者の
ホームに置きます。note は docs/DISTRIBUTION.json を書き直すもので、通常は main の
image workflow が公開イメージを作ったあとに自動で実行します。check は回答の
不足を示すだけで何も動かしません。secrets と smoke は利用者が実行します (鍵の
入力と、本人名義の試験依頼)。apply は AI が実行し、本体の起動まで進めます。

init は納品先 repo で実行します。外で取得した鍵を対話画面へ入力し、
設定・起動・本人名義の依頼から最初の PR の照合まで進めます。
project を省略すると保存名を聞きます。同じ project で再実行すると再開します。
--redo: prepare / consumer / tracker / models / runtime / smoke
stop は対象の本体だけを止め、台帳と作業記録を保持します。
`

func main() {
	// A closed reader on the progress output must not end the work. Piping
	// setup into `head` or a pager killed it partway through: the instance
	// was left stopped, and the shell reported the pager's success, so
	// nobody could tell (reported live 2026-09-17). Ignoring the signal
	// turns a closed pipe into a write error, and every progress line is
	// written best-effort already.
	signal.Ignore(syscall.SIGPIPE)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], quietWriter{os.Stdout}); err != nil {
		fmt.Fprintln(os.Stderr, "lassdas:", err)
		if errors.Is(err, initsmoke.ErrPending) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, output io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, err := io.WriteString(output, help)
		return err
	}
	command := args[0]
	rest := args[1:]
	if command == "setup" {
		if len(rest) == 0 {
			return errors.New("setup の操作名が必要です。lassdas --help を参照してください")
		}
		command = "setup " + rest[0]
		rest = rest[1:]
	}
	if command == "run" {
		if len(rest) == 0 {
			return errors.New("run の操作名が必要です。lassdas --help を参照してください")
		}
		command = "run " + rest[0]
		rest = rest[1:]
	}
	switch command {
	case "init", "run start", "run stop", "run status", "run logs", "setup install", "setup note", "setup check", "setup secrets", "setup apply", "setup smoke":
	default:
		return errors.New("コマンドが不明です。lassdas --help を参照してください")
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	project := flags.String("project", "", "")
	var repoRoot, redo string
	if command == "init" || strings.HasPrefix(command, "setup ") {
		flags.StringVar(&repoRoot, "repo-root", "", "")
	}
	if command == "init" || command == "setup apply" {
		flags.StringVar(&redo, "redo", "", "")
	}
	var install installOptions
	if command == "setup install" || command == "setup note" {
		flags.StringVar(&install.image, "image", "", "")
		flags.StringVar(&install.engineSHA, "engine-sha", "", "")
		flags.StringVar(&install.buildRecord, "build-record", "", "")
		flags.StringVar(&install.engineRepository, "engine-repository", "", "")
		flags.StringVar(&install.registryLogin, "registry-login", "", "")
	}
	if command == "setup install" {
		flags.StringVar(&install.skillsDir, "skills-dir", "", "")
		flags.StringVar(&install.note, "note", "", "")
	}
	if command == "setup note" {
		flags.StringVar(&install.out, "out", "", "")
		flags.BoolVar(&install.public, "public", false, "")
	}
	if err := flags.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, err = io.WriteString(output, help)
			return err
		}
		return errors.New("引数が不正です。lassdas --help を参照してください")
	}
	if flags.NArg() != 0 {
		return errors.New("余分な引数があります。lassdas --help を参照してください")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	manager := localrun.Manager{}
	if command == "setup note" {
		root, err := repositoryRoot(ctx, repoRoot)
		if err != nil {
			return errors.New("本体 repo の中で実行するか、--repo-root で本体 repo を指定してください")
		}
		return setupNote(ctx, root, install, output)
	}
	if command == "setup install" {
		if install.skillsDir == "" {
			configDir := os.Getenv("CLAUDE_CONFIG_DIR")
			if configDir == "" {
				configDir = filepath.Join(home, ".claude")
			}
			install.skillsDir = filepath.Join(configDir, "skills")
		}
		root, err := repositoryRoot(ctx, repoRoot)
		if err != nil {
			return errors.New("本体 repo の中で実行するか、--repo-root で本体 repo を指定してください")
		}
		return setupInstall(ctx, root, home, install, output)
	}
	if strings.HasPrefix(command, "setup ") {
		return runSetup(ctx, command, *project, repoRoot, home, redo, manager, output)
	}
	if command == "init" {
		ui := initwizard.TerminalUI{}
		if *project == "" {
			suggestedRoot := repoRoot
			if suggestedRoot == "" {
				suggestedRoot, _ = os.Getwd()
			}
			value, err := ui.Ask("project", "この本体の保存名 (英小文字・数字・ハイフン)", filepath.Base(suggestedRoot), false)
			if err != nil {
				return err
			}
			*project = value
		}
		dir, err := initwizard.ProjectDir(home, *project)
		if err != nil {
			return err
		}
		api := initwizard.API{}
		process := initwizard.ExecProcess{}
		smoke := initsmoke.Runner{UI: ui, API: api, Observer: initsmoke.RuntimeObserver{Manager: manager, Process: process, API: api, Dir: dir}}
		wizard := initwizard.Wizard{UI: ui, API: api, Process: process, Runtime: runtimeAdapter{manager}, Smoke: smoke.Run, RegistryLogin: noteRegistryLogin(home)}
		_, err = wizard.Run(ctx, initwizard.Options{Project: *project, Home: home, RepoRoot: repoRoot, Redo: redo})
		return err
	}
	dir, err := initwizard.ProjectDir(home, *project)
	if err != nil {
		return err
	}
	s, err := initwizard.LoadState(dir)
	if err != nil {
		return err
	}
	if s.Project != *project {
		return errors.New("この project の init 台帳がありません")
	}
	i := instance(s, dir)
	switch command {
	case "run start":
		status, err := manager.Start(ctx, i)
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(status)
	case "run stop":
		if err := manager.Stop(ctx, i); err != nil {
			return err
		}
		_, err = fmt.Fprintln(output, "本体を停止しました。台帳と作業記録は保持しています")
		return err
	case "run status":
		status, err := manager.Status(ctx, i)
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(status)
	case "run logs":
		return manager.Logs(ctx, i, output)
	}
	return nil
}

func instance(s *initwizard.State, dir string) localrun.Instance {
	return localrun.Instance{ID: s.Project, Dir: dir, Image: s.Image, EngineSHA: s.EngineSHA, DockerContext: s.DockerContext, BoardPort: s.BoardPort}
}

type runtimeAdapter struct{ manager localrun.Manager }

func (r runtimeAdapter) Start(ctx context.Context, s *initwizard.State, dir string) (json.RawMessage, error) {
	status, err := r.manager.Start(ctx, instance(s, dir))
	if err != nil {
		return nil, err
	}
	return json.Marshal(status)
}
func (r runtimeAdapter) Stop(ctx context.Context, s *initwizard.State, dir string) error {
	return r.manager.Stop(ctx, instance(s, dir))
}

// quietWriter carries the progress lines. A write that fails - the reader
// went away - is not the work failing, so the error is dropped here rather
// than travelling into a step's result.
type quietWriter struct{ to io.Writer }

func (w quietWriter) Write(p []byte) (int, error) {
	_, _ = w.to.Write(p)
	return len(p), nil
}
