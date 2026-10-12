package gitops

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
)

const (
	deployOverlaysDirectory   = "overlays"
	deployVolumesField        = "volumes"
	deployMetadataField       = "metadata"
	deployLabelsField         = "labels"
	deployWorkloadRoleLabel   = "codefly.dev/workload-role"
	deployContainersField     = "containers"
	deployInitContainersField = "initContainers"
	deployImageField          = "image"
	deployJobsFile            = "deploy-jobs.yaml"
	deployAPIVersionField     = "apiVersion"
	deploySpecField           = "spec"
	deployTemplateField       = "template"
	deployNamespaceField      = "namespace"
	deployAnnotationsField    = "annotations"
	deployJobTemplateField    = "jobTemplate"
	deployEnvField            = "env"
	deployValueFromField      = "valueFrom"
	deploySelectorField       = "selector"
	kindService               = "Service"
	deployRoleJob             = "deploy-job"

	// deploySharedWave is where the namespace-level prerequisites every unit
	// reads reconcile: before any workload band.
	deploySharedWave = -100

	// deployBarrierAnnotation is how a unit DECLARES where its own Job belongs
	// relative to its own workload. The aggregate cannot read that from the
	// manifest: "after" is right for a Job migrating a DEPENDENCY's datastore,
	// which needs that datastore's workload up first, and wrong for a Job
	// preparing the schema its own workload is about to serve. Absent, the Job
	// shares its unit's own band, which is how it reconciled when every unit
	// had an Application of its own.
	deployBarrierAnnotation = "codefly.dev/deploy-barrier"
	deployBarrierBefore     = "before-workload"
	deployBarrierAfter      = "after-workload"

	// argoHookSkip marks a Job the renderer ships but Argo must never apply.
	argoHookSkip = "Skip"

	// The generated import Job's budget, and the bounds a module bundle may
	// move it within.
	deployJobBackoffLimit    = 3
	deployJobDeadlineSeconds = 600
	deployJobMaxBackoffLimit = 10
	deployJobMaxDeadline     = 21600

	// deployCatalogMaxBytes bounds the catalog a Job mounts. A ConfigMap's
	// data is capped at 1 MiB, and under client-side apply — Argo CD's default
	// unless ServerSideApply is enabled — the
	// kubectl.kubernetes.io/last-applied-configuration annotation carries a
	// second full copy of the object, so the bound has to leave room for the
	// catalog TWICE plus the Job's own documents inside the API server's
	// request limit. See docs/configuration-contract.md for the
	// ServerSideApply alternative a larger catalog needs.
	deployCatalogMaxBytes = 384 * 1024
)

var deployJobName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
var deployJobCommand = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

type moduleBundleDeployJob struct {
	Name               string                   `json:"name"`
	Service            string                   `json:"service"`
	Command            string                   `json:"command"`
	Catalog            string                   `json:"catalog"`
	Force              bool                     `json:"force,omitempty"`
	Writes             moduleBundleDeployTarget `json:"writes"`
	After              []string                 `json:"after,omitempty"`
	ServiceEnvironment []string                 `json:"serviceEnvironment,omitempty"`
	// BackoffLimit and ActiveDeadlineSeconds move the generated Job's budget
	// off its default. Absent means the default; see deployJobBudget.
	BackoffLimit          *int `json:"backoffLimit,omitempty"`
	ActiveDeadlineSeconds *int `json:"activeDeadlineSeconds,omitempty"`
}

type moduleBundleDeployTarget struct {
	Service  string `json:"service"`
	Endpoint string `json:"endpoint"`
	Port     uint32 `json:"port"`
}

// renderModuleDeployJobs keeps the unit overlays authoritative and adds their
// references to the module overlay. One Argo Application then owns every wave:
// shared prerequisites, dependency workloads, their migrations, import, and
// consumers. Independently reconciling Applications cannot provide this barrier.
func renderModuleDeployJobs(moduleRoot, destination, environment, namespace, module string, jobs []moduleBundleDeployJob, units []InventoryUnit, services []*resources.Service) error {
	jobs = append([]moduleBundleDeployJob(nil), jobs...)
	sortedDeployJobs(jobs)
	units = append([]InventoryUnit(nil), units...)
	sort.Slice(units, func(i, j int) bool { return units[i].Name < units[j].Name })
	byName := map[string]*resources.Service{}
	for _, service := range services {
		byName[service.Name] = service
	}
	levels, err := deployServiceLevels(module, byName)
	if err != nil {
		return err
	}
	unitByName := map[string]InventoryUnit{}
	documents := map[string][]manifest{}
	stage := filepath.Dir(destination)
	for _, unit := range units {
		unitByName[unit.Name] = unit
		if unit.Path == "" {
			continue
		}
		docs, loadErr := effectiveConfiguration(filepath.Join(stage, filepath.FromSlash(unit.Path)), environment)
		if loadErr != nil {
			return fmt.Errorf("read deploy-job unit %s: %w", unit.Name, loadErr)
		}
		documents[unit.Name] = docs
	}
	seen := map[string]bool{}
	var generated []map[string]any
	for i := range jobs {
		job := &jobs[i]
		if len(job.Name) > 63 || !deployJobName.MatchString(job.Name) || !deployJobCommand.MatchString(job.Command) || seen[job.Name] {
			return fmt.Errorf("invalid or repeated deploy job %q", job.Name)
		}
		seen[job.Name] = true
		jobDocuments, jobErr := buildDeployJobDocuments(moduleRoot, namespace, module, job, byName, levels, unitByName, documents)
		if jobErr != nil {
			return jobErr
		}
		generated = append(generated, jobDocuments...)
	}
	overlay := filepath.Join(destination, deployOverlaysDirectory, environment)
	if writeErr := writeDeploymentDocuments(filepath.Join(overlay, deployJobsFile), generated); writeErr != nil {
		return writeErr
	}
	path, data, err := readDeployKustomization(overlay, module, environment)
	if err != nil {
		return err
	}
	var customization map[string]any
	if decodeErr := yaml.Unmarshal(data, &customization); decodeErr != nil {
		return decodeErr
	}
	// Annotate exact effective objects rather than names guessed from service
	// declarations. Namespace and name transforms have already been resolved.
	shared, err := effectiveConfiguration(destination, environment)
	if err != nil {
		return err
	}
	patches := sliceField(customization, "patches")
	for _, doc := range shared {
		patch, patchErr := deployDocumentPatch(doc, deploySharedWave, module)
		if patchErr != nil {
			return patchErr
		}
		if patch != nil {
			patches = append(patches, patch)
		}
	}
	resources := sliceField(customization, resourcesKey)
	resources = append(resources, deployJobsFile)
	for _, unit := range units {
		if unit.Path == "" {
			continue
		}
		relative, pathErr := filepath.Rel(overlay, filepath.Join(stage, filepath.FromSlash(unit.Path), deployOverlaysDirectory, environment))
		if pathErr != nil {
			return pathErr
		}
		resources = append(resources, filepath.ToSlash(relative))
		for _, doc := range documents[unit.Name] {
			band := deploySharedWave
			if _, workload := podSpec(doc); workload {
				band = levels[unit.Name] * 3
			}
			patch, patchErr := deployDocumentPatch(doc, band, unit.Name)
			if patchErr != nil {
				return patchErr
			}
			if patch != nil {
				patches = append(patches, patch)
			}
		}
	}
	customization[resourcesKey], customization["patches"] = resources, patches
	if writeErr := writeArgoYAML(path, customization); writeErr != nil {
		return writeErr
	}
	_, err = effectiveConfiguration(destination, environment)
	return err
}

func buildDeployJobDocuments(moduleRoot, namespace, module string, job *moduleBundleDeployJob, services map[string]*resources.Service, levels map[string]int, unitByName map[string]InventoryUnit, documents map[string][]manifest) ([]map[string]any, error) {
	service := services[job.Service]
	if service == nil || unitByName[job.Service].Managed || len(documents[job.Service]) == 0 {
		return nil, fmt.Errorf("deploy job %q has no rendered running service %q", job.Name, job.Service)
	}
	if writeErr := validateDeployWrite(module, service, job, unitByName, documents); writeErr != nil {
		return nil, writeErr
	}
	if !filepath.IsLocal(filepath.FromSlash(job.Catalog)) {
		return nil, fmt.Errorf("deploy job %q catalog escapes the module", job.Name)
	}
	catalog, err := readWithin(moduleRoot, filepath.FromSlash(job.Catalog))
	if err != nil {
		return nil, fmt.Errorf("deploy job %q catalog: %w", job.Name, err)
	}
	if len(catalog) > deployCatalogMaxBytes || !json.Valid(catalog) {
		return nil, fmt.Errorf(
			"deploy job %q catalog must be JSON of at most %d KiB: it is mounted from a ConfigMap, and client-side apply stores a second copy of that object in its last-applied-configuration annotation",
			job.Name, deployCatalogMaxBytes/1024,
		)
	}
	backoff, deadline, err := deployJobBudget(job)
	if err != nil {
		return nil, err
	}
	pod, container, err := deployJobPod(job, documents[job.Service], namespace)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(catalog)
	catalogName := argoObjectName(job.Name, "catalog", hex.EncodeToString(digest[:])[:12])
	volumeName := "codefly-deploy-catalog"
	podSpec := mapField(pod, deploySpecField)
	for _, raw := range sliceField(podSpec, deployVolumesField) {
		if mapFieldOrEmpty(raw)[envEntryName] == volumeName {
			return nil, fmt.Errorf("deploy job %q conflicts with reserved catalog volume", job.Name)
		}
	}
	podSpec[deployVolumesField] = append(sliceField(podSpec, deployVolumesField), map[string]any{envEntryName: volumeName, "configMap": map[string]any{envEntryName: catalogName}})
	catalogPath := "/codefly/deploy-catalog/catalog.json"
	container["volumeMounts"] = append(sliceField(container, "volumeMounts"), map[string]any{envEntryName: volumeName, "mountPath": "/codefly/deploy-catalog", "readOnly": true})
	args := []any{job.Command, "-catalog", catalogPath}
	if job.Force {
		args = append(args, "-force")
	}
	container["args"] = args
	// The import Job is the CLI's own, generated here rather than patched onto
	// something a unit wrote, so it is the one Job the aggregate makes a Sync
	// hook: the command is idempotent and re-runs with each sync.
	annotations := map[string]any{argoSyncWaveAnnotation: strconv.Itoa(levels[job.Service]*3 - 1), argoHookAnnotation: argoHookSync, argoHookDeletePolicy: argoHookBeforeHookCreation + ",HookSucceeded"}
	return []map[string]any{
		{deployAPIVersionField: "v1", clusterKindKind: kindConfigMap, deployMetadataField: map[string]any{envEntryName: catalogName, deployNamespaceField: namespace, deployAnnotationsField: map[string]any{argoSyncWaveAnnotation: strconv.Itoa(deploySharedWave)}}, "immutable": true, "data": map[string]any{"catalog.json": string(catalog)}},
		{deployAPIVersionField: "batch/v1", clusterKindKind: kindJob, deployMetadataField: map[string]any{envEntryName: job.Name, deployNamespaceField: namespace, deployAnnotationsField: annotations, deployLabelsField: map[string]any{"codefly.dev/deploy-service": job.Service}}, deploySpecField: map[string]any{"backoffLimit": backoff, "activeDeadlineSeconds": deadline, deployTemplateField: pod}},
	}, nil
}

// deployJobBudget resolves the generated Job's retry and deadline budget. A
// catalog import that needs longer than the default blocks its consumer's wave
// by design, so the module bundle can move the budget inside bounds that keep a
// stuck Job from holding a sync open indefinitely.
func deployJobBudget(job *moduleBundleDeployJob) (backoff, deadline int, err error) {
	backoff, deadline = deployJobBackoffLimit, deployJobDeadlineSeconds
	if job.BackoffLimit != nil {
		if *job.BackoffLimit < 0 || *job.BackoffLimit > deployJobMaxBackoffLimit {
			return 0, 0, fmt.Errorf("deploy job %q backoffLimit %d is outside 0..%d", job.Name, *job.BackoffLimit, deployJobMaxBackoffLimit)
		}
		backoff = *job.BackoffLimit
	}
	if job.ActiveDeadlineSeconds != nil {
		if *job.ActiveDeadlineSeconds < 1 || *job.ActiveDeadlineSeconds > deployJobMaxDeadline {
			return 0, 0, fmt.Errorf("deploy job %q activeDeadlineSeconds %d is outside 1..%d", job.Name, *job.ActiveDeadlineSeconds, deployJobMaxDeadline)
		}
		deadline = *job.ActiveDeadlineSeconds
	}
	return backoff, deadline, nil
}

func validateDeployWrite(module string, service *resources.Service, job *moduleBundleDeployJob, units map[string]InventoryUnit, docs map[string][]manifest) error {
	declared := false
	for _, dependency := range service.ServiceDependencies {
		if !dependency.Kind.Participates(resources.StageRun) || dependency.Name != job.Writes.Service || (dependency.Module != "" && dependency.Module != module) {
			continue
		}
		for _, endpoint := range dependency.Endpoints {
			declared = declared || endpoint.Name == job.Writes.Endpoint
		}
	}
	if !declared || job.Writes.Port == 0 || job.Writes.Port > 65535 {
		return fmt.Errorf("deploy job %q writes to an undeclared runtime dependency endpoint", job.Name)
	}
	afterWrite := false
	for _, after := range job.After {
		afterWrite = afterWrite || after == job.Writes.Service
		unit, exists := units[after]
		if !exists || len(docs[after]) == 0 {
			return fmt.Errorf("deploy job %q after service %q has no rendered readiness/migration barrier", job.Name, after)
		}
		barrier := false
		for _, doc := range docs[after] {
			_, workload := podSpec(doc)
			// A Skip hook is never applied, so it is a manifest and not a
			// barrier. It only looked like one while the aggregate rewrote
			// every unit Job into a Sync hook and made it run.
			if deployJobSkipped(doc) {
				continue
			}
			barrier = barrier || (workload && (!unit.Managed || doc.kind == kindJob))
		}
		if !barrier {
			return fmt.Errorf("deploy job %q after service %q has no rendered readiness/migration barrier", job.Name, after)
		}
		// after constrains the running service, too: otherwise its importer
		// could be placed before an unrelated service's readiness wave.
		dependencyFound := false
		for _, dependency := range service.ServiceDependencies {
			dependencyFound = dependencyFound || (dependency.Name == after && dependency.Kind.Participates(resources.StageRun) && (dependency.Module == "" || dependency.Module == module))
		}
		if !dependencyFound {
			return fmt.Errorf("deploy job %q after service %q is not a runtime dependency", job.Name, after)
		}
	}
	if !afterWrite {
		return fmt.Errorf("deploy job %q must run after its write target", job.Name)
	}
	return nil
}

func deployServiceLevels(module string, services map[string]*resources.Service) (map[string]int, error) {
	levels, active := map[string]int{}, map[string]bool{}
	var level func(string) (int, error)
	level = func(name string) (int, error) {
		if value, exists := levels[name]; exists {
			return value, nil
		}
		if active[name] {
			return 0, fmt.Errorf("deploy-job runtime dependency cycle at %q", name)
		}
		active[name] = true
		value := 0
		for _, dependency := range services[name].ServiceDependencies {
			if !dependency.Kind.Participates(resources.StageRun) || (dependency.Module != "" && dependency.Module != module) || services[dependency.Name] == nil {
				continue
			}
			prior, err := level(dependency.Name)
			if err != nil {
				return 0, err
			}
			value = max(value, prior+1)
		}
		active[name], levels[name] = false, value
		return value, nil
	}
	for name := range services {
		if _, err := level(name); err != nil {
			return nil, err
		}
	}
	return levels, nil
}

// readDeployKustomization resolves the overlay's kustomization under every
// spelling kustomize — and the rest of this package — accepts, and returns the
// path it found so the rewrite lands on the same file. A missing kustomization
// is named by module and environment: the bare open error said only that a
// path the caller never wrote did not exist.
func readDeployKustomization(overlay, module, environment string) (string, []byte, error) {
	for _, name := range kustomizationFileNames {
		path := filepath.Join(overlay, name)
		data, err := os.ReadFile(path)
		if err == nil {
			return path, data, nil
		}
		if !os.IsNotExist(err) {
			return "", nil, err
		}
	}
	return "", nil, fmt.Errorf(
		"module %s environment %s declares no kustomization in its bundle overlay %s (one of %s is required)",
		module, environment, overlay, strings.Join(kustomizationFileNames, ", "),
	)
}

// deployDocumentPatch orders one document of the aggregate, or returns no patch
// when the document carries its author's own placement.
//
// A Job the unit renders belongs to the unit, not to the aggregate. Argo
// already makes a plain Job in a wave a barrier — it reports Progressing until
// the Job completes and Degraded when it fails — so ordering this module needs
// no hook conversion, and converting one silently turned a `Skip` into a Job
// Argo ran on every sync and re-ran on the next. Only the wave is the
// aggregate's to set, and only when nothing else claims it.
func deployDocumentPatch(doc manifest, band int, owner string) (map[string]any, error) {
	if doc.kind != kindJob && doc.kind != kindCronJob {
		return deployWavePatch(doc, band), nil
	}
	offset, ordered, err := deployJobOrdering(doc, owner)
	if err != nil || !ordered {
		return nil, err
	}
	return deployWavePatch(doc, band+offset), nil
}

// deployJobSkipped reports whether a document is a hook Argo must never apply.
func deployJobSkipped(doc manifest) bool {
	annotations := mapField(mapField(doc.value, deployMetadataField), deployAnnotationsField)
	return quantityString(annotations[argoHookAnnotation]) == argoHookSkip
}

// deployJobOrdering returns the wave offset, relative to its own unit's band, of
// a Job or CronJob the unit renders. ordered is false when the document must be
// left exactly as the renderer wrote it.
func deployJobOrdering(doc manifest, owner string) (offset int, ordered bool, err error) {
	annotations := mapField(mapField(doc.value, deployMetadataField), deployAnnotationsField)
	name := metadataString(doc.value, envEntryName)
	// A declared sync-wave IS the placement. Replacing it discarded the only
	// statement anyone made about where the Job belongs.
	if quantityString(annotations[argoSyncWaveAnnotation]) != "" {
		return 0, false, nil
	}
	// A Skip hook is a manifest shipped for someone else to run. Ordering it
	// would be the first step towards applying it.
	if deployJobSkipped(doc) {
		return 0, false, nil
	}
	switch barrier := quantityString(annotations[deployBarrierAnnotation]); barrier {
	case deployBarrierBefore:
		return -1, true, nil
	case deployBarrierAfter:
		return 1, true, nil
	case "":
		// Nothing declared. The Job keeps its unit's own band, which is where
		// it reconciled when the unit had an Application to itself. Its phase,
		// when it declares one, still says what it says: a PreSync hook runs
		// before the whole Sync phase, its own workload included.
		return 0, true, nil
	default:
		return 0, false, fmt.Errorf(
			"%s %s/%s declares %s: %q; the aggregate orders a Job by %q or %q",
			doc.kind, owner, name, deployBarrierAnnotation, barrier, deployBarrierBefore, deployBarrierAfter,
		)
	}
}

// deployWorkloadRole is the discriminator a kind's pods carry. Job and CronJob
// pods are never a Service endpoint; every other pod-bearing kind's are.
func deployWorkloadRole(kind string) string {
	if kind == kindJob || kind == kindCronJob {
		return deployRoleJob
	}
	return UnitKindService
}

// deployPodTemplateLabels nests the discriminator under the path a kind carries
// its pod template at. EVERY kind podSpec/podTemplate counts as a workload has
// to be reachable here: a Service's selector is narrowed with the same label,
// so a selectable kind left unlabelled would leave its Service resolving to no
// endpoints at all while every manifest still applied cleanly.
func deployPodTemplateLabels(kind, role string) map[string]any {
	labels := map[string]any{deployMetadataField: map[string]any{deployLabelsField: map[string]any{deployWorkloadRoleLabel: role}}}
	if kind == kindCronJob {
		return map[string]any{deployJobTemplateField: map[string]any{deploySpecField: map[string]any{deployTemplateField: labels}}}
	}
	return map[string]any{deployTemplateField: labels}
}

func deployWavePatch(doc manifest, wave int) map[string]any {
	metadata := map[string]any{
		envEntryName:           metadataString(doc.value, envEntryName),
		deployAnnotationsField: map[string]any{argoSyncWaveAnnotation: strconv.Itoa(wave)},
	}
	if namespace := metadataString(doc.value, deployNamespaceField); namespace != "" {
		metadata[deployNamespaceField] = namespace
	}
	patch := map[string]any{deployAPIVersionField: doc.value[deployAPIVersionField], clusterKindKind: doc.kind, deployMetadataField: metadata}
	if doc.kind == kindService && len(mapField(mapField(doc.value, deploySpecField), deploySelectorField)) != 0 {
		patch[deploySpecField] = map[string]any{deploySelectorField: map[string]any{deployWorkloadRoleLabel: UnitKindService}}
	}
	if _, workload := podSpec(doc); workload {
		role := deployWorkloadRole(doc.kind)
		if doc.kind == kindPod {
			// A bare Pod IS its own template, so the discriminator goes beside
			// the name and namespace this patch already carries.
			metadata[deployLabelsField] = map[string]any{deployWorkloadRoleLabel: role}
		} else {
			patch[deploySpecField] = deployPodTemplateLabels(doc.kind, role)
		}
	}
	data, _ := yaml.Marshal(patch) // the patch holds only YAML scalar/map values
	_, version, qualified := strings.Cut(quantityString(doc.value[deployAPIVersionField]), "/")
	if !qualified {
		version = quantityString(doc.value[deployAPIVersionField])
	}
	return map[string]any{"target": map[string]any{"group": doc.group, "version": version, clusterKindKind: doc.kind, envEntryName: regexp.QuoteMeta(metadataString(doc.value, envEntryName)), deployNamespaceField: regexp.QuoteMeta(metadataString(doc.value, deployNamespaceField))}, "patch": string(data)}
}

func deployJobPod(job *moduleBundleDeployJob, documents []manifest, namespace string) (map[string]any, map[string]any, error) {
	configMaps, err := indexConfigurationMaps(documents)
	if err != nil {
		return nil, nil, err
	}
	var template map[string]any
	primaryName := ""
	for _, doc := range documents {
		if doc.kind != kindDeployment && doc.kind != kindStatefulSet {
			continue
		}
		spec, _ := podSpec(doc)
		for _, raw := range sliceField(spec, deployContainersField) {
			container := mapFieldOrEmpty(raw)
			// The name is how the copied pod's primary container is found
			// again. Without one nothing matches it later, and the Job would
			// be assembled around a container that does not exist.
			if quantityString(container[envEntryName]) == "" {
				return nil, nil, fmt.Errorf(
					"deploy job %q cannot inherit service %q: its %s %s declares a container with no name",
					job.Name, job.Service, doc.kind, metadataString(doc.value, envEntryName),
				)
			}
			identity, identityErr := configMaps.service(container, namespace)
			if identityErr != nil {
				return nil, nil, identityErr
			}
			if identity != job.Service {
				continue
			}
			if template != nil {
				return nil, nil, fmt.Errorf("deploy job %q has more than one service container", job.Name)
			}
			template, primaryName = podTemplate(doc), quantityString(container[envEntryName])
		}
	}
	if template == nil {
		return nil, nil, fmt.Errorf("deploy job %q cannot identify its running service container", job.Name)
	}
	data, err := yaml.Marshal(template)
	if err != nil {
		return nil, nil, err
	}
	var pod map[string]any
	if err := yaml.Unmarshal(data, &pod); err != nil {
		return nil, nil, err
	}
	spec := mapField(pod, deploySpecField)
	if quantityString(spec["serviceAccountName"]) == "" {
		return nil, nil, fmt.Errorf("deploy job %q requires its service's explicit ServiceAccount", job.Name)
	}
	metadata := mapField(pod, deployMetadataField)
	if metadata == nil {
		metadata = map[string]any{}
		pod[deployMetadataField] = metadata
	}
	// Preserve policy and identity labels. The aggregate adds a workload-role
	// discriminator to Service selectors so this pod is never a service endpoint.
	labels := mapField(metadata, deployLabelsField)
	if labels == nil {
		labels = map[string]any{}
	}
	labels["codefly.dev/deploy-job"] = job.Name
	labels[deployWorkloadRoleLabel] = deployRoleJob
	metadata[deployLabelsField] = labels
	annotations := mapField(metadata, deployAnnotationsField)
	if annotations == nil {
		annotations = map[string]any{}
		metadata[deployAnnotationsField] = annotations
	}
	annotations["sidecar.istio.io/nativeSidecar"] = "true"
	delete(annotations, "sidecar.istio.io/status")
	primary, inits, containerErr := deployJobContainers(job, spec, primaryName)
	if containerErr != nil {
		return nil, nil, containerErr
	}
	for _, key := range job.ServiceEnvironment {
		found := false
		for _, raw := range sliceField(primary, "env") {
			entry := mapFieldOrEmpty(raw)
			found = found || entry[envEntryName] == key
		}
		for _, raw := range sliceField(primary, "envFrom") {
			entry := mapFieldOrEmpty(raw)
			prefix, _ := entry["prefix"].(string)
			if suffix, applies := strings.CutPrefix(key, prefix); applies {
				ref := mapField(entry, "configMapRef")
				_, exists := configMaps[namespacedName{namespace: namespace, name: quantityString(ref[envEntryName])}][suffix]
				found = found || exists
			}
		}
		if !environmentDefaultKey.MatchString(key) || !found {
			return nil, nil, fmt.Errorf("deploy job %q cannot inherit required service environment %q", job.Name, key)
		}
	}
	spec[deployContainersField], spec[deployInitContainersField], spec["restartPolicy"] = []any{primary}, inits, "Never"
	return pod, primary, nil
}

func mapFieldOrEmpty(raw any) map[string]any {
	value, _ := raw.(map[string]any)
	return value
}

func writeDeploymentDocuments(path string, documents []map[string]any) error {
	var output strings.Builder
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	for _, document := range documents {
		if err := encoder.Encode(document); err != nil {
			return err
		}
	}
	if err := encoder.Close(); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(output.String()), 0o600)
}

func moduleIncludesUnits(root, environment string) bool {
	_, err := statWithin(root, filepath.Join(deployOverlaysDirectory, environment, deployJobsFile))
	return err == nil
}

// Stable order for the module overlay's unit references and exact object patches.
func sortedDeployJobs(jobs []moduleBundleDeployJob) {
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].Name < jobs[j].Name })
}

// A dev image change retains the Job's exact image inheritance. Only image
// fields of this service's generated Jobs are rewritten; catalog data and jobs
// belonging to another service remain unchanged.
func updateDeployJobImages(root string, inventory *Inventory, service string, images []string) (string, error) {
	if !inventory.ModuleIncludesUnits {
		return "", nil
	}
	relative := filepath.Join(filepath.FromSlash(inventory.ModulePath), deployOverlaysDirectory, inventory.Environment, deployJobsFile)
	data, err := readWithin(root, relative)
	if err != nil {
		return "", err
	}
	docs, _, err := decodeYAML(filepath.ToSlash(relative), data)
	if err != nil {
		return "", err
	}
	changed := false
	var output []map[string]any
	for _, doc := range docs {
		if doc.kind == kindJob && mapField(mapField(doc.value, deployMetadataField), deployLabelsField)["codefly.dev/deploy-service"] == service {
			spec, _ := podSpec(doc)
			for _, key := range []string{deployContainersField, deployInitContainersField} {
				for _, raw := range sliceField(spec, key) {
					container := mapFieldOrEmpty(raw)
					prior, _ := container[deployImageField].(string)
					for _, image := range images {
						if prior != image && imageRepositoryPattern(image).MatchString(prior) {
							container[deployImageField], changed = image, true
						}
					}
				}
			}
		}
		output = append(output, doc.value)
	}
	if !changed {
		return "", nil
	}
	if err := writeDeploymentDocuments(filepath.Join(root, relative), output); err != nil {
		return "", err
	}
	return filepath.ToSlash(relative), nil
}

// deployJobContainers partitions the copied pod's containers into the Job's one
// command container and the sidecars that become native restartable init
// containers, and holds every inherited image to the digest the Job claims.
//
// The init containers the pod already carries are inherited as they are, so
// they are checked too: updateDeployJobImages rewrites their images on a dev
// deployment, and an unpinned one would make "inherits the service's exact
// digest" untrue.
func deployJobContainers(job *moduleBundleDeployJob, spec map[string]any, primaryName string) (map[string]any, []any, error) {
	inits := sliceField(spec, deployInitContainersField)
	for _, raw := range inits {
		container := mapFieldOrEmpty(raw)
		if image, _ := container[deployImageField].(string); !digestImagePattern.MatchString(image) {
			return nil, nil, fmt.Errorf(
				"deploy job %q inherits init container %q with an image that is not digest-pinned",
				job.Name, quantityString(container[envEntryName]),
			)
		}
	}
	var primary map[string]any
	for _, raw := range sliceField(spec, deployContainersField) {
		container := mapFieldOrEmpty(raw)
		if image, _ := container[deployImageField].(string); !digestImagePattern.MatchString(image) {
			return nil, nil, fmt.Errorf("deploy job %q inherits an image that is not digest-pinned", job.Name)
		}
		if quantityString(container[envEntryName]) == primaryName {
			primary = container
			for _, key := range []string{"ports", "livenessProbe", "readinessProbe", "startupProbe", "lifecycle"} {
				delete(container, key)
			}
			continue
		}
		// Native sidecars terminate with the command; ordinary sidecars would
		// keep a successful Job running forever.
		container["restartPolicy"] = "Always"
		inits = append(inits, container)
	}
	if primary == nil {
		return nil, nil, fmt.Errorf("deploy job %q lost its service container %q copying the pod template", job.Name, primaryName)
	}
	return primary, inits, nil
}
