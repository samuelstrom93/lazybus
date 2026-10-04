package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
)

// Snapshot is one release's measurements: benchmarks/release/bench-<version>.json.
type Snapshot struct {
	SchemaVersion       int        `json:"schemaVersion"`
	Version             string     `json:"version"`
	GitSHA              string     `json:"gitSha"`
	GeneratedAt         string     `json:"generatedAt"`
	Runtime             Runtime    `json:"runtime"`
	BenchCommand        string     `json:"benchCommand"`
	SamplesPerBenchmark int        `json:"samplesPerBenchmark"`
	Results             []Result   `json:"results"`
	AcceptedRegressions []Accepted `json:"acceptedRegressions"`
}

const schemaVersion = 1

type Runtime struct {
	GoVersion string `json:"goVersion"`
	GOOS      string `json:"goos"`
	GOARCH    string `json:"goarch"`
	CPU       string `json:"cpu"`
}

// Result is one metric, e.g. ViewLargeList/ns_per_op. Samples keep the
// order they were measured in.
type Result struct {
	Name       string     `json:"name"`
	Unit       string     `json:"unit"`
	Samples    []float64  `json:"samples"`
	Median     float64    `json:"median"`
	P75        float64    `json:"p75"`
	P95        float64    `json:"p95"`
	Min        float64    `json:"min"`
	Max        float64    `json:"max"`
	Comparable bool       `json:"comparable"`
	Threshold  *Threshold `json:"threshold,omitempty"`
}

// Threshold: a head median is a material regression when it is more than
// MaxRegressionRatio times the base median AND more than
// MinAbsoluteRegression above it.
type Threshold struct {
	MaxRegressionRatio    float64 `json:"maxRegressionRatio"`
	MinAbsoluteRegression float64 `json:"minAbsoluteRegression"`
}

// Accepted is a regression a release knowingly ships, with why.
type Accepted struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

func readSnapshot(path string) (Snapshot, error) {
	var s Snapshot
	data, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("%s: %w", path, err)
	}
	if s.SchemaVersion != schemaVersion {
		return s, fmt.Errorf("%s: schemaVersion %d, want %d", path, s.SchemaVersion, schemaVersion)
	}
	return s, nil
}

func writeSnapshot(path string, s Snapshot) error {
	if s.AcceptedRegressions == nil {
		s.AcceptedRegressions = []Accepted{}
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// newResult aggregates samples. Percentiles are nearest-rank.
func newResult(name, unit string, samples []float64) Result {
	sorted := append([]float64(nil), samples...)
	sort.Float64s(sorted)
	r := Result{
		Name:    name,
		Unit:    unit,
		Samples: samples,
		Median:  percentile(sorted, 50),
		P75:     percentile(sorted, 75),
		P95:     percentile(sorted, 95),
		Min:     sorted[0],
		Max:     sorted[len(sorted)-1],
	}
	r.Comparable = unit == unitNs || unit == unitBytes || unit == unitCount
	if t, ok := thresholds[name]; ok && r.Comparable {
		r.Threshold = &t
	}
	return r
}

// percentile is the nearest-rank percentile p of sorted (non-empty).
func percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	i := min(n-1, max(0, int(math.Ceil(p/100*float64(n)))-1))
	return sorted[i]
}
