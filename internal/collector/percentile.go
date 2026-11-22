package collector

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// DefaultPercentile is the usage percentile used when the caller does not
// select one. It preserves the historical p95-based behaviour.
const DefaultPercentile = 0.95

// SupportedPercentiles is the closed set of percentile presets accepted by the
// CLI. Anything else is rejected during argument validation.
var SupportedPercentiles = []float64{0.50, 0.90, 0.95, 0.99}

// IsSupportedPercentile reports whether p is one of the allowed presets.
func IsSupportedPercentile(p float64) bool {
	for _, s := range SupportedPercentiles {
		if math.Abs(p-s) < 1e-9 {
			return true
		}
	}
	return false
}

// ParsePercentile accepts a preset written as "p95", "95", "95%" or "0.95" and
// returns its fraction. Values outside the preset list are rejected.
func ParsePercentile(s string) (float64, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	if t == "" {
		return 0, fmt.Errorf("percentile must be one of p50, p90, p95, p99")
	}
	t = strings.TrimPrefix(t, "p")
	t = strings.TrimSuffix(t, "%")
	v, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid percentile %q: choose one of p50, p90, p95, p99", s)
	}
	if v > 1 {
		v /= 100
	}
	if !IsSupportedPercentile(v) {
		return 0, fmt.Errorf("unsupported percentile %q: choose one of p50, p90, p95, p99", s)
	}
	return v, nil
}

// FormatPercentile renders a percentile fraction as a preset label (e.g. "p95").
func FormatPercentile(p float64) string {
	return fmt.Sprintf("p%g", p*100)
}
