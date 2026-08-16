package gitops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

const (
	argoNamespace   = "argocd"
	inClusterServer = "https://kubernetes.default.svc"
	tenantsDir      = "tenants"
	defaultTenant   = "in-cluster"
	// A stamped Application is named "<tenant>-<component>". Kubernetes caps object
	// names at 63 chars, so the tenant is truncated to a fixed budget in the
	// template and the component is bounded to the remainder — the two together can
	// never exceed the limit regardless of the runtime tenant folder name.
	tenantNameBudget    = 20
	componentNameBudget = 63 - tenantNameBudget - 1

	argoAPIVersion      = "argoproj.io/v1alpha1"
	kustomizeAPIVersion = "kustomize.config.k8s.io/v1beta1"

	kindApplication    = "Application"
	kindApplicationSet = "ApplicationSet"
	kindKustomization  = "Kustomization"
)

type argoResourceAuthority struct {
	Group string `yaml:"group"`
	Kind  string `yaml:"kind"`
}

type argoProjectManifest struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Spec struct {
		SourceRepos                []string                `yaml:"sourceRepos"`
		Destinations               []argoDestination       `yaml:"destinations"`
		ClusterResourceWhitelist   []argoResourceAuthority `yaml:"clusterResourceWhitelist"`
		NamespaceResourceWhitelist []argoResourceAuthority `yaml:"namespaceResourceWhitelist"`
	} `yaml:"spec"`
}

type argoDestination struct {
	Namespace string `yaml:"namespace"`
	Server    string `yaml:"server"`
}

// argoBootstrapComponent is one promotable layer (the module resources layer or
// a single service) that the ApplicationSet template stamps onto every tenant.
type argoBootstrapComponent struct {
	Component string
	Overlay   string
	Wave      string
}

type argoMetadata struct {
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
}

type argoApplicationSetManifest struct {
	APIVersion string                 `yaml:"apiVersion"`
	Kind       string                 `yaml:"kind"`
	Metadata   argoMetadata           `yaml:"metadata"`
	Spec       argoApplicationSetSpec `yaml:"spec"`
}

type argoApplicationSetSpec struct {
	GoTemplate        bool               `yaml:"goTemplate"`
	GoTemplateOptions []string           `yaml:"goTemplateOptions"`
	Generators        []argoSetGenerator `yaml:"generators"`
	Template          argoAppTemplate    `yaml:"template"`
}

type argoSetGenerator struct {
	Matrix argoMatrixGenerator `yaml:"matrix"`
}

type argoMatrixGenerator struct {
	Generators []argoLeafGenerator `yaml:"generators"`
}

type argoLeafGenerator struct {
	Git  *argoGitGenerator  `yaml:"git,omitempty"`
	List *argoListGenerator `yaml:"list,omitempty"`
}

type argoGitGenerator struct {
	RepoURL  string             `yaml:"repoURL"`
	Revision string             `yaml:"revision"`
	Files    []argoGitFileEntry `yaml:"files"`
}

type argoGitFileEntry struct {
	Path string `yaml:"path"`
}

type argoListGenerator struct {
	Elements []any `yaml:"elements"`
}

type argoTenantElement struct {
	Tenant string `yaml:"tenant"`
	Server string `yaml:"server"`
}

type argoComponentElement struct {
	Component string `yaml:"component"`
	Overlay   string `yaml:"overlay"`
	Wave      string `yaml:"wave"`
}

type argoAppTemplate struct {
	Metadata argoAppTemplateMetadata `yaml:"metadata"`
	Spec     argoAppTemplateSpec     `yaml:"spec"`
}

type argoAppTemplateMetadata struct {
	Name        string            `yaml:"name"`
	Namespace   string            `yaml:"namespace"`
	Annotations map[string]string `yaml:"annotations"`
	Finalizers  []string          `yaml:"finalizers"`
}

type argoAppTemplateSpec struct {
	Project     string          `yaml:"project"`
	Source      argoAppSource   `yaml:"source"`
	Destination argoDestination `yaml:"destination"`
	SyncPolicy  argoSyncPolicy  `yaml:"syncPolicy"`
}

type argoAppSource struct {
	RepoURL        string `yaml:"repoURL"`
	TargetRevision string `yaml:"targetRevision"`
	Path           string `yaml:"path"`
}

type argoSyncPolicy struct {
	Automated   argoSyncAutomated `yaml:"automated"`
	SyncOptions []string          `yaml:"syncOptions"`
}

type argoSyncAutomated struct {
	Prune    bool `yaml:"prune"`
	SelfHeal bool `yaml:"selfHeal"`
}

type kustomizationManifest struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Resources  []string `yaml:"resources"`
}

func generateArgoBootstrap(
	_ context.Context,
	config *repositoryConfig,
	target string,
	targetPath string,
	inventory *Inventory,
	environment string,
	snapshotRevision string,
	localFetchHost string,
) error {
	if inventory.AppProject == "" {
		return fmt.Errorf("Argo promotion requires an AppProject name")
	}
	if inventory.Namespace == "" {
		return fmt.Errorf("Argo promotion requires an exact destination namespace")
	}
	repository, err := argoRepository(config, localFetchHost)
	if err != nil {
		return err
	}
	clusterResources, namespaceResources, err := snapshotAuthority(target, inventory, environment)
	if err != nil {
		return err
	}
	bootstrap := filepath.Join(target, "bootstrap")
	if err := os.RemoveAll(bootstrap); err != nil {
		return err
	}

	project := argoProjectManifest{APIVersion: "argoproj.io/v1alpha1", Kind: "AppProject"}
	project.Metadata.Name = inventory.AppProject
	project.Metadata.Namespace = argoNamespace
	project.Spec.SourceRepos = []string{repository}
	project.Spec.Destinations = []argoDestination{{Namespace: inventory.Namespace, Server: inClusterServer}}
	project.Spec.ClusterResourceWhitelist = clusterResources
	project.Spec.NamespaceResourceWhitelist = namespaceResources
	if err := writeArgoYAML(filepath.Join(bootstrap, "project.yaml"), project); err != nil {
		return err
	}

	var components []argoBootstrapComponent
	if inventory.ModulePath != "" {
		components = append(components, argoBootstrapComponent{
			Component: argoBoundedName(componentNameBudget, inventory.Module, "resources"),
			Overlay:   filepath.ToSlash(filepath.Join(targetPath, inventory.ModulePath, "overlays", environment)),
			Wave:      "-1",
		})
	}
	for _, unit := range inventory.Units {
		if unit.Path == "" {
			continue
		}
		components = append(components, argoBootstrapComponent{
			Component: argoBoundedName(componentNameBudget, inventory.Module, unit.Name),
			Overlay:   filepath.ToSlash(filepath.Join(targetPath, unit.Path, "overlays", environment)),
			Wave:      "0",
		})
	}

	// The tenant registry is operator-owned content that lives outside every
	// module's publication path, so the driver never wipes it and `git add -A` on
	// the module path never stages its deletion — adding or removing a tenant is a
	// deliberate operator commit, never a side effect of re-publishing a service.
	registry := filepath.ToSlash(filepath.Join(tenantsDir, environment))
	if err := writeArgoApplicationSet(
		filepath.Join(bootstrap, "applicationset.yaml"),
		argoObjectName(inventory.Module, environment),
		inventory.AppProject,
		repository,
		snapshotRevision,
		registry,
		project.Spec.Destinations[0].Namespace,
		components,
	); err != nil {
		return err
	}
	return writeArgoYAML(filepath.Join(bootstrap, "kustomization.yaml"), kustomizationManifest{
		APIVersion: kustomizeAPIVersion,
		Kind:       kindKustomization,
		Resources:  []string{"project.yaml", "applicationset.yaml"},
	})
}

// argoRepository derives the Argo Application repoURL. localFetchHost, when
// non-empty, is the container DNS of the managed local k3d fetch remote; a fetch
// URL on that host is the reachable in-cluster endpoint and is accepted without
// matching the publication repo-url, so repo-url stays a portable github URL.
func argoRepository(config *repositoryConfig, localFetchHost string) (string, error) {
	repository := strings.TrimSpace(config.RepoURL)
	if fetch := strings.TrimSpace(config.FetchRepoURL); fetch != "" {
		parsed, err := url.Parse(fetch)
		if err != nil || parsed.Host == "" || parsed.Path == "" {
			return "", fmt.Errorf("environment gitops.fetch-repo-url must be an absolute HTTP(S) URL")
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return "", fmt.Errorf("environment gitops.fetch-repo-url must use HTTP(S)")
		}
		if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || strings.Contains(parsed.Host, "*") {
			return "", fmt.Errorf("environment gitops.fetch-repo-url contains unsafe authority")
		}
		isLocalFetchRemote := localFetchHost != "" && parsed.Hostname() == localFetchHost
		if !isLocalFetchRemote && !strings.HasPrefix(strings.TrimSpace(config.RepoURL), "file://") {
			matches, matchErr := repositoriesMatch(config.RepoURL, fetch)
			if matchErr != nil {
				return "", fmt.Errorf("environment gitops.fetch-repo-url: %w", matchErr)
			}
			if !matches {
				return "", fmt.Errorf("environment gitops.fetch-repo-url must identify the publication repository")
			}
		}
		repository = parsed.String()
	}
	if strings.HasPrefix(repository, "file://") {
		return "", fmt.Errorf("local Argo promotion requires gitops.fetch-repo-url")
	}
	if repository == "" {
		return "", fmt.Errorf("GitOps repository is required")
	}
	return repository, nil
}

func snapshotAuthority(target string, inventory *Inventory, environment string) ([]argoResourceAuthority, []argoResourceAuthority, error) {
	var sources []string
	if inventory.ModulePath != "" {
		sources = append(sources, filepath.Join(target, filepath.FromSlash(inventory.ModulePath), "overlays", environment))
	}
	for _, unit := range inventory.Units {
		if unit.Path != "" {
			sources = append(sources, filepath.Join(target, filepath.FromSlash(unit.Path), "overlays", environment))
		}
	}
	cluster := make(map[string]argoResourceAuthority)
	namespaced := make(map[string]argoResourceAuthority)
	for _, source := range sources {
		kustomizer := krusty.MakeKustomizer(krusty.MakeDefaultOptions())
		rendered, err := kustomizer.Run(filesys.MakeFsOnDisk(), source)
		if err != nil {
			return nil, nil, fmt.Errorf("build promotion source %s: %w", source, err)
		}
		data, err := rendered.AsYaml()
		if err != nil {
			return nil, nil, err
		}
		manifests, nested, err := decodeYAML(filepath.ToSlash(source), data)
		if err != nil {
			return nil, nil, err
		}
		if nested != nil {
			return nil, nil, fmt.Errorf("promotion source %s rendered a nested Kustomization", source)
		}
		for _, item := range manifests {
			authority := argoResourceAuthority{Group: item.group, Kind: item.kind}
			key := authority.Group + "/" + authority.Kind
			_, knownClusterScoped := clusterScopedKinds[item.kind]
			customClusterScoped := item.group != "" && !isBuiltInAPIGroup(item.group) &&
				item.group != argoAPIGroup && metadataString(item.value, "namespace") == ""
			if knownClusterScoped || customClusterScoped {
				cluster[key] = authority
			} else {
				namespaced[key] = authority
			}
		}
	}
	return sortedAuthority(cluster), sortedAuthority(namespaced), nil
}

func sortedAuthority(values map[string]argoResourceAuthority) []argoResourceAuthority {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]argoResourceAuthority, 0, len(keys))
	for _, key := range keys {
		result = append(result, values[key])
	}
	return result
}

// writeArgoApplicationSet emits a single ApplicationSet whose matrix generators
// fan the promotable components (module resources + services) out over every
// tenant. A built-in in-cluster tenant needs no repository state; every operator
// tenant is discovered from the registry (registry/<tenant>/cluster.json) on the
// mainline, so registering a tenant is adding a folder. The generators track HEAD
// to pick up new tenants, while every Application source is pinned to the
// immutable snapshot revision. The component overlay is carried under its own key
// so it can never collide with the reserved `path` parameter the git generator
// injects for the matched registry file.
func writeArgoApplicationSet(
	path string,
	name string,
	project string,
	repository string,
	revision string,
	registry string,
	namespace string,
	components []argoBootstrapComponent,
) error {
	elements := make([]any, 0, len(components))
	for _, component := range components {
		elements = append(elements, argoComponentElement(component))
	}
	componentGenerator := func() argoLeafGenerator {
		return argoLeafGenerator{List: &argoListGenerator{Elements: elements}}
	}
	inClusterFanOut := argoSetGenerator{Matrix: argoMatrixGenerator{Generators: []argoLeafGenerator{
		{List: &argoListGenerator{Elements: []any{
			argoTenantElement{Tenant: defaultTenant, Server: inClusterServer},
		}}},
		componentGenerator(),
	}}}
	registryFanOut := argoSetGenerator{Matrix: argoMatrixGenerator{Generators: []argoLeafGenerator{
		{Git: &argoGitGenerator{
			RepoURL:  repository,
			Revision: "HEAD",
			Files:    []argoGitFileEntry{{Path: registry + "/*/cluster.json"}},
		}},
		componentGenerator(),
	}}}
	applicationSet := argoApplicationSetManifest{
		APIVersion: argoAPIVersion,
		Kind:       kindApplicationSet,
		Metadata:   argoMetadata{Name: name, Namespace: argoNamespace},
		Spec: argoApplicationSetSpec{
			GoTemplate:        true,
			GoTemplateOptions: []string{"missingkey=error"},
			Generators:        []argoSetGenerator{inClusterFanOut, registryFanOut},
			Template: argoAppTemplate{
				Metadata: argoAppTemplateMetadata{
					Name:        fmt.Sprintf("{{ .tenant | trunc %d }}-{{ .component }}", tenantNameBudget),
					Namespace:   argoNamespace,
					Annotations: map[string]string{"argocd.argoproj.io/sync-wave": "{{ .wave }}"},
					Finalizers:  []string{"resources-finalizer.argocd.argoproj.io"},
				},
				Spec: argoAppTemplateSpec{
					Project: project,
					Source: argoAppSource{
						RepoURL:        repository,
						TargetRevision: revision,
						Path:           "{{ .overlay }}",
					},
					Destination: argoDestination{Namespace: namespace, Server: "{{ .server }}"},
					SyncPolicy: argoSyncPolicy{
						Automated:   argoSyncAutomated{Prune: true, SelfHeal: true},
						SyncOptions: []string{"CreateNamespace=false", "PruneLast=true"},
					},
				},
			},
		},
	}
	return writeArgoYAML(path, applicationSet)
}

func writeArgoYAML(path string, value any) error {
	data, err := yaml.Marshal(value)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func argoObjectName(parts ...string) string {
	return argoBoundedName(63, parts...)
}

// argoBoundedName joins parts into a DNS-safe name of at most limit characters,
// hash-truncating with a stable 10-hex suffix when the join is too long so the
// result stays unique.
func argoBoundedName(limit int, parts ...string) string {
	name := strings.Join(parts, "-")
	if len(name) <= limit {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return strings.TrimRight(name[:limit-11], "-") + "-" + hex.EncodeToString(sum[:])[:10]
}
