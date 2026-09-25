package worker

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// maxLocateScanBytes bounds one search across the writable scope.
const maxLocateScanBytes = 32 * 1024 * 1024

// TargetLocation is the deterministic answer to "which files hold the wording
// that must disappear". For a client-visible text change the target is not
// something the requester should have to know: it is wherever the current
// wording lives. Finding it by exact search needs no model, so the normal case
// is auditable and free.
type TargetLocation struct {
	Matches []string
	Scanned int
}

// writableScopePaths lists every regular file inside the mode's allowed
// prefixes, sorted, as relative paths. Symlinks, dotted names and anything
// outside the prefixes are never listed. It is a working set for the search
// below, not a record: nothing downstream is bound to it, and the only bound
// on its size is the scan budget the caller spends reading the files.
func writableScopePaths(repoRoot string, consumer ConsumerConfig) ([]string, error) {
	root, err := filepath.Abs(repoRoot)
	if err != nil || filepath.Clean(root) != root {
		return nil, errors.New("source root is invalid")
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("source root is invalid")
	}
	paths := make([]string, 0, 64)
	for _, prefix := range consumer.Mode.AllowedFilePrefixes {
		base := filepath.Join(root, filepath.FromSlash(prefix))
		if !strings.HasPrefix(base, root+string(os.PathSeparator)) {
			return nil, errors.New("allowed prefix escapes the source root")
		}
		info, statErr := os.Lstat(base)
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 {
			// A prefix that is absent in this revision contributes nothing.
			continue
		}
		if !strings.HasSuffix(prefix, "/") {
			if info.Mode().IsRegular() {
				paths = append(paths, prefix)
			}
			continue
		}
		if !info.IsDir() {
			continue
		}
		walkErr := filepath.WalkDir(base, func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			// A dotted name is never the subject of a user-visible wording
			// change, and searching one would put repository machinery (.git)
			// and secrets (.env) inside the searchable scope.
			if strings.HasPrefix(entry.Name(), ".") {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.IsDir() || !entry.Type().IsRegular() {
				return nil
			}
			relative, relErr := filepath.Rel(root, name)
			if relErr != nil {
				return relErr
			}
			candidate := filepath.ToSlash(relative)
			if !validRelativePath(candidate) || !allowedPath(candidate, consumer.Mode.AllowedFilePrefixes) {
				return nil
			}
			paths = append(paths, candidate)
			return nil
		})
		if walkErr != nil {
			return nil, errors.New("the writable scope could not be read")
		}
	}
	sort.Strings(paths)
	return paths, nil
}

// LocateTargetFiles searches the writable scope for the exact text the ticket
// says must be gone afterwards. It reports every file containing it: one match
// is the answer, none means the wording is not where this automation may write,
// and several means the requester has to say which occurrences they meant.
func LocateTargetFiles(repoRoot string, draft TicketDraft, config Config) (TargetLocation, error) {
	if err := config.Validate(); err != nil {
		return TargetLocation{}, errors.New("worker configuration is invalid")
	}
	if len(draft.AbsentText) < minAcceptanceTextBytes || validatePlainText(draft.AbsentText, 512, false) != nil {
		return TargetLocation{}, errors.New("ticket absent text is invalid")
	}
	consumer, err := config.ConsumerFor(draft.Repository)
	if err != nil {
		return TargetLocation{}, errors.New("ticket repository is not a configured consumer")
	}
	candidates, err := writableScopePaths(repoRoot, consumer)
	if err != nil {
		return TargetLocation{}, err
	}
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		return TargetLocation{}, errors.New("source root is invalid")
	}
	matches := make([]string, 0, 4)
	scanned := 0
	for _, candidate := range candidates {
		filename, err := regularFileWithin(root, candidate)
		if err != nil {
			continue
		}
		info, err := os.Lstat(filename)
		if err != nil || !info.Mode().IsRegular() || info.Size() > int64(consumer.Mode.MaxFileBytes) {
			continue
		}
		scanned += int(info.Size())
		if scanned > maxLocateScanBytes {
			return TargetLocation{}, errors.New("writable scope is too large to search")
		}
		content, err := os.ReadFile(filename)
		if err != nil {
			continue
		}
		if strings.Contains(string(content), draft.AbsentText) {
			matches = append(matches, candidate)
		}
	}
	sort.Strings(matches)
	return TargetLocation{Matches: matches, Scanned: scanned}, nil
}

// Resolve turns a search result into a completed contract. It refuses to guess:
// finding nothing, or finding more occurrences than the mode may change in one
// run, is reported rather than narrowed arbitrarily.
func (l TargetLocation) Resolve(draft TicketDraft, config Config) (TicketRequest, error) {
	consumer, err := config.ConsumerFor(draft.Repository)
	if err != nil {
		return TicketRequest{}, errors.New("ticket repository is not a configured consumer")
	}
	switch {
	case len(l.Matches) == 0:
		return TicketRequest{}, errors.New("the wording to replace was not found in the writable scope")
	case len(l.Matches) > consumer.Mode.MaxFiles:
		return TicketRequest{}, errors.New("the wording to replace appears in more files than this mode may change")
	}
	return draft.WithTargetFiles(l.Matches, config)
}
