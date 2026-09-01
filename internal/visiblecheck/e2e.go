package visiblecheck

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/network"
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

// E2ECookie is one session cookie to install before navigation.
type E2ECookie struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Domain   string `json:"domain"`
	Path     string `json:"path"`
	Secure   bool   `json:"secure"`
	HTTPOnly bool   `json:"httpOnly"`
}

// E2EObservation is what the browser actually saw.
type E2EObservation struct {
	RequestedURL   string
	FinalURL       string
	StatusCode     int
	ExpectedSeen   bool
	AbsentGone     bool
	ScreenshotPNG  []byte
	BrowserVersion string
	ObservedAt     time.Time
}

// e2ePollInterval matches the sealed observation's cadence.
const e2ePollInterval = 250 * time.Millisecond

func validSessionCookies(cookies []E2ECookie) error {
	for _, cookie := range cookies {
		if cookie.Name == "" || cookie.Domain == "" ||
			strings.ContainsAny(cookie.Name+cookie.Value+cookie.Domain+cookie.Path, "\x00\r\n") {
			return errors.New("session cookie is invalid")
		}
	}
	return nil
}

func setCookieActions(cookies []E2ECookie) []chromedp.Action {
	actions := make([]chromedp.Action, 0, len(cookies))
	for _, cookie := range cookies {
		cookie := cookie
		actions = append(actions, chromedp.ActionFunc(func(ctx context.Context) error {
			return network.SetCookie(cookie.Name, cookie.Value).
				WithDomain(cookie.Domain).
				WithPath(cookiePath(cookie.Path)).
				WithSecure(cookie.Secure).
				WithHTTPOnly(cookie.HTTPOnly).
				Do(ctx)
		}))
	}
	return actions
}

// LoadSessionCookies reads the operator-provisioned session file (Playwright
// storageState JSON). Every failure degrades to "no cookies" with a
// human-readable note — the observation itself then reports the login page
// honestly instead of failing.
func LoadSessionCookies(path string) ([]E2ECookie, string) {
	if path == "" {
		return nil, "確認用セッションが設定されていません。"
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > 1<<20 {
		return nil, "確認用セッションのファイルを読めませんでした。"
	}
	var state struct {
		Cookies []E2ECookie `json:"cookies"`
	}
	if err := json.Unmarshal(raw, &state); err != nil || len(state.Cookies) == 0 {
		return nil, "確認用セッションのファイルの形式が想定と異なります。"
	}
	return state.Cookies, ""
}

// ObserveForE2E opens a clean Chrome profile, installs the given cookies,
// navigates to the target and reports whether the expected text appeared
// (and the absent text disappeared) within the page-ready budget — with a
// full-page screenshot taken either way.
func ObserveForE2E(parent context.Context, targetURL, expectedText, absentText string, cookies []E2ECookie) (E2EObservation, error) {
	if parent == nil || !validExactHTTPSURL(targetURL) || expectedText == "" ||
		strings.ContainsAny(expectedText+absentText, "\x00\r\n") {
		return E2EObservation{}, errors.New("browser observation request is invalid")
	}
	if err := validSessionCookies(cookies); err != nil {
		return E2EObservation{}, err
	}
	executable, err := fixedChromeExecutable()
	if err != nil {
		return E2EObservation{}, errors.New("browser executable is invalid")
	}
	profile, err := os.MkdirTemp("", "e2e-check-chrome-")
	if err != nil {
		return E2EObservation{}, errors.New("browser profile could not be created")
	}
	defer func() { _ = os.RemoveAll(profile) }()

	ctx, cancel := context.WithTimeout(parent, browserTimeout)
	defer cancel()
	options := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	options = append(options,
		chromedp.ExecPath(executable),
		chromedp.UserDataDir(profile),
		chromedp.Flag("incognito", true),
		chromedp.Flag("disable-application-cache", true),
		chromedp.Flag("disk-cache-size", 1),
	)
	allocator, cancelAllocator := chromedp.NewExecAllocator(ctx, options...)
	defer cancelAllocator()
	browserContext, cancelBrowser := chromedp.NewContext(allocator)
	defer cancelBrowser()

	var responseMu sync.Mutex
	responses := make([]documentResponse, 0, 2)
	chromedp.ListenTarget(browserContext, func(event any) {
		response, ok := event.(*network.EventResponseReceived)
		if !ok || response.Type != network.ResourceTypeDocument || response.Response == nil {
			return
		}
		responseMu.Lock()
		responses = append(responses, documentResponse{
			url: response.Response.URL, status: int(response.Response.Status), mimeType: response.Response.MimeType,
		})
		responseMu.Unlock()
	})

	var browserVersion string
	prepare := []chromedp.Action{
		network.Enable(),
		network.SetCacheDisabled(true),
		chromedp.EmulateViewport(1440, 1000),
		chromedp.ActionFunc(func(ctx context.Context) error {
			_, product, _, _, _, err := browser.GetVersion().Do(ctx)
			if err != nil {
				return err
			}
			browserVersion, err = parseChromeProduct(product)
			return err
		}),
	}
	prepare = append(prepare, setCookieActions(cookies)...)
	prepare = append(prepare,
		chromedp.Navigate(targetURL),
		chromedp.WaitReady("body", chromedp.ByQuery),
	)
	if err := chromedp.Run(browserContext, prepare...); err != nil {
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
		if err := chromedp.Run(browserContext, chromedp.Evaluate(script, &state)); err != nil {
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
		case <-ctx.Done():
			return E2EObservation{}, errors.New("browser observation timed out")
		case <-time.After(e2ePollInterval):
		}
	}

	var finalURL string
	var screenshot []byte
	var dimensions struct {
		Width  int64 `json:"width"`
		Height int64 `json:"height"`
	}
	if err := chromedp.Run(browserContext,
		chromedp.Location(&finalURL),
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
		chromedp.FullScreenshot(&screenshot, 90),
	); err != nil {
		return E2EObservation{}, errors.New("browser observation failed")
	}

	responseMu.Lock()
	observedResponses := append([]documentResponse(nil), responses...)
	responseMu.Unlock()
	status, err := exactDocumentStatus(observedResponses, finalURL)
	if err != nil {
		return E2EObservation{}, err
	}
	return E2EObservation{
		RequestedURL: targetURL, FinalURL: finalURL, StatusCode: status,
		ExpectedSeen: expectedSeen, AbsentGone: absentGone,
		ScreenshotPNG: screenshot, BrowserVersion: browserVersion, ObservedAt: time.Now().UTC(),
	}, nil
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
