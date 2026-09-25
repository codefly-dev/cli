package solutionrun

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/codefly-dev/core/resources"
)

// --- Deployed federation secrets ---
//
// A render names every federation secret a deployed composition needs as a
// reference into the environment's secret store (DerivedDeployInputs), and says
// the operator writes each secret and its digest into the store once. This is
// what that write is derived from, so it is never typed: which stored key holds
// which credential, and how a key that aggregates several credentials — a
// consuming backend's `prefix:secret` map, a registrar's `prefix:sha256hex`
// digests — is encoded from them. The rules are the run's
// (DerivedRunInputs): the secret a solution registers with is keyed by its
// module name, a consumed module federates under its facade prefix, and a
// registrar module holds digests and is never handed a plaintext.
//
// Credentials are identified, never valued, here: a caller that already holds a
// credential in the store must keep it, so minting is the caller's decision.

// CredentialKind is what principal a federation credential authenticates.
type CredentialKind string

const (
	// SolutionRegistration is the secret a solution registers its gateway
	// upstream and frontend remote with, identified by the solution's id.
	SolutionRegistration CredentialKind = "solution-registration"
	// ModuleRegistration is the secret a consuming backend registers a federated
	// prefix with, identified by that prefix.
	ModuleRegistration CredentialKind = "module-registration"
	// ModuleIdentity is the secret a consumed module presents to mint its own
	// service-principal work context, identified by the prefix it federates under.
	ModuleIdentity CredentialKind = "module-identity"
)

// Credential names one federation secret. Two stored keys that name the same
// Credential must hold the same value — that is what makes the plaintext one end
// presents and the digest the other end checks agree.
type Credential struct {
	Kind     CredentialKind
	Identity string
}

func (credential Credential) String() string {
	return string(credential.Kind) + "/" + credential.Identity
}

// SecretDerivation is how one stored key's value follows from credentials: a
// single credential verbatim, or `identity:value` entries joined on "," where
// the value is the credential (a consuming backend's map) or its sha256 hex
// digest (a registrar's declaration).
type SecretDerivation struct {
	Credentials []Credential
	// Encoded selects the `identity:value,…` form; otherwise the one credential
	// is the value.
	Encoded bool
	// Digest replaces each credential by its sha256 hex digest.
	Digest bool
}

// Value derives the stored value from the credentials' plaintexts. A missing
// credential is an error, never an empty entry: a registrar refuses a malformed
// declaration at startup.
func (derivation SecretDerivation) Value(plaintexts map[Credential]string) (string, error) {
	entries := make([]string, 0, len(derivation.Credentials))
	for _, credential := range derivation.Credentials {
		plaintext, ok := plaintexts[credential]
		if !ok || plaintext == "" {
			return "", fmt.Errorf("no value for %s", credential)
		}
		value := plaintext
		if derivation.Digest {
			value = moduleSecretDigest(plaintext)
		}
		if !derivation.Encoded {
			return value, nil
		}
		entries = append(entries, credential.Identity+":"+value)
	}
	return strings.Join(entries, ","), nil
}

// Plaintexts recovers the credentials a stored plaintext value carries, so a
// credential already held somewhere in the store is reused rather than minted
// again. A digest carries no plaintext, so it yields nothing; an entry naming an
// identity this derivation does not declare is ignored rather than trusted.
func (derivation SecretDerivation) Plaintexts(stored string) map[Credential]string {
	if derivation.Digest || stored == "" {
		return nil
	}
	if !derivation.Encoded {
		if len(derivation.Credentials) != 1 {
			return nil
		}
		return map[Credential]string{derivation.Credentials[0]: stored}
	}
	found := map[Credential]string{}
	for _, entry := range strings.Split(stored, ",") {
		identity, value, ok := strings.Cut(strings.TrimSpace(entry), ":")
		if !ok || value == "" {
			continue
		}
		for _, credential := range derivation.Credentials {
			if credential.Identity == identity {
				found[credential] = value
			}
		}
	}
	return found
}

// MintCredential returns a fresh federation credential with the same entropy
// and encoding a run mints, so a deployed credential satisfies every parser a
// run's does.
func MintCredential() string {
	return mintModuleSecret()
}

// DeployedSecrets is the federation's half of what a deployed environment's
// secret store must hold: for each service unique, the stored keys it
// references and how each is derived.
type DeployedSecrets struct {
	Services map[string]map[string]SecretDerivation
	Notes    []Note
}

// For returns the derivation of key for the service with the given unique.
func (secrets DeployedSecrets) For(unique, key string) (SecretDerivation, bool) {
	derivation, ok := secrets.Services[unique][key]
	return derivation, ok
}

// Credentials lists every credential some stored key derives from, once each.
func (secrets DeployedSecrets) Credentials() []Credential {
	var credentials []Credential
	for _, unique := range sortedKeys(secrets.Services) {
		for _, key := range sortedKeys(secrets.Services[unique]) {
			for _, credential := range secrets.Services[unique][key].Credentials {
				if !slices.Contains(credentials, credential) {
					credentials = append(credentials, credential)
				}
			}
		}
	}
	return credentials
}

func (secrets *DeployedSecrets) set(unique, key string, derivation SecretDerivation) {
	if secrets.Services[unique] == nil {
		secrets.Services[unique] = map[string]SecretDerivation{}
	}
	secrets.Services[unique][key] = derivation
}

// solutionRegistrationConfigurationGroup is the workspace configuration group a
// solution's entry service declares to receive the secret it registers with, and
// solutionRegistrationSecretKey the key inside it: the declared carrier the
// solution runtimes read (see solutionRegistrationSecretEnvironmentVariable).
const (
	solutionRegistrationConfigurationGroup = "solution-registration"
	// #nosec G101 -- a configuration key name, not a credential
	solutionRegistrationSecretKey = "SECRET"
)

// workspaceSecretKey is the stored key of a secret workspace configuration
// value, as core names it in a service's environment.
func workspaceSecretKey(group, key string) string {
	return resources.WorkspaceSecretConfigurationPrefix + "__" + resources.NameToKey(group) + "__" + resources.NameToKey(key)
}

// DeployedFederationSecrets derives, for every solution the workspace composes,
// which stored key of which service holds which federation credential. It walks
// the same graph DerivedDeployInputs renders references from, so every key it
// names is one a render references:
//
//   - a solution's entry service holds its consumed prefixes' registration
//     secrets as a `prefix:secret` map, and — when it declares the
//     solution-registration group — the secret it registers under its own id;
//   - every service of a consumed module holds that module's identity secret,
//     under the canonical carrier and the deprecated alias alike;
//   - every registrar service holds the digests of all of them, in the
//     federation group.
//
// A workspace with no registrar derives nothing, as a render references
// nothing: no credential could be admitted.
func DeployedFederationSecrets(ctx context.Context, workspace *resources.Workspace) (DeployedSecrets, error) {
	secrets := DeployedSecrets{Services: map[string]map[string]SecretDerivation{}}
	registrars := federationRegistrars(ctx, workspace)
	if len(registrars) == 0 {
		return secrets, nil
	}
	holdsDigests := registrarModules(registrars)

	var registrations, identities, solutions []Credential
	boundPrefix := map[string]string{}
	for _, ref := range workspace.Modules {
		mod, err := workspace.LoadModuleFromReference(ctx, ref)
		if err != nil || mod.ServiceEntry == "" {
			continue
		}
		solutionManifest, err := moduleManifest(mod)
		if err != nil {
			return DeployedSecrets{}, fmt.Errorf("solution %s: %w", mod.Name, err)
		}
		if solutionManifest == nil {
			continue
		}
		entry := resources.ServiceUnique(mod.Name, mod.ServiceEntry)

		if !slices.Contains(holdsDigests, mod.Name) && isRegistrationIdentity(mod.Name) {
			entryService, err := workspace.LoadService(ctx, &resources.ServiceWithModule{Name: mod.ServiceEntry, Module: mod.Name})
			if err == nil && slices.Contains(entryService.WorkspaceConfigurationDependencies, solutionRegistrationConfigurationGroup) {
				credential := Credential{Kind: SolutionRegistration, Identity: mod.Name}
				solutions = append(solutions, credential)
				secrets.set(entry, workspaceSecretKey(solutionRegistrationConfigurationGroup, solutionRegistrationSecretKey),
					SecretDerivation{Credentials: []Credential{credential}})
			}
		}

		consumed, _ := federatedConsumedAPIs(solutionManifest.ConsumedAPIs(), holdsDigests)
		var prefixes []Credential
		for i := range consumed {
			prefix := consumed[i].As
			credential := Credential{Kind: ModuleRegistration, Identity: prefix}
			if prefix == "" || slices.Contains(prefixes, credential) {
				continue
			}
			prefixes = append(prefixes, credential)
			if !slices.Contains(registrations, credential) {
				registrations = append(registrations, credential)
			}
		}
		if len(prefixes) == 0 {
			continue
		}
		secrets.set(entry, moduleRegistrationSecretsEnvironmentVariable, SecretDerivation{Credentials: prefixes, Encoded: true})

		for _, bound := range consumedModuleBindings(consumed) {
			if slices.Contains(holdsDigests, bound.module) {
				continue
			}
			if earlier, seen := boundPrefix[bound.module]; seen {
				if earlier != bound.prefix {
					return DeployedSecrets{}, fmt.Errorf(
						"module %s is consumed under the prefixes %q and %q; a module federates under one prefix, which is its identity",
						bound.module, earlier, bound.prefix)
				}
				continue
			}
			boundPrefix[bound.module] = bound.prefix
			services, err := moduleServiceUniques(ctx, workspace, bound.module)
			if err != nil {
				secrets.Notes = append(secrets.Notes, Note{Warning: true, Message: fmt.Sprintf(
					"no %s derived for %s (%v)", moduleIdentitySecretEnvironmentVariable, bound.module, err)})
				continue
			}
			// The registrar holds an identity digest only for a module whose
			// services hold the plaintext: a digest nothing can present admits no
			// one, and minting its preimage would leave a credential no service
			// ever receives.
			credential := Credential{Kind: ModuleIdentity, Identity: bound.prefix}
			if len(services) > 0 && !slices.Contains(identities, credential) {
				identities = append(identities, credential)
			}
			identity := SecretDerivation{Credentials: []Credential{credential}}
			for _, unique := range services {
				secrets.set(unique, moduleIdentitySecretEnvironmentVariable, identity)
				secrets.set(unique, moduleRegistrationSecretEnvironmentVariable, identity)
			}
		}
	}

	for _, registrar := range registrars {
		if len(registrations) > 0 {
			secrets.set(registrar, workspaceSecretKey(federationConfigurationGroup, moduleRegistrationSecretsKey),
				SecretDerivation{Credentials: registrations, Encoded: true, Digest: true})
			secrets.set(registrar, workspaceSecretKey(federationConfigurationGroup, moduleIdentitySecretsKey),
				SecretDerivation{Credentials: identities, Encoded: true, Digest: true})
		}
		if len(solutions) > 0 {
			secrets.set(registrar, workspaceSecretKey(federationConfigurationGroup, solutionRegistrationSecretsKey),
				SecretDerivation{Credentials: solutions, Encoded: true, Digest: true})
		}
	}
	return secrets, nil
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
