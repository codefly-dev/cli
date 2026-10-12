package gitops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
)

// Local secrets.
//
// A render carries a service's secrets only as references: secretKeyRefs on
// secret-<service>, which in a cell an ExternalSecret materializes from the
// environment's store. A local k3d cluster has no store, so nothing would
// create that Secret and every pod reading it would stay in
// CreateContainerConfigError. The render already holds the values (it turned
// them into references), so for a local environment it writes each service's
// Secret here — beside the workspace's other local state, outside the
// deployments tree, never committed — and `codefly run cluster` applies it to
// the cluster. The promoted tree stays value-free, as every render does.

// localSecretManifest is the Secret a cell's ExternalSecret would create.
type localSecretManifest struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Type       string `json:"type"`
	Metadata   struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
	StringData map[string]string `json:"stringData"`
}

// LocalSecretsDir is where a local environment's Secret manifests are kept:
// <workspace>/.codefly/local-secrets/<environment>/<module>.
func LocalSecretsDir(workspaceDir, environment, module string) string {
	return filepath.Join(workspaceDir, ".codefly", "local-secrets", environment, module)
}

// writeLocalSecrets writes one Secret per service that references secret
// values, and refuses a referenced key with no value — naming it, never a value.
func writeLocalSecrets(
	workspaceDir, environment, module, namespace string,
	keys map[string][]string,
	values map[string]map[string]string,
	templates renderTemplates,
	deployed map[string]*basev0.Configuration,
) error {
	if err := refuseCommittableLocalState(workspaceDir); err != nil {
		return err
	}
	var missing []string
	manifests := map[string][]byte{}
	for unique, declared := range keys {
		if len(declared) == 0 {
			continue
		}
		service := unique[strings.LastIndex(unique, "/")+1:]
		held := values[unique]
		data := make(map[string]string, len(declared))
		for _, key := range declared {
			value, ok := held[key]
			if !ok {
				value, ok = assembleLocalTemplate(templates[key], values, deployed)
			}
			if !ok {
				missing = append(missing, unique+" "+key)
				continue
			}
			data[key] = value
		}
		secret := localSecretManifest{APIVersion: "v1", Kind: "Secret", Type: "Opaque", StringData: data}
		secret.Metadata.Name = "secret-" + service
		secret.Metadata.Namespace = namespace
		secret.Metadata.Labels = map[string]string{"app.kubernetes.io/managed-by": "codefly-local-cluster-run"}
		manifest, err := json.Marshal(secret)
		if err != nil {
			return fmt.Errorf("encode the local secret for %s: %w", unique, err)
		}
		manifests[service] = manifest
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("a local cluster has no secret store, and these referenced secret keys have no value in environment %q: %s; "+
			"declare each in configurations/%s/<group>.secret.env", environment, strings.Join(missing, ", "), environment)
	}
	dir := LocalSecretsDir(workspaceDir, environment, module)
	// Replace the module's set whole: a service that no longer references a
	// secret must not keep a stale one.
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if len(manifests) == 0 {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	for service, manifest := range manifests {
		path := filepath.Join(dir, service+".json")
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("write the local secret for %s: %w", service, err)
		}
		_, writeErr := file.Write(manifest)
		closeErr := file.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

// assembleLocalTemplate evaluates a producer's template for a consumer, as a
// cell's ExternalSecret does from the store: each reference resolves to the
// producer's own value — its secret primitives from the producer's local
// values, its public ones from the configuration it deployed — never another
// service's.
func assembleLocalTemplate(template deliveredTemplate, values map[string]map[string]string, deployed map[string]*basev0.Configuration) (string, bool) {
	if template.template == nil {
		return "", false
	}
	producer := template.producer
	public := resources.ProducerConfigurationValueLookup(deployed[producer])
	lookup := func(configuration, key string) (string, bool) {
		if value, ok := values[producer][resources.ServiceSecretConfigurationKeyFromUnique(producer, configuration, key)]; ok && value != "" {
			return value, true
		}
		return public(configuration, key)
	}
	assembled, err := resources.EvaluateConfigurationValueTemplate(template.template, lookup)
	return assembled, err == nil
}

// LocalSecretManifests returns the module's Secret manifests for a local run
// to apply, or none when the module references no secret.
func LocalSecretManifests(workspaceDir, environment, module string) ([][]byte, error) {
	dir := LocalSecretsDir(workspaceDir, environment, module)
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("local secrets %s must be a private directory (0700, not a link)", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var manifests [][]byte
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if !entry.Type().IsRegular() {
			return nil, fmt.Errorf("local secret %s is not a regular file", entry.Name())
		}
		manifest, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		manifests = append(manifests, manifest)
	}
	return manifests, nil
}

// refuseCommittableLocalState refuses to write secrets where git would track
// them: a workspace under git must ignore .codefly.
func refuseCommittableLocalState(workspaceDir string) error {
	probe := filepath.Join(".codefly", "local-secrets")
	cmd := exec.CommandContext(context.Background(), "git", "-C", workspaceDir, "rev-parse", "--is-inside-work-tree")
	if out, err := cmd.Output(); err != nil || strings.TrimSpace(string(out)) != "true" {
		return nil
	}
	if err := exec.CommandContext(context.Background(), "git", "-C", workspaceDir, "check-ignore", "-q", probe).Run(); err != nil {
		return fmt.Errorf("local secrets would be written to %s, which git does not ignore; add .codefly/ to the workspace's .gitignore",
			filepath.Join(workspaceDir, probe))
	}
	return nil
}
