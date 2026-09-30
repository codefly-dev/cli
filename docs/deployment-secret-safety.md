# Deployment secret safety

`codefly deploy secrets` reads the ExternalSecrets recorded by the render and
seeds the referenced store. The remote key and property identify a storage
location. The rendered `secretKey`, together with its reading service for
federation credentials, identifies the value that belongs there.

## Output and errors

The public plan and CLI output contain locations, configuration keys and declared
credential identities. They never include identities parsed from stored payloads.
If rewriting a value would remove an identity that the workspace no longer
derives, the plan refuses and says stored identities are withheld.

Backend stdout and stderr are withheld on failure, including debug formatting of
backend errors. Backend diagnostics can contain payloads or HTTP response bodies;
forwarding them is unsafe. The error names the command that failed and the
planner adds the affected remote key. JSON decoding failures likewise omit the
backend content. Values sent to the store travel on stdin, never in process
arguments. Rendering stores references, not secret payloads.

Before this correction, a malformed encoded value could place a payload fragment
in the public plan's source description. The CLI printed that description before
apply, including in dry runs. Terminal recordings, redirected stdout and CI logs
could therefore retain material. Rendering alone did not emit the stored payload
on this path. Code review cannot determine whether an earlier execution persisted
material: that requires execution and output-retention history from the affected
environment. Inspecting or sharing those records must not reproduce payloads.

## Existing environments

Older rendered ExternalSecrets already contain the service, `secretKey` and
`remoteRef` needed for resolution. There is no render-inventory migration. The
reader reconstructs each service/key/property association from the existing,
inventory-verified files.

Valid stored credentials remain unchanged. A seed operation does not rotate them
or prove that a manually seeded credential authenticates to its intended service.
Configuration aliases must agree across every referenced property, including
properties in one document. Aliases use one generated value when none exists,
or reuse the consistent value already held anywhere in that alias group.
Federation identities remain scoped to their actual reading services.

A nonempty federation value with an invalid encoding is refused before minting
or writing. Duplicate identities, missing entry fields and malformed digests
are errors. Empty required properties are also errors. Previously, an invalid
mapped encoding could be interpreted as absence and replaced during an applied
seed operation. Whether that happened in an existing environment requires its
store-version and execution history; compatibility with an old render does not
answer that question.

Missing values with no declared source are listed as required. Apply validates
those requirements before confirmation or no-op success. `--dry-run` can report an
incomplete plan without writing; `--allow-missing` can write its remaining valid
properties. Neither option permits malformed values or contradictory sources.

These checks validate the render and store relationship. They do not verify live
service authentication, cluster health, durable storage, or historical disclosure.
