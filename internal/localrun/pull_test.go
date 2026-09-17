package localrun

import (
	"context"
	"errors"
	"strings"
	"testing"
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

func TestPullUnknownShowsLastLine(t *testing.T) {
	d := &scriptedDocker{outputs: [][]byte{[]byte("Pulling\nError response from daemon: something else\n")}}
	m := Manager{Docker: d}
	err := m.pull(context.Background(), Instance{Image: "x@sha256:" + strings.Repeat("a", 64)})
	if err == nil || !strings.HasSuffix(err.Error(), "something else") {
		t.Fatalf("unknown reason not surfaced: %v", err)
	}
}
