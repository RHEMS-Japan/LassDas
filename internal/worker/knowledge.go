package worker

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	// MaxKnowledgeBytes bounds everything one agent is handed to read before
	// it works. What an agent knows is configuration, not a channel: a
	// destination cannot grow this without someone changing the configuration.
	MaxKnowledgeBytes = 8 * 1024 * 1024
	// MaxKnowledgeFiles bounds how many separate things are placed.
	MaxKnowledgeFiles = 4096
)

// KnowledgeConfig is what an agent is given to read before it works. The
// framework does not know what any of it says: it places bytes where the
// configuration points and runs the agent.
//
// There are two shapes because agents read in two ways. Rules are loaded by
// the agent itself from a fixed place it looks in, so they are written there.
// A library is too large to load every time and is not always relevant, so it
// is put where the agent can read it when it decides to.
type KnowledgeConfig struct {
	// Rules are placed relative to the agent's home, where its own loader
	// finds them without being asked.
	Rules []KnowledgePlacement `json:"rules,omitempty"`
	// Library is placed inside the working copy, where the agent can already
	// read without further permission. It is hidden from change detection, so
	// what is placed there can never be mistaken for what the agent changed.
	Library *KnowledgePlacement `json:"library,omitempty"`
}

// KnowledgePlacement is one thing to place: bytes from the automation
// repository, put at a path the agent reads from.
type KnowledgePlacement struct {
	From string `json:"from"`
	To   string `json:"to"`
}

func (p KnowledgePlacement) validate() error {
	if !validRelativePath(p.From) {
		return errors.New("knowledge placement path is invalid")
	}
	return nil
}

// validAgentHomePath accepts a path under an agent's home. Unlike a path in a
// repository it may start with a dot, because that is where agents keep what
// they read. It must still stay inside the home.
func validAgentHomePath(value string) bool {
	if value == "" || len(value) > 512 || strings.ContainsAny(value, "\\\r\n\x00") ||
		strings.HasPrefix(value, "/") || value != path.Clean(value) {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." || !agentHomeComponent.MatchString(part) {
			return false
		}
	}
	return true
}

var agentHomeComponent = regexp.MustCompile(`^[A-Za-z0-9.][A-Za-z0-9._-]{0,127}$`)

func (k KnowledgeConfig) validate() error {
	if len(k.Rules) > 8 {
		return errors.New("too many knowledge rules")
	}
	seen := make(map[string]struct{}, len(k.Rules))
	for _, placement := range k.Rules {
		if err := placement.validate(); err != nil {
			return err
		}
		if !validAgentHomePath(placement.To) {
			return errors.New("a knowledge rule is placed outside the agent's home")
		}
		if _, duplicate := seen[placement.To]; duplicate {
			return errors.New("two knowledge rules are placed at the same path")
		}
		seen[placement.To] = struct{}{}
	}
	if k.Library != nil {
		if err := k.Library.validate(); err != nil {
			return err
		}
		if !validRelativePath(k.Library.To) {
			return errors.New("the knowledge library is placed outside the working copy")
		}
		if hasHiddenComponent(k.Library.To) {
			return errors.New("the knowledge library must be placed at a visible path")
		}
	}
	return nil
}

// Empty reports whether this agent is handed nothing, which is the shape of a
// destination that has written none of its knowledge down yet.
func (k KnowledgeConfig) Empty() bool { return len(k.Rules) == 0 && k.Library == nil }

// PlaceKnowledge puts everything an agent is given to read where it reads it,
// and reports what was placed. The rules go under the agent's home; the
// library goes into the working copy and is hidden from change detection, so a
// library file can never be reported as something the agent changed.
func PlaceKnowledge(knowledge KnowledgeConfig, sourceRoot, home, workspace string) ([]string, error) {
	if err := knowledge.validate(); err != nil {
		return nil, err
	}
	placed := make([]string, 0, 8)
	budget := MaxKnowledgeBytes
	for _, rule := range knowledge.Rules {
		written, err := copyKnowledgeFile(sourceRoot, rule.From, home, rule.To, &budget)
		if err != nil {
			return nil, err
		}
		placed = append(placed, written)
	}
	if knowledge.Library != nil {
		written, err := copyKnowledgeTree(sourceRoot, knowledge.Library.From, workspace, knowledge.Library.To, &budget)
		if err != nil {
			return nil, err
		}
		if err := hideFromChangeDetection(workspace, knowledge.Library.To); err != nil {
			return nil, err
		}
		placed = append(placed, written...)
	}
	sort.Strings(placed)
	return placed, nil
}

func copyKnowledgeFile(sourceRoot, from, destinationRoot, to string, budget *int) (string, error) {
	name, err := regularFileWithin(sourceRoot, from)
	if err != nil {
		return "", errors.New("knowledge to place was not found")
	}
	content, err := os.ReadFile(name) // #nosec G304 -- path is validated relative to a fixed root.
	if err != nil {
		return "", errors.New("knowledge to place could not be read")
	}
	if len(content) > *budget {
		return "", errors.New("knowledge to place is larger than allowed")
	}
	*budget -= len(content)
	target := filepath.Join(destinationRoot, filepath.FromSlash(to))
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return "", errors.New("knowledge could not be placed")
	}
	if err := os.WriteFile(target, content, 0o600); err != nil {
		return "", errors.New("knowledge could not be placed")
	}
	return to, nil
}

// copyKnowledgeTree places a directory of knowledge. Only regular files are
// copied: a link could otherwise point out of the tree and hand the agent
// something nobody configured.
func copyKnowledgeTree(sourceRoot, from, destinationRoot, to string, budget *int) ([]string, error) {
	root := filepath.Join(sourceRoot, filepath.FromSlash(from))
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("the knowledge library was not found")
	}
	placed := make([]string, 0, 64)
	walkErr := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return errors.New("the knowledge library could not be read")
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		relative, err := filepath.Rel(root, name)
		if err != nil {
			return errors.New("the knowledge library could not be read")
		}
		relative = filepath.ToSlash(relative)
		if !validRelativePath(relative) || hasHiddenComponent(relative) {
			return nil
		}
		if len(placed) >= MaxKnowledgeFiles {
			return errors.New("the knowledge library holds more files than allowed")
		}
		content, err := os.ReadFile(name) // #nosec G304 -- walked from a fixed root, regular files only.
		if err != nil {
			return errors.New("the knowledge library could not be read")
		}
		if len(content) > *budget {
			return errors.New("the knowledge library is larger than allowed")
		}
		*budget -= len(content)
		target := filepath.Join(destinationRoot, filepath.FromSlash(to), filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return errors.New("the knowledge library could not be placed")
		}
		if err := os.WriteFile(target, content, 0o600); err != nil {
			return errors.New("the knowledge library could not be placed")
		}
		placed = append(placed, to+"/"+relative)
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return placed, nil
}

// hideFromChangeDetection makes git ignore the placed library without touching
// anything the destination tracks. This is what keeps placed knowledge from
// ever being reported as a change the agent made.
func hideFromChangeDetection(workspace, directory string) error {
	exclude := filepath.Join(workspace, ".git", "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(exclude), 0o750); err != nil {
		return errors.New("change detection could not be told to ignore placed knowledge")
	}
	existing, err := os.ReadFile(exclude) // #nosec G304 -- fixed path inside the workspace.
	if err != nil && !os.IsNotExist(err) {
		return errors.New("change detection could not be told to ignore placed knowledge")
	}
	line := "/" + strings.TrimSuffix(directory, "/") + "/"
	if strings.Contains(string(existing), line+"\n") {
		return nil
	}
	updated := string(existing)
	if updated != "" && !strings.HasSuffix(updated, "\n") {
		updated += "\n"
	}
	updated += line + "\n"
	if err := os.WriteFile(exclude, []byte(updated), 0o600); err != nil {
		return errors.New("change detection could not be told to ignore placed knowledge")
	}
	return nil
}
