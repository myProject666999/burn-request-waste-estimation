package analyzer

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// HeadroomFactor is the safety margin applied to observed percentile usage when
// deriving a reference request. It is shared with the advisor so that both
// surfaces reason from the same recommendation basis: observed usage × factor.
const HeadroomFactor = 1.5

// DefaultMinObservedSpan is the minimum metric coverage a pod needs before its
// percentile usage is trusted for waste estimation. Rolling-update pods that
// appeared recently have shorter coverage than the analysis window and must not
// be treated as if they had been observed for the full period. The threshold is
// injectable via WithMinObservedSpan.
const DefaultMinObservedSpan = time.Hour

// Resource estimation statuses. Requests that are not above the reference are
// reported as under-requested with zero waste; they never offset another
// object's waste with a negative amount.
const (
	WasteStatusEstimated        = "estimated"
	WasteStatusUnderRequested   = "under_requested"
	WasteStatusUnavailable      = "unavailable"         // percentile metric missing
	WasteStatusInsufficientData = "insufficient_sample" // metric coverage below threshold
	WasteStatusNoRequest        = "no_request"
)

// WasteConfig controls a waste estimate run.
type WasteConfig struct {
	// Period is the effective analysis window (after retention shortening).
	Period string
	// Percentile is the usage percentile used to derive the reference request.
	Percentile float64
	// Factor is the headroom multiplier applied to the observed percentile.
	Factor float64
	// MinObservedSpan is the minimum metric coverage required to trust a pod's
	// percentile usage. Zero disables the check.
	MinObservedSpan time.Duration
}

// PodWasteEstimate is the per-pod, per-resource request-above-observation
// estimate. Amounts are monthly USD; zero when the request does not exceed the
// reference (see CPUStatus/MemStatus).
type PodWasteEstimate struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	OwnerKind string `json:"owner_kind,omitempty"`
	OwnerName string `json:"owner_name,omitempty"`

	CPURequestMilli   int64   `json:"cpu_request_millicores"`
	CPUReferenceCores float64 `json:"cpu_reference_cores"`
	CPUWasteMonthly   float64 `json:"cpu_waste_monthly"`
	CPUStatus         string  `json:"cpu_status"`

	MemRequestBytes int64   `json:"mem_request_bytes"`
	MemReferenceGiB float64 `json:"mem_reference_gib"`
	MemWasteMonthly float64 `json:"mem_waste_monthly"`
	MemStatus       string  `json:"mem_status"`

	GPURequest      int64 `json:"gpu_request,omitempty"`
	GPUNotEstimated bool  `json:"gpu_not_estimated,omitempty"`

	ObservedSpanSeconds int64   `json:"observed_span_seconds"`
	MonthlyWaste        float64 `json:"monthly_waste"`
}

// WorkloadWaste aggregates replica estimates at the workload level. Replicas of
// the same workload that share a resource configuration form one entry: the
// reference is the largest per-replica reference in the group, then multiplied
// by the number of replicas that contributed usable data. Replicas with
// differing configurations are split into separate entries (see Config).
type WorkloadWaste struct {
	Namespace string `json:"namespace"`
	Kind      string `json:"kind"` // empty for a bare pod
	Name      string `json:"name"`
	Config    string `json:"config"` // e.g. "2000m/4.00Gi/0gpu"

	Replicas      int `json:"replicas"`
	EffectiveCPUs int `json:"effective_cpu_replicas"`
	EffectiveMems int `json:"effective_mem_replicas"`

	CPURequestMilli   int64   `json:"cpu_request_millicores"`
	CPUReferenceCores float64 `json:"cpu_reference_cores"`
	CPUWasteMonthly   float64 `json:"cpu_waste_monthly"`
	CPUStatus         string  `json:"cpu_status"`

	MemRequestBytes int64   `json:"mem_request_bytes"`
	MemReferenceGiB float64 `json:"mem_reference_gib"`
	MemWasteMonthly float64 `json:"mem_waste_monthly"`
	MemStatus       string  `json:"mem_status"`

	GPURequest      int64 `json:"gpu_request,omitempty"`
	GPUNotEstimated bool  `json:"gpu_not_estimated,omitempty"`

	MonthlyWaste float64 `json:"monthly_waste"`
}

// NamespaceWaste rolls workload estimates up per namespace. PerPodSumMonthly is
// the naive per-pod sum (overwaste and under-request treated independently per
// replica); MonthlyWaste uses the workload reference (max replica reference ×
// effective replica count). Difference exposes the numeric gap between the two
// accounting bases so neither is silently mixed with the other.
type NamespaceWaste struct {
	Namespace        string  `json:"namespace"`
	Workloads        int     `json:"workloads"`
	MonthlyWaste     float64 `json:"monthly_waste"`
	PerPodSumMonthly float64 `json:"per_pod_sum_monthly"`
	Difference       float64 `json:"difference"`
}

// WasteEstimate is the structured request-above-observation estimate attached to
// a CostReport. Existing report fields keep their original meaning; this is an
// additive, separately-scoped view of over-requesting.
type WasteEstimate struct {
	Period          string    `json:"period"`
	Percentile      float64   `json:"percentile"`
	Factor          float64   `json:"factor"`
	MinObservedSpan string    `json:"min_observed_span"`
	GeneratedAt     time.Time `json:"generated_at"`

	MonthlyWaste     float64 `json:"monthly_waste"`
	CPUWasteMonthly  float64 `json:"cpu_waste_monthly"`
	MemWasteMonthly  float64 `json:"mem_waste_monthly"`
	PerPodSumMonthly float64 `json:"per_pod_sum_monthly"`

	// Counts that must stay separate from the monetary totals. UnderRequested*
	// count pods whose resource request is at or below the reference (zero
	// waste, never a negative offset). Unavailable* and InsufficientSamples count
	// pods that could not be priced for that reason; InsufficientSamples is
	// counted once per pod even when both resources are affected.
	UnderRequestedCPU   int `json:"under_requested_cpu"`
	UnderRequestedMem   int `json:"under_requested_mem"`
	UnavailableCPU      int `json:"unavailable_cpu"`
	UnavailableMem      int `json:"unavailable_mem"`
	InsufficientSamples int `json:"insufficient_samples"`
	NotEstimatedPods    int `json:"not_estimated_pods"`
	SkippedPods         int `json:"skipped_pods"`
	GPUPods             int `json:"gpu_pods"`

	Pods       []PodWasteEstimate `json:"pods"`
	Workloads  []WorkloadWaste    `json:"workloads"`
	Namespaces []NamespaceWaste   `json:"namespaces"`
}

// EstimateWaste computes the request-above-observation estimate.
//
// Units: CPU usage is cores and requests are millicores (divided by 1000);
// memory usage is bytes and prices are per GiB (divided by 1024^3). Unit prices
// are $/core/hour and $/GiB/hour, so monthly amounts multiply by hoursPerMonth.
// All amounts are non-negative: under-requested resources contribute zero and
// are counted, never subtracted.
func EstimateWaste(pods []PodEfficiency, cfg WasteConfig) *WasteEstimate {
	if cfg.Percentile <= 0 {
		cfg.Percentile = 0.95
	}
	if cfg.Factor <= 0 {
		cfg.Factor = HeadroomFactor
	}
	if cfg.MinObservedSpan < 0 {
		cfg.MinObservedSpan = 0
	}

	est := &WasteEstimate{
		Period:          cfg.Period,
		Percentile:      cfg.Percentile,
		Factor:          cfg.Factor,
		MinObservedSpan: cfg.MinObservedSpan.String(),
		GeneratedAt:     time.Now().UTC(),
		Pods:            make([]PodWasteEstimate, 0, len(pods)),
	}

	groups := newWorkloadGroups()
	for i := range pods {
		p := &pods[i]
		pe := estimatePod(p, cfg)
		est.Pods = append(est.Pods, pe)

		est.CPUWasteMonthly += pe.CPUWasteMonthly
		est.MemWasteMonthly += pe.MemWasteMonthly
		est.PerPodSumMonthly += pe.MonthlyWaste

		switch pe.CPUStatus {
		case WasteStatusUnderRequested:
			est.UnderRequestedCPU++
		case WasteStatusUnavailable:
			est.UnavailableCPU++
		}
		switch pe.MemStatus {
		case WasteStatusUnderRequested:
			est.UnderRequestedMem++
		case WasteStatusUnavailable:
			est.UnavailableMem++
		}
		if pe.CPUStatus == WasteStatusInsufficientData || pe.MemStatus == WasteStatusInsufficientData {
			est.InsufficientSamples++
		}
		if pe.GPUNotEstimated {
			est.GPUPods++
		}
		if !podHasEstimate(pe) {
			est.NotEstimatedPods++
		}

		groups.add(p, pe)
	}

	est.Workloads = groups.workloads(cfg)
	est.Namespaces = namespacesFromWorkloads(est.Workloads, est.Pods)

	for _, w := range est.Workloads {
		est.MonthlyWaste += w.MonthlyWaste
	}

	sort.SliceStable(est.Pods, func(i, j int) bool {
		if est.Pods[i].Namespace != est.Pods[j].Namespace {
			return est.Pods[i].Namespace < est.Pods[j].Namespace
		}
		return est.Pods[i].Name < est.Pods[j].Name
	})

	return est
}

func podHasEstimate(pe PodWasteEstimate) bool {
	return pe.CPUStatus == WasteStatusEstimated ||
		pe.CPUStatus == WasteStatusUnderRequested ||
		pe.MemStatus == WasteStatusEstimated ||
		pe.MemStatus == WasteStatusUnderRequested
}

// estimatePod evaluates one pod's CPU and memory requests against its observed
// reference. A pod skipped by calculatePodEfficiencies never reaches here.
func estimatePod(p *PodEfficiency, cfg WasteConfig) PodWasteEstimate {
	pe := PodWasteEstimate{
		Name:                p.Name,
		Namespace:           p.Namespace,
		OwnerKind:           p.OwnerKind,
		OwnerName:           p.OwnerName,
		CPURequestMilli:     p.CPURequest,
		MemRequestBytes:     p.MemRequest,
		GPURequest:          p.GPURequest,
		GPUNotEstimated:     p.GPURequest > 0,
		ObservedSpanSeconds: int64(p.ObservedSpan.Seconds()),
	}

	// CPU.
	switch {
	case p.CPURequest <= 0:
		pe.CPUStatus = WasteStatusNoRequest
	case !p.CPUP95Available:
		pe.CPUStatus = WasteStatusUnavailable
	case insufficientSample(p.ObservedSpan, cfg.MinObservedSpan):
		pe.CPUStatus = WasteStatusInsufficientData
	default:
		refCores := p.CPUP95Usage * cfg.Factor
		pe.CPUReferenceCores = refCores
		reqCores := float64(p.CPURequest) / 1000.0
		if reqCores > refCores {
			pe.CPUWasteMonthly = (reqCores - refCores) * p.CPUUnitPrice * hoursPerMonth
			pe.CPUStatus = WasteStatusEstimated
		} else {
			pe.CPUStatus = WasteStatusUnderRequested
		}
	}

	// Memory.
	switch {
	case p.MemRequest <= 0:
		pe.MemStatus = WasteStatusNoRequest
	case !p.MemP95Available:
		pe.MemStatus = WasteStatusUnavailable
	case insufficientSample(p.ObservedSpan, cfg.MinObservedSpan):
		pe.MemStatus = WasteStatusInsufficientData
	default:
		refBytes := float64(p.MemoryP95Usage) * cfg.Factor
		pe.MemReferenceGiB = refBytes / (1024 * 1024 * 1024)
		reqGiB := float64(p.MemRequest) / (1024 * 1024 * 1024)
		if reqGiB > pe.MemReferenceGiB {
			pe.MemWasteMonthly = (reqGiB - pe.MemReferenceGiB) * p.RAMUnitPrice * hoursPerMonth
			pe.MemStatus = WasteStatusEstimated
		} else {
			pe.MemStatus = WasteStatusUnderRequested
		}
	}

	pe.MonthlyWaste = pe.CPUWasteMonthly + pe.MemWasteMonthly
	return pe
}

func insufficientSample(span, min time.Duration) bool {
	return min > 0 && span > 0 && span < min
}

func configSignature(p *PodEfficiency) string {
	return fmt.Sprintf("%dm/%.2fGi/%dgpu",
		p.CPURequest,
		float64(p.MemRequest)/(1024*1024*1024),
		p.GPURequest,
	)
}

type workloadGroupKey struct {
	namespace string
	kind      string
	name      string
	config    string
}

type workloadGroup struct {
	key     workloadGroupKey
	members []*PodEfficiency
	pods    []PodWasteEstimate
}

type workloadGroups struct {
	order []*workloadGroup
	byKey map[string]*workloadGroup
}

func newWorkloadGroups() *workloadGroups {
	return &workloadGroups{byKey: make(map[string]*workloadGroup)}
}

// add places a pod into its workload group. Workload identity is
// namespace + OwnerKind + OwnerName, so same-name objects of different kinds
// never merge. Replicas with a differing resource configuration land in
// separate groups via the config signature. Pods without an owner kind are
// independent single-member groups.
func (g *workloadGroups) add(p *PodEfficiency, pe PodWasteEstimate) {
	key := workloadGroupKey{
		namespace: p.Namespace,
		kind:      p.OwnerKind,
		name:      p.OwnerName,
		config:    configSignature(p),
	}
	if p.OwnerKind == "" {
		key.name = p.Name
	}
	// \x00 keeps namespace/kind/name/config boundaries unambiguous.
	id := strings.Join([]string{key.namespace, key.kind, key.name, key.config}, "\x00")
	grp, ok := g.byKey[id]
	if !ok {
		grp = &workloadGroup{key: key}
		g.byKey[id] = grp
		g.order = append(g.order, grp)
	}
	grp.members = append(grp.members, p)
	grp.pods = append(grp.pods, pe)
}

// workloads converts groups into workload-level entries using the shared
// reference rule: largest per-replica reference × effective replica count.
func (g *workloadGroups) workloads(cfg WasteConfig) []WorkloadWaste {
	out := make([]WorkloadWaste, 0, len(g.order))
	for _, grp := range g.order {
		w := WorkloadWaste{
			Namespace: grp.key.namespace,
			Kind:      grp.key.kind,
			Name:      grp.key.name,
			Config:    grp.key.config,
			Replicas:  len(grp.members),
			CPUStatus: WasteStatusUnavailable,
			MemStatus: WasteStatusUnavailable,
		}

		// Request per replica is identical within a config group.
		rep := grp.members[0]
		w.CPURequestMilli = rep.CPURequest
		w.MemRequestBytes = rep.MemRequest
		w.GPURequest = rep.GPURequest
		w.GPUNotEstimated = rep.GPURequest > 0

		// CPU: max reference among usable replicas × usable replica count. The
		// price comes from the replica holding the max reference (replicas of one
		// config normally share a node class); a zero-reference max still keeps a
		// usable price so a measured-zero percentile is priced, not dropped.
		var maxCPU float64
		var maxCPUPrice float64
		usableCPU := 0
		for i, pe := range grp.pods {
			if pe.CPUStatus == WasteStatusEstimated || pe.CPUStatus == WasteStatusUnderRequested {
				usableCPU++
				if maxCPUPrice == 0 {
					maxCPUPrice = grp.members[i].CPUUnitPrice
				}
				if pe.CPUReferenceCores > maxCPU {
					maxCPU = pe.CPUReferenceCores
					maxCPUPrice = grp.members[i].CPUUnitPrice
				}
			}
		}
		w.EffectiveCPUs = usableCPU
		if usableCPU > 0 {
			w.CPUReferenceCores = maxCPU
			reqCores := float64(w.CPURequestMilli) / 1000.0
			if reqCores > maxCPU {
				w.CPUWasteMonthly = (reqCores - maxCPU) * float64(usableCPU) * maxCPUPrice * hoursPerMonth
				w.CPUStatus = WasteStatusEstimated
			} else {
				w.CPUStatus = WasteStatusUnderRequested
			}
		}

		// Memory: same rule.
		var maxMem float64
		var maxMemPrice float64
		usableMem := 0
		for i, pe := range grp.pods {
			if pe.MemStatus == WasteStatusEstimated || pe.MemStatus == WasteStatusUnderRequested {
				usableMem++
				if maxMemPrice == 0 {
					maxMemPrice = grp.members[i].RAMUnitPrice
				}
				if pe.MemReferenceGiB > maxMem {
					maxMem = pe.MemReferenceGiB
					maxMemPrice = grp.members[i].RAMUnitPrice
				}
			}
		}
		w.EffectiveMems = usableMem
		if usableMem > 0 {
			w.MemReferenceGiB = maxMem
			reqGiB := float64(w.MemRequestBytes) / (1024 * 1024 * 1024)
			if reqGiB > maxMem {
				w.MemWasteMonthly = (reqGiB - maxMem) * float64(usableMem) * maxMemPrice * hoursPerMonth
				w.MemStatus = WasteStatusEstimated
			} else {
				w.MemStatus = WasteStatusUnderRequested
			}
		}

		w.MonthlyWaste = w.CPUWasteMonthly + w.MemWasteMonthly
		out = append(out, w)
	}

	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Config < b.Config
	})
	return out
}

func namespacesFromWorkloads(workloads []WorkloadWaste, pods []PodWasteEstimate) []NamespaceWaste {
	byNS := make(map[string]*NamespaceWaste)
	order := make([]string, 0)
	ensure := func(ns string) *NamespaceWaste {
		n, ok := byNS[ns]
		if !ok {
			n = &NamespaceWaste{Namespace: ns}
			byNS[ns] = n
			order = append(order, ns)
		}
		return n
	}

	for _, w := range workloads {
		n := ensure(w.Namespace)
		n.Workloads++
		n.MonthlyWaste += w.MonthlyWaste
	}
	for _, p := range pods {
		ensure(p.Namespace).PerPodSumMonthly += p.MonthlyWaste
	}

	out := make([]NamespaceWaste, 0, len(order))
	for _, ns := range order {
		n := byNS[ns]
		n.Difference = n.MonthlyWaste - n.PerPodSumMonthly
		out = append(out, *n)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].MonthlyWaste != out[j].MonthlyWaste {
			return out[i].MonthlyWaste > out[j].MonthlyWaste
		}
		return out[i].Namespace < out[j].Namespace
	})
	return out
}
