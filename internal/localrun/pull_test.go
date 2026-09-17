package localrun

import (
	"context"
	"errors"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/imagepull"
)

type scriptedDocker struct {
	outputs [][]byte
	calls   [][]string
}

func (d *scriptedDocker) Run(_ context.Context, args []string) ([]byte, error) {
	d.calls = append(d.calls, append([]string{}, args...))
	out := d.outputs[0]
	d.outputs = d.outputs[1:]
	if out != nil {
		return out, errors.New("exit status 1")
	}
	return nil, nil
}

func TestPullExplainsDenial(t *testing.T) {
	d := &scriptedDocker{outputs: [][]byte{[]byte("Error response from daemon: unauthorized: authentication required")}}
	m := Manager{Docker: d}
	err := m.pull(context.Background(), Instance{Image: "ghcr.io/x/runtime@sha256:" + strings.Repeat("a", 64)})
	if err == nil || !strings.Contains(err.Error(), "denied by the registry") {
		t.Fatalf("denial not explained: %v", err)
	}
	if len(d.calls) != 1 || d.calls[0][0] != "--context" || d.calls[0][1] != "desktop-linux" || d.calls[0][2] != "pull" {
		t.Fatalf("pull command shape: %v", d.calls)
	}
}

func TestPullRetriesNetworkOnce(t *testing.T) {
	d := &scriptedDocker{outputs: [][]byte{[]byte("dial tcp: i/o timeout"), nil}}
	m := Manager{Docker: d}
	if err := m.pull(context.Background(), Instance{Image: "x@sha256:" + strings.Repeat("a", 64), DockerContext: "other"}); err != nil {
		t.Fatalf("second attempt should succeed: %v", err)
	}
	if len(d.calls) != 2 || d.calls[1][1] != "other" {
		t.Fatalf("expected one retry on the given context, got %v", d.calls)
	}
	d = &scriptedDocker{outputs: [][]byte{[]byte("unexpected EOF"), []byte("unexpected EOF")}}
	m = Manager{Docker: d}
	err := m.pull(context.Background(), Instance{Image: "x@sha256:" + strings.Repeat("a", 64)})
	if err == nil || !strings.Contains(err.Error(), "network, not authentication") || len(d.calls) != 2 {
		t.Fatalf("network failure not explained after two pulls: %v (%d)", err, len(d.calls))
	}
}

func TestPullUnknownNamesTheCommandNotTheOutput(t *testing.T) {
	d := &scriptedDocker{outputs: [][]byte{[]byte("Pulling\nError response from daemon: something else at /Users/someone/.docker\n")}}
	m := Manager{Docker: d}
	image := "x@sha256:" + strings.Repeat("a", 64)
	err := m.pull(context.Background(), Instance{Image: image})
	if err == nil || strings.Contains(err.Error(), "something else") || strings.Contains(err.Error(), "/Users/") {
		t.Fatalf("raw docker output must not be echoed: %v", err)
	}
	if !strings.Contains(err.Error(), "docker --context desktop-linux pull --platform linux/arm64 "+image) {
		t.Fatalf("unknown case should name the command to run: %v", err)
	}
}

func TestPullCancelledIsNotNetwork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := &scriptedDocker{outputs: [][]byte{[]byte("context canceled"), []byte("context canceled")}}
	m := Manager{Docker: d}
	err := m.pull(ctx, Instance{Image: "x@sha256:" + strings.Repeat("a", 64)})
	if !errors.Is(err, context.Canceled) || len(d.calls) != 1 {
		t.Fatalf("cancelled pull should return the context error without retry: %v (%d calls)", err, len(d.calls))
	}
}

func TestPullFailureWording(t *testing.T) {
	args := []string{"--context", "desktop-linux", "pull", "x"}
	cases := map[imagepull.Class]string{
		imagepull.Denied:  "denied by the registry",
		imagepull.Missing: "not in the registry for linux/arm64",
		imagepull.Network: "network, not authentication",
		imagepull.Daemon:  "start Docker Desktop",
		imagepull.Disk:    "no disk space left",
		imagepull.Unknown: "run `docker --context desktop-linux pull x` to read it",
	}
	for class, want := range cases {
		if got := pullFailure(class, args); !strings.Contains(got, want) {
			t.Errorf("class %v: %q lacks %q", class, got, want)
		}
	}
}
