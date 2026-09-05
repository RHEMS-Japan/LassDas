package runner

import "testing"

func TestMetricValueReadsProbeExcerpts(t *testing.T) {
	cases := []struct {
		metric, excerpt string
		want            float64
		ok              bool
	}{
		{"time_total", "status=200 time_total=0.412 bytes=18422\n", 0.412, true},
		{"status", "status=302 time_total=0.1 bytes=0\n", 302, true},
		{"bytes", "status=200 time_total=0.1 bytes=18422 rotated=true\n", 18422, true},
		{"time_total", "status=200\n", 0, false},
		{"rows", "count\n42\n7\n", 2, true},
		{"rows", "", 0, false},
		{"value", "count\n42\n", 42, true},
		{"value", "count\n", 0, false},
		{"value", "count\nabc\n", 0, false},
		{"other", "x", 0, false},
	}
	for _, tc := range cases {
		got, ok := metricValue(tc.metric, tc.excerpt)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("metricValue(%q, %q) = %v %v, want %v %v", tc.metric, tc.excerpt, got, ok, tc.want, tc.ok)
		}
	}
}

func TestMeasurementVerificationIsSkippedWithoutADesign(t *testing.T) {
	p := &Pipeline{Workspace: t.TempDir()}
	checked, _, _, err := p.measurementVerification(nil, t.TempDir(), "staging")
	if checked || err != nil {
		t.Errorf("no design: checked %v err %v", checked, err)
	}
}

func TestMeasurementCheckFilesLiveUnderTheWorkspace(t *testing.T) {
	p := &Pipeline{Workspace: t.TempDir()}
	if got := p.stageFile("history/stage-1", "staging-measurement-check.json"); got != p.path("history/stage-1/staging-measurement-check.json") {
		t.Errorf("relative stage dir resolved to %q", got)
	}
	abs := t.TempDir()
	if got := p.stageFile(abs, "x.json"); got != abs+"/x.json" {
		t.Errorf("absolute stage dir resolved to %q", got)
	}
}
