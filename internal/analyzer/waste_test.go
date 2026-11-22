package analyzer

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/tanrikuluozlem/burn/internal/collector"
)

const gib = 1024 * 1024 * 1024

func approx(t *testing.T, got, want float64, label string) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %v, want %v", label, got, want)
	}
}

func podForWaste(name, ns string, cpuReq int64, memReq int64) PodEfficiency {
	return PodEfficiency{
		Name:         name,
		Namespace:    ns,
		CPURequest:   cpuReq,
		MemRequest:   memReq,
		CPUUnitPrice: 0.03,
		RAMUnitPrice: 0.004,
	}
}

// Hand-worked sample from the task: request 2 cores, p95 0.5 core, factor 1.5,
// $0.03/core/hour → reference 0.75 core, excess 1.25 core, hourly $0.0375,
// monthly 0.0375 × 730.
func TestEstimateWaste_HandWorkedCPU(t *testing.T) {
	p := podForWaste("app", "prod", 2000, 0)
	p.CPUP95Usage = 0.5
	p.CPUP95Available = true
	p.ObservedSpan = 7 * 24 * time.Hour

	est := EstimateWaste([]PodEfficiency{p}, WasteConfig{Period: "7d", Percentile: 0.95, Factor: 1.5})

	pe := est.Pods[0]
	approx(t, pe.CPUReferenceCores, 0.75, "CPUReferenceCores")
	approx(t, pe.CPUWasteMonthly, 1.25*0.03*730, "CPUWasteMonthly")
	approx(t, pe.CPUWasteMonthly, 27.375, "CPUWasteMonthly hand value")
	if pe.CPUStatus != WasteStatusEstimated {
		t.Errorf("CPUStatus = %q, want %q", pe.CPUStatus, WasteStatusEstimated)
	}
	approx(t, est.MonthlyWaste, 27.375, "MonthlyWaste")
}

func TestEstimateWaste_Table(t *testing.T) {
	base := func() PodEfficiency { return podForWaste("app", "prod", 2000, 4*gib) }

	threeReplicas := func() []PodEfficiency {
		var out []PodEfficiency
		for i, p95 := range []float64{0.5, 1.0, 0.2} {
			p := base()
			p.Name = "app-" + string(rune('a'+i))
			p.OwnerKind = "Deployment"
			p.OwnerName = "app"
			p.CPUP95Usage = p95
			p.CPUP95Available = true
			// Reference above the request → no memory waste; isolates CPU.
			p.MemoryP95Usage = 4 * gib
			p.MemP95Available = true
			p.ObservedSpan = 7 * 24 * time.Hour
			out = append(out, p)
		}
		return out
	}

	tests := []struct {
		name       string
		pod        PodEfficiency
		wantCPU    string
		wantMem    string
		wantCPUMo  float64
		wantGPUNot bool
	}{
		{
			name: "request exceeds reference — estimated",
			pod: func() PodEfficiency {
				p := base()
				p.CPUP95Usage = 0.5
				p.CPUP95Available = true
				p.MemoryP95Usage = 1 * gib
				p.MemP95Available = true
				p.ObservedSpan = 7 * 24 * time.Hour
				return p
			}(),
			wantCPU:   WasteStatusEstimated,
			wantMem:   WasteStatusEstimated,
			wantCPUMo: 27.375,
		},
		{
			name: "request below reference — under requested, zero waste",
			pod: func() PodEfficiency {
				p := podForWaste("app", "prod", 500, 1*gib)
				p.CPUP95Usage = 2.0 // reference 3.0 > 0.5 request
				p.CPUP95Available = true
				p.MemoryP95Usage = 8 * gib
				p.MemP95Available = true
				p.ObservedSpan = 7 * 24 * time.Hour
				return p
			}(),
			wantCPU: WasteStatusUnderRequested,
			wantMem: WasteStatusUnderRequested,
		},
		{
			name: "missing CPU percentile — unavailable, memory still estimated",
			pod: func() PodEfficiency {
				p := base()
				p.CPUP95Available = false
				p.MemoryP95Usage = 1 * gib
				p.MemP95Available = true
				p.ObservedSpan = 7 * 24 * time.Hour
				return p
			}(),
			wantCPU: WasteStatusUnavailable,
			wantMem: WasteStatusEstimated,
		},
		{
			name: "zero request — no_request",
			pod: func() PodEfficiency {
				p := podForWaste("app", "prod", 0, 0)
				p.CPUP95Usage = 0.5
				p.CPUP95Available = true
				p.MemoryP95Usage = 1 * gib
				p.MemP95Available = true
				return p
			}(),
			wantCPU: WasteStatusNoRequest,
			wantMem: WasteStatusNoRequest,
		},
		{
			name: "GPU request — not estimated",
			pod: func() PodEfficiency {
				p := base()
				p.GPURequest = 1
				p.CPUP95Usage = 0.5
				p.CPUP95Available = true
				p.MemoryP95Usage = 1 * gib
				p.MemP95Available = true
				p.ObservedSpan = 7 * 24 * time.Hour
				return p
			}(),
			wantCPU:    WasteStatusEstimated,
			wantMem:    WasteStatusEstimated,
			wantCPUMo:  27.375,
			wantGPUNot: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			est := EstimateWaste([]PodEfficiency{tt.pod}, WasteConfig{Percentile: 0.95, Factor: 1.5})
			pe := est.Pods[0]
			if pe.CPUStatus != tt.wantCPU {
				t.Errorf("CPUStatus = %q, want %q", pe.CPUStatus, tt.wantCPU)
			}
			if pe.MemStatus != tt.wantMem {
				t.Errorf("MemStatus = %q, want %q", pe.MemStatus, tt.wantMem)
			}
			if tt.wantCPUMo != 0 {
				approx(t, pe.CPUWasteMonthly, tt.wantCPUMo, "CPUWasteMonthly")
			}
			if pe.GPUNotEstimated != tt.wantGPUNot {
				t.Errorf("GPUNotEstimated = %v, want %v", pe.GPUNotEstimated, tt.wantGPUNot)
			}
			if pe.CPUWasteMonthly < 0 || pe.MemWasteMonthly < 0 {
				t.Errorf("negative waste: cpu=%v mem=%v", pe.CPUWasteMonthly, pe.MemWasteMonthly)
			}
		})
	}

	t.Run("three replicas take max reference", func(t *testing.T) {
		est := EstimateWaste(threeReplicas(), WasteConfig{Percentile: 0.95, Factor: 1.5})
		if len(est.Workloads) != 1 {
			t.Fatalf("Workloads = %d, want 1", len(est.Workloads))
		}
		w := est.Workloads[0]
		if w.Replicas != 3 || w.EffectiveCPUs != 3 {
			t.Fatalf("Replicas=%d EffectiveCPUs=%d, want 3/3", w.Replicas, w.EffectiveCPUs)
		}
		// Max p95 = 1.0 → reference 1.5; request 2.0 → excess 0.5/core × 3 replicas.
		approx(t, w.CPUReferenceCores, 1.5, "workload CPUReferenceCores")
		approx(t, w.CPUWasteMonthly, 0.5*3*0.03*730, "workload CPUWasteMonthly")

		// Per-pod sum must be larger: each replica priced against its own p95.
		// excess: (2-0.75)+(2-1.5)+(2-0.3) = 1.25+0.5+1.7 = 3.45 cores
		approx(t, est.PerPodSumMonthly, 3.45*0.03*730, "PerPodSumMonthly")
		if !(est.MonthlyWaste < est.PerPodSumMonthly) {
			t.Errorf("workload total %v should be below per-pod sum %v", est.MonthlyWaste, est.PerPodSumMonthly)
		}
	})
}

func TestEstimateWaste_ShortSampleReported(t *testing.T) {
	p := podForWaste("rolling", "prod", 2000, 4*gib)
	p.CPUP95Usage = 0.5
	p.CPUP95Available = true
	p.MemoryP95Usage = 1 * gib
	p.MemP95Available = true
	p.ObservedSpan = 10 * time.Minute // shorter than the one-hour threshold

	est := EstimateWaste([]PodEfficiency{p}, WasteConfig{
		Period:          "7d",
		Percentile:      0.95,
		Factor:          1.5,
		MinObservedSpan: time.Hour,
	})

	pe := est.Pods[0]
	if pe.CPUStatus != WasteStatusInsufficientData {
		t.Errorf("CPUStatus = %q, want %q", pe.CPUStatus, WasteStatusInsufficientData)
	}
	if pe.MemStatus != WasteStatusInsufficientData {
		t.Errorf("MemStatus = %q, want %q", pe.MemStatus, WasteStatusInsufficientData)
	}
	if pe.CPUWasteMonthly != 0 {
		t.Errorf("CPUWasteMonthly = %v, want 0 for insufficient sample", pe.CPUWasteMonthly)
	}
	if est.InsufficientSamples != 1 {
		t.Errorf("InsufficientSamples = %d, want 1", est.InsufficientSamples)
	}
	if est.Period != "7d" {
		t.Errorf("Period = %q, want 7d", est.Period)
	}
}

func TestEstimateWaste_EffectiveWindowShortened(t *testing.T) {
	p := podForWaste("app", "prod", 2000, 0)
	p.CPUP95Usage = 0.5
	p.CPUP95Available = true
	p.ObservedSpan = 3 * 24 * time.Hour

	est := EstimateWaste([]PodEfficiency{p}, WasteConfig{Period: "3d", Percentile: 0.95, Factor: 1.5})
	if est.Period != "3d" {
		t.Errorf("Period = %q, want effective window 3d", est.Period)
	}
}

func TestEstimateWaste_NamespaceSummationModes(t *testing.T) {
	var pods []PodEfficiency
	for i, p95 := range []float64{0.5, 1.0} {
		p := podForWaste("app-"+string(rune('a'+i)), "prod", 2000, 0)
		p.OwnerKind = "Deployment"
		p.OwnerName = "app"
		p.CPUP95Usage = p95
		p.CPUP95Available = true
		p.ObservedSpan = 24 * time.Hour
		pods = append(pods, p)
	}

	est := EstimateWaste(pods, WasteConfig{Percentile: 0.95, Factor: 1.5})
	if len(est.Namespaces) != 1 {
		t.Fatalf("Namespaces = %d, want 1", len(est.Namespaces))
	}
	ns := est.Namespaces[0]
	// Workload basis: max reference 1.5, excess 0.5 × 2 replicas.
	approx(t, ns.MonthlyWaste, 0.5*2*0.03*730, "namespace MonthlyWaste")
	// Per-pod basis: (2-0.75)+(2-1.5) = 1.75 cores.
	approx(t, ns.PerPodSumMonthly, 1.75*0.03*730, "namespace PerPodSumMonthly")
	approx(t, ns.Difference, ns.MonthlyWaste-ns.PerPodSumMonthly, "Difference")
}

func TestEstimateWaste_BarePodIndependent(t *testing.T) {
	var pods []PodEfficiency
	for i := 0; i < 2; i++ {
		p := podForWaste("bare-"+string(rune('a'+i)), "prod", 2000, 0)
		p.CPUP95Usage = 0.5
		p.CPUP95Available = true
		p.ObservedSpan = 24 * time.Hour
		pods = append(pods, p)
	}

	est := EstimateWaste(pods, WasteConfig{Percentile: 0.95, Factor: 1.5})
	if len(est.Workloads) != 2 {
		t.Fatalf("Workloads = %d, want 2 independent bare pods", len(est.Workloads))
	}
	for _, w := range est.Workloads {
		if w.Kind != "" {
			t.Errorf("bare pod kind = %q, want empty", w.Kind)
		}
		if w.Replicas != 1 {
			t.Errorf("bare pod replicas = %d, want 1", w.Replicas)
		}
	}
}

func TestEstimateWaste_SameNameDifferentKindNotMerged(t *testing.T) {
	dep := podForWaste("web-1", "prod", 2000, 0)
	dep.OwnerKind = "Deployment"
	dep.OwnerName = "web"
	dep.CPUP95Usage = 0.5
	dep.CPUP95Available = true
	dep.ObservedSpan = 24 * time.Hour

	sts := podForWaste("web-0", "prod", 2000, 0)
	sts.OwnerKind = "StatefulSet"
	sts.OwnerName = "web"
	sts.CPUP95Usage = 0.5
	sts.CPUP95Available = true
	sts.ObservedSpan = 24 * time.Hour

	est := EstimateWaste([]PodEfficiency{dep, sts}, WasteConfig{Percentile: 0.95, Factor: 1.5})
	if len(est.Workloads) != 2 {
		t.Fatalf("Workloads = %d, want 2 (kind distinguishes them)", len(est.Workloads))
	}
}

func TestEstimateWaste_ConfigMismatchSplitsGroup(t *testing.T) {
	a := podForWaste("app-a", "prod", 2000, 2*gib)
	a.OwnerKind = "Deployment"
	a.OwnerName = "app"
	a.CPUP95Usage = 0.5
	a.CPUP95Available = true
	a.ObservedSpan = 24 * time.Hour

	b := podForWaste("app-b", "prod", 4000, 2*gib)
	b.OwnerKind = "Deployment"
	b.OwnerName = "app"
	b.CPUP95Usage = 0.5
	b.CPUP95Available = true
	b.ObservedSpan = 24 * time.Hour

	est := EstimateWaste([]PodEfficiency{a, b}, WasteConfig{Percentile: 0.95, Factor: 1.5})
	if len(est.Workloads) != 2 {
		t.Fatalf("Workloads = %d, want 2 (different configs must group separately)", len(est.Workloads))
	}
}

func TestEstimateWaste_SkippedPodsCountedViaAnalyzer(t *testing.T) {
	// Pods with no requests are skipped by calculatePodEfficiencies; only the
	// count should appear on the estimate.
	node := collector.NodeInfo{
		Name:           "node-1",
		InstanceType:   "t3.medium",
		Region:         "eu-central-1",
		CloudProvider:  collector.CloudAWS,
		CPUAllocatable: 2000,
		MemAllocatable: 4 * gib,
		CPUUsage:       1.0, // marks the report as Prometheus-sourced
		Pods: []collector.PodInfo{
			{Name: "init-only", Namespace: "prod"}, // no CPU/mem request
			{Name: "app", Namespace: "prod", CPURequest: 2000, MemoryRequest: 1 * gib},
		},
	}
	report, err := New(&mockPricing{price: 0.10}).Analyze(context.Background(), &collector.ClusterInfo{
		TotalNodes: 1,
		TotalPods:  2,
		Nodes:      []collector.NodeInfo{node},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.WasteEstimate == nil {
		t.Fatal("WasteEstimate is nil")
	}
	if report.WasteEstimate.SkippedPods != 1 {
		t.Errorf("SkippedPods = %d, want 1", report.WasteEstimate.SkippedPods)
	}
	if len(report.WasteEstimate.Pods) != 1 {
		t.Errorf("Pods in estimate = %d, want 1", len(report.WasteEstimate.Pods))
	}
}

func TestAnalyze_DefaultPercentilePreservesLegacyFields(t *testing.T) {
	info := &collector.ClusterInfo{
		TotalNodes: 1,
		TotalPods:  1,
		Nodes: []collector.NodeInfo{{
			Name:           "node-1",
			InstanceType:   "t3.medium",
			Region:         "us-east-1",
			CPUAllocatable: 2000,
			MemAllocatable: 4 * gib,
			CPUUsage:       0.5,
			MemoryUsage:    512 * 1024 * 1024,
			Pods: []collector.PodInfo{{
				Name:            "app",
				Namespace:       "prod",
				CPURequest:      1000,
				MemoryRequest:   1 * gib,
				CPUUsage:        0.5,
				MemoryUsage:     512 * 1024 * 1024,
				CPUP95Usage:     0.4,
				CPUP95Available: true,
				MemoryP95Usage:  512 * 1024 * 1024,
				MemP95Available: true,
			}},
		}},
		Percentile: 0, // unset → analyzer default
	}
	report, err := New(&mockPricing{price: 0.10}).Analyze(context.Background(), info)
	if err != nil {
		t.Fatal(err)
	}
	if report.WasteEstimate == nil {
		t.Fatal("WasteEstimate is nil")
	}
	approx(t, report.WasteEstimate.Percentile, collector.DefaultPercentile, "default percentile")

	// Legacy fields keep their meaning: InefficientPods still keyed on
	// CPUEfficiency, and the estimate does not rewrite them.
	if report.MetricsSource != "prometheus" {
		t.Errorf("MetricsSource = %q, want prometheus", report.MetricsSource)
	}
	if len(report.AllPods) != 1 || report.AllPods[0].CPUEfficiency != 0.5 {
		t.Errorf("legacy CPUEfficiency changed: %+v", report.AllPods)
	}
}

// TestWasteUnitConversion guards against mixing millicores/cores and bytes/GiB.
func TestWasteUnitConversion(t *testing.T) {
	p := PodEfficiency{
		Name:            "app",
		Namespace:       "prod",
		CPURequest:      1000, // 1 core
		MemRequest:      2 * gib,
		CPUUnitPrice:    0.03,  // $/core/hour
		RAMUnitPrice:    0.004, // $/GiB/hour
		CPUP95Usage:     0.4,
		CPUP95Available: true,
		MemoryP95Usage:  1 * gib,
		MemP95Available: true,
		ObservedSpan:    24 * time.Hour,
	}
	est := EstimateWaste([]PodEfficiency{p}, WasteConfig{Percentile: 0.95, Factor: 1.5})
	pe := est.Pods[0]
	// CPU: 1 - 0.6 = 0.4 core. Memory: 2 - 1.5 = 0.5 GiB.
	approx(t, pe.CPUWasteMonthly, 0.4*0.03*730, "CPUWasteMonthly")
	approx(t, pe.MemWasteMonthly, 0.5*0.004*730, "MemWasteMonthly")
	approx(t, pe.MonthlyWaste, 0.4*0.03*730+0.5*0.004*730, "MonthlyWaste")
}
