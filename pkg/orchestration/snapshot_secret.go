package orchestration

import (
	"context"

	"github.com/codefly-dev/core/configurations"
)

// snapshotSecretPlaceholder is the inert value a snapshot render substitutes for
// every secret reference. promotableConfiguration drops secret values before they
// reach a manifest, so this string never lands in the rendered tree; it exists
// only so resolution succeeds without contacting a provider.
const snapshotSecretPlaceholder = "codefly-gitops-snapshot-placeholder" //nolint:gosec // G101: inert placeholder that promotableConfiguration discards, never a real credential

// snapshotSecretResolver keeps a GitOps snapshot render value-free. A promotable
// render retains only secret KEYS — promotableConfiguration replaces each secret
// value with a secretKeyRef and the ExternalSecret's remoteRef is derived from
// the environment's declared store mapping — so the secret VALUE is always
// discarded. Resolving references through their real provider would nonetheless
// demand provider authentication (or a local plaintext *.secret.env value file)
// for values that never reach a manifest. This resolver short-circuits that: it
// answers every reference with an inert placeholder, so a render derives its refs
// from the committed reference-only declarations alone and never from local
// values. OnePasswordScheme is the only reference scheme core recognizes, so
// covering it covers every reference resolution that would otherwise fail.
type snapshotSecretResolver struct{}

func (snapshotSecretResolver) Scheme() string { return configurations.OnePasswordScheme }

func (snapshotSecretResolver) Resolve(context.Context, *configurations.SecretReference) (string, error) {
	return snapshotSecretPlaceholder, nil
}
