package main

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOverviewShowsQueueUsageAndAnUnavailableReading(t *testing.T) {
	s, err := newServer(t.TempDir(), "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	s.config = map[string]any{"intake": map[string]any{"disk_notice_percent": float64(90)}}
	for _, percent := range []float64{84.2, 90, 91.5} {
		s.diskPercent = func(path string) (float64, error) {
			if path != s.runDir {
				t.Fatal("not measuring the queue filesystem")
			}
			return percent, nil
		}
		w := httptest.NewRecorder()
		s.overview(w, httptest.NewRequest("GET", "/", nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), "90%") {
			t.Fatalf("missing threshold: %s", w.Body.String())
		}
		if percent < 90 && !strings.Contains(w.Body.String(), "84.2%") {
			t.Fatal("usage missing")
		}
		if percent > 90 && !strings.Contains(w.Body.String(), "91.5%") {
			t.Fatal("high usage missing")
		}
		if strings.Contains(w.Body.String(), `<p class="attn">`) != (percent >= 90) {
			t.Fatal("warning does not match the configured threshold")
		}
	}
	s.diskPercent = func(string) (float64, error) { return 0, errors.New("fixture usage unavailable") }
	w := httptest.NewRecorder()
	s.overview(w, httptest.NewRequest("GET", "/", nil))
	if !strings.Contains(w.Body.String(), "fixture usage unavailable") || strings.Contains(w.Body.String(), "0.0%") {
		t.Fatal("unknown usage was hidden")
	}
}

func TestDiskThresholdIsTypedAndZeroDisablesWarning(t *testing.T) {
	s, err := newServer(t.TempDir(), "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	s.diskPercent = func(string) (float64, error) { return 100, nil }
	for _, value := range []any{float64(85.5), float64(-1), float64(101), "85"} {
		s.config = map[string]any{"intake": map[string]any{"disk_notice_percent": value}}
		if reading, err := s.readDisk(); err == nil || reading != nil {
			t.Fatalf("bad setting %v silently accepted", value)
		}
	}
	s.config = map[string]any{"intake": map[string]any{"disk_notice_percent": 0}}
	reading, err := s.readDisk()
	if err != nil || reading.Warning || reading.Threshold != 0 {
		t.Fatalf("off: %+v %v", reading, err)
	}
	s.config = nil
	reading, err = s.readDisk()
	if err != nil || !reading.Warning || reading.Threshold != 85 {
		t.Fatalf("default: %+v %v", reading, err)
	}
	// A server built directly still has a safe measurement function.
	_, _ = (&server{runDir: t.TempDir()}).readDisk()
}
