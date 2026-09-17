package initwizard

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// explainingProcess scripts `docker pull`: each call pops the next stderr text
// ("" means success) and records how many pulls ran.
type explainingProcess struct {
	fakeProcess
	pullStderr []string
	pulls      int
}

func (p *explainingProcess) RunExplained(_ context.Context, _ string, args []string, _ string) ([]byte, string, error) {
	p.commands = append(p.commands, append([]string{}, args...))
	p.pulls++
	stderr := p.pullStderr[0]
	p.pullStderr = p.pullStderr[1:]
	if stderr != "" {
		return nil, stderr, errors.New("docker の実行に失敗しました")
	}
	return nil, "", nil
}

func TestPullExplainsDenialWithoutRetry(t *testing.T) {
	p := &explainingProcess{pullStderr: []string{"Error response from daemon: Head \"https://ghcr.io/v2/x/manifests/sha256:ab\": denied"}}
	w := &Wizard{Process: p}
	s := &State{Image: "ghcr.io/x/runtime@sha256:" + strings.Repeat("a", 64), DockerContext: "desktop-linux"}
	err := w.pull(context.Background(), s)
	if err == nil || !strings.Contains(err.Error(), "拒否") || !strings.Contains(err.Error(), "registry_login") {
		t.Fatalf("denial not explained: %v", err)
	}
	if p.pulls != 1 {
		t.Fatalf("denial retried: %d pulls", p.pulls)
	}
	if p.commands[0][0] != "--context" || p.commands[0][1] != "desktop-linux" || p.commands[0][2] != "pull" {
		t.Fatalf("pull command shape: %v", p.commands[0])
	}
}

func TestPullRetriesNetworkOnce(t *testing.T) {
	p := &explainingProcess{pullStderr: []string{"read tcp: connection reset by peer", ""}}
	w := &Wizard{Process: p}
	s := &State{Image: "ghcr.io/x/runtime@sha256:" + strings.Repeat("a", 64)}
	if err := w.pull(context.Background(), s); err != nil {
		t.Fatalf("second attempt should succeed: %v", err)
	}
	if p.pulls != 2 {
		t.Fatalf("expected one retry, got %d pulls", p.pulls)
	}
	p = &explainingProcess{pullStderr: []string{"net/http: TLS handshake timeout", "unexpected EOF"}}
	w = &Wizard{Process: p}
	err := w.pull(context.Background(), s)
	if err == nil || !strings.Contains(err.Error(), "ネットワーク") || !strings.Contains(err.Error(), "認証の問題ではありません") {
		t.Fatalf("network failure not explained: %v", err)
	}
	if p.pulls != 2 {
		t.Fatalf("network failure should stop after two pulls, got %d", p.pulls)
	}
}

func TestPullShowsUnknownReasonAndMissingDigest(t *testing.T) {
	p := &explainingProcess{pullStderr: []string{"sha256:ab: Pulling\nError response from daemon: something new\n"}}
	w := &Wizard{Process: p}
	s := &State{Image: "ghcr.io/x/runtime@sha256:" + strings.Repeat("a", 64)}
	err := w.pull(context.Background(), s)
	if err == nil || !strings.HasSuffix(err.Error(), "Error response from daemon: something new") {
		t.Fatalf("unknown reason not surfaced: %v", err)
	}
	p = &explainingProcess{pullStderr: []string{"Error response from daemon: manifest unknown"}}
	w = &Wizard{Process: p}
	err = w.pull(context.Background(), s)
	if err == nil || !strings.Contains(err.Error(), "存在しません") || p.pulls != 1 {
		t.Fatalf("missing digest not explained: %v (%d pulls)", err, p.pulls)
	}
}

func TestPullWithoutExplainerStaysGeneric(t *testing.T) {
	p := &failingProcess{}
	w := &Wizard{Process: p}
	s := &State{Image: "ghcr.io/x/runtime@sha256:" + strings.Repeat("a", 64)}
	err := w.pull(context.Background(), s)
	if err == nil || !strings.Contains(err.Error(), "理由を出力しませんでした") {
		t.Fatalf("plain Process should yield the generic message: %v", err)
	}
}

type failingProcess struct{ fakeProcess }

func (p *failingProcess) Run(_ context.Context, _ string, args []string, _ string) ([]byte, error) {
	p.commands = append(p.commands, append([]string{}, args...))
	return nil, errors.New("docker の実行に失敗しました (出力は秘密保護のため非表示)")
}
