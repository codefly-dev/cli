# Execution inventory contract

This independently versioned module owns `codefly/execution-inventory/v1`, the
configuration schema produced by the CLI after final rendering and consumed by
the protected infrastructure compiler and deployment registry. It has its own
`go.mod`, no parent CLI/Core imports, no third-party dependencies, and no network
I/O. Python calls the same executable; it does not implement a second validator.

**Successful validation is offline consistency, never platform authorization.**
The trusted caller must independently authenticate the validation context,
authorize target/ownership/first creation/generation against current state, fetch
retained content, verify signatures, and approve the resulting digest. An
inventory cannot contain or supply its own profile documents. This module does
not verify signatures, contact Kubernetes/registries, assemble contributions,
grant credentials, attest running images, or establish installation. Those are
subsequent consumers of the contract, not alternate paths through validation.

The existing Configuration carrier is unchanged:

```text
origin = "_workspace"
infos[name="execution-inventory"].data.kind = "codefly/execution-inventory/v1"
infos[name="execution-inventory"].data.content = <canonical JSON bytes>
infos[name="execution-inventory"].data.secret = false
```

Deliver this group only to scoped deployment consumers, not every application.

## Image identity and authority

A **registry + repository + SHA-256 image-manifest digest is a complete, primary
image identity**. No publisher, package name, version, dev flag, skip flag, or
unsigned branch is required or offered. The digest is what admission compares;
a development build needs no release ceremony to be approvable. `members[].release`
is optional descriptive metadata. When present, all three fields (publisher,
name, semantic version) must be valid. It cannot replace an image digest.

Local and hosted targets follow the same inventory/digest/signature/validation
path. Their independently authorized approver identities and profiles can differ.
H1 validation is the consistency step within that path, not a substitute for its
signatures or current authorization checks.

Every image requires exact retained image-manifest **and image-configuration**
bytes in the separate context. SHA-256, manifest media type, configuration link,
configuration size, rootfs layer identities/count, OS/architecture/variant and
node selection are checked. OCI and Docker schema-2 manifests are supported;
both index/list types are refused even when their digest syntax is valid. Layers
are digest-bound by the manifest; this module does not fetch/unpack their bytes.
Requested references remain complete strings (a different repository changes the
inventory digest). Runtime mirror qualification and live observation are owned
by the observing consumer; matching a bare runtime digest is not an H1 verdict.

## Frozen wire schema and executable schema rules

`types.go`, `inventory.schema.json`, and `context.schema.json` describe the closed
wire types. All fields and collections are required and explicit, including empty
maps/lists and nullable fields; only `members[].release` may be absent. Unknown
fields at any depth, duplicate JSON keys (including escaped aliases), invalid
Unicode, floating-point numbers, out-of-range integers and trailing data refuse.

The schema uses the required `codeflyRules` vocabulary in `schema-meta.json`.
Its implementation is `CheckSchema` (intrinsic constraints, including selector
intersection) plus `Validate` (independent context and evidence). Arbitrary
selector intersection cannot be expressed by standard JSON Schema keywords. An
engine lacking the required vocabulary **must refuse this schema**; running a
structural-only JSON Schema checker is not contract validation. The checked-in
schema is generated from the actual wire types and drift-tested. `CheckSchema`
cannot produce a `Validated` result or executable projection.

Each workload explicitly declares its authenticating application container, or
null with `credential_kind=none`. The image map never selects it. `Validated.Rows()`
derives exactly these seven fields from the authoritative template:

| Key | Source |
|---|---|
| `ns` | controller namespace |
| `labels` | nonempty equality selector |
| `sa` | explicit template ServiceAccount |
| `container` | explicit authenticating-container designation, including null |
| `images` | all application and init names → complete image references |
| `app` | application names in template order |
| `init` | init names in execution order |

Container names are unique across both roles. Every other container is
non-authenticating. Same-namespace equality selectors overlap unless a shared key
has different values. Every overlapping pair is refused, including carrier rows.
The executable never picks the first selector or searches by image to break ties.
Credential-bearing workloads also require distinct service-account principals,
matching independent identity grants and appropriately scoped catalogue entries.

## Kubernetes 1.34 profile

The accepted compiler profile is `codefly/compiler-profile/v1`, pinned to
**Kubernetes API 1.34** and normalization `explicit-v1`. Supported Pod-producing
kinds are Deployment, StatefulSet, DaemonSet, Job and CronJob. Bare Pods and all
other kinds refuse. The context may enable a subset, never extend this list.

Normalization is the identity operation over the supported, explicit desired Pod
template. No Kubernetes default is silently inserted or discarded. Every
supported defaultable field must be supplied, including image pull/restart
policies, token automount, DNS policy, scheduler, service links, termination
settings, env source optionality and file modes. Arrays retain order. Empty
command/args retain Kubernetes image-default semantics, bound through the image
manifest/configuration digest. Missing values refuse rather than choosing a
version-dependent default. A new defaulting algorithm requires a new profile.

**The excluded-field list is exactly `[]`.** Controller-generated metadata and
runtime-assigned fields are not accepted in a desired template. For example,
`metadata.uid` and `spec.nodeName` refuse; nothing is stripped before hashing.
Live Pod comparison/normalization is a downstream enforcement task, not a
projection that may erase fields from this inventory. Fixtures freeze this
profile; no live Kubernetes defaulting or CEL qualification is claimed here.

The closed initial PodSpec supports explicit Linux amd64/arm64 selection,
application/init containers (including restartable init sidecars), command/args,
working directory, named ports, literal/config-map/secret env values and read-only
config-map/secret mounts with enumerated keys. Service-account token automount is
disabled. Security contexts require non-root users, no added capabilities,
`drop: [ALL]`, RuntimeDefault seccomp, no privilege escalation, and a read-only
root filesystem. Grace is bounded at 30 seconds. Unsupported fields (including
host namespaces, ephemeral containers, projected tokens, probes/hooks, resource
settings and arbitrary storage mechanisms) refuse, never disappear from output.
Future supported subsets require explicit schema/profile revisions.

Dependencies resolve named endpoints in the aggregate or independently scoped
catalogue. Egress supports exact DNS host/protocol/port/consumer sets; CIDRs refuse
until an enforcement profile is qualified. Nonempty ingress and resource-binding
requests also refuse in this initial profile. Their wire fields are explicit so
unsupported restrictions cannot silently survive a partial compiler.

## Context and retained content

Context is a **separate input**, `codefly/validation-context/v1`:

* `documents`: exactly the execution, identity, trust and compiler profile documents
  named by `platform_refs`, each with ID, revision and base64 exact content bytes.
* `blobs`: SHA-256 digest → base64 exact image-manifest/configuration bytes.

Reference digests are over **retained bytes**, not reserialized documents. Every
blob key is recomputed. Profile schemas are versioned and closed; missing,
duplicate, extra, mismatched or unsupported documents refuse. Execution profile
sources/catalogue, identity grants and verifier references are independent
caller-authenticated facts; the module cannot establish their provenance. Trust
verifier references are bound metadata for the signature-verifying consumer,
not evidence that this executable checked a signature.

Artifact paths are relative, normalized and confined to a context-approved source
ID; traversal, encoded paths and absolute paths refuse. The caller owns fetching
and source confinement (including symlink handling). Artifact metadata is not a
claim that H1 parsed or verified the rendered Kubernetes artifact bytes. Member
artifact/workload ownership is closed; missing, duplicated and cross-binding
references refuse. Carrier templates must match the independent catalogue's
canonical-template digest, keeping the later document payload out of the
inventory's own digest graph.

## Exact canonical bytes and digest

`codefly-json-v1` is a deliberately limited encoding, **not RFC 8785**:

1. Input is one UTF-8 JSON value, no BOM. Maximum 16 MiB and depth 64.
2. Strings must contain valid Unicode scalar values. Escaped surrogate pairs
   decode normally; lone surrogates and invalid UTF-8 refuse. No Unicode
   normalization is performed.
3. Numbers are decimal integers from -9007199254740991 to 9007199254740991.
   Fractions, exponents and `-0` refuse. The JSON grammar rejects leading zeroes.
4. Object keys sort by UTF-8 byte order. Arrays preserve their input order,
   including all set-like collections, app/init lists, args and env. Producers
   wanting stable ordering of set-like collections must choose it before signing.
5. No whitespace or trailing newline is emitted. Strings use JSON escapes for
   quotation mark, backslash, control characters (`\b\t\n\f\r` where applicable;
   other controls as lowercase `\u00xx`). `<`, `>`, `&`, U+2028 and U+2029 use
   lowercase `\u` escapes. All other Unicode is emitted as UTF-8; `/` is literal.
6. Inventory digest = `sha256:` plus lowercase SHA-256 of those exact bytes.

Every inventory field, including target, previous digest, ownership, generation,
release metadata, execution settings, array order and platform reference digests,
is covered. The separate context is bound by those profile and image digests,
not appended to the canonical inventory. Even unchanged image bytes under another
requested repository spelling change the inventory digest. There is no omitted
runtime field or unsigned dev field. `Validated` retains private bytes and returns
copies, so mutations after validation cannot change a digest or compiled row.

## Go and executable/Python entrypoints

From this directory (use the module's own Go version):

```sh
GOWORK=off GOPROXY=off go build -o bin/deployment-contract ./cmd/deployment-contract
bin/deployment-contract validate < request.json
bin/deployment-contract schema
bin/deployment-contract context-schema
```

`validate` takes exactly `{"inventory": <object>, "context": <object>}` on stdin.
The request limit is 16 MiB including base64. On success stdout is one JSON object
with `valid:true`, the exact canonical JSON string, digest, seven-key rows and
`violation:null`. On refusal it has `valid:false`, null canonical/digest/rows and
`violation:{rule,path,message}`. Exit 0 means consistent, 1 refused, 2 invocation
or I/O failure. Rule IDs are stable API; paths/messages are diagnostics. There is
no file path resolution, environment-profile selection, network fetch or skip
option in this protocol.

```go
result, err := deployment.Validate(inventoryBytes, independentlyTrustedContextBytes)
// After handling err, obtain copies:
canonical := result.Canonical()
digest := result.Digest()
rows := result.Rows()
```

```python
from deployment_contract import validate  # python/deployment_contract.py
result = validate(
    "/absolute/path/to/pinned/deployment-contract",
    inventory_bytes,
    independently_trusted_context_bytes,
)
canonical_bytes = result["canonical"]
```

The Python adapter transports raw input so Go still rejects duplicate keys and
malformed Unicode. Consumers must pin/authenticate the executable release and
context outside this protocol. Release this module with the Go nested-module tag
prefix `contracts/deployment/v…`; publish the executable built from the same
revision. This working tree does not claim a published release.

## Conformance and boundaries

`testdata/base.json` and `base-context.json` are the only base documents.
`mutations.json` is reviewed DATA describing one named mutation per case (an
atomic graph mutation can update several linked references). `generate.py`
materializes the shared input/context/canonical files and `manifest.json`, whose
rows name input, outcome, stable rule ID, canonical-byte file and expected digest.
Expected verdicts are not learned from the implementation. The canonical oracle
is independent Python code used only for test-data generation, never validation.

```sh
python3 testdata/generate.py
GOWORK=off GOPROXY=off go build ./...
GOWORK=off GOPROXY=off go vet ./...
gofmt -l .
GOWORK=off GOPROXY=off go test ./...
```

`go test` runs every case through the Go API **and** the actual built executable
via the Python adapter (A27), checks byte-exact regeneration/schema drift, tests
unknown-field closure and mutation after validation (H1's A13 boundary), and
checks all production source imports and their transitive dependency graph.
Network transports, subprocesses, plugins, cgo, native objects and linkname
escapes fail the boundary test, including inactive build-tagged source files.
The independent CI workflow runs with workspace/module-network resolution off.

H1 does not qualify later acceptance-test paths: live CEL/admission, signatures,
current authorization transactions, Kubernetes observation, withdrawal, retirement,
credential revocation, fleet integration and timing remain unverified here.

## Reading the projection

`Check(inventory)` accepts an inventory's intrinsic form — schema version,
required vocabulary, structural rules — and returns a `*Checked` carrying the
canonical bytes, their digest, and `Rows()`: the seven-key projection
(`ns`, `labels`, `sa`, `container`, `images`, `app`, `init`) that the cluster's
admission policy compares.

`Rows()` needs **no validation context**, deliberately. The projection is a
pure function of `Workloads`, and a consumer holding an inventory that was
already approved — but no longer holding the context that approval was checked
against — must still be able to ask for its rows. If it cannot, it writes its
own traversal of `Workloads[].Template.Spec`, which is a second implementation
of the projection. The first consumer of this module hit exactly that.

`Validate(inventory, context)` is `Check` plus the external reference and image
evidence checks, and returns a `*Validated` that embeds the same `*Checked`, so
the two forms cannot disagree about canonical bytes, digest or rows. A test
pins that agreement.

Neither is an authorization decision. `Validate` is an offline consistency
check; what may execute is approved elsewhere, by an authority that is not this
module and not its caller's inventory.
