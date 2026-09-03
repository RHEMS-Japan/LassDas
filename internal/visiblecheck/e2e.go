package visiblecheck

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"automation.internal/ticket-ingress/internal/worker"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/storage"
	"github.com/chromedp/chromedp"
)

// The debug role's observation deviates from the release evidence in two
// deliberate ways. It may carry a requester-provided session cookie set —
// the staging console renders a login page to a credential-free profile,
// which would make every observation meaningless — and it captures the
// screenshot even when the expected text never appears, because a failed
// check with a picture of what WAS shown is the whole point of the role.
// It never participates in the release-proof chain: ObserveAndSealStaging
// stays the only sealed path.

const (
	// SessionFileEnvironment names the operator-provisioned session jar
	// (Playwright storageState JSON): the seed a person makes by logging
	// in once.
	SessionFileEnvironment = "LASSDAS_E2E_SESSION_FILE"
	// SessionStateFileEnvironment names the engine's own copy of the jar,
	// rewritten after every sign-in that lands, so that a console session
	// with a short lifetime is re-minted from the identity provider's
	// longer one, and that one keeps rolling forward. The copy remembers
	// which seed it grew from: it is the jar in use for as long as that
	// seed is the one mounted, and an operator's fresh seed replaces it.
	SessionStateFileEnvironment = "LASSDAS_E2E_SESSION_STATE_FILE"
)

// E2ECookie is one session cookie to install before navigation.
type E2ECookie struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Domain   string `json:"domain"`
	Path     string `json:"path"`
	Secure   bool   `json:"secure"`
	HTTPOnly bool   `json:"httpOnly"`
	// Expires is the storageState expiry: seconds since the epoch, or -1
	// for a cookie that lives as long as the browser session.
	Expires  float64 `json:"expires,omitempty"`
	SameSite string  `json:"sameSite,omitempty"`
}

// expired reports whether the cookie's own expiry has passed. A cookie
// without an expiry never expires here; the server decides about it.
func (c E2ECookie) expired(now time.Time) bool {
	return c.Expires > 0 && !time.Unix(int64(c.Expires), 0).After(now)
}

// SignIn is how the observation browser gets into a destination for one
// environment. The browser opens LoginURL with the jar installed and
// counts itself signed in once it comes to rest under LandedPrefix — the
// environment's own origin: a login that needs a person parks the browser
// on the identity provider instead, and a console that rejects the jar
// sends the browser away to its portal. Measured live (2026-09-03): a
// console whose own session lasts a day was signed in again, with nobody
// at the keyboard, from the identity provider's fortnight-long session.
type SignIn struct {
	LoginURL     string
	LandedPrefix string
	// Language is what the browser asks pages for (a BCP 47 tag such as
	// "ja"): a console that follows the browser's language would otherwise
	// render its default wording and no promised text in another language
	// could ever be seen. Independent of the login: a public page may need
	// the language and no entry.
	Language string
	// SeedPath and KeepJarAt: when KeepJarAt is set, a login that lands
	// writes the jar it left behind there, stamped with the digest of the
	// seed at SeedPath. The identity provider was seen to replace its
	// session cookie on the first login made from a person's seed (the
	// values the seed carried were dead an hour later), so the jar a login
	// leaves behind is the only one known to work next time — and every
	// login, not only the attendant's renewal, must keep it.
	SeedPath  string
	KeepJarAt string
}

func (s SignIn) configured() bool { return s.LoginURL != "" }

// Block says why an observation could not judge the page at all — as
// opposed to judging it and finding the wording wrong. The two are
// different results for the requester: a wrong wording is the change's
// fault; a page that never opened says nothing about the change.
type Block string

const (
	// BlockNone: the target page opened at its own URL; ExpectedSeen and
	// AbsentGone are the verdict.
	BlockNone Block = ""
	// BlockSignIn: the browser was not let in — the configured login did
	// not land, or the target sent the browser off the destination's own
	// origin (a portal, an identity provider). The jar is the operator's
	// to renew; the target was never shown.
	BlockSignIn Block = "sign_in"
	// BlockRedirect: the target sent the browser to another page of the
	// same destination (an onboarding step, a default route), so the
	// promised page was never shown. Where it landed is in FinalURL and the
	// screenshot; the requester picks a page that does not redirect.
	BlockRedirect Block = "redirect"
)

// E2EObservation is what the browser actually saw. ExpectedSeen and
// AbsentGone are the observed facts about whatever page was shown, even
// under a Block — callers decide by Block first, never by the flags alone.
type E2EObservation struct {
	RequestedURL   string
	FinalURL       string
	StatusCode     int
	ExpectedSeen   bool
	AbsentGone     bool
	Block          Block
	ScreenshotPNG  []byte
	BrowserVersion string
	ObservedAt     time.Time
}

const (
	// e2ePollInterval matches the sealed observation's cadence.
	e2ePollInterval = 250 * time.Millisecond
	// signInTimeout bounds the login round trip; signInSettle is how long
	// the browser must stay on the landing after the document completed —
	// a console that rejects the jar shows its shell for a moment and only
	// then sends the browser away. The observation's own budget is
	// extended by the timeout when a login is configured.
	signInTimeout = 30 * time.Second
	signInSettle  = 4 * time.Second
	// maxSessionFileBytes bounds the jar file either way.
	maxSessionFileBytes = 1 << 20
)

// ErrSignInDidNotLand is the login that reached its timeout with the
// browser somewhere that says nothing about the jar: still loading, an
// error page on the login host, nowhere at all. ErrSignInRefused is the
// login that reached its timeout with the browser parked where a person
// is being asked to log in — a login page, the identity provider, a
// portal, a callback carrying an error — which is the destination
// refusing the jar.
var (
	ErrSignInDidNotLand = errors.New("browser sign-in did not land")
	ErrSignInRefused    = errors.New("browser sign-in was refused: the jar is not accepted")
)

func validSessionCookies(cookies []E2ECookie) error {
	for _, cookie := range cookies {
		if cookie.Name == "" || cookie.Domain == "" ||
			strings.ContainsAny(cookie.Name+cookie.Value+cookie.Domain+cookie.Path, "\x00\r\n") {
			return errors.New("session cookie is invalid")
		}
	}
	return nil
}

// installableCookies drops the cookies whose expiry has already passed:
// installing them would only send a dead value the server rejects, and a
// jar that is visibly stale is the operator's signal, not the browser's.
func installableCookies(cookies []E2ECookie, now time.Time) []E2ECookie {
	kept := make([]E2ECookie, 0, len(cookies))
	for _, cookie := range cookies {
		if !cookie.expired(now) {
			kept = append(kept, cookie)
		}
	}
	return kept
}

func setCookieActions(cookies []E2ECookie, now time.Time) []chromedp.Action {
	live := installableCookies(cookies, now)
	actions := make([]chromedp.Action, 0, len(live))
	for _, cookie := range live {
		cookie := cookie
		actions = append(actions, chromedp.ActionFunc(func(ctx context.Context) error {
			params := network.SetCookie(cookie.Name, cookie.Value).
				WithDomain(cookie.Domain).
				WithPath(cookiePath(cookie.Path)).
				WithSecure(cookie.Secure).
				WithHTTPOnly(cookie.HTTPOnly)
			if cookie.Expires > 0 {
				expires := cdp.TimeSinceEpoch(time.Unix(int64(cookie.Expires), 0))
				params = params.WithExpires(&expires)
			}
			if sameSite := sameSiteValue(cookie.SameSite); sameSite != "" {
				params = params.WithSameSite(sameSite)
			}
			return params.Do(ctx)
		}))
	}
	return actions
}

// sessionFile is the storageState document, plus the digest of the seed a
// renewed copy grew from.
type sessionFile struct {
	Cookies    []E2ECookie `json:"cookies"`
	SeedSHA256 string      `json:"seed_sha256,omitempty"`
}

// LoadSessionCookies reads the session jar: the engine's renewed copy when
// it grew from the seed that is mounted now, the operator's seed otherwise.
// The decision is by content, never by file times — a secret mount is
// rewritten on every pod start and would win every restart by time alone.
// Every failure degrades to "no cookies" with a human-readable note — the
// observation itself then reports the login page honestly instead of
// failing.
func LoadSessionCookies(seedPath, statePath string) ([]E2ECookie, string) {
	seed, seedNote := loadSessionFile(seedPath)
	if statePath == "" {
		return seed.Cookies, seedNote
	}
	renewed, renewedNote := loadSessionFile(statePath)
	switch {
	case renewedNote != "":
		return seed.Cookies, seedNote
	case seedNote != "":
		// No usable seed at all: the renewed copy is the jar.
		return renewed.Cookies, ""
	case renewed.SeedSHA256 == SeedDigest(seedPath):
		return renewed.Cookies, ""
	default:
		// The seed changed since the copy was made: an operator logged in
		// again, and that login is the one to start from.
		return seed.Cookies, ""
	}
}

// SeedDigest is the content digest that ties a renewed copy to the seed it
// grew from; empty when the seed cannot be read.
func SeedDigest(seedPath string) string {
	if seedPath == "" {
		return ""
	}
	raw, err := os.ReadFile(seedPath)
	if err != nil || len(raw) > maxSessionFileBytes {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func loadSessionFile(path string) (sessionFile, string) {
	if path == "" {
		return sessionFile{}, "確認用セッションが設定されていません。"
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > maxSessionFileBytes {
		return sessionFile{}, "確認用セッションのファイルを読めませんでした。"
	}
	var state sessionFile
	if err := json.Unmarshal(raw, &state); err != nil || len(state.Cookies) == 0 {
		return sessionFile{}, "確認用セッションのファイルの形式が想定と異なります。"
	}
	return state, ""
}

// WriteSessionFile stores the jar as storageState JSON the loader reads
// back, stamped with the digest of the seed it grew from: owner-only, in
// an owner-only directory, replaced atomically so a reader never sees a
// half-written jar.
func WriteSessionFile(path string, cookies []E2ECookie, seedSHA256 string) error {
	if path == "" || !filepath.IsAbs(path) || len(cookies) == 0 {
		return errors.New("session file path is invalid")
	}
	if err := validSessionCookies(cookies); err != nil {
		return err
	}
	encoded, err := json.Marshal(sessionFile{Cookies: cookies, SeedSHA256: seedSHA256})
	if err != nil {
		return err
	}
	if len(encoded) > maxSessionFileBytes {
		return errors.New("session jar is too large")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	sweepStaleTemporaries(filepath.Dir(path))
	temporary, err := os.CreateTemp(filepath.Dir(path), ".session-*.json")
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(temporary.Name())
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(encoded); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary.Name(), path); err != nil {
		return err
	}
	keep = true
	return nil
}

// staleTemporaryAge is how old a half-written jar may be before it is
// taken for the leftover of a killed writer and removed.
const staleTemporaryAge = 10 * time.Minute

// sweepStaleTemporaries removes the temporaries of writers that died
// between creating and renaming their file — a killed card, a pod
// restart — so a jar never lingers under a name nobody reads.
func sweepStaleTemporaries(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, ".session-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		info, err := entry.Info()
		if err != nil || time.Since(info.ModTime()) < staleTemporaryAge {
			continue
		}
		_ = os.Remove(filepath.Join(dir, name))
	}
}

// ObserveForE2E opens a clean Chrome profile, installs the given cookies,
// signs in when the consumer has a login entry, navigates to the target and
// reports whether the expected text appeared (and the absent text
// disappeared) within the page-ready budget — with a full-page screenshot
// taken either way. A login that does not land, or a target that sends the
// browser elsewhere, is a Block, not an error: the screenshot then shows
// where the browser ended up.
func ObserveForE2E(parent context.Context, targetURL, expectedText, absentText string, cookies []E2ECookie, entry SignIn) (E2EObservation, error) {
	if parent == nil || !validExactHTTPSURL(targetURL) || expectedText == "" ||
		strings.ContainsAny(expectedText+absentText, "\x00\r\n") {
		return E2EObservation{}, errors.New("browser observation request is invalid")
	}
	return observeSession(parent, targetURL, expectedText, absentText, cookies, entry)
}

// ObserveForReference is the no-promise observation: open the page with
// the session cookies and capture what it shows. No expectation and no
// verdict — the empty expected text trivially "appears" — so the capture
// is reference material for a human, never promotion-grade proof.
func ObserveForReference(parent context.Context, targetURL string, cookies []E2ECookie, entry SignIn) (E2EObservation, error) {
	if parent == nil || !validExactHTTPSURL(targetURL) {
		return E2EObservation{}, errors.New("browser observation request is invalid")
	}
	return observeSession(parent, targetURL, "", "", cookies, entry)
}

// RenewSession opens a fresh browser with the jar, drives the consumer's
// login and returns the jar as it stands afterwards: the console's own
// session re-minted, the identity provider's session rolled forward. The
// second value is where the browser came to rest (scheme, host and path
// only), for the operator's diagnosis when the login did not land.
func RenewSession(parent context.Context, entry SignIn, cookies []E2ECookie) ([]E2ECookie, string, error) {
	if parent == nil || !entry.configured() {
		return nil, "", errors.New("browser sign-in request is invalid")
	}
	session, err := openBrowser(parent, cookies, entry.Language, signInTimeout)
	if err != nil {
		return nil, "", err
	}
	defer session.close()
	where, err := signIn(session.ctx, session.browser, entry)
	if err != nil {
		return nil, where, err
	}
	renewed, err := exportJar(session.browser, relevantDomains(cookies, entry))
	if err != nil {
		return nil, where, err
	}
	return renewed, where, nil
}

// JarRejected reads a failed login: refused when the login itself said
// so (ErrSignInRefused — the browser parked on a login page, an identity
// provider, a portal, or a callback carrying an error), or when the
// browser came to rest on a web host other than the login entry's and
// the landing's. A browser that never got anywhere (a browser-internal
// error page, no location at all) says nothing about the jar: the login
// was unreachable, and that is not the operator's session to renew.
func JarRejected(err error, where string, entry SignIn) bool {
	if errors.Is(err, ErrSignInRefused) {
		return true
	}
	parsed, parseErr := url.Parse(where)
	if parseErr != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Hostname() == "" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host != hostOf(entry.LoginURL) && host != hostOf(entry.LandedPrefix)
}

// refusedAt reads the browser's resting place when a login timed out:
// a login page (on any host), a callback carrying an error, or a web
// host that is neither the login's nor the landing's, is the destination
// asking for a person — the jar refused. Anything else (still on the
// login host, a browser-internal page) is unreachable, not refused.
func refusedAt(location string, entry SignIn) bool {
	if stillSigningIn(location, entry) {
		return true
	}
	parsed, err := url.Parse(location)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Hostname() == "" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host != hostOf(entry.LoginURL) && host != hostOf(entry.LandedPrefix)
}

func hostOf(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(parsed.Hostname())
}

// SafeURL is a URL with its query and fragment removed: what may be
// written to a log or a ticket. A login round trip parks on callback URLs
// carrying authorization codes and states; those never leave the browser.
func SafeURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host + parsed.Path
}

// relevantDomains is where the kept jar may hold cookies: the domains the
// input jar already had, the login entry's host and the landing's host.
// A login round trip may touch other sites on the way; their cookies are
// not the observation's business and are not kept.
func relevantDomains(cookies []E2ECookie, entry SignIn) []string {
	seen := map[string]bool{}
	var domains []string
	add := func(domain string) {
		domain = strings.ToLower(strings.TrimPrefix(domain, "."))
		if domain == "" || seen[domain] {
			return
		}
		seen[domain] = true
		domains = append(domains, domain)
	}
	for _, cookie := range cookies {
		add(cookie.Domain)
	}
	add(hostOf(entry.LoginURL))
	add(hostOf(entry.LandedPrefix))
	return domains
}

// keepCookie reports whether a cookie's domain is one of the relevant
// domains or sits under (or above) one of them on a label boundary.
func keepCookie(domain string, relevant []string) bool {
	domain = strings.ToLower(strings.TrimPrefix(domain, "."))
	for _, candidate := range relevant {
		if domain == candidate || strings.HasSuffix(domain, "."+candidate) || strings.HasSuffix(candidate, "."+domain) {
			return true
		}
	}
	return false
}

// exportJar reads the browser's cookie jar back into storageState form:
// what a login left behind, for the next login to start from.
func exportJar(browserContext context.Context, relevant []string) ([]E2ECookie, error) {
	var jar []*network.Cookie
	if err := chromedp.Run(browserContext, chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		jar, err = storage.GetCookies().Do(ctx)
		return err
	})); err != nil {
		return nil, errors.New("browser cookie export failed")
	}
	exported := make([]E2ECookie, 0, len(jar))
	for _, cookie := range jar {
		if cookie == nil || !keepCookie(cookie.Domain, relevant) {
			continue
		}
		exported = append(exported, E2ECookie{
			Name: cookie.Name, Value: cookie.Value, Domain: cookie.Domain, Path: cookie.Path,
			Secure: cookie.Secure, HTTPOnly: cookie.HTTPOnly, Expires: cookie.Expires,
			SameSite: sameSiteWord(cookie.SameSite),
		})
	}
	if len(exported) == 0 {
		return nil, errors.New("browser sign-in left no cookies")
	}
	return exported, nil
}

// signInAndKeep drives the login and, when it lands and the entry names a
// place, keeps the jar it left behind, stamped with the seed's digest. A
// jar that cannot be kept does not fail the observation — the login
// itself landed. A copy somebody else rewrote while this login ran is
// left alone: it grew from a newer jar than this one started with.
func signInAndKeep(session *browserSession, entry SignIn, cookies []E2ECookie) (string, error) {
	before := fileStamp(entry.KeepJarAt)
	where, err := signIn(session.ctx, session.browser, entry)
	if err != nil || entry.KeepJarAt == "" {
		return where, err
	}
	if jar, err := exportJar(session.browser, relevantDomains(cookies, entry)); err == nil && fileStamp(entry.KeepJarAt) == before {
		_ = WriteSessionFile(entry.KeepJarAt, jar, SeedDigest(entry.SeedPath))
	}
	return where, nil
}

// FileStamp identifies a jar file's current content well enough to notice
// that somebody else rewrote it: size and modification time, or "" when
// the file is absent. The two writers (the attendant's renewal and an
// observation's own login) each keep their jar only if the file is still
// the one they started from.
func FileStamp(path string) string { return fileStamp(path) }

func fileStamp(path string) string {
	if path == "" {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return info.ModTime().UTC().Format(time.RFC3339Nano) + "/" + strconv.FormatInt(info.Size(), 10)
}

func sameSiteWord(value network.CookieSameSite) string {
	switch value {
	case network.CookieSameSiteStrict:
		return "Strict"
	case network.CookieSameSiteLax:
		return "Lax"
	case network.CookieSameSiteNone:
		return "None"
	default:
		return ""
	}
}

func sameSiteValue(word string) network.CookieSameSite {
	switch word {
	case "Strict":
		return network.CookieSameSiteStrict
	case "Lax":
		return network.CookieSameSiteLax
	case "None":
		return network.CookieSameSiteNone
	default:
		return ""
	}
}

// browserSession is one clean Chrome profile with the jar installed, the
// document responses it received, and the cancel chain that tears it down.
type browserSession struct {
	ctx       context.Context
	browser   context.Context
	version   string
	cancel    []func()
	responses *documentResponses
}

type documentResponses struct {
	mu   sync.Mutex
	seen []documentResponse
}

func (d *documentResponses) reset() {
	d.mu.Lock()
	d.seen = d.seen[:0]
	d.mu.Unlock()
}

func (d *documentResponses) snapshot() []documentResponse {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]documentResponse(nil), d.seen...)
}

func (s *browserSession) close() {
	for index := len(s.cancel) - 1; index >= 0; index-- {
		s.cancel[index]()
	}
}

// openBrowser starts Chrome on a throwaway profile (removed afterwards),
// records every document response, and installs the jar's live cookies.
// The language, when given, is what the browser asks every page for; the
// extra time is added to the observation's budget for a login round trip.
func openBrowser(parent context.Context, cookies []E2ECookie, language string, extra time.Duration) (*browserSession, error) {
	if err := validSessionCookies(cookies); err != nil {
		return nil, err
	}
	executable, err := fixedChromeExecutable()
	if err != nil {
		return nil, errors.New("browser executable is invalid")
	}
	profile, err := os.MkdirTemp("", "e2e-check-chrome-")
	if err != nil {
		return nil, errors.New("browser profile could not be created")
	}
	session := &browserSession{responses: &documentResponses{}}
	session.cancel = append(session.cancel, func() { _ = os.RemoveAll(profile) })
	ctx, cancel := context.WithTimeout(parent, browserTimeout+extra)
	session.cancel = append(session.cancel, cancel)
	allocator, cancelAllocator := chromedp.NewExecAllocator(ctx, browserOptions(executable, profile, language)...)
	session.cancel = append(session.cancel, cancelAllocator)
	browserContext, cancelBrowser := chromedp.NewContext(allocator)
	session.cancel = append(session.cancel, cancelBrowser)
	session.ctx, session.browser = ctx, browserContext

	responses := session.responses
	chromedp.ListenTarget(browserContext, func(event any) {
		response, ok := event.(*network.EventResponseReceived)
		if !ok || response.Type != network.ResourceTypeDocument || response.Response == nil {
			return
		}
		responses.mu.Lock()
		responses.seen = append(responses.seen, documentResponse{
			url: response.Response.URL, status: int(response.Response.Status), mimeType: response.Response.MimeType,
		})
		responses.mu.Unlock()
	})

	prepare := []chromedp.Action{
		network.Enable(),
		network.SetCacheDisabled(true),
		chromedp.EmulateViewport(1440, 1000),
		chromedp.ActionFunc(func(ctx context.Context) error {
			_, product, _, _, _, err := browser.GetVersion().Do(ctx)
			if err != nil {
				return err
			}
			session.version, err = parseChromeProduct(product)
			return err
		}),
	}
	prepare = append(prepare, setCookieActions(cookies, time.Now())...)
	if err := chromedp.Run(browserContext, prepare...); err != nil {
		session.close()
		return nil, errors.New("browser observation failed")
	}
	return session, nil
}

// browserOptions is the Chrome command line every observation starts
// with. Flag values are strings or booleans only: the allocator refuses
// any other type before it even starts Chrome, and the refusal used to be
// indistinguishable from a wrong page — the pod's browser had never
// launched once (35 runs, no screenshot) before a container test showed
// "invalid exec pool flag" for a cache-size flag given as a number.
func browserOptions(executable, profile, language string) []chromedp.ExecAllocatorOption {
	options := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	// No incognito window: the throwaway profile is the isolation, and an
	// incognito context keeps its cookies where the jar export cannot read
	// them back.
	options = append(options,
		chromedp.ExecPath(executable),
		chromedp.UserDataDir(profile),
		chromedp.Flag("disable-application-cache", true),
		chromedp.Flag("disk-cache-size", "1"),
	)
	if language != "" {
		// The UI locale and the Accept-Language header both follow the
		// consumer's language; a page's own detection reads either.
		options = append(options, chromedp.Flag("lang", language), chromedp.Flag("accept-lang", language))
	}
	return options
}

// signIn opens the login entry and waits for the browser to come to rest
// under the landing prefix: the document complete and the location still
// there after the settle window. Returns where the browser is either way,
// as a URL safe to write down.
func signIn(ctx context.Context, browserContext context.Context, entry SignIn) (string, error) {
	if !entry.configured() {
		return "", nil
	}
	if !worker.ValidLoginURL(entry.LoginURL) || entry.LandedPrefix == "" {
		return "", errors.New("browser sign-in request is invalid")
	}
	if err := chromedp.Run(browserContext, chromedp.Navigate(entry.LoginURL)); err != nil {
		return "", errors.New("browser sign-in failed")
	}
	deadline := time.Now().Add(signInTimeout)
	var location string
	var settled time.Time
	for {
		var state struct {
			Location string `json:"location"`
			Complete bool   `json:"complete"`
		}
		script := `({location: String(window.location.href), complete: document.readyState === "complete"})`
		if err := chromedp.Run(browserContext, chromedp.Evaluate(script, &state)); err != nil {
			return SafeURL(location), errors.New("browser sign-in failed")
		}
		location = state.Location
		switch {
		case !landed(location, entry.LandedPrefix) || stillSigningIn(location, entry) || !state.Complete:
			settled = time.Time{}
		case settled.IsZero():
			settled = time.Now()
		case time.Since(settled) >= signInSettle:
			return SafeURL(location), nil
		}
		if time.Now().After(deadline) {
			if refusedAt(location, entry) {
				return SafeURL(location), ErrSignInRefused
			}
			return SafeURL(location), ErrSignInDidNotLand
		}
		select {
		case <-ctx.Done():
			return SafeURL(location), errors.New("browser sign-in timed out")
		case <-time.After(e2ePollInterval):
		}
	}
}

// landed reports whether the browser is on the environment the login was
// supposed to reach. The prefix is an origin (no trailing slash), so the
// origin itself, its root, and any path under it count; a longer host
// name that merely starts with the same letters does not.
func landed(location, prefix string) bool {
	if prefix == "" || location == prefix {
		return prefix != ""
	}
	return strings.HasPrefix(location, strings.TrimRight(prefix, "/")+"/") ||
		strings.HasPrefix(location, strings.TrimRight(prefix, "/")+"?")
}

// stillSigningIn reports a browser that has not left the login even
// though it is on the destination's origin: parked on the entry itself
// (an admin console's login page lives on the console's own origin), on a
// page whose path names a login, or returned with an error parameter —
// the OpenID convention for a round trip the identity provider or the
// callback refused.
func stillSigningIn(location string, entry SignIn) bool {
	// The login page itself, with or without its query and trailing
	// slash — not everything under its path: a landing may well live
	// under the login's path, and a login at the root would otherwise
	// cover the whole site.
	if base := loginBase(entry.LoginURL); base != "" {
		if sameDocument(location, base) || strings.HasPrefix(location, base+"?") || strings.HasPrefix(location, base+"/?") {
			return true
		}
	}
	parsed, err := url.Parse(location)
	if err != nil {
		return false
	}
	if parsed.Query().Get("error") != "" {
		return true
	}
	if fragment, err := url.ParseQuery(parsed.Fragment); err == nil && fragment.Get("error") != "" {
		return true
	}
	// A page whose last path segment names a login, or an OpenID
	// interaction path — not a landing that merely lives under one.
	segments := strings.Split(strings.Trim(strings.ToLower(parsed.Path), "/"), "/")
	last := segments[len(segments)-1]
	if last == "login" || last == "signin" || last == "sign-in" {
		return true
	}
	for _, segment := range segments {
		if segment == "oidc" {
			return true
		}
	}
	return false
}

// loginBase is the login entry without its query: the page a browser
// that still needs a person stays on.
func loginBase(loginURL string) string {
	parsed, err := url.Parse(loginURL)
	if err != nil || parsed.Host == "" {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host + strings.TrimRight(parsed.Path, "/")
}

// sameDocument reports whether the browser is on the page it was sent to,
// allowing what a server or a router adds without changing the page: a
// trailing slash and a fragment.
func sameDocument(finalURL, targetURL string) bool {
	normalize := func(raw string) string {
		parsed, err := url.Parse(raw)
		if err != nil {
			return raw
		}
		parsed.Fragment, parsed.RawFragment = "", ""
		parsed.Path = strings.TrimRight(parsed.Path, "/")
		parsed.RawPath = ""
		return parsed.String()
	}
	return normalize(finalURL) == normalize(targetURL)
}

// sameOrigin reports whether two URLs share scheme and host.
func sameOrigin(a, b string) bool {
	first, err := url.Parse(a)
	if err != nil {
		return false
	}
	second, err := url.Parse(b)
	if err != nil {
		return false
	}
	return first.Scheme == second.Scheme && strings.EqualFold(first.Host, second.Host)
}

func observeSession(parent context.Context, targetURL, expectedText, absentText string, cookies []E2ECookie, entry SignIn) (E2EObservation, error) {
	var extra time.Duration
	if entry.configured() {
		extra = signInTimeout
	}
	session, err := openBrowser(parent, cookies, entry.Language, extra)
	if err != nil {
		return E2EObservation{}, err
	}
	defer session.close()
	observation := E2EObservation{RequestedURL: targetURL, BrowserVersion: session.version}
	if entry.configured() {
		where, err := signInAndKeep(session, entry, cookies)
		if err != nil {
			// A login that did not land is a RESULT: the screenshot of the
			// identity provider's page (or the portal) is the diagnosis.
			observation.Block, observation.FinalURL = BlockSignIn, where
			observation.ScreenshotPNG, _ = fullPageScreenshot(session.browser)
			observation.ObservedAt = time.Now().UTC()
			return observation, nil
		}
	}
	session.responses.reset()
	if err := chromedp.Run(session.browser,
		chromedp.Navigate(targetURL),
		chromedp.WaitReady("body", chromedp.ByQuery),
	); err != nil {
		return E2EObservation{}, errors.New("browser observation failed")
	}

	// The verdict poll: unlike the sealed observation, a page that never
	// shows the expected text is a RESULT here, not an error — the loop
	// runs out and the screenshot below still happens.
	deadline := time.Now().Add(pageReadyTimeout)
	expectedSeen, absentGone := false, false
	for {
		var state struct {
			Complete bool `json:"complete"`
			Expected bool `json:"expected"`
			Absent   bool `json:"absent"`
		}
		script := `({
			complete: document.readyState === "complete" && !!document.body,
			expected: !!document.body && (document.body.innerText || "").includes(` + jsString(expectedText) + `),
			absent: ` + jsString(absentText) + ` === "" || !document.body || !(document.body.innerText || "").includes(` + jsString(absentText) + `)
		})`
		if err := chromedp.Run(session.browser, chromedp.Evaluate(script, &state)); err != nil {
			return E2EObservation{}, errors.New("browser observation failed")
		}
		if state.Complete && state.Expected && state.Absent {
			expectedSeen, absentGone = true, true
			break
		}
		expectedSeen, absentGone = state.Expected, state.Absent
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-session.ctx.Done():
			return E2EObservation{}, errors.New("browser observation timed out")
		case <-time.After(e2ePollInterval):
		}
	}

	var finalURL string
	if err := chromedp.Run(session.browser, chromedp.Location(&finalURL)); err != nil {
		return E2EObservation{}, errors.New("browser observation failed")
	}
	screenshot, err := fullPageScreenshot(session.browser)
	if err != nil {
		return E2EObservation{}, err
	}
	observation.ScreenshotPNG = screenshot
	observation.ExpectedSeen, observation.AbsentGone = expectedSeen, absentGone
	observation.ObservedAt = time.Now().UTC()
	observedResponses := session.responses.snapshot()
	if !sameDocument(finalURL, targetURL) {
		// The promised page was never shown. Off the destination's own
		// origin the browser was sent away — not let in; on it, sent to
		// another page. Either way the flags above describe the page that
		// WAS shown, and the Block says it was not the target.
		observation.Block = BlockRedirect
		if !sameOrigin(finalURL, targetURL) {
			observation.Block = BlockSignIn
		}
		observation.FinalURL = SafeURL(finalURL)
		observation.StatusCode = anyDocumentStatus(observedResponses, finalURL)
		return observation, nil
	}
	status, err := exactDocumentStatus(observedResponses, finalURL)
	if err != nil {
		return E2EObservation{}, err
	}
	observation.FinalURL, observation.StatusCode = SafeURL(finalURL), status
	return observation, nil
}

// fullPageScreenshot captures the whole document after checking that its
// size is one the evidence rules accept.
func fullPageScreenshot(browserContext context.Context) ([]byte, error) {
	var screenshot []byte
	var dimensions struct {
		Width  int64 `json:"width"`
		Height int64 `json:"height"`
	}
	if err := chromedp.Run(browserContext,
		chromedp.Evaluate(`({
			width: Math.ceil(Math.max(document.documentElement.scrollWidth, document.body ? document.body.scrollWidth : 1)),
			height: Math.ceil(Math.max(document.documentElement.scrollHeight, document.body ? document.body.scrollHeight : 1))
		})`, &dimensions),
		chromedp.ActionFunc(func(context.Context) error {
			if dimensions.Width < 1 || dimensions.Height < 1 || dimensions.Width > maxScreenshotWidth ||
				dimensions.Height > maxScreenshotHeight || dimensions.Width*dimensions.Height > maxScreenshotPixels {
				return errors.New("rendered page dimensions are invalid")
			}
			return nil
		}),
		chromedp.FullScreenshot(&screenshot, screenshotQuality),
	); err != nil {
		return nil, errors.New("browser observation failed")
	}
	return screenshot, nil
}

// anyDocumentStatus is the lenient reading for a page the browser was
// sent to: the status of its document when exactly one was seen, zero
// otherwise. Never used for evidence.
func anyDocumentStatus(responses []documentResponse, finalURL string) int {
	status, err := exactDocumentStatus(responses, finalURL)
	if err != nil {
		return 0
	}
	return status
}

func cookiePath(path string) string {
	if path == "" {
		return "/"
	}
	return path
}

// jsString renders a Go string as a safe JS string literal for the verdict
// script. JSON string encoding IS a valid JS string literal, escapes
// included, so the standard library does the quoting.
func jsString(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return `""`
	}
	return string(encoded)
}
