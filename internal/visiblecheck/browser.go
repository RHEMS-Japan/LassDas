package visiblecheck

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/releaseproof"

	"github.com/chromedp/chromedp"
)

const (
	chromeExecutable = "/opt/google/chrome/chrome"
	browserTimeout   = 90 * time.Second
	pageReadyTimeout = 45 * time.Second
)

// The sealed observation tool's exit codes, read by the runner: a refusal
// worth waiting out (the page opened but did not show the promise — a
// deployment may still be switching over) is told apart from a login the
// destination refused (waiting changes nothing) and from every other
// failure (invalid inputs, unwritable outputs).
const (
	ExitEvidenceRejected = 3
	ExitSignInRefused    = 4
)

// ErrObservationSignIn is the sealed observation's login that did not land.
var ErrObservationSignIn = errors.New("browser sign-in failed")

type documentResponse struct {
	url      string
	status   int
	mimeType string
}

// ObserveAndSealStaging starts a clean Chrome profile, installs the
// operator-provisioned session cookies (the consoles render a login page to
// a credential-free profile), signs in through the consumer's login entry
// when it has one, navigates to the exact configured staging URL, and seals
// that in-process observation against the complete staging release proof.
// The cookies never enter the sealed evidence.
func ObserveAndSealStaging(
	parent context.Context,
	proof releaseproof.StagingProof,
	input releaseproof.StagingInputs,
	cookies []E2ECookie,
	entry SignIn,
) (Evidence, []byte, error) {
	binding, err := stagingBinding(proof, input)
	if err != nil {
		return Evidence{}, nil, err
	}
	request := input.Request
	observed, err := observe(parent, binding.origin+request.VerificationPath, request.ExpectedText, request.AbsentText, cookies, entry)
	if err != nil {
		return Evidence{}, nil, err
	}
	evidence, err := seal(observed, binding, input, time.Now().UTC())
	if err != nil {
		return Evidence{}, nil, err
	}
	return evidence, append([]byte(nil), observed.screenshotPNG...), nil
}

// ObserveAndSealProduction requires the prior staging evidence and screenshot
// as well as the complete production proof. Callers cannot supply a browser
// executable, profile, status, DOM, observation timestamp, or result PNG.
func ObserveAndSealProduction(
	parent context.Context,
	proof releaseproof.ProductionProof,
	staging releaseproof.StagingProof,
	stagingEvidence Evidence,
	stagingScreenshot []byte,
	input releaseproof.StagingInputs,
	cookies []E2ECookie,
	entry SignIn,
) (Evidence, []byte, error) {
	now := time.Now().UTC()
	binding, err := productionBinding(proof, staging, stagingEvidence, stagingScreenshot, input, now)
	if err != nil {
		return Evidence{}, nil, err
	}
	request := input.Request
	observed, err := observe(parent, binding.origin+request.VerificationPath, request.ExpectedText, request.AbsentText, cookies, entry)
	if err != nil {
		return Evidence{}, nil, err
	}
	evidence, err := seal(observed, binding, input, time.Now().UTC())
	if err != nil {
		return Evidence{}, nil, err
	}
	return evidence, append([]byte(nil), observed.screenshotPNG...), nil
}

// observe is the sealed observation: it refuses anything short of the
// target page, at its own URL, showing the promised wording. A login that
// does not land is a refusal like any other — the courtesy observation
// that follows a refusal is where the reason gets told.
func observe(parent context.Context, targetURL, expectedText, absentText string, cookies []E2ECookie, entry SignIn) (capture, error) {
	if parent == nil || !validExactHTTPSURL(targetURL) || expectedText == "" || strings.ContainsAny(expectedText+absentText, "\x00\r\n") {
		return capture{}, errors.New("browser observation request is invalid")
	}
	var extra time.Duration
	if entry.configured() {
		extra = signInTimeout
	}
	session, err := openBrowser(parent, cookies, entry.Language, extra)
	if err != nil {
		return capture{}, err
	}
	defer session.close()
	if entry.configured() {
		if _, err := signInAndKeep(session, entry, cookies); err != nil {
			return capture{}, ErrObservationSignIn
		}
	}
	// Only the target navigation's documents count: the login round trip
	// may well have shown the very same page a moment ago.
	session.responses.reset()

	var finalURL string
	var visibleText string
	var screenshot []byte
	var textBytes int64
	var dimensions struct {
		Width  int64 `json:"width"`
		Height int64 `json:"height"`
	}
	var ready bool
	actions := []chromedp.Action{
		chromedp.Navigate(targetURL),
		chromedp.WaitReady("body", chromedp.ByQuery),
		chromedp.PollFunction(
			`(expected, absent) => {
				if (document.readyState !== "complete" || !document.body) return false;
				const text = document.body.innerText || "";
				return text.includes(expected) && (!absent || !text.includes(absent));
			}`,
			&ready,
			chromedp.WithPollingArgs(expectedText, absentText),
			chromedp.WithPollingInterval(250*time.Millisecond),
			chromedp.WithPollingTimeout(pageReadyTimeout),
		),
		chromedp.Sleep(2 * time.Second),
		chromedp.Location(&finalURL),
		chromedp.Evaluate(`document.body ? new TextEncoder().encode(document.body.innerText || "").length : 0`, &textBytes),
		chromedp.ActionFunc(func(context.Context) error {
			if textBytes < 1 || textBytes > MaxVisibleText {
				return errors.New("rendered text size is invalid")
			}
			return nil
		}),
		chromedp.Evaluate(`document.body.innerText || ""`, &visibleText),
		chromedp.Evaluate(`({
			width: Math.ceil(Math.max(document.documentElement.scrollWidth, document.body.scrollWidth)),
			height: Math.ceil(Math.max(document.documentElement.scrollHeight, document.body.scrollHeight))
		})`, &dimensions),
		chromedp.ActionFunc(func(context.Context) error {
			if dimensions.Width < 1 || dimensions.Height < 1 || dimensions.Width > maxScreenshotWidth ||
				dimensions.Height > maxScreenshotHeight || dimensions.Width*dimensions.Height > maxScreenshotPixels {
				return errors.New("rendered page dimensions are invalid")
			}
			return nil
		}),
		chromedp.FullScreenshot(&screenshot, 90),
	}
	if err := chromedp.Run(session.browser, actions...); err != nil {
		return capture{}, errors.New("browser observation failed")
	}
	if !ready || len([]byte(visibleText)) != int(textBytes) {
		return capture{}, errors.New("browser rendered text is invalid")
	}

	status, err := exactDocumentStatus(session.responses.snapshot(), finalURL)
	if err != nil {
		return capture{}, err
	}
	result := capture{
		requestedURL: targetURL, finalURL: finalURL, statusCode: status,
		visibleText: visibleText, screenshotPNG: screenshot,
		browser: "chrome", BrowserVersion: session.version, observedAt: time.Now().UTC(),
	}
	return result, nil
}

func fixedChromeExecutable() (string, error) {
	resolved, err := filepath.EvalSymlinks(chromeExecutable)
	if err != nil || !filepath.IsAbs(resolved) || filepath.Clean(resolved) != resolved {
		return "", errors.New("Chrome path is invalid")
	}
	// /usr/lib/chromium/ is where Debian's chromium package keeps the real
	// ELF (/usr/bin/chromium is a wrapper script that would fail the size
	// and regular-file checks below).
	if !strings.HasPrefix(resolved, "/opt/google/chrome/") && !strings.HasPrefix(resolved, "/usr/bin/") &&
		!strings.HasPrefix(resolved, "/usr/lib/chromium/") {
		return "", errors.New("Chrome path is not allowlisted")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 || info.Size() < 1024*1024 {
		return "", errors.New("Chrome executable is invalid")
	}
	return resolved, nil
}

func parseChromeProduct(product string) (string, error) {
	version := ""
	for _, prefix := range []string{"HeadlessChrome/", "Chrome/"} {
		if strings.HasPrefix(product, prefix) {
			version = strings.TrimPrefix(product, prefix)
			break
		}
	}
	if !versionPattern.MatchString(version) {
		return "", errors.New("browser version is invalid")
	}
	return version, nil
}

func exactDocumentStatus(responses []documentResponse, finalURL string) (int, error) {
	if !validExactHTTPSURL(finalURL) {
		return 0, errors.New("browser final URL is invalid")
	}
	matches := make([]documentResponse, 0, 1)
	for _, response := range responses {
		if response.url == finalURL {
			matches = append(matches, response)
		}
	}
	if len(matches) != 1 || matches[0].status < 200 || matches[0].status > 299 ||
		(matches[0].mimeType != "text/html" && matches[0].mimeType != "application/xhtml+xml") {
		return 0, errors.New("browser document response is invalid")
	}
	return matches[0].status, nil
}
