package visiblecheck

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/worker"
)

func validPNG(t *testing.T) []byte {
	t.Helper()
	value := image.NewRGBA(image.Rect(0, 0, 4, 3))
	value.Set(1, 1, color.RGBA{R: 0x40, G: 0x80, B: 0xc0, A: 0xff})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, value); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func TestValidateCaptureRequiresExactLiveNavigationAndRenderedAC(t *testing.T) {
	completed := time.Now().UTC().Add(-time.Minute)
	now := completed.Add(2 * time.Minute)
	request := worker.TicketRequest{ExpectedText: "Unique updated label", AbsentText: "Retired old label"}
	valid := capture{
		requestedURL: "https://stg.example.com" + "/settings", finalURL: "https://stg.example.com" + "/settings",
		statusCode: 200, visibleText: "Settings\nUnique updated label", screenshotPNG: validPNG(t),
		browser: "chrome", BrowserVersion: "140.0.7339.41", observedAt: completed.Add(time.Second),
	}
	if err := validateCapture(valid, valid.requestedURL, completed, now, request); err != nil {
		t.Fatal(err)
	}
	tests := map[string]func(*capture){
		"redirect":          func(c *capture) { c.finalURL = "https://stg.example.com" + "/login" },
		"http failure":      func(c *capture) { c.statusCode = 404 },
		"expected missing":  func(c *capture) { c.visibleText = "Settings" },
		"absent visible":    func(c *capture) { c.visibleText += "\nRetired old label" },
		"before deployment": func(c *capture) { c.observedAt = completed.Add(-time.Second) },
		"future":            func(c *capture) { c.observedAt = now.Add(2 * time.Minute) },
		"fake png":          func(c *capture) { c.screenshotPNG = []byte("\x89PNG\r\n\x1a\nsynthetic") },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			observed := valid
			observed.screenshotPNG = append([]byte(nil), valid.screenshotPNG...)
			mutate(&observed)
			if err := validateCapture(observed, valid.requestedURL, completed, now, request); err == nil {
				t.Fatal("validateCapture() accepted forged evidence")
			}
		})
	}
}

func TestChromeProductAndDocumentResponseAreExact(t *testing.T) {
	version, err := parseChromeProduct("HeadlessChrome/140.0.7339.41")
	if err != nil || version != "140.0.7339.41" {
		t.Fatalf("parseChromeProduct() = %q, %v", version, err)
	}
	for _, product := range []string{"Chromium/140.0.1.2", "HeadlessChrome/0.0.0.0", "HeadlessChrome/140"} {
		if _, err := parseChromeProduct(product); err == nil {
			t.Fatalf("parseChromeProduct(%q) accepted", product)
		}
	}
	url := "https://staging.example.com/settings"
	status, err := exactDocumentStatus([]documentResponse{{url: url, status: 200, mimeType: "text/html"}}, url)
	if err != nil || status != 200 {
		t.Fatalf("exactDocumentStatus() = %d, %v", status, err)
	}
	for name, responses := range map[string][]documentResponse{
		"missing":   nil,
		"ambiguous": {{url: url, status: 200, mimeType: "text/html"}, {url: url, status: 200, mimeType: "text/html"}},
		"not html":  {{url: url, status: 200, mimeType: "application/json"}},
		"failure":   {{url: url, status: 503, mimeType: "text/html"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := exactDocumentStatus(responses, url); err == nil {
				t.Fatal("exactDocumentStatus() accepted an invalid document response")
			}
		})
	}
}

func TestEvidenceValidatesTheCompleteRawScreenshot(t *testing.T) {
	screenshot := validPNG(t)
	evidence := Evidence{ScreenshotBytes: len(screenshot), ScreenshotSHA256: digest(screenshot)}
	if err := evidence.ValidateScreenshot(screenshot); err != nil {
		t.Fatal(err)
	}
	truncated := append([]byte(nil), screenshot[:len(screenshot)-1]...)
	if err := evidence.ValidateScreenshot(truncated); err == nil {
		t.Fatal("ValidateScreenshot() accepted a truncated PNG")
	}
	forged := append([]byte(nil), screenshot...)
	forged[len(forged)-1] ^= 0xff
	evidence.ScreenshotSHA256 = digest(forged)
	if err := evidence.ValidateScreenshot(forged); err == nil {
		t.Fatal("ValidateScreenshot() accepted an invalid PNG with a matching digest")
	}
	oversized := append([]byte(nil), screenshot...)
	binary.BigEndian.PutUint32(oversized[16:20], uint32(maxScreenshotWidth+1))
	binary.BigEndian.PutUint32(oversized[29:33], crc32.ChecksumIEEE(oversized[12:29]))
	evidence = Evidence{ScreenshotBytes: len(oversized), ScreenshotSHA256: digest(oversized)}
	if err := evidence.ValidateScreenshot(oversized); err == nil {
		t.Fatal("ValidateScreenshot() decoded an oversized PNG")
	}
}

func TestPromotionMustFollowTheStagingObservation(t *testing.T) {
	observed := time.Date(2026, 8, 3, 1, 2, 3, 0, time.UTC)
	if !promotionFollowsObservation(observed, observed) || !promotionFollowsObservation(observed.Add(time.Second), observed) {
		t.Fatal("promotion chronology rejected a valid ordering")
	}
	if promotionFollowsObservation(observed.Add(-time.Second), observed) ||
		promotionFollowsObservation(time.Time{}, observed) || promotionFollowsObservation(observed, time.Time{}) {
		t.Fatal("promotion chronology accepted a forged ordering")
	}
}
