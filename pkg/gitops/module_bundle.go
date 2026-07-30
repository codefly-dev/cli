package gitops

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

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
	graph []InventoryService,
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

	relativeModule, err := filepath.Rel(workspace.Dir(), module.Dir())
	if err != nil || !filepath.IsLocal(relativeModule) {
		return fmt.Errorf("module %q is outside the workspace", module.Name)
	}
	stagedModule := filepath.Join(stage, relativeModule)
	if err := copyTree(module.Dir(), stagedModule); err != nil {
		return fmt.Errorf("stage module bundle input: %w", err)
	}
	workspaceData, err := os.ReadFile(filepath.Join(workspace.Dir(), resources.WorkspaceConfigurationName))
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(stage, resources.WorkspaceConfigurationName), workspaceData, 0o600); err != nil {
		return err
	}
	if _, err := command(ctx, stage, binary, stagedModule, module.Name); err != nil {
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
	return nil
}

func loadSelectedModuleBundle(
	root string,
	module string,
	environment *resources.Environment,
	graph []InventoryService,
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
			expectedManaged = append(expectedManaged, service.Service)
		} else {
			expectedServices = append(expectedServices, service.Service)
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

func retainManagedBootstrap(root, service, environment string) (bool, error) {
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
	if len(jobs) == 0 {
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
