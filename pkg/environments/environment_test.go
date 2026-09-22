package environments_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
)

// writeServiceScopedWorkspace lays down a modules-layout workspace with a single
// module "saas" owning service "accounts", plus an environment carrying
// envDeclarations. It is the real graph ValidateEnvironments cross-checks
// against.
func writeServiceScopedWorkspace(t *testing.T, envDeclarations string) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		resources.WorkspaceConfigurationName: `name: platform
layout: modules
modules:
  - name: saas
environments:
  - name: prod
    namespace: platform
` + envDeclarations,
		filepath.Join("modules", "saas", resources.ModuleConfigurationName): `kind: module
name: saas
services:
    - name: accounts
`,
		filepath.Join("modules", "saas", "services", "accounts", resources.ServiceConfigurationName): `kind: service
name: accounts
version: 0.0.0
agent:
  kind: runtime::service
  name: go-grpc
  version: 0.0.1
  publisher: codefly.ai
`,
	}
	for rel, content := range files {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func serviceSecretsDeclaration(serviceName string) string {
	return `    service-secrets:
      secret-store:
        name: azure-keyvault-prod
        kind: ClusterSecretStore
      services:
        ` + serviceName + `:
          remote-keys:
            workos-client-secret: workos/prod/client-secret
`
}

func serviceConfigDeclaration(serviceName string) string {
	return `    service-config:
      services:
        ` + serviceName + `:
          values:
            DATABASE_HOST: accounts.database.example
`
}

func serviceIdentityDeclaration(serviceName string) string {
	return `    service-identity:
      default:
        principal: platform-prod-workload
      services:
        ` + serviceName + `:
          principal: accounts-prod
`
}

// A service-secrets, service-config or service-identity entry naming a real
// service passes the graph check.
func TestValidateEnvironmentsAllowsKnownService(t *testing.T) {
	for name, declaration := range map[string]string{
		"service-secrets":  serviceSecretsDeclaration("accounts"),
		"service-config":   serviceConfigDeclaration("accounts"),
		"service-identity": serviceIdentityDeclaration("accounts"),
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			ws, err := loadDeploymentWorkspace(ctx, writeServiceScopedWorkspace(t, declaration))
			if err != nil {
				t.Fatal(err)
			}
			if err := ws.ValidateEnvironments(ctx); err != nil {
				t.Fatalf("ValidateEnvironments = %v, want nil", err)
			}
		})
	}
}

// A typo'd service name (here "accunts") would silently drop the secret override,
// the resolved values, or the identity the workload authenticates as at
// projection time; the graph check must reject it.
func TestValidateEnvironmentsRejectsUnknownService(t *testing.T) {
	for name, declaration := range map[string]string{
		"service-secrets":  serviceSecretsDeclaration("accunts"),
		"service-config":   serviceConfigDeclaration("accunts"),
		"service-identity": serviceIdentityDeclaration("accunts"),
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			ws, err := loadDeploymentWorkspace(ctx, writeServiceScopedWorkspace(t, declaration))
			if err != nil {
				t.Fatal(err)
			}
			err = ws.ValidateEnvironments(ctx)
			if err == nil {
				t.Fatal("expected ValidateEnvironments to reject unknown service")
			}
			if !strings.Contains(err.Error(), "accunts") || !strings.Contains(err.Error(), name) {
				t.Fatalf("error = %v, want it to name the block and the unknown service", err)
			}
		})
	}
}

// An empty list item under environments unmarshals to a nil *Environment;
// postLoad must reject it with an error instead of dereferencing it into a panic.
func TestLoadWorkspaceRejectsEmptyEnvironmentEntry(t *testing.T) {
	root := t.TempDir()
	workspace := `name: platform
layout: flat
environments:
  - name: prod
    namespace: platform
  -
`
	if err := os.WriteFile(filepath.Join(root, resources.WorkspaceConfigurationName), []byte(workspace), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadDeploymentWorkspace(context.Background(), root)
	if err == nil {
		t.Fatal("expected an error for an empty environment entry")
	}
	if !strings.Contains(err.Error(), "empty environment") {
		t.Fatalf("error = %v, want it to name the empty environment entry", err)
	}
}

func TestEnvironmentGitopsTopologyLoadsFromWorkspace(t *testing.T) {
	root := t.TempDir()
	workspace := `name: platform
layout: modules
environments:
  - name: aws
    namespace: platform
    cluster:
      kind: eks
    registry:
      url: registry.example.com/platform
      auth: ecr
    gitops:
      repo-url: https://github.com/acme/platform.git
      path: clusters/aws
      branch: production
      revision: release-v1
      checkout: .
      inventory: clusters/aws/deployments/modules/users/.codefly-render.json
    ingress:
      - name: edge
        service: gateway
        endpoint: https
        hosts:
          - api.example.com
    managed-services:
      store:
        kind: rds-postgresql
        external-name: store.internal.example.com
        egress-cidrs:
          - 10.12.0.0/16
        secret-references:
          - name: store-secrets
            remote-key: platform/users/store
            secret-store:
              name: aws-secrets-manager
              kind: ClusterSecretStore
`
	if err := os.WriteFile(filepath.Join(root, resources.WorkspaceConfigurationName), []byte(workspace), 0o644); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadDeploymentWorkspace(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Environments) != 1 {
		t.Fatalf("environments = %d, want 1", len(loaded.Environments))
	}
	environment := loaded.Environments[0]
	if environment.Gitops == nil || environment.Gitops.Revision != "release-v1" {
		t.Fatalf("gitops = %+v", environment.Gitops)
	}
	if len(environment.Ingress) != 1 || environment.Ingress[0].Hosts[0] != "api.example.com" {
		t.Fatalf("ingress = %+v", environment.Ingress)
	}
	store, ok := environment.ManagedServices["store"]
	if !ok || store.Kind != "rds-postgresql" || len(store.SecretReferences) != 1 {
		t.Fatalf("managed store = %+v, present = %t", store, ok)
	}
}

func TestEnvironmentServiceSecretsLoadFromWorkspace(t *testing.T) {
	root := t.TempDir()
	workspace := `name: platform
layout: modules
environments:
  - name: prod
    namespace: platform
    cluster:
      kind: aks
    service-secrets:
      secret-store:
        name: azure-keyvault-prod
        kind: ClusterSecretStore
      services:
        accounts:
          remote-keys:
            workos-client-secret: workos/prod/client-secret
        billing:
          secret-store:
            name: aws-secrets-billing
            kind: SecretStore
          remote-keys:
            stripe-key: stripe/prod/key
`
	if err := os.WriteFile(filepath.Join(root, resources.WorkspaceConfigurationName), []byte(workspace), 0o644); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadDeploymentWorkspace(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Environments) != 1 {
		t.Fatalf("environments = %d, want 1", len(loaded.Environments))
	}
	secrets := loaded.Environments[0].ServiceSecrets
	if secrets == nil {
		t.Fatal("service-secrets did not load")
	}
	if secrets.SecretStore.Name != "azure-keyvault-prod" || secrets.SecretStore.Kind != "ClusterSecretStore" {
		t.Fatalf("service secret store = %+v", secrets.SecretStore)
	}
	if got := secrets.Services["accounts"].RemoteKeys["workos-client-secret"]; got.Key != "workos/prod/client-secret" || got.Property != "" {
		t.Fatalf("accounts remote key override = %+v", got)
	}
	// accounts inherits the environment-wide store.
	if secrets.Services["accounts"].SecretStore != nil {
		t.Fatalf("accounts secret store override = %+v, want nil", secrets.Services["accounts"].SecretStore)
	}
	// billing overrides the store so it can resolve from a different backend.
	billing := secrets.Services["billing"].SecretStore
	if billing == nil {
		t.Fatal("billing secret store override did not load")
	}
	if billing.Name != "aws-secrets-billing" || billing.Kind != "SecretStore" {
		t.Fatalf("billing secret store override = %+v", billing)
	}
}

// A workspace with no service-secrets block must load with a nil ServiceSecrets,
// which is the "no service secret projection is rendered" default — validation
// must not treat absence as an error.
func TestEnvironmentServiceSecretsAbsentIsNil(t *testing.T) {
	root := t.TempDir()
	workspace := `name: platform
layout: modules
environments:
  - name: prod
    namespace: platform
    cluster:
      kind: aks
`
	if err := os.WriteFile(filepath.Join(root, resources.WorkspaceConfigurationName), []byte(workspace), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadDeploymentWorkspace(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Environments[0].ServiceSecrets != nil {
		t.Fatalf("service-secrets = %+v, want nil", loaded.Environments[0].ServiceSecrets)
	}
}

// A declared service-secrets block whose store name/kind is missing (here a
// mistyped `secret-stores:` key that YAML silently drops) must fail at load
// rather than reach the cluster as an ExternalSecret pointing at store "".
func TestEnvironmentServiceSecretsRejectsEmptyStore(t *testing.T) {
	root := t.TempDir()
	workspace := `name: platform
layout: modules
environments:
  - name: prod
    namespace: platform
    service-secrets:
      secret-stores:
        name: azure-keyvault-prod
        kind: ClusterSecretStore
      services:
        accounts:
          remote-keys:
            workos-client-secret: workos/prod/client-secret
`
	if err := os.WriteFile(filepath.Join(root, resources.WorkspaceConfigurationName), []byte(workspace), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadDeploymentWorkspace(context.Background(), root)
	if err == nil {
		t.Fatal("expected load to fail for empty service-secrets store")
	}
	if !strings.Contains(err.Error(), "secret-store") {
		t.Fatalf("error = %v, want it to mention secret-store", err)
	}
}

// A remote-key mapping to an empty path resolves to no remote at all; it must
// fail at load instead of projecting a key with a blank remoteRef.
func TestEnvironmentServiceSecretsRejectsEmptyRemoteKeyPath(t *testing.T) {
	root := t.TempDir()
	workspace := `name: platform
layout: modules
environments:
  - name: prod
    namespace: platform
    service-secrets:
      secret-store:
        name: azure-keyvault-prod
        kind: ClusterSecretStore
      services:
        accounts:
          remote-keys:
            workos-client-secret: ""
`
	if err := os.WriteFile(filepath.Join(root, resources.WorkspaceConfigurationName), []byte(workspace), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadDeploymentWorkspace(context.Background(), root)
	if err == nil {
		t.Fatal("expected load to fail for empty remote-key path")
	}
	if !strings.Contains(err.Error(), "workos-client-secret") {
		t.Fatalf("error = %v, want it to mention the offending key", err)
	}
}

// A remote-key may be declared either as a bare scalar (which is the remote key
// with no property) or as an explicit {key, property} mapping; both must decode
// through EnvironmentSecretRemoteRef.UnmarshalYAML.
func TestEnvironmentSecretRemoteRefDecodesScalarAndMapping(t *testing.T) {
	root := t.TempDir()
	workspace := `name: platform
layout: modules
environments:
  - name: prod
    namespace: platform
    service-secrets:
      secret-store:
        name: azure-keyvault-prod
        kind: ClusterSecretStore
      services:
        accounts:
          remote-keys:
            scalar-key: lodestar-accounts
            mapping-key:
              key: lodestar-identity
              property: client_secret
`
	if err := os.WriteFile(filepath.Join(root, resources.WorkspaceConfigurationName), []byte(workspace), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadDeploymentWorkspace(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	keys := loaded.Environments[0].ServiceSecrets.Services["accounts"].RemoteKeys
	if scalar := keys["scalar-key"]; scalar.Key != "lodestar-accounts" || scalar.Property != "" {
		t.Fatalf("scalar form = %+v, want {Key: lodestar-accounts}", scalar)
	}
	if mapping := keys["mapping-key"]; mapping.Key != "lodestar-identity" || mapping.Property != "client_secret" {
		t.Fatalf("mapping form = %+v, want {Key: lodestar-identity, Property: client_secret}", mapping)
	}
}

// Marshalling mirrors decoding: a property-less remote ref serializes back to the
// terse scalar form (so a round-trip that rewrites the workspace, like
// `environment import`, does not expand it), while one with a property serializes
// as the {key, property} mapping.
func TestEnvironmentSecretRemoteRefMarshalsScalarAndMapping(t *testing.T) {
	scalar, err := yaml.Marshal(environments.EnvironmentSecretRemoteRef{Key: "lodestar-accounts"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(scalar)); got != "lodestar-accounts" {
		t.Fatalf("scalar marshal = %q, want %q", got, "lodestar-accounts")
	}
	mapping, err := yaml.Marshal(environments.EnvironmentSecretRemoteRef{Key: "lodestar-identity", Property: "client_secret"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(mapping); !strings.Contains(got, "key: lodestar-identity") || !strings.Contains(got, "property: client_secret") {
		t.Fatalf("mapping marshal = %q, want key+property", got)
	}
}

// A defaults block carrying "{key}" (and "{service}") placeholders is a template,
// not a literal path; it must load and validate rather than being rejected as an
// empty or malformed remote reference.
func TestEnvironmentServiceSecretsDefaultsTemplateValidates(t *testing.T) {
	root := t.TempDir()
	workspace := `name: platform
layout: modules
environments:
  - name: prod
    namespace: platform
    service-secrets:
      secret-store:
        name: azure-keyvault-prod
        kind: ClusterSecretStore
      services:
        accounts:
          defaults:
            key: lodestar-{service}
            property: "{key}"
`
	if err := os.WriteFile(filepath.Join(root, resources.WorkspaceConfigurationName), []byte(workspace), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadDeploymentWorkspace(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defaults := loaded.Environments[0].ServiceSecrets.Services["accounts"].Defaults
	if defaults == nil {
		t.Fatal("defaults did not load")
	}
	if defaults.Key != "lodestar-{service}" || defaults.Property != "{key}" {
		t.Fatalf("defaults = %+v", defaults)
	}
}

// A defaults template carrying an unrecognized placeholder (a typo such as
// "{sevice}") must fail at load. Unsubstituted, it would render literally into
// the ExternalSecret's remoteRef and pass every downstream render check, only
// breaking in-cluster when the store lookup misses — so it has to be caught here.
func TestEnvironmentServiceSecretsRejectsUnknownDefaultsPlaceholder(t *testing.T) {
	root := t.TempDir()
	workspace := `name: platform
layout: modules
environments:
  - name: prod
    namespace: platform
    service-secrets:
      secret-store:
        name: azure-keyvault-prod
        kind: ClusterSecretStore
      services:
        accounts:
          defaults:
            key: lodestar-{sevice}
            property: "{key}"
`
	if err := os.WriteFile(filepath.Join(root, resources.WorkspaceConfigurationName), []byte(workspace), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadDeploymentWorkspace(context.Background(), root)
	if err == nil {
		t.Fatal("expected load to fail for unknown defaults placeholder")
	}
	if !strings.Contains(err.Error(), "{sevice}") {
		t.Fatalf("error = %v, want it to name the offending placeholder", err)
	}
}

func TestEnvironmentServiceConfigLoadsFromWorkspace(t *testing.T) {
	root := t.TempDir()
	workspace := `name: platform
layout: modules
environments:
  - name: prod
    namespace: platform
    service-config:
      services:
        accounts:
          values:
            DATABASE_HOST: accounts.database.example
            DATABASE_PORT: "6432"
        billing:
          values:
            DATABASE_HOST: billing.database.example
`
	if err := os.WriteFile(filepath.Join(root, resources.WorkspaceConfigurationName), []byte(workspace), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadDeploymentWorkspace(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	config := loaded.Environments[0].ServiceConfig
	if config == nil {
		t.Fatal("service-config did not load")
	}
	accounts := config.Services["accounts"].Values
	if accounts["DATABASE_HOST"] != "accounts.database.example" || accounts["DATABASE_PORT"] != "6432" {
		t.Fatalf("accounts values = %+v", accounts)
	}
	if got := config.Services["billing"].Values["DATABASE_HOST"]; got != "billing.database.example" {
		t.Fatalf("billing DATABASE_HOST = %q", got)
	}
}

// Absence is the valid "this environment hands over no resolved values" state.
func TestEnvironmentServiceConfigAbsentIsNil(t *testing.T) {
	root := t.TempDir()
	workspace := `name: platform
layout: modules
environments:
  - name: prod
    namespace: platform
`
	if err := os.WriteFile(filepath.Join(root, resources.WorkspaceConfigurationName), []byte(workspace), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadDeploymentWorkspace(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Environments[0].ServiceConfig != nil {
		t.Fatalf("service-config = %+v, want nil", loaded.Environments[0].ServiceConfig)
	}
}

// Workspace YAML drops unknown keys, so a mistyped `values:` (here `value:`)
// leaves a service declared with nothing to inject, and an unresolved value
// leaves the workload dialing "". Both must fail at load rather than start a
// service missing exactly what it was handed over.
func TestEnvironmentServiceConfigRejectsNothingToInject(t *testing.T) {
	for name, block := range map[string]string{
		"dropped values key": `    service-config:
      services:
        accounts:
          value:
            DATABASE_HOST: accounts.database.example
`,
		"unresolved value": `    service-config:
      services:
        accounts:
          values:
            DATABASE_HOST: ""
`,
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			workspace := `name: platform
layout: modules
environments:
  - name: prod
    namespace: platform
` + block
			if err := os.WriteFile(filepath.Join(root, resources.WorkspaceConfigurationName), []byte(workspace), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := loadDeploymentWorkspace(context.Background(), root)
			if err == nil {
				t.Fatal("expected load to fail")
			}
			if !strings.Contains(err.Error(), "accounts") && !strings.Contains(err.Error(), "field value not found") {
				t.Fatalf("error = %v, want it to name the offending service", err)
			}
		})
	}
}

// An environment declares what its services authenticate as without declaring
// any managed service for them to consume, and resolution keys by consuming
// service: the entry for a service that names one, the environment-wide default
// for every service that does not.
func TestEnvironmentServiceIdentityLoadsFromWorkspace(t *testing.T) {
	root := t.TempDir()
	workspace := `name: platform
layout: modules
environments:
  - name: prod
    namespace: platform
    service-identity:
      default:
        kind: azure-workload-identity
        principal: platform-prod
        annotations:
          azure.workload.identity/client-id: 00000000-0000-0000-0000-000000000000
        labels:
          azure.workload.identity/use: "true"
      services:
        billing:
          principal: billing-prod
`
	if err := os.WriteFile(filepath.Join(root, resources.WorkspaceConfigurationName), []byte(workspace), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadDeploymentWorkspace(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	env := loaded.Environments[0]
	if env.ManagedServices != nil {
		t.Fatalf("managed-services = %+v, want nil", env.ManagedServices)
	}
	accounts := env.WorkloadIdentity("accounts")
	if accounts == nil || accounts.Principal != "platform-prod" || accounts.Kind != "azure-workload-identity" {
		t.Fatalf("accounts identity = %+v, want the environment default", accounts)
	}
	if accounts.Annotations["azure.workload.identity/client-id"] != "00000000-0000-0000-0000-000000000000" ||
		accounts.Labels["azure.workload.identity/use"] != "true" {
		t.Fatalf("attachments were not carried verbatim: %+v", accounts)
	}
	billing := env.WorkloadIdentity("billing")
	if billing == nil || billing.Principal != "billing-prod" {
		t.Fatalf("billing identity = %+v, want its own entry", billing)
	}
}

// Absence is the valid "this environment projects no identity" state.
func TestEnvironmentServiceIdentityAbsentIsNil(t *testing.T) {
	root := t.TempDir()
	workspace := `name: platform
layout: modules
environments:
  - name: prod
    namespace: platform
`
	if err := os.WriteFile(filepath.Join(root, resources.WorkspaceConfigurationName), []byte(workspace), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadDeploymentWorkspace(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Environments[0].ServiceIdentity != nil {
		t.Fatalf("service-identity = %+v, want nil", loaded.Environments[0].ServiceIdentity)
	}
	if got := loaded.Environments[0].WorkloadIdentity("accounts"); got != nil {
		t.Fatalf("identity = %+v, want none", got)
	}
}

// Workspace YAML drops unknown keys, so a mistyped `default:` leaves a block
// that declares an identity and attaches none; an entry bound to no principal is
// the same failure one level down. Both would otherwise leave the pods on the
// namespace default where token minting has no identity, and the first signal is
// a secret lookup failing in-cluster.
func TestEnvironmentServiceIdentityRejectsIncompleteDeclaration(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block string
		want  string
	}{
		{"dropped default key", `    service-identity:
      defualt:
        principal: platform-prod
`, `unknown service-identity field "defualt"`},
		{"no principal", `    service-identity:
      services:
        accounts:
          kind: azure-workload-identity
`, "service-identity"},
		// A valid default standing beside a mistyped `services` is the one case
		// the emptiness check cannot catch: the block loads, and every service
		// that meant to override silently authenticates as the default.
		{"dropped services key", `    service-identity:
      default:
        principal: platform-prod
      servces:
        accounts:
          principal: accounts-prod
`, `unknown service-identity field "servces"`},
		{"dropped annotations key", `    service-identity:
      default:
        principal: platform-prod
        annotatons:
          azure.workload.identity/client-id: "00000000-0000-0000-0000-000000000000"
`, `unknown workload identity field "annotatons"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			workspace := `name: platform
layout: modules
environments:
  - name: prod
    namespace: platform
` + tc.block
			if err := os.WriteFile(filepath.Join(root, resources.WorkspaceConfigurationName), []byte(workspace), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := loadDeploymentWorkspace(context.Background(), root)
			if err == nil {
				t.Fatal("expected load to fail")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// Workspace load refuses the same collision the coordinate contract does, and accepts
// the same key under two different services, which is not a collision at all.
func TestEnvironmentRefusesKeyDeclaredAsBothValueAndSecret(t *testing.T) {
	workspaceWith := func(configService string) string {
		return `name: platform
layout: modules
environments:
  - name: prod
    namespace: platform
    service-config:
      services:
        ` + configService + `:
          values:
            DATABASE_HOST: accounts.database.example
    service-secrets:
      secret-store:
        name: azure-keyvault-prod
        kind: ClusterSecretStore
      services:
        accounts:
          remote-keys:
            DATABASE_HOST: accounts/prod/host
`
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, resources.WorkspaceConfigurationName), []byte(workspaceWith("accounts")), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadDeploymentWorkspace(context.Background(), root)
	if err == nil {
		t.Fatal("expected load to refuse a key declared as both a value and a secret")
	}
	if !strings.Contains(err.Error(), "DATABASE_HOST") || !strings.Contains(err.Error(), "accounts") {
		t.Fatalf("error = %v, want it to name the service and the key", err)
	}

	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, resources.WorkspaceConfigurationName), []byte(workspaceWith("billing")), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDeploymentWorkspace(context.Background(), other); err != nil {
		t.Fatalf("same key under two services = %v, want nil", err)
	}
}

// A managed secret reference may name a property inside a structured remote
// document, alongside its remote key.
func TestEnvironmentManagedSecretReferenceDecodesProperty(t *testing.T) {
	root := t.TempDir()
	workspace := `name: platform
layout: modules
environments:
  - name: prod
    namespace: platform
    managed-services:
      accounts:
        kind: external
        external-name: accounts.prod.svc
        secret-references:
          - name: store-connection
            remote-key: lodestar-accounts
            property: store_read_write_connection
            secret-store:
              name: azure-keyvault-prod
              kind: ClusterSecretStore
`
	if err := os.WriteFile(filepath.Join(root, resources.WorkspaceConfigurationName), []byte(workspace), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadDeploymentWorkspace(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	refs := loaded.Environments[0].ManagedServices["accounts"].SecretReferences
	if len(refs) != 1 {
		t.Fatalf("secret references = %d, want 1", len(refs))
	}
	if refs[0].RemoteKey != "lodestar-accounts" || refs[0].Property != "store_read_write_connection" {
		t.Fatalf("managed reference = %+v", refs[0])
	}
}

//
//func TestEnvironment(t *testing.T) {
//	ctx := context.Background()
//	Dir := t.TempDir()
//
//	defer func() {
//		os.RemoveAll(Dir)
//	}()
//
//	var action actions.Action
//	var err error
//
//	action, err = action.NewActionNew(ctx, &actionsv0.New{
//		Name: "test-",
//		Path: Dir,
//	})
//	out, err := action.Run(ctx)
//require.NoError(t, err)
//	 := shared.Must(actions.As[resources.](out))
//
//	action, err = actionenviroment.NewActionAddEnvironment(ctx, &actionsv0.AddEnvironment{
//		Name:        "test-environment",
//		Path: .Dir(),
//	})
//require.NoError(t, err)
//	_, err = action.Run(ctx)
//require.NoError(t, err)
//
//	// Make sure the environment is created
//	content, err := os.ReadFile(path.Join(.Dir(), resources.ConfigurationName))
//require.NoError(t, err)
//	require.Contains(t, string(content), "name: test-environment")
//}

func TestEnvironmentAppHost(t *testing.T) {
	service := &resources.ServiceIdentity{Module: "users", Name: "accounts"}

	suffixed := &environments.Environment{DNS: &environments.EnvironmentDNS{AppHostSuffix: "staging.eastus2.azure.example.com"}}
	if got := suffixed.AppHost(service); got != "accounts-users.staging.eastus2.azure.example.com" {
		t.Errorf("AppHost = %q", got)
	}

	// No suffix declared: no derived host (falls back to a local dns.codefly.yaml).
	if got := (&environments.Environment{}).AppHost(service); got != "" {
		t.Errorf("AppHost without suffix = %q, want empty", got)
	}
	if got := (&environments.Environment{DNS: &environments.EnvironmentDNS{}}).AppHost(service); got != "" {
		t.Errorf("AppHost with empty suffix = %q, want empty", got)
	}
}

type deploymentWorkspace struct {
	*resources.Workspace
	Environments []*environments.Environment
}

func (w *deploymentWorkspace) ValidateEnvironments(ctx context.Context) error {
	return environments.ValidateWorkspace(ctx, w.Workspace)
}
func loadDeploymentWorkspace(ctx context.Context, dir string) (*deploymentWorkspace, error) {
	w, err := resources.LoadWorkspaceFromDir(ctx, dir)
	if err != nil {
		return nil, err
	}
	envs, err := environments.FromWorkspace(w)
	if err != nil {
		return nil, err
	}
	return &deploymentWorkspace{Workspace: w, Environments: envs}, nil
}
