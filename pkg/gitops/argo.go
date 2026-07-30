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

type argoApplicationManifest struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name        string            `yaml:"name"`
		Namespace   string            `yaml:"namespace"`
		Annotations map[string]string `yaml:"annotations"`
		Finalizers  []string          `yaml:"finalizers"`
	} `yaml:"metadata"`
	Spec struct {
		Project string `yaml:"project"`
		Source  struct {
			RepoURL        string `yaml:"repoURL"`
			TargetRevision string `yaml:"targetRevision"`
			Path           string `yaml:"path"`
		} `yaml:"source"`
		Destination argoDestination `yaml:"destination"`
		SyncPolicy  struct {
			Automated struct {
				Prune    bool `yaml:"prune"`
				SelfHeal bool `yaml:"selfHeal"`
			} `yaml:"automated"`
			SyncOptions []string `yaml:"syncOptions"`
		} `yaml:"syncPolicy"`
	} `yaml:"spec"`
}

func generateArgoBootstrap(
	_ context.Context,
	config *repositoryConfig,
	target string,
	targetPath string,
	inventory *Inventory,
	environment string,
	snapshotRevision string,
) error {
	if inventory.AppProject == "" {
		return fmt.Errorf("Argo promotion requires an AppProject name")
	}
	if inventory.Namespace == "" {
		return fmt.Errorf("Argo promotion requires an exact destination namespace")
	}
	repository, err := argoRepository(config)
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

	applications := filepath.Join(bootstrap, "applications")
	resources := []string{"project.yaml"}
	if inventory.ModulePath != "" {
		name := argoObjectName(inventory.Module, "resources")
		sourcePath := filepath.ToSlash(filepath.Join(targetPath, inventory.ModulePath, "overlays", environment))
		if err := writeArgoApplication(
			filepath.Join(applications, name+".yaml"),
			name,
			inventory.AppProject,
			repository,
			snapshotRevision,
			sourcePath,
			project.Spec.Destinations[0].Namespace,
			"-1",
		); err != nil {
			return err
		}
		resources = append(resources, filepath.ToSlash(filepath.Join("applications", name+".yaml")))
	}
	for _, service := range inventory.ServiceGraph {
		if service.Path == "" {
			continue
		}
		name := argoObjectName(inventory.Module, service.Service)
		sourcePath := filepath.ToSlash(filepath.Join(targetPath, service.Path, "overlays", environment))
		if err := writeArgoApplication(
			filepath.Join(applications, name+".yaml"),
			name,
			inventory.AppProject,
			repository,
			snapshotRevision,
			sourcePath,
			project.Spec.Destinations[0].Namespace,
			"0",
		); err != nil {
			return err
		}
		resources = append(resources, filepath.ToSlash(filepath.Join("applications", name+".yaml")))
	}
	return writeArgoYAML(filepath.Join(bootstrap, "kustomization.yaml"), map[string]any{
		"apiVersion": "kustomize.config.k8s.io/v1beta1",
		"kind":       "Kustomization",
		"resources":  resources,
	})
}

func argoRepository(config *repositoryConfig) (string, error) {
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
	for _, service := range inventory.ServiceGraph {
		if service.Path != "" {
			sources = append(sources, filepath.Join(target, filepath.FromSlash(service.Path), "overlays", environment))
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

func writeArgoApplication(
	path string,
	name string,
	project string,
	repository string,
	revision string,
	sourcePath string,
	namespace string,
	wave string,
) error {
	application := argoApplicationManifest{APIVersion: "argoproj.io/v1alpha1", Kind: "Application"}
	application.Metadata.Name = name
	application.Metadata.Namespace = argoNamespace
	application.Metadata.Annotations = map[string]string{"argocd.argoproj.io/sync-wave": wave}
	application.Metadata.Finalizers = []string{"resources-finalizer.argocd.argoproj.io"}
	application.Spec.Project = project
	application.Spec.Source.RepoURL = repository
	application.Spec.Source.TargetRevision = revision
	application.Spec.Source.Path = sourcePath
	application.Spec.Destination = argoDestination{Namespace: namespace, Server: inClusterServer}
	application.Spec.SyncPolicy.Automated.Prune = true
	application.Spec.SyncPolicy.Automated.SelfHeal = true
	application.Spec.SyncPolicy.SyncOptions = []string{"CreateNamespace=false", "PruneLast=true"}
	return writeArgoYAML(path, application)
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
	name := strings.Join(parts, "-")
	if len(name) <= 63 {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return strings.TrimRight(name[:52], "-") + "-" + hex.EncodeToString(sum[:])[:10]
}
