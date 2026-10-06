# H1 security decisions required before implementation

Status: blocked; no deployment-contract implementation or executable has been
introduced. These questions follow the H1 brief's instruction to stop on
security-relevant ambiguity rather than guess. The module location is settled:
`github.com/codefly-dev/cli/contracts/deployment`, with its own `go.mod` and no
dependency on the parent CLI module or Core.

## Q1 — independently authorized validation context

**Question:** What is the H1 input contract for the independently authorized
platform catalog and compiler profile, and which component establishes their
authorization before H1 uses them?

SPEC section 2 requires all of the following:

- External references resolve through an independently authorized platform catalog.
- The compiler profile enumerates supported Pod-producing kinds.
- Unsupported PodSpec fields, configuration mechanisms and enforcement
  restrictions are refused.
- Kubernetes defaulting and normalization are pinned to the compiler profile's
  API version, with an explicit list of excluded fields.

The inventory's `platform_refs` identifies documents by ID, revision and digest;
it does not contain those documents or establish their authorization. The spec
does not define the catalog/profile input schema, initial supported Kubernetes
version, normalization rules or executable handoff. Defining the platform
profiles is assigned to later item I1.

This affects which execution settings validation accepts and which bytes enter
the signed digest. A profile submitted by the delivery cannot authorize itself.
An arbitrary built-in allowlist would also choose execution policy that the
spec assigns to the platform.

**Proposed resolution, not implemented:** H1 defines a separate explicit
validation-context input carrying exact referenced profile/catalog content.
The trusted caller authenticates that context independently of the inventory;
H1 checks reference digests and refuses unsupported profile versions or missing
entries. The executable is an offline validator, with successful validation
explicitly distinct from platform authorization. Please establish this boundary
and the initial profile's API version, supported fields/kinds and normalization
rules before the accepted wire contract and positive fixtures are frozen.

## Q2 — evidence for image-manifest kind and platform

**Question:** How does H1 obtain and verify evidence that each approved image
digest identifies a platform-specific image manifest, including its platform
and image configuration, rather than an image index?

SPEC section 2 validation rules 2 and 8 require immutable image-manifest digests
and refuse indexes in the initial implementation. Section 5 also requires
resolving image defaults from the pinned image configuration. A reference of
the form `registry.example/team/image@sha256:<64 hex>` cannot distinguish an
image manifest from an index or prove the image's platform/defaults. The
existing Go image readers inspected for precedent validate reference/digest
syntax; they do not supply this evidence.

Accepting a delivery-authored `manifest` designation would not enforce the
required refusal. Fetching registry content instead introduces a resolver,
credentials and an execution environment that the H1 executable interface does
not yet specify.

**Proposed resolution, not implemented:** Require retained image-manifest and
image-configuration bytes as content-addressed validation inputs; recompute their
digests, refuse index media types, validate manifest-to-configuration links and
check the image platform against the independently authorized execution profile.
The trusted caller owns fetching; the shared executable remains offline. Please
confirm that evidence contract and whether its definition belongs in H1.

## Work remaining after these decisions

All five H1 implementation deliverables remain: inventory types/schema, exact
canonicalization/digest, semantic validator with stable rule IDs, generated
data-based conformance corpus, and the executable Python calls. The module
README, import-boundary check and required Go build/vet/format/test checks also
remain. No build or conformance result is claimed by this document.
