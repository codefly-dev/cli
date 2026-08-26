package gitops

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
)

const moduleBundleSchema = "codefly.dev/module-bundle/v1"

type moduleBundle struct {
	SchemaVersion string                    `json:"schemaVersion"`
	Module        string                    `json:"module"`
	Environments  []moduleBundleEnvironment `json:"environments"`
}

type moduleBundleEnvironment struct {
	Name                   string                       `json:"name"`
	Namespace              string                       `json:"namespace"`
	Cluster                string                       `json:"cluster"`
	ResourcePath           string                       `json:"resourcePath"`
	Services               []string                     `json:"services"`
	ManagedServiceHandoffs []moduleBundleManagedHandoff `json:"managedServiceHandoffs,omitempty"`
}

type moduleBundleManagedHandoff struct {
	Service string `json:"service"`
}

func renderModuleBundle(
	ctx context.Context,
	workspace *resources.Workspace,
	module *resources.Module,
	environment *resources.Environment,
	destination string,
	graph []InventoryUnit,
) error {
	binary, err := module.Agent.Path(ctx)
	if err != nil {
		return fmt.Errorf("resolve module bundle generator %s: %w", module.Agent.Identifier(), err)
	}
	stage, err := os.MkdirTemp("", "codefly-module-bundle-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)

	stagedModule := filepath.Join(stage, workspaceStagedModulePath(workspace, module))
	if err := copyModuleInputTree(module.Dir(), stagedModule); err != nil {
		return fmt.Errorf("stage module bundle input: %w", err)
	}
	moduleWorkspaceData, err := encodeTransportNeutralModuleWorkspace(workspace)
	if err != nil {
		return fmt.Errorf("prepare transport-neutral module workspace: %w", err)
	}
	if err := os.WriteFile(filepath.Join(stage, resources.WorkspaceConfigurationName), moduleWorkspaceData, 0o600); err != nil {
		return err
	}
	moduleEnvironment, err := transportNeutralModuleEnvironment(stage)
	if err != nil {
		return fmt.Errorf("prepare transport-neutral module environment: %w", err)
	}
	if _, err := commandWithEnvironment(ctx, stage, moduleEnvironment, binary, stagedModule, module.Name); err != nil {
		return fmt.Errorf("generate transport-neutral module bundle: %w", err)
	}

	root := filepath.Join(stagedModule, "deployment", "kustomize")
	_, selected, err := loadSelectedModuleBundle(root, module.Name, environment, graph)
	if err != nil {
		return err
	}
	if selected.ResourcePath != filepath.ToSlash(filepath.Join("overlays", environment.Name)) {
		return fmt.Errorf(
			"module bundle environment %q resource path is %q, expected %q",
			environment.Name,
			selected.ResourcePath,
			filepath.ToSlash(filepath.Join("overlays", environment.Name)),
		)
	}
	if err := copyEnvironmentBootstrap(root, environment.Name, destination); err != nil {
		return fmt.Errorf("copy selected module bundle: %w", err)
	}
	if _, err := projectResourceQuota(destination, environment.Name, environment.Namespace, environment.ResourceQuota); err != nil {
		return fmt.Errorf("project module namespace resource quota: %w", err)
	}
	if err := validateTransportNeutralModuleBundle(destination); err != nil {
		return err
	}
	return nil
}

// workspaceStagedModulePath is where a module's input tree is copied inside the
// transport-neutral staging root. A vendored module keeps its real
// workspace-relative position; a path-referenced module — one a composition-root
// workspace declares out-of-repo (see AddModuleReference) — has no in-workspace
// position, so it is staged at the modules-layout canonical path. Either way the
// module stays nested under the staged workspace configuration, matching the
// layout the module bundle generator is handed. Only modules-layout workspaces
// carry referenced modules: a flat workspace's sole module always resolves to the
// workspace directory itself and takes the first branch.
func workspaceStagedModulePath(workspace *resources.Workspace, module *resources.Module) string {
	if relative, err := filepath.Rel(workspace.Dir(), module.Dir()); err == nil && filepath.IsLocal(relative) {
		return relative
	}
	return filepath.Join("modules", module.Name)
}

var moduleInputPrunedDirectories = map[string]struct{}{
	".cache": {}, ".codefly": {}, ".git": {}, ".next": {}, ".turbo": {},
	"__pycache__": {}, "build": {}, "coverage": {}, "dist": {}, "node_modules": {},
	"playwright-report": {}, "test-results": {}, "vendor": {},
}

func copyModuleInputTree(source, destination string) error {
	return filepath.Walk(source, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if relative != "." {
			for _, part := range strings.Split(filepath.ToSlash(relative), "/") {
				if _, excluded := moduleInputPrunedDirectories[part]; excluded {
					if info.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
			}
		}
		return copyTreeEntry(destination, path, relative, info)
	})
}

type transportNeutralModuleWorkspace struct {
	Name         string                                 `yaml:"name"`
	Environments []transportNeutralWorkspaceEnvironment `yaml:"environments"`
}

type transportNeutralWorkspaceEnvironment struct {
	Name                 string                                         `yaml:"name"`
	Description          string                                         `yaml:"description,omitempty"`
	NamingScope          string                                         `yaml:"naming-scope,omitempty"`
	Fixture              string                                         `yaml:"fixture,omitempty"`
	ConfigurationProfile string                                         `yaml:"configuration-profile,omitempty"`
	Cluster              *transportNeutralModuleCluster                 `yaml:"cluster,omitempty"`
	Namespace            string                                         `yaml:"namespace,omitempty"`
	Ingress              []resources.EnvironmentIngressRoute            `yaml:"ingress,omitempty"`
	ManagedServices      map[string]resources.EnvironmentManagedService `yaml:"managed-services,omitempty"`
}

type transportNeutralModuleCluster struct {
	Kind string `yaml:"kind,omitempty"`
}

func encodeTransportNeutralModuleWorkspace(workspace *resources.Workspace) ([]byte, error) {
	input := transportNeutralModuleWorkspace{
		Name:         workspace.Name,
		Environments: make([]transportNeutralWorkspaceEnvironment, 0, len(workspace.Environments)),
	}
	for _, environment := range workspace.Environments {
		if environment == nil {
			return nil, fmt.Errorf("workspace contains an empty environment")
		}
		projected := transportNeutralWorkspaceEnvironment{
			Name:                 environment.Name,
			Description:          environment.Description,
			NamingScope:          environment.NamingScope,
			Fixture:              environment.Fixture,
			ConfigurationProfile: environment.ConfigurationProfile,
			Namespace:            environment.Namespace,
			Ingress:              environment.Ingress,
			ManagedServices:      environment.ManagedServices,
		}
		if environment.Cluster != nil {
			projected.Cluster = &transportNeutralModuleCluster{Kind: environment.Cluster.Kind}
		}
		input.Environments = append(input.Environments, projected)
	}
	return yaml.Marshal(input)
}

func transportNeutralModuleEnvironment(stage string) ([]string, error) {
	home := filepath.Join(stage, ".codefly-module-home")
	temp := filepath.Join(stage, ".codefly-module-tmp")
	for _, directory := range []string{home, temp} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, err
		}
	}
	environment := make([]string, 0, 13)
	for _, key := range []string{
		"PATH",
		"LANG",
		"LC_ALL",
		"LC_CTYPE",
		"TZ",
		"SYSTEMROOT",
		"WINDIR",
		"COMSPEC",
		"PATHEXT",
	} {
		if value, found := os.LookupEnv(key); found {
			environment = append(environment, key+"="+value)
		}
	}
	return append(
		environment,
		"HOME="+home,
		"USERPROFILE="+home,
		"TMPDIR="+temp,
		"TMP="+temp,
		"TEMP="+temp,
	), nil
}

func loadSelectedModuleBundle(
	root string,
	module string,
	environment *resources.Environment,
	graph []InventoryUnit,
) (moduleBundle, moduleBundleEnvironment, error) {
	data, err := os.ReadFile(filepath.Join(root, "bundle.json"))
	if err != nil {
		return moduleBundle{}, moduleBundleEnvironment{}, fmt.Errorf("read module bundle: %w", err)
	}
	var bundle moduleBundle
	if err := json.Unmarshal(data, &bundle); err != nil {
		return moduleBundle{}, moduleBundleEnvironment{}, fmt.Errorf("decode module bundle: %w", err)
	}
	if bundle.SchemaVersion != moduleBundleSchema {
		return moduleBundle{}, moduleBundleEnvironment{}, fmt.Errorf("unsupported module bundle schema %q", bundle.SchemaVersion)
	}
	if bundle.Module != module {
		return moduleBundle{}, moduleBundleEnvironment{}, fmt.Errorf("module bundle belongs to %q, expected %q", bundle.Module, module)
	}
	var selected *moduleBundleEnvironment
	for index := range bundle.Environments {
		if bundle.Environments[index].Name == environment.Name {
			if selected != nil {
				return moduleBundle{}, moduleBundleEnvironment{}, fmt.Errorf("module bundle repeats environment %q", environment.Name)
			}
			selected = &bundle.Environments[index]
		}
	}
	if selected == nil {
		return moduleBundle{}, moduleBundleEnvironment{}, fmt.Errorf("module bundle has no environment %q", environment.Name)
	}
	if environment.Namespace == "" {
		return moduleBundle{}, moduleBundleEnvironment{}, fmt.Errorf(
			"environment %q requires an explicit namespace for GitOps rendering",
			environment.Name,
		)
	}
	if selected.Namespace != environment.Namespace {
		return moduleBundle{}, moduleBundleEnvironment{}, fmt.Errorf(
			"module bundle environment %q namespace is %q, expected %q",
			environment.Name,
			selected.Namespace,
			environment.Namespace,
		)
	}
	if environment.Cluster == nil || environment.Cluster.Kind == "" {
		return moduleBundle{}, moduleBundleEnvironment{}, fmt.Errorf(
			"environment %q requires an explicit cluster kind for GitOps rendering",
			environment.Name,
		)
	}
	if selected.Cluster != environment.Cluster.Kind {
		return moduleBundle{}, moduleBundleEnvironment{}, fmt.Errorf(
			"module bundle environment %q cluster is %q, expected %q",
			environment.Name,
			selected.Cluster,
			environment.Cluster.Kind,
		)
	}

	var expectedServices []string
	var expectedManaged []string
	for _, service := range graph {
		if service.Managed {
			expectedManaged = append(expectedManaged, service.Name)
		} else {
			expectedServices = append(expectedServices, service.Name)
		}
	}
	actualServices := append([]string(nil), selected.Services...)
	var actualManaged []string
	for _, handoff := range selected.ManagedServiceHandoffs {
		actualManaged = append(actualManaged, handoff.Service)
	}
	for _, values := range [][]string{expectedServices, expectedManaged, actualServices, actualManaged} {
		sort.Strings(values)
	}
	if !equalStrings(actualServices, expectedServices) {
		return moduleBundle{}, moduleBundleEnvironment{}, fmt.Errorf(
			"module bundle environment %q services %v differ from rendered in-cluster graph %v",
			environment.Name,
			actualServices,
			expectedServices,
		)
	}
	if !equalStrings(actualManaged, expectedManaged) {
		return moduleBundle{}, moduleBundleEnvironment{}, fmt.Errorf(
			"module bundle environment %q managed handoffs %v differ from rendered managed graph %v",
			environment.Name,
			actualManaged,
			expectedManaged,
		)
	}
	return bundle, *selected, nil
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func validateTransportNeutralModuleBundle(root string) error {
	return walkRegularFiles(root, func(path, relative string, _ os.FileInfo) error {
		extension := strings.ToLower(filepath.Ext(relative))
		if extension != ".yaml" && extension != ".yml" && extension != jsonExtension {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		manifests, _, err := decodeYAML(relative, data)
		if err != nil {
			return err
		}
		for _, item := range manifests {
			if item.group != argoAPIGroup {
				continue
			}
			switch item.kind {
			case "Application", "ApplicationSet", "AppProject":
				return fmt.Errorf(
					"module bundle contains CLI-owned Argo transport resource %s in %s",
					item.kind,
					item.path,
				)
			}
		}
		return nil
	})
}

// retainManagedBundle assembles a managed service's promotable bundle from the
// two things a managed service contributes to the cluster: the bootstrap Jobs its
// agent emitted (a one-shot handoff) and the ExternalSecret projection its
// environment declares. It reports whether a bundle was produced; when the service
// contributes neither, its rendered tree is removed and false is returned so the
// caller records it as an unrendered managed unit.
func retainManagedBundle(
	root, service, environment, namespace string,
	secretRefs []resources.EnvironmentManagedSecretReference,
) (bool, error) {
	var jobs []map[string]any
	err := walkRegularFiles(root, func(path, relative string, _ os.FileInfo) error {
		extension := filepath.Ext(relative)
		if extension != ".yaml" && extension != ".yml" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		manifests, _, err := decodeYAML(relative, data)
		if err != nil {
			return err
		}
		for _, item := range manifests {
			if item.group != "batch" || item.kind != "Job" {
				continue
			}
			metadata, _ := item.value["metadata"].(map[string]any)
			labels, _ := metadata["labels"].(map[string]any)
			if labels["codefly.dev/bootstrap-service"] == service {
				jobs = append(jobs, item.value)
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	projection, err := managedSecretProjection(service, namespace, secretRefs)
	if err != nil {
		return false, err
	}
	if len(jobs) == 0 && projection == nil {
		if err := os.RemoveAll(root); err != nil {
			return false, err
		}
		return false, nil
	}

	replacement, err := os.MkdirTemp(filepath.Dir(root), ".managed-bootstrap-*")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(replacement)
	base := filepath.Join(replacement, "base")
	overlay := filepath.Join(replacement, "overlays", environment)
	if err := os.MkdirAll(base, 0o755); err != nil {
		return false, err
	}
	if err := os.MkdirAll(overlay, 0o755); err != nil {
		return false, err
	}
	var resourcesList []string
	for index, job := range jobs {
		name := fmt.Sprintf("job-%d.yaml", index+1)
		data, err := yaml.Marshal(job)
		if err != nil {
			return false, err
		}
		if err := os.WriteFile(filepath.Join(base, name), data, 0o644); err != nil {
			return false, err
		}
		resourcesList = append(resourcesList, name)
	}
	if projection != nil {
		projected, marshalErr := yaml.Marshal(projection)
		if marshalErr != nil {
			return false, marshalErr
		}
		// The projection is an ExternalSecret — a reference to remote keys, never a
		// secret value — so it stays a world-readable manifest like its siblings.
		if writeErr := os.WriteFile(filepath.Join(base, "external-secret.yaml"), projected, 0o644); writeErr != nil { //nolint:gosec
			return false, writeErr
		}
		resourcesList = append(resourcesList, "external-secret.yaml")
	}
	baseKustomization := map[string]any{
		"apiVersion": "kustomize.config.k8s.io/v1beta1",
		"kind":       "Kustomization",
		"resources":  resourcesList,
	}
	data, err := yaml.Marshal(baseKustomization)
	if err != nil {
		return false, err
	}
	if err := os.WriteFile(filepath.Join(base, "kustomization.yaml"), data, 0o644); err != nil {
		return false, err
	}
	kustomization := map[string]any{
		"apiVersion": "kustomize.config.k8s.io/v1beta1",
		"kind":       "Kustomization",
		"resources":  []string{"../../base"},
	}
	data, err = yaml.Marshal(kustomization)
	if err != nil {
		return false, err
	}
	if err := os.WriteFile(filepath.Join(overlay, "kustomization.yaml"), data, 0o644); err != nil {
		return false, err
	}
	if err := os.RemoveAll(root); err != nil {
		return false, err
	}
	if err := os.Rename(replacement, root); err != nil {
		return false, err
	}
	return true, nil
}
