package initwizard

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// failingCheck answers every docker call except the one whose arguments
// contain the named verb, which fails with text on stderr - the way the
// worker reports a failed check from inside the image.
type failingCheck struct {
	fakeProcess
	verb   string
	stderr string
}

func (p *failingCheck) RunExplained(_ context.Context, _ string, args []string, _ string) ([]byte, string, error) {
	p.commands = append(p.commands, append([]string{}, args...))
	for _, arg := range args {
		if arg == p.verb {
			return nil, p.stderr, errors.New("docker の実行に失敗しました")
		}
	}
	return nil, "", nil
}

// A stopped setup has to say why it stopped. The worker names the failing
// command and the tool whose version did not match; before 2026-09-18 the
// wizard threw that away and said only "check the tools, the commands and
// the managed files", which is unactionable and sends whoever reads it
// changing settings at random.
func TestAStoppedConsumerCheckSaysWhatTheImageSaid(t *testing.T) {
	s, secrets := wizardFixture(t)
	process := &failingCheck{verb: "check-consumer", stderr: "worker: consumer check tool go: tool version is invalid\n"}
	w := &Wizard{Process: process}
	err := w.checkConsumer(context.Background(), s, secrets, t.TempDir())
	if err == nil {
		t.Fatal("失敗が報告されませんでした")
	}
	if !strings.Contains(err.Error(), "consumer check tool go") {
		t.Fatalf("image が言った理由が届いていません: %v", err)
	}
}

// The same for the check that runs before the engine starts.
func TestAStoppedRuntimeCheckSaysWhatTheImageSaid(t *testing.T) {
	s, _ := wizardFixture(t)
	process := &failingCheck{verb: "check-runtime", stderr: "worker: runtime config: report destination is invalid\n"}
	w := &Wizard{Process: process}
	err := w.checkRuntime(context.Background(), s, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "report destination is invalid") {
		t.Fatalf("image が言った理由が届いていません: %v", err)
	}
}

// With nothing on stderr the message still names what the check looks at.
func TestASilentFailureStillNamesWhatWasChecked(t *testing.T) {
	text := checkFailure("image 内の納品先検査に失敗しました", "", "道具・検証コマンド・検証中の管理ファイル変更を確認してください")
	if !strings.Contains(text, "道具") {
		t.Fatalf("%q", text)
	}
	// A build log is not a message. Only its end is shown.
	long := make([]string, 0, 60)
	for index := 0; index < 60; index++ {
		long = append(long, "line")
	}
	long = append(long, "the actual reason")
	text = checkFailure("headline", strings.Join(long, "\n"), "hint")
	if strings.Count(text, "\n") > maxCheckReasonLines || !strings.Contains(text, "the actual reason") {
		t.Fatalf("%d 行: %q", strings.Count(text, "\n"), text)
	}
}

// The link the fakes cannot prove: that a real child process's stderr is
// what RunExplained hands back. The in-image checks are only readable
// because of it.
func TestARealProcessesStderrIsWhatComesBack(t *testing.T) {
	out, detail, err := ExecProcess{}.RunExplained(context.Background(), "/bin/sh",
		[]string{"-c", "echo 'worker: consumer check tool go: tool version is invalid' >&2; exit 1"}, "")
	if err == nil {
		t.Fatal("失敗が報告されませんでした")
	}
	if !strings.Contains(detail, "tool version is invalid") {
		t.Fatalf("stderr が届いていません: %q", detail)
	}
	if len(out) != 0 {
		t.Fatalf("出力: %q", out)
	}
	// The same text, through the message a person actually reads.
	text := checkFailure("image 内の納品先検査に失敗しました", detail, "hint")
	if !strings.Contains(text, "tool version is invalid") || strings.Contains(text, "hint") {
		t.Fatalf("%q", text)
	}
}
