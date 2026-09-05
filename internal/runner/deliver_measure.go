package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/probe"
	"automation.internal/ticket-ingress/internal/worker"
	"automation.internal/ticket-ingress/internal/worker/investigate"
)

// A design may promise a measurement instead of wording: after deployment
// the kernel runs the design's probe against the deployed environment and
// compares the metric with the threshold (docs/INVESTIGATING_DESIGNER.md
// §4.3). The outcome is sealed next to the screen check's and judged like
// it: a value over the threshold is a failed check, never a pass.

// MeasurementCheck is the sealed outcome of one post-deployment measurement.
type MeasurementCheck struct {
	SchemaVersion int       `json:"schema_version"`
	Phase         string    `json:"phase"`
	Probe         string    `json:"probe"`
	Metric        string    `json:"metric"`
	Threshold     float64   `json:"threshold"`
	Value         float64   `json:"value"`
	MeasurementID string    `json:"measurement_id"`
	Pass          bool      `json:"pass"`
	Detail        string    `json:"detail,omitempty"`
	MeasuredAt    time.Time `json:"measured_at"`
}

// DeliverMeasurementsFile keeps the deployment measurements apart from the
// investigation's.
const DeliverMeasurementsFile = "deliver-measurements.jsonl"

var metricPattern = regexp.MustCompile(`(?m)\b(status|time_total|bytes)=([0-9.]+)`)

// measurementVerification runs the approved design's measurement, when the
// design promised one. checked is false when there is nothing to measure.
func (p *Pipeline) measurementVerification(ctx context.Context, stageDir, phase string) (checked bool, pass bool, detail string, err error) {
	designPath := p.approvedDesignPath()
	if designPath == "" {
		return false, false, "", nil
	}
	design, err := investigate.ReadDesign(designPath)
	if err != nil || !design.DigestMatches() {
		return false, false, "", errors.New("the approved design could not be read for its measurement")
	}
	if design.Verification.Form != investigate.VerificationMeasurement {
		return false, false, "", nil
	}
	config, err := worker.LoadConfig(p.Config.ConsumerConfigPath)
	if err != nil {
		return true, false, "", errors.New("consumer config could not be loaded for the measurement")
	}
	catalog, err := config.ProbeCatalog()
	if err != nil {
		return true, false, "", err
	}
	recorder, err := probe.OpenRecorder(p.path(DeliverMeasurementsFile))
	if err != nil {
		return true, false, "", err
	}
	declared := map[string]bool{}
	for _, spec := range catalog.Specs() {
		if spec.DSNEnv != "" {
			declared[spec.DSNEnv] = true
		}
	}
	session := &probe.Session{
		Catalog: catalog, Recorder: recorder, Limits: probe.Limits{MaxProbes: recorder.Count() + 1, MaxTotalBytes: 16 << 20, ExcerptBytes: 32 << 10},
		Env: probe.EnvFromProcess(probe.ExecEnvironmentNames),
		DSN: func(name string) string {
			if declared[name] {
				return os.Getenv(name)
			}
			return ""
		},
		Used: recorder.Count(),
	}
	if seed, state := observationSessionPaths(); seed != "" || state != "" {
		session.Jar = observationJarFromFiles(seed, state)
	}
	outcome, err := session.Run(ctx, probe.Request{Probe: design.Verification.Probe, Args: design.Verification.Args})
	if err != nil {
		return true, false, "", err
	}
	check := MeasurementCheck{SchemaVersion: 1, Phase: phase, Probe: design.Verification.Probe, Metric: design.Verification.Metric,
		Threshold: design.Verification.Threshold, MeasurementID: outcome.Measurement.ID, MeasuredAt: time.Now().UTC()}
	switch {
	case outcome.Measurement.Refused:
		check.Detail = "計測が実行されませんでした: " + outcome.Measurement.Reason
	case outcome.Measurement.ExitCode != 0:
		check.Detail = "計測が失敗しました: " + outcome.Measurement.Reason
	default:
		value, ok := metricValue(design.Verification.Metric, outcome.Excerpt)
		if !ok {
			check.Detail = fmt.Sprintf("計測結果に %s が含まれていません", design.Verification.Metric)
		} else {
			check.Value = value
			check.Pass = value <= design.Verification.Threshold
			if !check.Pass {
				check.Detail = fmt.Sprintf("%s = %g が閾値 %g を超えています", design.Verification.Metric, value, design.Verification.Threshold)
			}
		}
	}
	encoded, _ := json.MarshalIndent(check, "", "  ")
	if err := os.WriteFile(filepath.Join(stageDir, phase+"-measurement-check.json"), append(encoded, '\n'), 0o600); err != nil {
		return true, false, "", err
	}
	return true, check.Pass, check.Detail, nil
}

// metricValue reads the promised metric out of a probe's excerpt: the http
// probe's `key=value` line, or the first cell of a sql probe's first row.
func metricValue(metric, excerpt string) (float64, bool) {
	switch metric {
	case "status", "time_total", "bytes":
		for _, match := range metricPattern.FindAllStringSubmatch(excerpt, -1) {
			if match[1] == metric {
				value, err := strconv.ParseFloat(match[2], 64)
				return value, err == nil
			}
		}
		return 0, false
	case "rows":
		lines := strings.Split(strings.TrimRight(excerpt, "\n"), "\n")
		if len(lines) < 1 || lines[0] == "" {
			return 0, false
		}
		return float64(len(lines) - 1), true
	case "value":
		lines := strings.Split(strings.TrimRight(excerpt, "\n"), "\n")
		if len(lines) < 2 {
			return 0, false
		}
		cell := strings.SplitN(lines[1], "\t", 2)[0]
		value, err := strconv.ParseFloat(strings.TrimSpace(cell), 64)
		return value, err == nil
	}
	return 0, false
}

// observationJarFromFiles loads the screen check's jar for http probes that
// declare it; the probes read it and never write it back.
func observationJarFromFiles(seed, state string) []probe.Cookie {
	cookies, _ := loadE2ESessionCookies(seed, state)
	out := make([]probe.Cookie, 0, len(cookies))
	for _, cookie := range cookies {
		out = append(out, probe.Cookie{Name: cookie.Name, Value: cookie.Value, Domain: cookie.Domain, Path: cookie.Path, Secure: cookie.Secure})
	}
	return out
}

// fillMeasurement copies the sealed measurement check, when one was made,
// into the report next to the screen check.
func (p *Pipeline) fillMeasurement(report *DeliverReport, stageDir, phase string) {
	raw, err := os.ReadFile(filepath.Join(stageDir, phase+"-measurement-check.json"))
	if err != nil {
		return
	}
	var check MeasurementCheck
	if json.Unmarshal(raw, &check) == nil && check.Probe != "" {
		report.Measurement = &check
	}
}

// sealMeasurementFailure ends the phase with its own verdict: the deployment
// happened, the screen (if promised) may be fine, and the measurement the
// design promised is not — which the report says in those words.
func (p *Pipeline) sealMeasurementFailure(stageDir, phase, detail string) error {
	report := DeliverReport{Phase: phase, Verdict: "measure_failed",
		Detail: phase + "での計測が設計書の閾値を満たしませんでした（" + detail + "）。"}
	if phase == "staging" {
		report.Detail += "本番反映は行えません。"
	} else {
		report.Detail += "自動的な追加変更やロールバックは行っていません。"
	}
	p.fillMeasurement(&report, stageDir, phase)
	if phase == "staging" {
		p.fillObservation(&report, stageDir, "staging", DeliverStagingVisibleFile, DeliverStagingShotFile)
	} else {
		p.fillObservation(&report, stageDir, "production", DeliverProductionVisibleFile, DeliverProductionShotFile)
	}
	return p.sealDeliverReport(report)
}
