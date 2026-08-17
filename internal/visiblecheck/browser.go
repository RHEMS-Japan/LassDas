package visiblecheck

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"automation.internal/ticket-ingress/internal/releaseproof"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

const (
	chromeExecutable = "/opt/google/chrome/chrome"
	browserTimeout   = 90 * time.Second
	pageReadyTimeout = 45 * time.Second
)

type documentResponse struct {
	url      string
	status   int
	mimeType string
}

// ObserveAndSealStaging starts a clean, credential-free Chrome profile,
// navigates to the exact configured staging URL, and seals that in-process
// observation against the complete staging release proof.
func ObserveAndSealStaging(
	parent context.Context,
	proof releaseproof.StagingProof,
	input releaseproof.StagingInputs,
) (Evidence, []byte, error) {
	binding, err := stagingBinding(proof, input)
	if err != nil {
		return Evidence{}, nil, err
	}
	request := input.Request
	observed, err := observe(parent, binding.origin+request.VerificationPath, request.ExpectedText, request.AbsentText)
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
) (Evidence, []byte, error) {
	now := time.Now().UTC()
	binding, err := productionBinding(proof, staging, stagingEvidence, stagingScreenshot, input, now)
	if err != nil {
		return Evidence{}, nil, err
	}
	request := input.Request
	observed, err := observe(parent, binding.origin+request.VerificationPath, request.ExpectedText, request.AbsentText)
	if err != nil {
		return Evidence{}, nil, err
	}
	evidence, err := seal(observed, binding, input, time.Now().UTC())
	if err != nil {
		return Evidence{}, nil, err
	}
	return evidence, append([]byte(nil), observed.screenshotPNG...), nil
}

func observe(parent context.Context, targetURL, expectedText, absentText string) (capture, error) {
	if parent == nil || !validExactHTTPSURL(targetURL) || expectedText == "" || strings.ContainsAny(expectedText+absentText, "\x00\r\n") {
		return capture{}, errors.New("browser observation request is invalid")
	}
	executable, err := fixedChromeExecutable()
	if err != nil {
		return capture{}, errors.New("browser executable is invalid")
	}
	profile, err := os.MkdirTemp("", "visible-check-chrome-")
	if err != nil {
		return capture{}, errors.New("browser profile could not be created")
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

	var finalURL string
	var visibleText string
	var screenshot []byte
	var browserVersion string
	var textBytes int64
	var dimensions struct {
		Width  int64 `json:"width"`
		Height int64 `json:"height"`
	}
	var ready bool
	if err := chromedp.Run(browserContext,
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
		chromedp.Sleep(2*time.Second),
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
	); err != nil {
		return capture{}, errors.New("browser observation failed")
	}
	if !ready || len([]byte(visibleText)) != int(textBytes) {
		return capture{}, errors.New("browser rendered text is invalid")
	}

	responseMu.Lock()
	observedResponses := append([]documentResponse(nil), responses...)
	responseMu.Unlock()
	status, err := exactDocumentStatus(observedResponses, finalURL)
	if err != nil {
		return capture{}, err
	}
	result := capture{
		requestedURL: targetURL, finalURL: finalURL, statusCode: status,
		visibleText: visibleText, screenshotPNG: screenshot,
		browser: "chrome", BrowserVersion: browserVersion, observedAt: time.Now().UTC(),
	}
	return result, nil
}

func fixedChromeExecutable() (string, error) {
	resolved, err := filepath.EvalSymlinks(chromeExecutable)
	if err != nil || !filepath.IsAbs(resolved) || filepath.Clean(resolved) != resolved {
		return "", errors.New("Chrome path is invalid")
	}
	if !strings.HasPrefix(resolved, "/opt/google/chrome/") && !strings.HasPrefix(resolved, "/usr/bin/") {
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
