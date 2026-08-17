package gitops

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Pod-template-bearing workload kinds whose containers declare resources.
const (
	kindPod         = "Pod"
	kindDeployment  = "Deployment"
	kindStatefulSet = "StatefulSet"
	kindDaemonSet   = "DaemonSet"
	kindReplicaSet  = "ReplicaSet"
	kindJob         = "Job"
	kindCronJob     = "CronJob"
)

// ResourceAmount is a summed CPU/memory quantity in canonical units:
// milli-CPU (1000 = one core) and bytes.
type ResourceAmount struct {
	MilliCPU    int64 `json:"milliCPU"`
	MemoryBytes int64 `json:"memoryBytes"`
}

// CPUString renders the amount as a Kubernetes CPU quantity ("1500m", "2").
func (a ResourceAmount) CPUString() string { return formatCPU(a.MilliCPU) }

// MemoryString renders the amount as a Kubernetes memory quantity ("512Mi", "2Gi").
func (a ResourceAmount) MemoryString() string { return formatMemory(a.MemoryBytes) }

// WorkloadSizing is the summed sizing for one rendered workload. Requests and
// Limits are per-replica totals across the workload's containers; the render
// total multiplies them by Replicas, matching what the scheduler reserves.
type WorkloadSizing struct {
	Kind            string         `json:"kind"`
	Namespace       string         `json:"namespace,omitempty"`
	Name            string         `json:"name"`
	Replicas        int            `json:"replicas"`
	Containers      int            `json:"containers"`
	Requests        ResourceAmount `json:"requests"`
	Limits          ResourceAmount `json:"limits"`
	MissingRequests bool           `json:"missingRequests,omitempty"`
	MissingLimits   bool           `json:"missingLimits,omitempty"`
}

// SizingReport is the render-time view of what a rendered tree asks the target
// cell to schedule: per-workload sizing plus the reservation totals and the
// coverage gaps codefly can catch offline (workloads with no requests or no
// limits). The cell-capacity comparison the issue calls for needs node/SKU data
// the environment does not yet carry; TotalRequests is the number to compare
// against it once it does.
type SizingReport struct {
	Workloads                []WorkloadSizing `json:"workloads"`
	TotalRequests            ResourceAmount   `json:"totalRequests"`
	TotalLimits              ResourceAmount   `json:"totalLimits"`
	WorkloadsMissingRequests int              `json:"workloadsMissingRequests"`
	WorkloadsMissingLimits   int              `json:"workloadsMissingLimits"`
}

// renderSizing computes the sizing report for a rendered owned tree, counting
// the effective (kustomize-built) workloads so base and overlay copies of the
// same object are not summed twice.
func renderSizing(root string) (SizingReport, error) {
	manifests, err := effectiveManifests(root)
	if err != nil {
		return SizingReport{}, err
	}
	return computeSizing(manifests), nil
}

// effectiveManifests returns the deduplicated manifest set for a rendered tree:
// kustomize-built output for anything a kustomization covers, raw manifests for
// everything else. It mirrors the coverage bookkeeping validateTree relies on.
func effectiveManifests(root string) ([]manifest, error) {
	var manifests []manifest
	var kustomizations []kustomization
	err := walkRegularFiles(root, func(path, relative string, _ os.FileInfo) error {
		if relative == InventoryFilename {
			return nil
		}
		extension := strings.ToLower(filepath.Ext(relative))
		if extension != yamlExtension && extension != ymlExtension && extension != jsonExtension {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", relative, err)
		}
		decoded, customization, err := decodeYAML(relative, data)
		if err != nil {
			return err
		}
		manifests = append(manifests, decoded...)
		if customization != nil {
			kustomizations = append(kustomizations, *customization)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	covered, effective, err := renderKustomizations(root, kustomizations)
	if err != nil {
		return nil, err
	}
	result := append([]manifest(nil), effective...)
	for _, item := range manifests {
		source := strings.SplitN(item.path, "#", 2)[0]
		if covered[source] {
			continue
		}
		result = append(result, item)
	}
	return result, nil
}

func computeSizing(manifests []manifest) SizingReport {
	var report SizingReport
	for _, item := range manifests {
		spec, ok := podSpec(item)
		if !ok {
			continue
		}
		containers := sliceField(spec, "containers")
		if len(containers) == 0 {
			continue
		}
		replicas := workloadReplicas(item)
		workload := WorkloadSizing{
			Kind:       item.kind,
			Namespace:  metadataString(item.value, "namespace"),
			Name:       metadataString(item.value, "name"),
			Replicas:   replicas,
			Containers: len(containers),
		}
		for _, raw := range containers {
			container, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			resources := mapField(container, "resources")
			requestCPU, requestMemory, hasRequests := amounts(resources, "requests")
			limitCPU, limitMemory, hasLimits := amounts(resources, "limits")
			workload.Requests.MilliCPU += requestCPU
			workload.Requests.MemoryBytes += requestMemory
			workload.Limits.MilliCPU += limitCPU
			workload.Limits.MemoryBytes += limitMemory
			if !hasRequests {
				workload.MissingRequests = true
			}
			if !hasLimits {
				workload.MissingLimits = true
			}
		}
		report.Workloads = append(report.Workloads, workload)
		report.TotalRequests.MilliCPU += workload.Requests.MilliCPU * int64(replicas)
		report.TotalRequests.MemoryBytes += workload.Requests.MemoryBytes * int64(replicas)
		report.TotalLimits.MilliCPU += workload.Limits.MilliCPU * int64(replicas)
		report.TotalLimits.MemoryBytes += workload.Limits.MemoryBytes * int64(replicas)
		if workload.MissingRequests {
			report.WorkloadsMissingRequests++
		}
		if workload.MissingLimits {
			report.WorkloadsMissingLimits++
		}
	}
	sort.Slice(report.Workloads, func(i, j int) bool {
		if report.Workloads[i].Kind != report.Workloads[j].Kind {
			return report.Workloads[i].Kind < report.Workloads[j].Kind
		}
		return report.Workloads[i].Name < report.Workloads[j].Name
	})
	return report
}

// podSpec returns the PodSpec for the pod-template-bearing kinds render emits,
// reporting whether the manifest carries one at all.
func podSpec(item manifest) (map[string]any, bool) {
	spec := mapField(item.value, "spec")
	if spec == nil {
		return nil, false
	}
	switch item.kind {
	case kindPod:
		return spec, true
	case kindDeployment, kindStatefulSet, kindDaemonSet, kindReplicaSet, kindJob:
		podSpec := mapField(mapField(spec, "template"), "spec")
		return podSpec, podSpec != nil
	case kindCronJob:
		podSpec := mapField(mapField(mapField(mapField(spec, "jobTemplate"), "spec"), "template"), "spec")
		return podSpec, podSpec != nil
	default:
		return nil, false
	}
}

// workloadReplicas returns the declared replica count for the kinds that carry
// one, defaulting to 1 when unset and honoring an explicit 0. DaemonSets scale
// with node count, which is a capacity-side unknown at render time, so they
// count as a single replica.
func workloadReplicas(item manifest) int {
	switch item.kind {
	case kindDeployment, kindStatefulSet, kindReplicaSet:
		if replicas, ok := intValue(mapField(item.value, "spec")["replicas"]); ok {
			if replicas < 0 {
				return 0
			}
			return replicas
		}
	}
	return 1
}

// amounts sums the cpu and memory of one requests/limits block, reporting
// whether both were present and parseable. A block missing either quantity is
// an incomplete declaration for coverage purposes.
func amounts(resources map[string]any, key string) (int64, int64, bool) {
	block := mapField(resources, key)
	if block == nil {
		return 0, 0, false
	}
	cpu, cpuOK := parseCPU(quantityString(block["cpu"]))
	memory, memoryOK := parseMemory(quantityString(block["memory"]))
	return cpu, memory, cpuOK && memoryOK
}

func mapField(value map[string]any, key string) map[string]any {
	nested, _ := value[key].(map[string]any)
	return nested
}

func sliceField(value map[string]any, key string) []any {
	nested, _ := value[key].([]any)
	return nested
}

func intValue(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), true
	default:
		return 0, false
	}
}

// quantityString normalizes a YAML scalar into the string form the quantity
// parsers expect. Suffixed quantities ("100m", "512Mi") decode as strings;
// bare numbers ("cpu: 1") decode as int or float.
func quantityString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return ""
	}
}

// parseCPU parses a Kubernetes CPU quantity into milli-CPU.
func parseCPU(value string) (int64, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if milli, ok := strings.CutSuffix(value, "m"); ok {
		amount, err := strconv.ParseFloat(milli, 64)
		if err != nil {
			return 0, false
		}
		return int64(math.Round(amount)), true
	}
	cores, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, false
	}
	return int64(math.Round(cores * 1000)), true
}

var binaryMemorySuffix = map[string]int64{
	"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30,
	"Ti": 1 << 40, "Pi": 1 << 50, "Ei": 1 << 60,
}

var decimalMemorySuffix = map[string]float64{
	"m": 1e-3, "k": 1e3, "M": 1e6, "G": 1e9,
	"T": 1e12, "P": 1e15, "E": 1e18,
}

// parseMemory parses a Kubernetes memory quantity into bytes.
func parseMemory(value string) (int64, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	for suffix, multiplier := range binaryMemorySuffix {
		if digits, ok := strings.CutSuffix(value, suffix); ok {
			amount, err := strconv.ParseFloat(digits, 64)
			if err != nil {
				return 0, false
			}
			return int64(amount * float64(multiplier)), true
		}
	}
	if multiplier, ok := decimalMemorySuffix[value[len(value)-1:]]; ok {
		amount, err := strconv.ParseFloat(value[:len(value)-1], 64)
		if err != nil {
			return 0, false
		}
		return int64(amount * multiplier), true
	}
	bytes, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, false
	}
	return int64(bytes), true
}

func formatCPU(milliCPU int64) string {
	if milliCPU == 0 {
		return "0"
	}
	if milliCPU%1000 == 0 {
		return strconv.FormatInt(milliCPU/1000, 10)
	}
	return strconv.FormatInt(milliCPU, 10) + "m"
}

func formatMemory(bytes int64) string {
	switch {
	case bytes == 0:
		return "0"
	case bytes%(1<<30) == 0:
		return strconv.FormatInt(bytes/(1<<30), 10) + "Gi"
	case bytes%(1<<20) == 0:
		return strconv.FormatInt(bytes/(1<<20), 10) + "Mi"
	case bytes%(1<<10) == 0:
		return strconv.FormatInt(bytes/(1<<10), 10) + "Ki"
	default:
		return fmt.Sprintf("%.1fMi", float64(bytes)/(1<<20))
	}
}
