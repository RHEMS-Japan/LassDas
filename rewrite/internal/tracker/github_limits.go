package tracker

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GitHub counts the requests made with a token and says when to stop. The
// engine asks the same repository from several places at once (the intake,
// the queue, the watcher of each running request), so what one of them
// learns, all of them keep: one state per API and credential in the process.
type githubShared struct {
	mu sync.Mutex
	// kept is the last answer read from each address, with the validator
	// GitHub gave it, so that asking again costs nothing while nothing changed.
	kept  map[string]githubKept
	order []string
	size  int
	// quiet is when GitHub may be asked again, after it said to wait.
	quiet time.Time
	// strikes counts the limits met in a row that did not say for how long.
	strikes int
	// lastChange is when the last change was sent.
	lastChange time.Time
}

type githubKept struct {
	etag   string
	data   []byte
	header http.Header
}

const (
	// An answer longer than githubKeptItem is read afresh each time rather
	// than kept, and no more than githubKeptCount answers or githubKeptSize
	// bytes are kept; the oldest go first.
	githubKeptItem  = 8 << 20
	githubKeptSize  = 64 << 20
	githubKeptCount = 512
	// GitHub asks for at least a second between changes.
	githubChangeSpacing = time.Second
)

// The clock and the waiting are the tests' to replace.
var (
	githubNow  = time.Now
	githubWait = func(ctx context.Context, wait time.Duration) error {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}
)

var githubStates = struct {
	sync.Mutex
	shared map[string]*githubShared
}{shared: map[string]*githubShared{}}

func (g GitHub) shared() *githubShared {
	key := g.base() + "\x00" + g.KeyEnv
	githubStates.Lock()
	defer githubStates.Unlock()
	state := githubStates.shared[key]
	if state == nil {
		state = &githubShared{kept: map[string]githubKept{}}
		githubStates.shared[key] = state
	}
	return state
}

// closedUntil is when GitHub may be asked again, or zero when it may be now.
func (s *githubShared) closedUntil(now time.Time) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.Before(s.quiet) {
		return s.quiet
	}
	return time.Time{}
}

// spaceChange waits until a second has passed since the change sent before,
// and takes the moment for this one, so changes sent from several places are
// still a second apart.
func (s *githubShared) spaceChange(ctx context.Context) error {
	s.mu.Lock()
	now, next := githubNow(), s.lastChange.Add(githubChangeSpacing)
	if !now.Before(next) {
		s.lastChange = now
		s.mu.Unlock()
		return nil
	}
	s.lastChange = next
	s.mu.Unlock()
	return githubWait(ctx, next.Sub(now))
}

// learn reads what an answer says about asking again: the seconds a refusal
// gives, the end of a spent hourly allowance, or, for a limit that says
// neither, a minute that doubles while such limits come in a row.
func (s *githubShared) learn(status int, header http.Header, body []byte, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var until time.Time
	if seconds, err := strconv.Atoi(strings.TrimSpace(header.Get("Retry-After"))); err == nil && seconds >= 0 {
		until = now.Add(time.Duration(seconds) * time.Second)
	}
	if strings.TrimSpace(header.Get("X-RateLimit-Remaining")) == "0" {
		reset := now.Add(time.Minute)
		if epoch, err := strconv.ParseInt(strings.TrimSpace(header.Get("X-RateLimit-Reset")), 10, 64); err == nil {
			reset = time.Unix(epoch, 0)
		}
		if reset.After(until) {
			until = reset
		}
	}
	limited := status == http.StatusTooManyRequests ||
		(status == http.StatusForbidden && strings.Contains(strings.ToLower(string(body)), "rate limit"))
	switch {
	case limited && until.IsZero():
		s.strikes++
		until = now.Add(min(time.Minute<<min(s.strikes-1, 6), time.Hour))
	case status < 300 || status == http.StatusNotModified:
		s.strikes = 0
	}
	if until.After(s.quiet) {
		s.quiet = until
	}
}

func (s *githubShared) recall(address string) (githubKept, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept, found := s.kept[address]
	return kept, found
}

func (s *githubShared) keep(address string, kept githubKept) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if earlier, found := s.kept[address]; found {
		s.size -= len(earlier.data)
		delete(s.kept, address)
		for i, name := range s.order {
			if name == address {
				s.order = append(s.order[:i], s.order[i+1:]...)
				break
			}
		}
	}
	if kept.etag == "" || len(kept.data) > githubKeptItem {
		return
	}
	s.kept[address] = kept
	s.order = append(s.order, address)
	s.size += len(kept.data)
	for len(s.order) > githubKeptCount || s.size > githubKeptSize {
		oldest := s.order[0]
		s.order = s.order[1:]
		s.size -= len(s.kept[oldest].data)
		delete(s.kept, oldest)
	}
}
