// Package imagepull classifies why `docker pull` of the pinned runtime image
// failed, from the text docker printed. Pull output names the registry's answer
// (denied, missing manifest, network trouble) and never carries a credential:
// docker reads those from its own config and does not echo them.
package imagepull

import "strings"

// Class is the kind of pull failure the caller should explain to the user.
type Class int

const (
	// Unknown covers output no rule recognised; callers should show LastLine.
	Unknown Class = iota
	// Denied means the registry refused access: the image is private (or the
	// login expired). Retrying without registry access cannot succeed.
	Denied
	// Missing means the registry has no such manifest: the digest in the
	// distributor's note does not exist there.
	Missing
	// Network means the transfer was interrupted or never connected. Retrying
	// resumes from the layers docker already holds.
	Network
	// Daemon means docker itself is not reachable (Docker Desktop not running).
	Daemon
	// Disk means the host ran out of space while extracting layers.
	Disk
)

var rules = []struct {
	class Class
	hints []string
}{
	{Daemon, []string{"cannot connect to the docker daemon", "is the docker daemon running", "docker desktop is not running"}},
	{Disk, []string{"no space left on device"}},
	{Denied, []string{"denied", "unauthorized", "authentication required", "no basic auth credentials", "forbidden"}},
	{Missing, []string{"manifest unknown", "not found", "does not exist"}},
	{Network, []string{"timeout", "timed out", "deadline exceeded", "connection reset", "connection refused", "unexpected eof", "eof", "tls handshake", "no such host", "network is unreachable", "context canceled", "temporary failure", "broken pipe", "i/o error", "dial tcp", "proxyconnect"}},
}

// Explain classifies docker's output. Rules are checked in a fixed order so a
// message naming both a timeout and a denial reads as the daemon/denial it is.
func Explain(output string) Class {
	lower := strings.ToLower(output)
	for _, rule := range rules {
		for _, hint := range rule.hints {
			if strings.Contains(lower, hint) {
				return rule.class
			}
		}
	}
	return Unknown
}

// LastLine returns the final non-empty line of docker's output, trimmed to at
// most 200 characters, for the Unknown class where the raw reason is the only
// help available.
func LastLine(output string) string {
	lines := strings.Split(strings.ReplaceAll(output, "\r", "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if len(line) > 200 {
			return line[:200] + "…"
		}
		return line
	}
	return ""
}
