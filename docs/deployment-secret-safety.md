# Deployment secret safety

`codefly deploy secrets` reads the ExternalSecrets recorded by the render and
seeds the referenced store. The remote key and property identify a storage
location. The rendered `secretKey` identifies the value that belongs there.

## Output and errors

The public plan and CLI output contain locations and configuration keys. They
never include stored payloads.

Backend stdout and stderr are withheld on failure, including debug formatting of
backend errors. Backend diagnostics can contain payloads or HTTP response bodies;
forwarding them is unsafe. The error names the command that failed and the
planner adds the affected remote key. JSON decoding failures likewise omit the
backend content. GCP values travel on stdin and Azure values in the SDK's HTTPS
request body, never in process arguments. Rendering stores references, not secret
payloads.

## Azure Key Vault

The existing `azurekv` SecretStore or ClusterSecretStore selects the vault.
Publication supports Azure public-cloud WorkloadIdentity stores. The tenant comes
from the store's `tenantId` or its referenced service account's
`azure.workload.identity/tenant-id` annotation, exactly one source. A namespaced
store cannot reference a service account in another namespace.

The Microsoft Key Vault SDK defaults to the operator's Azure CLI login, pinned to
that tenant. An authorized in-cluster publisher can explicitly select
`CODEFLY_AZURE_CREDENTIAL=workload`; the SDK then uses its projected
`AZURE_FEDERATED_TOKEN_FILE`, `AZURE_CLIENT_ID` and `AZURE_TENANT_ID`. The tenant must
match the store's declaration. Neither mode falls back to another credential, and
neither borrows the service account ESO uses to read secrets. The publisher must
already have its own write grant; this option creates no identity, grant or Job.
A mismatched vault tenant is refused by the SDK's authentication
challenge validation. Metadata-only planning lists active/deleted secret metadata
and versions; it never fetches a secret value. Disabled, expired, not-yet-valid,
managed and recoverable deleted secrets are refused, not treated as absent.
SDK errors omit response bodies and authentication diagnostics.

## Retained credentials and concurrent writers

Existing keys with no readable version fail planning. First creation of an
`ed25519-pkcs8` signing key requires `--mint CODEFLY__…` naming its exact stored
configuration key; `--yes` alone is insufficient. An existing document missing
that signing property must have its original material restored. `--mint` does
not permit reminting it. Renaming an entire remote key still needs human review:
the current render cannot identify material filed under an old, unreferenced key.

Apply checks all read sources against the plan before writing and checks each
destination again immediately before writing. Changed metadata or payloads abort
with the keys already written returned to the caller. Rebuild the plan after a
failure; existing values are retained on the retry.

These are optimistic checks, not an atomic compare-and-set or an all-stores
transaction. GCP version creation and Azure Set Secret do not offer a conditional
version-creation operation here. Serialize writers to an environment; another
writer acting between the final check and write can still race. Runtime ESO
projection and grants require separate live qualification.

## Existing environments

Older rendered ExternalSecrets already contain the `secretKey` and `remoteRef`
needed for resolution. There is no render-inventory migration.

Valid stored values remain unchanged. A seed operation does not rotate them or
prove that a manually seeded value authenticates to its intended service.
Configuration aliases must agree across every referenced property, including
properties in one document. Aliases use one generated value when none exists,
or reuse the consistent value already held anywhere in that alias group.

An empty required property is an error. Missing values with no declared source
are listed as required. Apply validates those requirements before confirmation
or no-op success. `--dry-run` can report an incomplete plan without writing;
`--allow-missing` can write its remaining valid properties. Neither option
permits contradictory sources.

These checks validate the render and store relationship. They do not verify live
service authentication, cluster health, durable storage, or historical disclosure.
