package visiblecheck

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// The observation browser's jar: what goes into Chrome, which of the two
// jar files is in use, how a renewed jar is kept, and how a login's
// landing and refusal are read.

func TestInstallableCookiesDropOnlyTheExpired(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	cookies := []E2ECookie{
		{Name: "dead", Domain: "example.invalid", Expires: float64(now.Add(-time.Hour).Unix())},
		{Name: "live", Domain: "example.invalid", Expires: float64(now.Add(time.Hour).Unix()), SameSite: "Lax"},
		{Name: "session", Domain: "example.invalid", Expires: -1},
		{Name: "unset", Domain: "example.invalid"},
	}
	kept := installableCookies(cookies, now)
	if len(kept) != 3 || kept[0].Name != "live" || kept[1].Name != "session" || kept[2].Name != "unset" {
		t.Fatalf("installable = %+v", kept)
	}
	if len(setCookieActions(cookies, now)) != 3 {
		t.Fatal("the browser actions must follow the installable set")
	}
	if sameSiteValue("Lax") == "" || sameSiteValue("") != "" || sameSiteWord(sameSiteValue("Strict")) != "Strict" {
		t.Fatal("sameSite must round-trip between the file and the browser")
	}
}

func TestLandedAcceptsTheOriginAndItsPathsOnly(t *testing.T) {
	origin := "https://console.example.invalid"
	for _, location := range []string{origin, origin + "/", origin + "/console/new", origin + "/console/new?embedded=1", origin + "?x=1"} {
		if !landed(location, origin) {
			t.Errorf("landed(%q) = false", location)
		}
	}
	for _, location := range []string{"", "https://console.example.invalid.evil.test/", "https://portal.example.invalid/", "https://idp.example.invalid/sign-in?app=console.example.invalid"} {
		if landed(location, origin) {
			t.Errorf("landed(%q) = true", location)
		}
	}
	if landed(origin, "") {
		t.Fatal("an empty prefix lands nowhere")
	}
}

// An admin console keeps its login page on its own origin, and a refused
// round trip comes back to the origin with an error parameter: neither is
// a landing, even though both pass the origin check.
func TestStillSigningInKeepsTheLoginPageAndErrorsOffTheLanding(t *testing.T) {
	edge := SignIn{LoginURL: "https://edge-stg.example.invalid/admin/auth/login?returnTo=%2Fadmin%2F", LandedPrefix: "https://edge-stg.example.invalid"}
	for _, location := range []string{
		"https://edge-stg.example.invalid/admin/auth/login?returnTo=%2Fadmin%2F",
		"https://edge-stg.example.invalid/admin/auth/login",
		"https://edge-stg.example.invalid/admin/auth/login/",
		"https://edge-stg.example.invalid/admin/?error=not_admin",
		"https://edge-stg.example.invalid/signin",
		"https://edge-stg.example.invalid/oidc/auth/abc",
	} {
		if !landed(location, edge.LandedPrefix) || !stillSigningIn(location, edge) {
			t.Errorf("%q must count as still signing in", location)
		}
	}
	for _, location := range []string{"https://edge-stg.example.invalid/admin/", "https://edge-stg.example.invalid/admin/keys?tab=usage", "https://edge-stg.example.invalid/admin/auth/login/complete"} {
		if stillSigningIn(location, edge) {
			t.Errorf("%q is a landing", location)
		}
	}
	console := SignIn{LoginURL: "https://api-stg.example.invalid/console/auth/login?returnTo=/console/new", LandedPrefix: "https://stg.example.invalid"}
	if stillSigningIn("https://stg.example.invalid/console/new", console) || !stillSigningIn("https://stg.example.invalid/console?error=no_identity", console) ||
		!stillSigningIn("https://stg.example.invalid/console#error=access_denied&state=x", console) {
		t.Fatal("a console landing is a landing; a callback error in the query or the fragment is not")
	}
	// A login at the root covers the root only, never the whole site.
	root := SignIn{LoginURL: "https://site.example.invalid/", LandedPrefix: "https://site.example.invalid"}
	if !stillSigningIn("https://site.example.invalid/", root) || !stillSigningIn("https://site.example.invalid/?next=%2Fapp", root) || stillSigningIn("https://site.example.invalid/app", root) {
		t.Fatal("a root login must not swallow the pages under it")
	}
	if loginBase("not a url") != "" || loginBase("https://a.example.invalid/x/") != "https://a.example.invalid/x" {
		t.Fatal("loginBase strips the query and a trailing slash")
	}
}

// A refusal is the login saying so (the browser parked on a login page —
// on any host, an admin console's included — a callback carrying an
// error, the identity provider, a portal), or the browser at rest on a
// web host that is neither the login's nor the landing's. Nowhere, a
// browser-internal error page, or the login host with a page that is not
// a login, is an unreachable login, not a refused jar.
func TestJarRejectedReadsWhereTheBrowserCameToRest(t *testing.T) {
	console := SignIn{LoginURL: "https://api-stg.example.invalid/console/auth/login?returnTo=/console/new", LandedPrefix: "https://stg.example.invalid"}
	edge := SignIn{LoginURL: "https://edge-stg.example.invalid/admin/auth/login?returnTo=%2Fadmin%2F", LandedPrefix: "https://edge-stg.example.invalid"}
	for _, location := range []string{
		"https://idp.example.invalid/sign-in", "https://portal.example.invalid/", "https://social.example.test/workspace-signin?redir=x",
		"https://api-stg.example.invalid/console/auth/login?returnTo=/console/new", "https://stg.example.invalid/console?error=no_identity",
	} {
		if !refusedAt(location, console) {
			t.Errorf("refusedAt(%q) = false for the console", location)
		}
	}
	for _, location := range []string{"", "chrome-error://chromewebdata/", "https://api-stg.example.invalid/console/auth/healthz", "https://api-stg.example.invalid/console/auth/callback", "not a url"} {
		if refusedAt(location, console) {
			t.Errorf("refusedAt(%q) = true for the console", location)
		}
	}
	// An admin console's login page lives on the landing host: parked
	// there is refused all the same.
	if !refusedAt("https://edge-stg.example.invalid/admin/auth/login?returnTo=%2Fadmin%2F", edge) || refusedAt("https://edge-stg.example.invalid/admin/status", edge) {
		t.Fatal("the login page on the landing host is a refusal; another page there is not")
	}
	// JarRejected: the login's own verdict first, the resting host second.
	if !JarRejected(ErrSignInRefused, "https://edge-stg.example.invalid/admin/auth/login", edge) {
		t.Fatal("a refused login is a rejected jar wherever it parked")
	}
	if JarRejected(ErrSignInDidNotLand, "https://edge-stg.example.invalid/admin/status", edge) || JarRejected(ErrSignInDidNotLand, "", edge) ||
		JarRejected(ErrSignInDidNotLand, "chrome-error://chromewebdata/", edge) || JarRejected(errors.New("browser sign-in failed"), "", edge) {
		t.Fatal("an unreachable login is not a rejected jar")
	}
	if !JarRejected(ErrSignInDidNotLand, "https://portal.example.invalid/", edge) {
		t.Fatal("a browser at rest on a foreign web host is a rejected jar")
	}
}

func TestSafeURLDropsQueryAndFragment(t *testing.T) {
	if got := SafeURL("https://api.example.invalid/auth/callback?code=SECRET&state=s#frag"); got != "https://api.example.invalid/auth/callback" {
		t.Fatalf("SafeURL = %q", got)
	}
	if SafeURL("") != "" || SafeURL("relative/path") != "" {
		t.Fatal("no scheme or host, nothing safe to say")
	}
}

// A trailing slash or a fragment added by the server or the router does
// not make another page; another origin does make another site.
func TestSameDocumentAndSameOrigin(t *testing.T) {
	target := "https://stg.example.invalid/console/new"
	for _, final := range []string{target, target + "/", target + "#step-1", target + "/#top"} {
		if !sameDocument(final, target) {
			t.Errorf("sameDocument(%q) = false", final)
		}
	}
	for _, final := range []string{"https://stg.example.invalid/console/onboard", target + "?embedded=1", "https://portal.example.invalid/"} {
		if sameDocument(final, target) {
			t.Errorf("sameDocument(%q) = true", final)
		}
	}
	if !sameOrigin("https://stg.example.invalid/console/onboard", target) || sameOrigin("https://portal.example.invalid/", target) || sameOrigin("http://stg.example.invalid/", target) {
		t.Fatal("sameOrigin follows scheme and host")
	}
}

// The kept jar holds cookies for the domains the seed had, the login host
// and the landing — never for a site the round trip merely passed through.
func TestRelevantDomainsAndKeepCookie(t *testing.T) {
	entry := SignIn{LoginURL: "https://api-stg.example.invalid/login", LandedPrefix: "https://stg.example.invalid"}
	seed := []E2ECookie{{Name: "s", Domain: ".example.invalid"}, {Name: "i", Domain: "idp.example.test"}}
	relevant := relevantDomains(seed, entry)
	if strings.Join(relevant, " ") != "example.invalid idp.example.test api-stg.example.invalid stg.example.invalid" {
		t.Fatalf("relevant = %v", relevant)
	}
	for _, domain := range []string{".example.invalid", "example.invalid", "stg.example.invalid", "api-stg.example.invalid", "idp.example.test", ".idp.example.test", "example.test"} {
		if !keepCookie(domain, relevant) {
			t.Errorf("keepCookie(%q) = false", domain)
		}
	}
	for _, domain := range []string{"social.example.net", ".other.invalid", "example.invalid.evil.test"} {
		if keepCookie(domain, relevant) {
			t.Errorf("keepCookie(%q) = true", domain)
		}
	}
}

func writeJar(t *testing.T, path, name, seedDigest string) {
	t.Helper()
	if err := WriteSessionFile(path, []E2ECookie{{Name: name, Value: "v", Domain: "example.invalid", Path: "/", Expires: 1893456000}}, seedDigest); err != nil {
		t.Fatal(err)
	}
}

// The renewed copy is the jar for as long as the seed it grew from is the
// seed mounted now; a seed the operator replaced wins over the copy. File
// times play no part: a secret mount is rewritten on every pod start.
func TestLoadSessionCookiesFollowsTheSeedDigestNotFileTimes(t *testing.T) {
	dir := t.TempDir()
	seed := filepath.Join(dir, "seed.json")
	renewed := filepath.Join(dir, "renewed.json")
	writeJar(t, seed, "seed", "")
	if cookies, note := LoadSessionCookies(seed, ""); len(cookies) != 1 || cookies[0].Name != "seed" || note != "" || cookies[0].Expires != 1893456000 {
		t.Fatalf("seed alone = %+v %q", cookies, note)
	}
	if cookies, _ := LoadSessionCookies(seed, renewed); len(cookies) != 1 || cookies[0].Name != "seed" {
		t.Fatalf("a missing renewal leaves the seed in charge: %+v", cookies)
	}
	writeJar(t, renewed, "renewed", SeedDigest(seed))
	// The seed is rewritten with the same content later (a pod restart).
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(seed, future, future); err != nil {
		t.Fatal(err)
	}
	if cookies, _ := LoadSessionCookies(seed, renewed); len(cookies) != 1 || cookies[0].Name != "renewed" {
		t.Fatalf("the renewal from the mounted seed wins, whatever the file times: %+v", cookies)
	}
	writeJar(t, seed, "fresh-seed", "")
	if cookies, _ := LoadSessionCookies(seed, renewed); len(cookies) != 1 || cookies[0].Name != "fresh-seed" {
		t.Fatalf("an operator's fresh seed beats a renewal of the old one: %+v", cookies)
	}
	writeJar(t, renewed, "renewed-again", SeedDigest(seed))
	if cookies, _ := LoadSessionCookies(seed, renewed); len(cookies) != 1 || cookies[0].Name != "renewed-again" {
		t.Fatalf("a renewal of the fresh seed wins again: %+v", cookies)
	}
	if cookies, _ := LoadSessionCookies("", renewed); len(cookies) != 1 || cookies[0].Name != "renewed-again" {
		t.Fatalf("without a seed the renewal is the jar: %+v", cookies)
	}
	if cookies, _ := LoadSessionCookies(filepath.Join(dir, "absent.json"), renewed); len(cookies) != 1 || cookies[0].Name != "renewed-again" {
		t.Fatalf("an unreadable seed leaves the renewal in charge: %+v", cookies)
	}
	if cookies, note := LoadSessionCookies("", ""); cookies != nil || note == "" {
		t.Fatalf("no jar at all must say so: %+v %q", cookies, note)
	}
	if SeedDigest("") != "" || SeedDigest(filepath.Join(dir, "absent.json")) != "" || len(SeedDigest(seed)) != 64 {
		t.Fatal("SeedDigest is a hex sha256 of a readable seed and empty otherwise")
	}
}

func TestWriteSessionFileRoundTripsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "session.json")
	cookies := []E2ECookie{
		{Name: "a", Value: "1", Domain: ".example.invalid", Path: "/", Secure: true, HTTPOnly: true, Expires: 1893456000, SameSite: "Lax"},
		{Name: "b", Value: "2", Domain: "idp.example.invalid", Path: "/", Expires: -1},
	}
	if err := WriteSessionFile(path, cookies, "abc123"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("session file mode = %v (%v)", info.Mode(), err)
	}
	if parent, err := os.Stat(filepath.Dir(path)); err != nil || parent.Mode().Perm() != 0o700 {
		t.Fatalf("session directory mode = %v (%v)", parent.Mode(), err)
	}
	loaded, note := LoadSessionCookies(path, "")
	if note != "" || len(loaded) != 2 || loaded[0] != cookies[0] || loaded[1] != cookies[1] {
		t.Fatalf("round trip = %+v %q", loaded, note)
	}
	raw, _ := os.ReadFile(path)
	var state struct {
		Cookies    []map[string]any `json:"cookies"`
		SeedSHA256 string           `json:"seed_sha256"`
	}
	if json.Unmarshal(raw, &state) != nil || state.Cookies[0]["expires"] != float64(1893456000) || state.Cookies[0]["sameSite"] != "Lax" || state.SeedSHA256 != "abc123" {
		t.Fatalf("stored jar = %s", raw)
	}
	if err := WriteSessionFile(path, nil, ""); err == nil {
		t.Fatal("an empty jar must not replace a good one")
	}
	if err := WriteSessionFile("relative.json", cookies, ""); err == nil {
		t.Fatal("a relative path must be refused")
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Fatalf("temporary files must not linger: %d entries", len(entries))
	}
	// The leftover of a writer killed between create and rename is swept
	// by the next writer; a fresh temporary of a writer still at work is
	// left alone.
	stale := filepath.Join(filepath.Dir(path), ".session-dead.json")
	fresh := filepath.Join(filepath.Dir(path), ".session-busy.json")
	for _, name := range []string{stale, fresh} {
		if err := os.WriteFile(name, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-staleTemporaryAge - time.Minute)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if err := WriteSessionFile(path, cookies, "abc123"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Fatal("a stale temporary must be swept")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("a fresh temporary must be left alone")
	}
	if FileStamp(path) == "" || FileStamp(filepath.Join(filepath.Dir(path), "absent.json")) != "" {
		t.Fatal("FileStamp names a present file and nothing else")
	}
	stampBefore := FileStamp(path)
	if err := WriteSessionFile(path, append(cookies, E2ECookie{Name: "c", Value: "3", Domain: "example.invalid", Path: "/"}), "abc123"); err != nil {
		t.Fatal(err)
	}
	if FileStamp(path) == stampBefore {
		t.Fatal("a rewritten jar must carry a different stamp")
	}
}

// The allocator validates the flag list before it starts Chrome, so a
// launch against a path that does not exist must fail at the exec — an
// "invalid exec pool flag" here would mean no observation ever launches.
func TestBrowserOptionsAreAcceptedByTheAllocator(t *testing.T) {
	for _, language := range []string{"", "ja"} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		allocator, cancelAllocator := chromedp.NewExecAllocator(ctx, browserOptions(filepath.Join(t.TempDir(), "no-such-chrome"), t.TempDir(), language)...)
		browserContext, cancelBrowser := chromedp.NewContext(allocator)
		err := chromedp.Run(browserContext, chromedp.ActionFunc(func(context.Context) error { return nil }))
		cancelBrowser()
		cancelAllocator()
		cancel()
		if err == nil || strings.Contains(err.Error(), "invalid exec pool flag") {
			t.Fatalf("language=%q: the flags must reach the exec, got %v", language, err)
		}
	}
}

func TestRenewSessionRejectsANilContextAndAnEmptyEntry(t *testing.T) {
	if _, _, err := RenewSession(nil, SignIn{LoginURL: "https://a.example.invalid/login", LandedPrefix: "https://a.example.invalid"}, nil); err == nil {
		t.Fatal("a nil context must be refused")
	}
	if _, _, err := RenewSession(context.Background(), SignIn{}, nil); err == nil {
		t.Fatal("an entry without a login must be refused before any browser")
	}
	if !(SignIn{LoginURL: "https://a.example.invalid/login"}).configured() || (SignIn{}).configured() {
		t.Fatal("configured() follows the login URL")
	}
}
