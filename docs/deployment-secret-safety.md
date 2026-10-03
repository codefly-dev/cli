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
backend content. Values sent to the store travel on stdin, never in process
arguments. Rendering stores references, not secret payloads.

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
