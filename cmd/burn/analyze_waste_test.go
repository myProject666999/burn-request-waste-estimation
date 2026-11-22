package main

import (
	"strings"
	"testing"

	"github.com/tanrikuluozlem/burn/internal/analyzer"
	"github.com/tanrikuluozlem/burn/internal/collector"
)

func TestPercentileFlagValidation(t *testing.T) {
	valid := []string{"p50", "p90", "p95", "p99", "95", "0.95"}
	for _, p := range valid {
		if _, err := collector.ParsePercentile(p); err != nil {
			t.Errorf("%q: unexpected error %v", p, err)
		}
	}
	invalid := []string{"p75", "80", "0.80", "", "abc"}
	for _, p := range invalid {
		if _, err := collector.ParsePercentile(p); err == nil {
			t.Errorf("%q: expected rejection", p)
		}
	}
}

func TestOutputWasteEstimate_RendersTotalsAndCounts(t *testing.T) {
	w := &analyzer.WasteEstimate{
		Period:              "7d",
		Percentile:          0.95,
		Factor:              1.5,
		MonthlyWaste:        27.375,
		CPUWasteMonthly:     27.375,
		PerPodSumMonthly:    27.375,
		UnderRequestedMem:   2,
		UnavailableCPU:      1,
		InsufficientSamples: 3,
		GPUPods:             1,
		NotEstimatedPods:    4,
		SkippedPods:         2,
	}
	out := captureOutput(func() { outputWasteEstimate(w) })
	for _, want := range []string{
		"OVER-REQUEST ESTIMATE",
		"Window: 7d | Percentile: p95 | Factor: 1.50",
		"$27.38",
		"Under-requested (zero waste, not offset): CPU 0, memory 2",
		"Skipped (no requests): 2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
}
