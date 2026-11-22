package collector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeProm serves canned results keyed by the exact query string it receives.
// It records the queries so tests can assert on the generated PromQL.
type fakeProm struct {
	queries []string
	results map[string]struct {
		metric map[string]string
		value  string
	}
}

func (f *fakeProm) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		f.queries = append(f.queries, q)
		row, ok := f.results[q]
		if !ok {
			json.NewEncoder(w).Encode(map[string]any{
				"status": "success",
				"data":   map[string]any{"resultType": "vector", "result": []any{}},
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data": map[string]any{
				"resultType": "vector",
				"result": []any{map[string]any{
					"metric": row.metric,
					"value":  []any{float64(time.Now().Unix()), row.value},
				}},
			},
		})
	}
}

func newFakeServer(t *testing.T, f *fakeProm) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	return srv
}

func TestPercentileQuery_PresetsDifferOnlyByQuantile(t *testing.T) {
	tests := []struct {
		quantile float64
		want     string
	}{
		{0.50, `quantile_over_time(0.5, sum(rate(container_cpu_usage_seconds_total{container!="",container!="POD"}[5m])) by (pod, namespace)[7d:5m])`},
		{0.90, `quantile_over_time(0.9, sum(rate(container_cpu_usage_seconds_total{container!="",container!="POD"}[5m])) by (pod, namespace)[7d:5m])`},
		{0.95, `quantile_over_time(0.95, sum(rate(container_cpu_usage_seconds_total{container!="",container!="POD"}[5m])) by (pod, namespace)[7d:5m])`},
		{0.99, `quantile_over_time(0.99, sum(rate(container_cpu_usage_seconds_total{container!="",container!="POD"}[5m])) by (pod, namespace)[7d:5m])`},
	}
	for _, tt := range tests {
		got := percentileQuery(podCPUUsageExpr, "7d", tt.quantile)
		if got != tt.want {
			t.Errorf("percentileQuery(%v) = %q, want %q", tt.quantile, got, tt.want)
		}
	}

	// CPU and memory share the same builder shape.
	mem := percentileQuery(podMemUsageExpr, "7d", 0.9)
	wantMem := `quantile_over_time(0.9, sum(container_memory_working_set_bytes{container!="",container!="POD"}) by (pod, namespace)[7d:5m])`
	if mem != wantMem {
		t.Errorf("memory percentileQuery = %q, want %q", mem, wantMem)
	}
}

func TestGetPodCPUUsageP95_UsesSelectedPercentile(t *testing.T) {
	f := &fakeProm{results: map[string]struct {
		metric map[string]string
		value  string
	}{}}
	wantQuery := percentileQuery(podCPUUsageExpr, "24h", 0.90)
	f.results[wantQuery] = struct {
		metric map[string]string
		value  string
	}{metric: map[string]string{"pod": "app", "namespace": "prod"}, value: "0.42"}

	srv := newFakeServer(t, f)
	pc := NewPrometheusClient(srv.URL, "24h")
	pc.SetPercentile(0.90)

	got, err := pc.GetPodCPUUsageP95(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(f.queries) != 1 || f.queries[0] != wantQuery {
		t.Fatalf("queries = %v, want [%s]", f.queries, wantQuery)
	}
	if got["prod/app"] != 0.42 {
		t.Errorf("usage[prod/app] = %v, want 0.42", got["prod/app"])
	}
}

func TestGetPodMemoryUsageP95_UsesSelectedPercentile(t *testing.T) {
	f := &fakeProm{results: map[string]struct {
		metric map[string]string
		value  string
	}{}}
	wantQuery := percentileQuery(podMemUsageExpr, "30d", 0.99)
	f.results[wantQuery] = struct {
		metric map[string]string
		value  string
	}{metric: map[string]string{"pod": "db", "namespace": "prod"}, value: "1073741824"}

	srv := newFakeServer(t, f)
	pc := NewPrometheusClient(srv.URL, "30d")
	pc.SetPercentile(0.99)

	got, err := pc.GetPodMemoryUsageP95(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(f.queries) != 1 || f.queries[0] != wantQuery {
		t.Fatalf("queries = %v, want [%s]", f.queries, wantQuery)
	}
	if got["prod/db"] != 1073741824 {
		t.Errorf("usage[prod/db] = %v, want 1073741824", got["prod/db"])
	}
}

func TestGetPodObservedSpans_CapsAtWindow(t *testing.T) {
	f := &fakeProm{results: map[string]struct {
		metric map[string]string
		value  string
	}{}}
	q := coverageQuery("2d")
	f.results[q] = struct {
		metric map[string]string
		value  string
	}{metric: map[string]string{"pod": "fresh", "namespace": "prod"}, value: "999999999"}

	srv := newFakeServer(t, f)
	pc := NewPrometheusClient(srv.URL, "2d")

	spans, err := pc.GetPodObservedSpans(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(f.queries) != 1 || f.queries[0] != q {
		t.Fatalf("queries = %v, want [%s]", f.queries, q)
	}
	if got := spans["prod/fresh"]; got != 48*time.Hour {
		t.Errorf("span = %v, want 48h (capped at 2d window)", got)
	}
}

func TestParsePercentile(t *testing.T) {
	ok := map[string]float64{
		"p50": 0.50, "p90": 0.90, "p95": 0.95, "p99": 0.99,
		"95": 0.95, "95%": 0.95, "0.95": 0.95, "p95%": 0.95,
	}
	for in, want := range ok {
		got, err := ParsePercentile(in)
		if err != nil {
			t.Errorf("ParsePercentile(%q) error = %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParsePercentile(%q) = %v, want %v", in, got, want)
		}
	}
	for _, bad := range []string{"", "p75", "0.80", "abc", "p0"} {
		if _, err := ParsePercentile(bad); err == nil {
			t.Errorf("ParsePercentile(%q) = nil error, want rejection", bad)
		}
	}
}

func TestFormatPercentile(t *testing.T) {
	for _, p := range []float64{0.50, 0.90, 0.95, 0.99} {
		if want := map[float64]string{0.50: "p50", 0.90: "p90", 0.95: "p95", 0.99: "p99"}[p]; FormatPercentile(p) != want {
			t.Errorf("FormatPercentile(%v) = %q, want %q", p, FormatPercentile(p), want)
		}
	}
}
