package imagepull

import (
	"strings"
	"testing"
)

func TestExplain(t *testing.T) {
	cases := []struct {
		output string
		want   Class
	}{
		{"Error response from daemon: Head \"https://ghcr.io/v2/x/manifests/sha256:ab\": denied", Denied},
		{"Error response from daemon: unauthorized: authentication required", Denied},
		{"Error response from daemon: manifest unknown", Missing},
		{"Error response from daemon: manifest for ghcr.io/x@sha256:ab not found", Missing},
		{"Error response from daemon: Get \"https://ghcr.io/v2/\": net/http: request canceled while waiting for connection (Client.Timeout exceeded while awaiting headers)", Network},
		{"error pulling image configuration: download failed after attempts=6: read tcp 10.0.0.2:5000->1.2.3.4:443: read: connection reset by peer", Network},
		{"Error response from daemon: Get \"https://ghcr.io/v2/\": dial tcp: lookup ghcr.io: no such host", Network},
		{"unexpected EOF", Network},
		{"context canceled", Network},
		{"Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?", Daemon},
		{"failed to register layer: write /usr/lib/x: no space left on device", Disk},
		{"something new", Unknown},
		{"", Unknown},
		// A denial whose text also mentions a timeout reads as the denial.
		{"denied: timeout waiting for authorization", Denied},
	}
	for _, c := range cases {
		if got := Explain(c.output); got != c.want {
			t.Errorf("Explain(%q) = %v, want %v", c.output, got, c.want)
		}
	}
}

func TestLastLine(t *testing.T) {
	if got := LastLine("sha256:ab: Pulling from x\n\nError response from daemon: manifest unknown\n\n"); got != "Error response from daemon: manifest unknown" {
		t.Fatalf("LastLine = %q", got)
	}
	if got := LastLine("a\r\nb\r"); got != "b" {
		t.Fatalf("LastLine with CR = %q", got)
	}
	if got := LastLine(strings.Repeat("x", 300)); len([]rune(got)) != 201 || !strings.HasSuffix(got, "…") {
		t.Fatalf("LastLine did not trim: %d", len(got))
	}
	if got := LastLine("\n \n"); got != "" {
		t.Fatalf("LastLine of blanks = %q", got)
	}
}
