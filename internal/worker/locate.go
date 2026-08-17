package worker

import (
	"errors"
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
	listing, err := ReadCandidateListing(repoRoot, strings.Repeat("0", 40), consumer, config)
	if err != nil {
		return TargetLocation{}, err
	}
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		return TargetLocation{}, errors.New("source root is invalid")
	}
	matches := make([]string, 0, 4)
	scanned := 0
	for _, candidate := range listing.Paths {
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
