package gitops

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestParseCPU(t *testing.T) {
	tests := []struct {
		in    string
		want  int64
		wantK bool
	}{
		{"100m", 100, true},
		{"0.5", 500, true},
		{"1", 1000, true},
		{"2", 2000, true},
		{"250m", 250, true},
		{"1.5", 1500, true},
		{"", 0, false},
		{"garbage", 0, false},
	}
	for _, test := range tests {
		got, ok := parseCPU(test.in)
		if ok != test.wantK || (ok && got != test.want) {
			t.Errorf("parseCPU(%q) = (%d, %v), want (%d, %v)", test.in, got, ok, test.want, test.wantK)
		}
	}
}

func TestParseMemory(t *testing.T) {
	tests := []struct {
		in    string
		want  int64
		wantK bool
	}{
		{"128Mi", 128 << 20, true},
		{"1Gi", 1 << 30, true},
		{"512Ki", 512 << 10, true},
		{"1M", 1_000_000, true},
		{"1G", 1_000_000_000, true},
		{"1000000", 1_000_000, true},
		{"2.3Mi", 2_411_725, true}, // 2.3*2^20 = 2411724.8, rounded not truncated
		{"", 0, false},
		{"nonsense", 0, false},
	}
	for _, test := range tests {
		got, ok := parseMemory(test.in)
		if ok != test.wantK || (ok && got != test.want) {
			t.Errorf("parseMemory(%q) = (%d, %v), want (%d, %v)", test.in, got, ok, test.want, test.wantK)
		}
	}
}

func TestResourceAmountStrings(t *testing.T) {
	amount := ResourceAmount{MilliCPU: 1500, MemoryBytes: 2 << 30}
	if got := amount.CPUString(); got != "1500m" {
		t.Errorf("CPUString() = %q, want %q", got, "1500m")
	}
	if got := amount.MemoryString(); got != "2Gi" {
		t.Errorf("MemoryString() = %q, want %q", got, "2Gi")
	}
	whole := ResourceAmount{MilliCPU: 2000, MemoryBytes: 256 << 20}
	if got := whole.CPUString(); got != "2" {
		t.Errorf("CPUString() = %q, want %q", got, "2")
	}
	if got := whole.MemoryString(); got != "256Mi" {
		t.Errorf("MemoryString() = %q, want %q", got, "256Mi")
	}
}

func deploymentManifest(name string, replicas int, containers []map[string]any) manifest {
	value := map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": name},
		"spec": map[string]any{
			"replicas": replicas,
			"template": map[string]any{
				"spec": map[string]any{"containers": toAnySlice(containers)},
			},
		},
	}
	return manifest{path: name + ".yaml#1", group: "apps", kind: "Deployment", value: value}
}

func toAnySlice(containers []map[string]any) []any {
	out := make([]any, len(containers))
	for i, c := range containers {
		out[i] = c
	}
	return out
}

func container(cpu, memory, limitCPU, limitMemory string) map[string]any {
	resources := map[string]any{}
	if cpu != "" || memory != "" {
		requests := map[string]any{}
		if cpu != "" {
			requests["cpu"] = cpu
		}
		if memory != "" {
			requests["memory"] = memory
		}
		resources["requests"] = requests
	}
	if limitCPU != "" || limitMemory != "" {
		limits := map[string]any{}
		if limitCPU != "" {
			limits["cpu"] = limitCPU
		}
		if limitMemory != "" {
			limits["memory"] = limitMemory
		}
		resources["limits"] = limits
	}
	return map[string]any{"name": "c", "resources": resources}
}

func TestComputeSizing(t *testing.T) {
	manifests := []manifest{
		deploymentManifest("api", 2, []map[string]any{
			container("100m", "128Mi", "200m", "256Mi"),
			container("50m", "64Mi", "100m", "128Mi"),
		}),
		deploymentManifest("worker", 1, []map[string]any{
			container("100m", "128Mi", "", ""), // missing limits
		}),
		{path: "cm.yaml#1", group: "", kind: "ConfigMap", value: map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "cfg"},
		}},
	}

	report := computeSizing(manifests)

	if len(report.Workloads) != 2 {
		t.Fatalf("workloads = %d, want 2 (ConfigMap must be ignored)", len(report.Workloads))
	}

	api := report.Workloads[0]
	if api.Name != "api" || api.Replicas != 2 || api.Containers != 2 {
		t.Fatalf("api workload = %+v", api)
	}
	if api.Requests.MilliCPU != 150 || api.Requests.MemoryBytes != 192<<20 {
		t.Errorf("api per-replica requests = %+v, want 150m / 192Mi", api.Requests)
	}
	if api.MissingRequests || api.MissingLimits {
		t.Errorf("api should be fully covered: %+v", api)
	}

	worker := report.Workloads[1]
	if !worker.MissingLimits || worker.MissingRequests {
		t.Errorf("worker coverage flags = %+v, want missing limits only", worker)
	}

	// Totals reserve per-replica requests times replicas: api 2×150m + worker 1×100m.
	if report.TotalRequests.MilliCPU != 400 {
		t.Errorf("total request CPU = %dm, want 400m", report.TotalRequests.MilliCPU)
	}
	if report.TotalRequests.MemoryBytes != (2*(192<<20))+(128<<20) {
		t.Errorf("total request memory = %d bytes", report.TotalRequests.MemoryBytes)
	}
	if report.WorkloadsMissingLimits != 1 || report.WorkloadsMissingRequests != 0 {
		t.Errorf("coverage counts = missingRequests %d, missingLimits %d",
			report.WorkloadsMissingRequests, report.WorkloadsMissingLimits)
	}
}

func sidecarContainer(cpu, memory string) map[string]any {
	c := container(cpu, memory, cpu, memory)
	c["restartPolicy"] = "Always"
	return c
}

// TestComputeSizingIncludesInitContainersAndSidecars proves the reservation
// counts native sidecars (restartable init containers), which run for the pod's
// lifetime, and takes the scheduler's max against a plain init container's peak.
// Summing regular containers alone under-reported — the dangerous direction.
func TestComputeSizingIncludesInitContainersAndSidecars(t *testing.T) {
	// Regular 100m/128Mi + sidecar 300m/256Mi run together = 400m/384Mi.
	// A plain init container needs 200m/64Mi while the sidecar is already up:
	// its peak is 200m+300m = 500m CPU, 64Mi+256Mi = 320Mi. The effective
	// request is the max per resource: 500m CPU, 384Mi memory.
	deployment := deploymentManifest("api", 3, []map[string]any{
		container("100m", "128Mi", "", ""),
	})
	spec := deployment.value["spec"].(map[string]any)
	template := spec["template"].(map[string]any)
	pod := template["spec"].(map[string]any)
	pod["initContainers"] = toAnySlice([]map[string]any{
		sidecarContainer("300m", "256Mi"),
		container("200m", "64Mi", "", ""),
	})

	report := computeSizing([]manifest{deployment})

	if len(report.Workloads) != 1 {
		t.Fatalf("workloads = %d, want 1", len(report.Workloads))
	}
	workload := report.Workloads[0]
	if workload.Containers != 3 {
		t.Errorf("containers = %d, want 3 (1 regular + 2 init)", workload.Containers)
	}
	if workload.Requests.MilliCPU != 500 || workload.Requests.MemoryBytes != 384<<20 {
		t.Errorf("effective per-replica requests = %s CPU / %s memory, want 500m / 384Mi",
			workload.Requests.CPUString(), workload.Requests.MemoryString())
	}
	// Reservation is per-replica × 3 replicas.
	if report.TotalRequests.MilliCPU != 1500 || report.TotalRequests.MemoryBytes != 3*(384<<20) {
		t.Errorf("total reserved requests = %s CPU / %s memory",
			report.TotalRequests.CPUString(), report.TotalRequests.MemoryString())
	}
}

// TestComputeSizingFlagsIncompleteButStillSumsIt proves a container that
// declares only a memory request is flagged for coverage yet still contributes
// its declared memory to the totals — the warning must not claim the workload
// reserves nothing.
func TestComputeSizingFlagsIncompleteButStillSumsIt(t *testing.T) {
	deployment := deploymentManifest("cache", 1, []map[string]any{
		container("", "256Mi", "", ""), // memory request only, no cpu
	})

	report := computeSizing([]manifest{deployment})

	workload := report.Workloads[0]
	if !workload.MissingRequests {
		t.Errorf("memory-only request should be flagged incomplete: %+v", workload)
	}
	if workload.Requests.MemoryBytes != 256<<20 {
		t.Errorf("declared memory must still be summed, got %s", workload.Requests.MemoryString())
	}
	if report.TotalRequests.MemoryBytes != 256<<20 {
		t.Errorf("total memory = %s, want 256Mi", report.TotalRequests.MemoryString())
	}
}

const unbuildableKustomization = `resources:
  - deployment.yaml
  - does-not-exist.yaml
`

// TestRenderNonPromotableSizingDoesNotFailRender proves the sizing pass no
// longer holds veto power over a render: a non-promotable render whose
// kustomization cannot be built by kustomize (a missing reference) still
// succeeds, because sizing consumes validateTree's manifests rather than
// re-building the tree on its own error path.
func TestRenderNonPromotableSizingDoesNotFailRender(t *testing.T) {
	result, err := RenderOwnedTree(context.Background(), &RenderOptions{
		Destination: filepath.Join(t.TempDir(), "owned"),
		Module:      "payments", Environment: "production", Promotable: false,
	}, func(_ context.Context, root string) error {
		service := filepath.Join(root, "services", "api")
		if err := os.MkdirAll(service, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(service, "deployment.yaml"), []byte(sizedDeployment), 0o644); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(service, "kustomization.yaml"), []byte(unbuildableKustomization), 0o644)
	})
	if err != nil {
		t.Fatalf("non-promotable render must not fail on sizing: %v", err)
	}
	if len(result.Sizing.Workloads) != 1 {
		t.Fatalf("workloads = %d, want 1", len(result.Sizing.Workloads))
	}
}

const sizedDeployment = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  replicas: 2
  template:
    spec:
      containers:
        - name: api
          image: ghcr.io/codefly-dev/api@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
          resources:
            requests:
              cpu: 250m
              memory: 256Mi
            limits:
              cpu: 500m
              memory: 512Mi
`

// TestRenderSizingReportDeduplicatesKustomizeOverlays renders a tree whose
// deployment is covered by a kustomization; the raw source and the built output
// must not both be summed.
func TestRenderSizingReportDeduplicatesKustomizeOverlays(t *testing.T) {
	result, err := RenderOwnedTree(context.Background(), &RenderOptions{
		Destination: filepath.Join(t.TempDir(), "owned"),
		Module:      "payments", Environment: "production", Promotable: true,
	}, func(_ context.Context, root string) error {
		service := filepath.Join(root, "services", "api")
		if err := os.MkdirAll(service, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(service, "deployment.yaml"), []byte(sizedDeployment), 0o644); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(service, "kustomization.yaml"), []byte("resources:\n  - deployment.yaml\n"), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(result.Sizing.Workloads) != 1 {
		t.Fatalf("workloads = %d, want 1 (kustomize source and build must not double count)", len(result.Sizing.Workloads))
	}
	workload := result.Sizing.Workloads[0]
	if workload.Name != "api" || workload.Replicas != 2 {
		t.Fatalf("workload = %+v", workload)
	}
	if workload.Requests.MilliCPU != 250 || workload.Requests.MemoryBytes != 256<<20 {
		t.Errorf("per-replica requests = %+v, want 250m / 256Mi", workload.Requests)
	}
	// Two replicas reserve 500m CPU and 512Mi memory.
	if result.Sizing.TotalRequests.MilliCPU != 500 || result.Sizing.TotalRequests.MemoryBytes != 512<<20 {
		t.Errorf("total requests = %+v, want 500m / 512Mi", result.Sizing.TotalRequests)
	}
	if result.Sizing.WorkloadsMissingRequests != 0 || result.Sizing.WorkloadsMissingLimits != 0 {
		t.Errorf("expected full coverage, got missingRequests %d missingLimits %d",
			result.Sizing.WorkloadsMissingRequests, result.Sizing.WorkloadsMissingLimits)
	}
}
