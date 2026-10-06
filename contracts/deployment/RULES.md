# Stable refusal rule IDs

Rule IDs are API; diagnostic messages and paths are not. Each library refusal
has a negative case in the shared data corpus. Unknown features fail closed.

| Rule ID | Owning implementation |
|---|---|
| `AGGREGATE_COMPLETE` | [validate.go](validate.go) |
| `AGGREGATE_PREVIOUS` | [validate.go](validate.go) |
| `ANNOTATION` | [validate.go](validate.go) |
| `ARTIFACT` | [references.go](references.go) |
| `ARTIFACT_OBJECT_KIND` | [references.go](references.go) |
| `ARTIFACT_SOURCE` | [references.go](references.go) |
| `AUTHENTICATING_CONTAINER` | [validate.go](validate.go) |
| `BLOB_DIGEST` | [context.go](context.go) |
| `CARRIER_TEMPLATE` | [references.go](references.go) |
| `CATALOG_ENTRY` | [context.go](context.go) |
| `CIDR_UNSUPPORTED` | [references.go](references.go) |
| `COMPILER_PROFILE` | [context.go](context.go) |
| `CONFIGURATION_CLOSURE` | [references.go](references.go) |
| `CONFIGURATION_REF` | [references.go](references.go) |
| `CONFIGURATION_USE` | [references.go](references.go) |
| `CONTAINER_REQUIRED` | [validate.go](validate.go) |
| `CONTAINER_SECURITY` | [validate.go](validate.go) |
| `CONTAINER_UNIQUE` | [validate.go](validate.go) |
| `CONTAINER_UNSUPPORTED` | [validate.go](validate.go) |
| `CONTEXT_DOCUMENT` | [context.go](context.go) |
| `CONTEXT_VERSION` | [context.go](context.go) |
| `CONTROLLER_KIND` | [validate.go](validate.go) |
| `CONTROLLER_PROFILE` | [references.go](references.go) |
| `CREDENTIAL_KIND` | [validate.go](validate.go) |
| `CREDENTIAL_PROFILE` | [references.go](references.go) |
| `EGRESS` | [references.go](references.go) |
| `ENDPOINT` | [validate.go](validate.go) |
| `EXECUTION_PROFILE` | [context.go](context.go) |
| `IDENTIFIER` | [validate.go](validate.go) |
| `IDENTITY_GRANT` | [context.go](context.go) |
| `IDENTITY_VALUE` | [references.go](references.go) |
| `IMAGE_CONFIGURATION` | [images.go](images.go) |
| `IMAGE_CONFIG_LINK` | [images.go](images.go) |
| `IMAGE_EVIDENCE` | [images.go](images.go) |
| `IMAGE_INDEX` | [images.go](images.go) |
| `IMAGE_MANIFEST` | [images.go](images.go) |
| `IMAGE_PLATFORM` | [images.go](images.go) |
| `IMAGE_REFERENCE` | [validate.go](validate.go) |
| `INGRESS_UNSUPPORTED` | [references.go](references.go) |
| `JSON_DUPLICATE` | [json.go](json.go) |
| `JSON_LIMIT` | [json.go](json.go) |
| `JSON_NUMBER` | [json.go](json.go) |
| `JSON_SYNTAX` | [json.go](json.go) |
| `JSON_UNICODE` | [json.go](json.go) |
| `LABEL` | [validate.go](validate.go) |
| `MEMBER` | [references.go](references.go) |
| `NAMESPACE` | [references.go](references.go) |
| `OBJECT_IDENTITY` | [references.go](references.go) |
| `OBJECT_UNIQUE` | [references.go](references.go) |
| `POD_PLATFORM` | [validate.go](validate.go) |
| `POD_UNSUPPORTED` | [validate.go](validate.go) |
| `PRINCIPAL_UNIQUE` | [references.go](references.go) |
| `PROFILE_DIGEST` | [context.go](context.go) |
| `PROFILE_MISSING` | [context.go](context.go) |
| `PROFILE_REFERENCE` | [context.go](context.go) |
| `PROFILE_VERSION` | [context.go](context.go) |
| `REFERENCE_RESOLUTION` | [context.go](context.go) |
| `RELEASE_METADATA` | [json.go](json.go) |
| `REMOVAL` | [references.go](references.go) |
| `RESOURCE_UNSUPPORTED` | [references.go](references.go) |
| `RESTART_POLICY` | [validate.go](validate.go) |
| `SCHEMA_REQUIRED` | [json.go](json.go) |
| `SCHEMA_TYPE` | [json.go](json.go) |
| `SCHEMA_UNKNOWN` | [json.go](json.go) |
| `SCHEMA_VERSION` | [validate.go](validate.go) |
| `SELECTOR_EMPTY` | [validate.go](validate.go) |
| `SELECTOR_OVERLAP` | [validate.go](validate.go) |
| `SELECTOR_TEMPLATE` | [validate.go](validate.go) |
| `SERVICE_ACCOUNT` | [validate.go](validate.go) |
| `TRUST_PROFILE` | [context.go](context.go) |
| `WORKLOAD_ID` | [validate.go](validate.go) |
| `WORKLOAD_ORIGIN` | [validate.go](validate.go) |

The executable additionally reports `REQUEST_SHAPE` for malformed envelopes,
`INPUT_IO` for input I/O failure, and `INTERNAL` for unexpected internal errors.
They never return canonical bytes, a digest, or projection rows on refusal.
