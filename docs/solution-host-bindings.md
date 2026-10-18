# Delivery documents: presence, authority, and how they reach the host

A solution or a module is **present** on a host because delivery declared it,
and holds **authority** there because a reviewed, signed document granted it to
one exact build — never because its process announced itself, and never for a
build other than the one approved. `codefly` renders both documents from the
composition, settles and signs them at publish, and delivers them through the
same Argo-managed tree the workloads travel in, plus one Job per document type
that POSTs them to the host's delivery API.

The documents, their invariants and their conformance fixtures live in
[codefly-dev/core `solutionhost`](https://github.com/codefly-dev/core): the
presence document (`codefly/solution-host-binding/v2`), the authority document
(`codefly/solution-authority/v1`) and the signed `{document, envelope}` carrier. This page is about what the CLI puts in
them and how they get there.

## The static half, and what the render never reads

The lifecycle has a static, deploy-time half and a dynamic, runtime half, and
nothing straddles them. The render produces the static half:

| record | writer | what it carries |
|---|---|---|
| presence document | the render; settled and signed at publish | kind, binding ID, generation, ownership domain, envelope revision, host, release digest, rendered artifacts and their digests, the workloads with the build each runs and the identity each presents |
| authority document | derived from the module's published contract, or from a composed solution's scoped `api.consumes`; signed at publish | the principal and the units of authority it holds, approved for one build, effective from one presence generation |

The dynamic half is the host's: the **envelope** (the ceiling a platform
administrator writes), an organisation's installation, team exposure. The
render never reads the envelope. A module contract is a *request*; the render
derives the authority document from it; the host checks it against the envelope
at apply, and refuses a request wider than the ceiling with the binding named.
That refusal happens at the host, far from the render that produced the
document — by design, because the envelope is the one dynamic record a static
delivery is validated against.

## Declaring the host

The environment declares the host it delivers to. Every field is required,
together, because every document needs all of them at once:

```yaml
environments:
  - name: production
    namespace: platform
    host:
      coordinate: example/production/region-a   # matched by the host; a mismatch is refused
      component: platform-host                  # the host component instance
      domain: example                           # the ownership domain this composition delivers under
      audience: accounts                        # the bare token audience the host verifies
      module_audience: module-capabilities      # the audience the host stamps on a module's own capability
      trust_domain: cluster.example             # the SPIFFE trust domain of the cell's mesh, which every workload's principal is under
      envelope_revision: 3                      # the envelope revision this composition was reviewed against
      delivery: platform/accounts/rest          # the host's delivery API, by composition identity
      release:
        key_id: delivery-v3                     # the platform Vault Transit key version
```

`release.key_id` is the host's reviewed release-key declaration. The publisher
holds the custody key to it before signing, and checks every fresh or reused
carrier against that key and `host.domain`. The host independently pins the
same public key. A hosted release publish needs this declaration; a local
publish uses the developer declaration instead.

A **declared local k3d** environment — `cluster.kind: k3d` and a
`cluster.context` of `k3d-<name>`, both written, and only such an environment:
any other declaring it is refused at load — may also list the developers its
local host admits. (The legacy reading that takes an undeclared environment
NAMED `local` for k3d never applies here, nor to self-promotion or the local
trust anchor: omitting a field must narrow what an environment may do, never
widen it. That reading survives only where nothing is admitted on it — the
unsigned `publish --local` qualification guard and the observe path, and the
fetch remote, which requires a `k3d-` context of its own.)

```yaml
    host:
      # … the fields above …
      developers:                              # the LOCAL host's allowlist
        - key_id: dev-0123456789abcdef
```

`developers` lists machine keys admitted by a local host. Run
`codefly delivery key` to create or load this machine's key and print its ID
and public PEM. A developer ID is `dev-` followed by 16 lowercase hexadecimal
characters derived from its public key. Release IDs are single-line values
without spaces. Unknown declaration fields are refused.

A k3d environment also states `trust_anchor`: the path the host reads its
trust anchor (the public verification policy) from, which is a constant
of the host. A local cluster run mounts the local trust anchor it provisions
there, the way a cell's platform mounts its own; stating another path moves
nothing on the host, which then has no anchor and refuses to start. Refused on
any environment that is not k3d: a deployed host's trust anchor is its
platform's, and the render mounts the platform's `<host.component>-delivery-trust`
ConfigMap at the host's constant path itself — the mount is the render's,
the content the platform's. See "Local runs" below, and "On a hosted cell"
there.

Nothing here is derived. A derived coordinate is a guess a host silently
refuses at reconcile time; an unlisted ownership domain is refused by the host
with the domain named; a wrong envelope revision is refused at apply with both
revisions named — and earlier, at publish, which holds every authority pair
against the revision this block declares *then*, so a render made before the
block was re-reviewed never leaves the publisher. A document **asserts** its own `domain`, so the host also
holds a policy saying which **signer identity** may make that assertion
(core's `Host.DomainsBySigner`, keyed by the key ID the host's envelope
verification names, such as `delivery-v3`).
That policy is the platform's, provisioned beside the identity allowlist and
never by a delivered document; the composition carries its domain (here, in
every document it renders) and nothing about who may
sign for it. A document signed by an identity the host does not let speak for
its domain is refused with the signer and the domain named. `trust_domain` is the mesh's — `cluster.local` for a mesh
that derives a workload's identity from (trust domain, namespace,
ServiceAccount) — so a document naming any other value names an identity the
mesh never presents, and the host refuses the connection.

An environment that composes a module **declares the host it runs on**, or
the render is refused with the instances and the block's fields named.
Rendering the instances' workloads with no declaration delivered them present
on no host, with a warning the operator could miss; a composition with nothing
to declare is one with no solution instance, not one that forgot its host.

`delivery` names the host's delivery API the way
`api.consumes` names a producing endpoint — by module, service and endpoint —
and the render resolves it to the in-cluster address the delivery Jobs POST to
(`<scheme>://<service>.<namespace>.svc.cluster.local:<port>`, the port being
the one the service declares under `spec.deployment.endpoint-ports`).

An environment that declares **no** `host` renders no document, and the render
says which modules it could not declare.

Two environments that deliver to one repository, branch and path replace each
other's module trees whole, so a publish of one would turn the other's
delivered generations and tombstones into absence — a replay would hold no
record of what was withdrawn. An environment that declares a host is refused,
at publish, while another environment of the workspace delivers to the same
path; each hosted environment needs a path of its own. What a path already
holds is kept whatever the configuration says now: a publish stages its own
environment's overlay beside every other environment's delivered documents
and removes nothing it did not author under its own environment — an
environment removed from the configuration keeps its delivered generations
and tombstones where they are. And a document delivered under this
environment's name to another host coordinate refuses the publish: a host
change is a new environment, never a publish over another host's record.

## Presence

One document per module instance, into
`solution-host-bindings/overlays/<environment>/<binding-id>.yaml` in the
module's owned tree, carried by a ConfigMap (the promotable ruleset refuses any
YAML in the tree that is not a manifest or a Kustomization). A module that
ships a solution manifest declares kind `solution` and claims its one route
alias — its instance name, as a single lowercase segment; any other module
declares kind `module` and claims no route.

**Binding ID**: `<workspace>.<environment>.<instance>`, derived from identity
alone and from nothing that changes between renders. Bounded to 63 characters
and to lowercase DNS labels, because it names a Kubernetes object and is stamped
as a label value.

**Three digests, three kinds, never compared to each other**:

| digest | pins | comparable to |
|---|---|---|
| `release.digest` | the module package the composition materialized (its content), or the artifact digest a packaged solution's executor reports | nothing at runtime; it records the release |
| `artifacts[].digest` | the rendered bytes of one unit in the delivery repository | nothing at runtime; it records what delivery wrote |
| `workloads[].image.digest` | the OCI image manifest the authenticating container must run | the pod's resolved `imageID`, by the host |

**Workloads**: for every Deployment, StatefulSet or DaemonSet a unit renders,
the document names the one container that authenticates — the container named
after the service, or the only container when there is one — its image
repository and digest, the identity it must present (the host's audience, the
principal the environment declares for the service (`service-identity.services`, keyed `<service>`, or `<module>/<service>` when two composed modules share the name — a bare key matching several is refused), and its **mesh principal**, the SPIFFE ID
`spiffe://<trust_domain>/ns/<namespace>/sa/<service account>` the cell's mesh issues to that account and presents on the workload's behalf — never a certificate the workload serves), and every
other container, init containers included, as one that must never be accepted
as the workload. A unit with several containers and none named after the
service is refused: "whichever one presented the token" is how a sidecar ends
up holding a workload's authority. Presence names what **mints** under the
service's principal — its serving workloads — and not a unit's bootstrap Job
or CronJob: those run their own images and never authenticate as the service,
so they are not here, where their image would be a build the host accepts a
token for; a managed service's bootstrap bundle is never a presence artifact
for the same reason.

Every serving workload a presence document declares **projects a
ServiceAccount token minted for the host's audience** into its authenticating
container, read-only, at `/var/run/secrets/codefly/token` — the path the SDK's
mint client reads — living **600 s** (`expirationSeconds: 600`, the longest
mint token a host admits, which bounds how long a copied token can be
replayed) — unless that container already provides exactly that: a projected
token for the audience whose file lands at that path, mounted without
`subPath` (a `subPath` mount is never refreshed, so the token would stop
rotating), living at most 600 s (unset is the kubelet's 3600 s), and hidden by
no other mount (one at the same directory, at or below the token, or between
the two). Every other source in the same projected volume must also write
explicit, disjoint paths: a duplicate token path, a path above or below it,
or a Secret/ConfigMap with unrestricted keys refuses reuse. A container whose mounts occupy that path otherwise is refused, with
the volume and the path; a token for the audience elsewhere does not stand in. The render writes it as a patch in the unit's environment overlay
(into the manifests of an overlay that has no kustomization), before presence
digests the unit, so the projection is part of the bytes the document pins.
It is how the pod proves who it is when it mints: the host reviews the token
and reads the pod's running image. No secret, prefix or registration value is
projected.

A serving workload names its own ServiceAccount: one running as the
namespace's `default` account — every pod's that names none — is refused,
because the identity a document names is the account's, and that one any pod
of the namespace can present.

The document cannot close the sidecar gap on its own: a projected
ServiceAccount token is the pod's, so a sidecar that mounts it presents it as
the workload and the host cannot tell which container asked. The platform's
admission policy is where it closes — a projected token minted for an explicit
`audience` must be mounted by the authenticating container and no other; a
declared volume nobody mounts is inert and admitted, on both sides — and
the render refuses the same pod template first, at publish, so the failure
lands on whoever wrote the template rather than on an operator reading a
denial at rollout. The rule keys on the token's `audience`, as admission does:
the token the ServiceAccount plugin injects (`kube-api-access-…`) is a
projected token too, mounted into every container, and names none, so a rule
keyed on "a projected token volume" would refuse every multi-container pod. A
render check is not the boundary — a hand-built manifest skips the renderer —
it is the same rule one step earlier.

Two approved builds of one ServiceAccount share one mesh principal: the mesh
derives the identity from the account, not the image. Binding identity to the
build is therefore not something the principal does; the host does it by
comparing the pod's resolved image to the approved build, and the platform by
refusing a pod whose image is not approved at admission. The identity a
document names is the account's. No workload serves or presents an
application-level certificate: inside the cell every hop is plain HTTP, and
the mesh (ztunnel mTLS) carries transport security and destination identity.

**A solution's generated `group` block.** The composing workspace organises
its solutions for presentation under `solution-groups` (core's
`resources.SolutionGroup`: `id`, `title`, `order`, `solutions`), and the render
and the native delivery project the group a solution instance is listed in, by
the name the composition carries it under (core's `Workspace.SolutionGroupOf`),
into the presence's top-level `group` block (`id`, `title`, `order`). It is
generated and authored nowhere else. A host groups its navigation and its
catalogue with it after each solution's own entitlements are judged; it grants
and installs nothing, and a solution's identity is unchanged by it. A headless
solution is grouped like any other, which is why the block is not part of
`frontend`; a module is grouped by nothing, whatever a workspace lists, and
core refuses the block on a module and on a tombstone. A change to the group's
title, order or membership alone changes the signed bytes, so it is a new
generation under the same binding; a withdrawal declares no group.

**A component's optional `frontend` block.** Modules and solutions both read
`solution.frontend.codefly.yaml` beside their entry service. Core's
`solutionhost.ParseFrontend` validates exactly the same fields and size bounds
as the signed presence document. A missing file (or no entry service) means a
headless component for either kind. An empty or malformed file refuses delivery
for both kinds.

A frontend's `payload` block is never authored (`ParseFrontend` refuses it in
the file). Both producers generate it from the release's own artifact
metadata, as core asks of a renderer: the runtime artifact named `frontend`
in the module's package manifest (`release-artifacts`) gives
`payload.artifact`, `payload.digest` (the SHA-256 of the complete bundle
bytes, as the release declares it) and `payload.media_type` (the explicit
canonical media type the artifact declares), verbatim. A release declaring no
such artifact gives the frontend no payload: a generation whose bundle is not
published, which the host decides whether to serve. A bundle declared under
another purpose, without a media type, or by a headless component is refused
by name (a name declared twice is refused by the manifest loader). In a cell,
the bundle bytes are the build owner's: the agent's build operation publishes
the artifact the manifest names through the build publisher (a digest-addressed
ORAS blob), and nothing here transports them; a host admits the bundle it
serves by this identity and by nothing a running process reports.

A native run delivers what runs. When a component's frontend build wrote its
bundle beside the entry service (`services/fe-remote/dist`, with
`mf-manifest.json` at its root; a dist without it is refused as no bundle), the
run archives that directory in core's canonical form (`CanonicalArchive`,
`application/x-tar`), retains the bytes in the workspace's artifact cache
(`.codefly/selection-artifacts/<digest>`, the same digest-named files an
acquisition retains and consults before any fetch) and stamps the presence's
payload with that digest, which outranks a release artifact the manifest names.
The host's native bundle source reads the cache read-only and verifies the
complete archive against the digest before serving it, so a mutable or corrupt
file fails there. A service that declares the render-owned workspace
configuration `artifact-cache` receives its one value, `ROOT`, the absolute
path of that cache, from the native run alone: a cell's render projects nothing
for it, so in Kubernetes the value is absent and a directory source stays
refused. A component that declares a page and built no bundle is refused by
name, with what to build (`services/fe-remote`, its own build workflow): a
host admits no new frontend without its signed payload, and no mutable
fallback is delivered in its place. A headless component needs no bundle.
Native and rendered delivery carry the same block for both kinds. A module page does not
change its shared tenancy or its per-organisation installation grants. The gateway
receives a route for a module page even when no solution consumes its API. The
page's entry service must declare exactly one HTTP-family endpoint; if that
module also has a consumed API, both must name the same upstream. Conflicting
page/API routes are refused at composition rather than delivered ambiguously.

The `delivery` ServiceAccount is the delivery Job's and nothing else's: a
serving workload naming it is refused at render, since a deployment caller
and a runtime caller under one
account is the collision the host's distinction between the two assumes
absent.

## Authority

A module that publishes `module.contract.codefly.yaml` (schema
`codefly/module-contract/v1`) and declares `module-identity: true` on **one**
service gets one authority document, into
`solution-authority/overlays/<environment>/`, delivered to the platform's
authority namespace (`platform-authority`). One service, because an authority
document approves one build and a host keeps one authority record per
binding: two services each claiming to be the module's identity would be two
builds under one principal, of which the host could activate at most one, so
the render refuses the pair and names both. A contract with **no** service
declaring `module-identity` is refused too, not passed over: the contract is
the module's request for authority, and with nothing to present it the
request would be dropped on the floor — the composition is inconsistent and
is told so, before any slot is resolved.

The authority overlay is applied under an **AppProject of its own**
(`<project>-authority`, written beside the module's `project.yaml`): one
destination, the authority namespace, and two kinds, ConfigMap and
`batch/Job`, with no cluster resources. The module's own project never names
the authority namespace — an AppProject destination applies to every
Application in the project, so adding it there would let any unit overlay
place a pod in the authority namespace running as the platform's `delivery`
account. The ApplicationSet stamps the project and the destination per
component, so the authority Application is the one Application of the module
that reaches that namespace, and a Deployment, CronJob or Secret in the
authority overlay is refused at apply. The overlay, the project and the
namespace name each other in the ApplicationSet, read from the `overlay`,
`project`, `namespace` and `component` fields the Application template
consumes, each required (an element naming none of them is another
generator's — the tenant matrix — and is not a component): a component stamped under
the authority project must point at the authority overlay and at that
namespace, and a component pointing at the authority overlay must be stamped
under that project — no unit overlay can borrow the project. What that
isolation does not cover is the Job's pod itself: the CLI's project admits a
`batch/Job` kind, and which image, account and mounts a Job in that namespace
may run with is the platform's admission (the authority Job runs as the
platform's `delivery` account, whose only reach is the delivery API, which
admits nothing unsigned) — the one boundary this render cannot enforce.

**A composed solution asks from its manifest, not from a contract.** A solution
declares what it consumes once, in `solution.codefly.yaml`'s `api.consumes`, which
also drives its generated client and its runtime's upstream map. Under schema
`codefly.solution-manifest/v1` an entry that carries `scopes` (`{resource_kind,
actions}` in the consumed module's permission vocabulary) and an `audience` slot
(`{from: <group>/<key>}`) asks for authority there. Core's `ConsumptionContract`
turns those entries into a **module contract** — one invoke binding per entry —
and the render resolves it through the same `Resolve` a published contract goes
through, so the audience is whatever the composition says that module verifies,
and the unit is `<presence binding>:<entry id>:invoke` with the document's own
`queues`, `namespaces` and `scope_ceilings` written `[]`. The principal is the name
the solution is composed under.

The manifest is read through core's `LoadConsumption`: the schema version and
`api.consumes`, strictly — an invalid or unknown field in a consumption fails the
render — and nothing else, so a section the strict loader refuses for reasons
unrelated to consumption never fails the render of a solution that asks for no
authority. On top of the module rules below (one `module-identity` service, a
presence, the envelope at the host), the render refuses a scoped entry for a
module the workspace does not compose, and one addressed to the solution itself
(by module, or by a resolved audience equal to its principal), because the host
could never install either. A module asks from **one** declaration: a contract and
scoped `api.consumes` together are refused by name — including a contract that
declares only destinations — until merging two declarations of one request is
decided. Entries without scopes ask for nothing.

The contract's **principal is the module's own name** — the contract says so
of itself, and it is held to that at render: the contract is written in the
module's repository, so a principal it names is self-asserted, and a module
claiming another's principal would claim that principal's bindings. And the
contract is carried **whole**: nothing it declares is dropped between the
contract and the signed document, because a host cannot enforce a declaration
it never receives, and a declaration that changed without the document
changing would be enforced as before.

Since core **v0.15.0** (core#735) the document carries every declaration but
one. The module's own `queues`, `namespaces` and `scope_ceilings` are on the
*document* — written out, and `[]` when the module declares none, so a renderer
that dropped one cannot deliver a document at all. A binding's `binding_key`
and `lookup.method` are on each of its *units* (`binding_key`,
`lookup_method`). A unit's queue and namespace stay the unit's and
single-valued: a module declaring exactly one of each puts it on every unit,
and one declaring none or several puts none on any — a narrower grant, never
every queue — while the document lists what the module owns. A carried scope
ceiling widens nothing the host grants: the host holds each one **whole**
against the owner-written envelope, so a ceiling the envelope does not hold is
refused there.

`destinations` are the one declaration the document does not restate. Their
service and endpoint are already the presence document's — the same fact signed
twice is how two readers come to disagree — and their kind is the vocabulary
of the host's envelope, which a document never carries. So the render holds
each destination to the endpoints the presence document lists for the
instance, and refuses one naming an endpoint the module does not render (or
another module's) by name, rather than delivering a route the host can never
take.

The contract declares the principal, the operation bindings the module redeems
(each with the operations it needs and a scope ceiling per operation), the
module's queues and namespaces, its own scope ceilings and the destinations it
exposes. Another module's vocabulary — a binding's audience, the resource kind
its scopes name — is never spelled in the module's repository: it is a **slot**,
`{from: <group>/<key>}`, resolved from the composition's workspace
configuration for the environment. A slot resolves public configuration only; a
slot pointing at a secret-classified value fails the render rather than inlining
it into a delivered document, and a bare string where a slot belongs is a schema
error. The render decides nothing about which record answers a slot: it hands
core every record of the group as the composition supplies it — both spellings
of a key, a key supplied twice, a key public in one place and secret in another
— and core's resolution rules, the same for every consumer, match the key in
either spelling, require the records of one key to agree on its value, read a
key any occurrence of which is secret as the secret, and hold a resolved value
to one non-empty line and a resolved resource kind to the grammar a literal
kind is held to. Core's resolution kit drives this provider
(`TestTheResolutionKitRunsThroughTheRendersProvider`), so a provider that
selected among the records would fail it.

A slot's **key name carries its meaning**, because no reader can check what the
value it resolves to means: a declared, supplied key holding a model profile
name resolves cleanly into a binding addressed to a profile, which the receiver
refuses far from the contract that caused it. The reader holds the convention
the modules publish against — `audience` from a `*-audience` key (or a
`*-prefix` one: a module's prefix is the audience a capability for it is
addressed to), `resource_kind` from a `*-resource-kind` key, `binding_key` from
a `*-binding` key — in either key spelling core accepts, and refuses a slot
whose key is named otherwise.

A binding's **scope ceiling** is written per operation in one of two spellings,
never mixed in one list. Bare actions (`invoke: [invoke, read]`) are qualified
by the binding's `resource_kind` slot, and a binding declaring no such slot
cannot write them: a scope names a resource kind (core's `WorkScopeV1` requires
one), so an action with no kind is a request nothing can mint. A binding whose
acts span several kinds spells each scope out instead:

```yaml
scope_ceiling:
    headless:
        - resource_kind: annotations.vocabularies
          actions: [write]
        - resource_kind: annotations.annotations
          actions: [redact]
```

naming the other module's permission namespace literally — a namespace is fixed
by the module that contributes it, exactly as its proto package is, while a
service the composition chooses stays a slot.

The derivation:

- **authority ID** `<binding-id>-authority`, granted over exactly that
  instance's presence binding (`binding: <binding-id>` in the document) and
  activating no other — without the target, an authority document activated
  any binding on the host and domain running the same image, a replacement
  that took a withdrawn alias included. The ID is named after the binding and
  not the service presenting it, because the host folds authority on the
  binding and holds a withdrawal over it terminal for every later authority
  ID: an ID carrying the service name would turn a module moving its identity
  to another service into a withdrawal plus a grant the host never activates.
  Named after the binding, that move is the next generation, approving the
  new service's build. The delivered ConfigMap is labelled
  `codefly.dev/binding: <binding-id>` — the one authority over a binding is
  selected by the binding, as the presence document is;
- **approved build** the image digest of the service's authenticating
  container in its serving workloads (Deployments, StatefulSets, DaemonSets —
  a bootstrap Job's image is declared in the presence document but does not
  mint under the principal), read off the same render — a unit whose serving
  workloads run two distinct builds cannot be approved by one document and is
  refused;
- **one unit of authority per (binding, operation)**, under the ID
  `<presence binding>:<binding>:<operation>` — scoped by the presence binding,
  so two instances of one module on one host hold distinct units and
  withdrawing one instance's leaves the other's alone — with the operation's
  ceiling as a sorted, comma-joined list of `<resource kind>:<action>` and the
  binding's revision from the contract (1 when omitted), which only ever
  increases **across the binding's whole history under the environment**: a
  decrease is refused at publish, as is a change to what the binding grants
  that keeps its revision — against the document delivered just before, and
  against a ledger beside the delivered authority documents
  (`solution-authority/overlays/<env>/bindings.ledger`) that keeps every
  binding ID's highest revision and the meaning it carried, so a binding
  removed from one generation and reintroduced later cannot come back below
  that revision, or at it with another meaning. The ledger is the
  publisher's own record under the repository's trust;
  nothing signs it and no Job reads it. What the binding grants is the whole
  unit — audience, scope, queue, namespace, binding key and lookup method —
  so a change to any of them is a new revision; the module's queue and
  namespace when it declares exactly one of each — none or several put none
  on any unit (a narrower grant, never every queue), while the document lists
  the module's own queues and namespaces whole;
- **effective from** the presence generation settled in the same publish.

The host's envelope must list the same binding IDs for core's exact-inclusion
check; the vocabulary of destination kinds (`module`, `host`,
`platform-internal`) is the envelope's.

### What the renderer checks before writing

A host admits **delivered** documents: core's `Host.Admit` takes the type only
its `VerifyDelivered` constructs, so no sequence of calls reaches a host's
admission without an attestation having held over exactly those bytes. The
renderer is the signer, and the set it is about to write is not signed yet —
signing happens at publish, and a `--local` qualification publish signs only as
a developer the environment's host lists (and otherwise never).
So the render and the publish settlement run core's `AdmitRendered` over the
parsed set, which takes no `Host` on purpose (nothing applied, no coordinate, no
signer policy — those need an attestation to be checkable) and refuses what a
host would refuse for reasons that need no host state: a document that breaks
its own rules, a binding declared twice, route aliases claimed twice on one
coordinate. `OneDelivery` is the other renderer check.

Activation has the same split. Before an authority document is signed, publish
holds it against the presence document it is granted over — the one it just
wrote to the staged tree — with core's `ActivateRendered`, which runs every
activation rule that needs no host state and answers a `RenderedMatch` that is
deliberately not an `Activation`: the fold of **both halves** against what the
base branch delivered (`AppliedAuthorityFrom` and `AppliedFrom` of the prior
documents, so a half that would migrate across domains, an authority that
would migrate between bindings, a withdrawal, or a rewritten generation is
refused), the target binding, host and domain agreeing, the approved build
being one the presence says the binding runs, and the effective-from
generation. The authority record is looked up by the **binding** the authority
is granted over, as the host folds it: a withdrawal over a binding is terminal
for every later authority ID over it. Where the base branch holds no record
for a half, publish says so (`FirstAuthorityRecord`, `FirstPresenceRecord`)
rather than passing a zero record, because core refuses a fold given neither —
"nothing applied" is the most permissive input the call takes, and a publish
that forgot to look must not read as a first delivery. Both halves must also
name the envelope revision the environment's `host.envelope_revision` declares
**at publish** — not merely agree with each other, which two documents stamped
against a superseded ceiling do. A render made before the host block was
re-reviewed is refused with both numbers named, and a composition that dropped
its host block is refused rather than having its authority documents written
for nobody to deliver. What stays the host's, because it needs host state: who
signed either half, and whether the authority fits the ceiling — the envelope
is the host's record (core refuses an envelope derived from the document under
check as "a document declaring its own ceiling"), and the renderer holds only
its revision.

The presence half is folded by core too: publish holds every settled
document against the record the base branch delivered for its binding — or
its stated absence — with `AdmitRenderedSets`, the renderer's entrypoint to
the same fold a host runs from its own store (a withdrawn binding presented
again is terminal; a generation behind, or rewritten, is refused; a document
arriving under another domain than the record's is refused, since the
delivered domain is what says who may change the binding). Publish chooses
only the generation's number — unchanged keeps it, changed is prior + 1 —
and restates none of the rules. At render, where no delivery record exists
yet, each set says so (`FirstRecord`), which is true of a render and said
rather than implied.

Verifying the carriers publish assembles — the host's own check, run early
against the signing identity — is the self-check publish runs before anything
is staged, under the environment's reviewed `host.release` policy (the
"Release identity" section above): the publisher's own check, which is not
host admission.

## What publish settles

A render declares; publish settles. Two things are only knowable where the
delivery repository's base branch is in hand:

**The generation.** It is monotonic per binding ID across everything ever
delivered, and the only record of that is the delivered tree on the base
branch. The render destination is per module and per render, so a render that
settled its own generation restarted at 1 after an environment switch. Publish
reads the base branch instead: an unchanged document keeps its generation
(decided by core's own canonical digest, so a byte the host would read as a
rewrite is a byte that bumps the generation); a changed one is prior + 1; a
document delivered for the first time is 1. The render reports a provisional
generation; the settled one is in the inventory's `delivery` record.

**Removal.** Within this module's own delivery, a binding delivered before and
absent from this render is removed — and removal is a generation, never an
absence. Publish writes the tombstone (`removed: true`, prior + 1) and carries
an existing tombstone forward verbatim. **A tombstone is terminal**, for a
binding and for an authority alike: the ID is the handle every other system
holds (installations, operation bindings, team grants), so a later generation
under the same ID would be indistinguishable from continuity of what was
withdrawn. An authority's withdrawal is terminal for the **binding** it was
granted over, not only for its ID — the host folds authority on the binding,
so re-signing under a new authority ID reinstates nothing. The host refuses it
(`ErrTombstoned`), and so does publish — where the fix is one line away: a
genuinely new instance gets a new name, and with it a new binding ID and a new
authority ID. Nothing is ever withdrawn by a
document disappearing: the host treats an empty or unreadable desired set as
removing nothing, and infers no removal from absence anywhere — not within an
ownership domain, not at a higher generation. What lets *publish* derive a
tombstone where a host may not is that publish holds the one complete set:
the current render is everything this module declares, and the previously
delivered set is read from the base branch with an unreadable document being
an error, never an absence — so a malformed document beside a sound one can
never read as a withdrawal of the sound one. A render that names no host over
a delivery that declared bindings is refused, not tombstoned — withdrawing
everything the moment a host declaration is removed is not a publish to make
silently.

**A withdrawal is authored by the domain that delivered the binding.** A
prior document delivered under another ownership domain than the one the
environment declares now is never tombstoned by this publish: a composition
whose domain moved renders nothing under the old one, and that absence must
not sign the old domain's bindings away. The scope is held against what the
environment declares, not inferred from the tombstones agreeing with each
other. A presence document moving domains under a delivered binding ID is
refused the same way, with the host's own reason.

**Authority follows presence.** Each authority document is made effective from
the presence generation settled for its module, so the two halves of a tuple
always travel together, and a binding whose authority content changed while its
revision did not is refused at publish: a credential seals the revision and is
refused when the live one moves, so a change that keeps the number keeps every
outstanding credential's old authority with nothing detecting it. Bump the
binding's revision in the contract.

**Held to the host declared now.** Every document the render declares is
held, at publish, to the environment's host block as it is then: the
ownership domain, the host coordinate and component, the envelope revision,
and the trust domain and audience every one of its workload identities is
issued under must be the ones declared now (the delivery Job's own token
audience is re-rendered from the target declared now), and a render that declares documents for an
environment that names no host any more is refused outright — never signed
and left for no Job to deliver. A render made before the host block changed
is refused by name; the fix is to render again.

**Unchanged is delivered as it was, once it is checked.** A document whose
settled generation is the delivered one keeps the carrier it was delivered
as — the same bytes, the same signature, no new log entry — and the Job that
posts it keeps its name, so a no-op promotion writes the same tree and gives
a re-sync nothing to do. Re-signing on every publish made that impossible.
A carrier is reused only once it is held to the document it would carry: it
must parse, its signed bytes must be exactly the document's canonical bytes,
and on a release publish it must still pass the self-check a host will run —
a carrier the base branch holds that disagrees with the document beside it,
or that cannot be read, refuses the publish rather than being republished as
signed. A local qualification publish to a host that lists no developer never
reuses a signed carrier: it delivers unsigned whatever the base branch holds,
so `signed: false` holds for the whole set. One whose host lists developers
reuses a carrier only when it verifies as one of those developers', exactly as
a release reuses one only under its release policy.

**Signed once.** Signatures need not be repeatable, so the plan signs and
hands its exact carriers to the publish that executes it (`carriers`, keyed
by kind, ID and generation). Execution verifies those bytes under the current
key declaration before reuse. A carrier under a removed or rotated key is
signed again only deliberately: plan with `--resign`, then execute that plan.
Without `--resign` the publisher refuses and names the carrier. Execution
never produces a signature its plan did not contain.

**Rollback re-settles.** A rollback restores the workloads an earlier revision
delivered and settles their documents anew against the base branch — the old
content at the next generation, signed now, the base's tombstones carried
forward — never the carriers that revision signed; and the historical
revision is restored whole only for a moment: every environment's delivery
overlay comes back from the base branch before anything is settled, so
another environment's tombstone is never replaced by that revision's live
bytes, and only this environment's historical overlay is re-delivered. A
host past generation 3
refuses the generation-3 carrier as stale, and a tree restored from before a
withdrawal would carry the withdrawn binding as present. A rollback across a
withdrawal is refused as terminal, as any render presenting a withdrawn
binding is. The environment's host block shapes a rollback publish exactly as
it shapes a render's. A tree from before the module declared anything settles the same way: the
delivered documents are withdrawn as tombstones, never dropped from history
by a settlement that did not run.

**What publish reads, it reads honestly.** The base branch must exist in the
publication checkout; an overlay absent from it is told from an unreadable
one by resolving the path, never by a listing that failed; a carrier
ConfigMap that lost its document key is an error, not another manifest. An
unreadable history read as "nothing delivered" would restart the generation
at 1 and lose every tombstone.

## Signing

Delivery uses platform-owned keys, as decided in
`delivery-signed-with-platform-owned-keys`. Core's `solutionhost.Signer`
is the custody boundary and `SignEnvelope` creates a DSSE envelope over each
canonical document. Core also verifies the returned signature against the
signer's public key before it can leave the publisher. No third-party service
participates in signing or verification.

Release custody is the platform's Vault Transit, configured only for a release
publish with `VAULT_ADDR`, `VAULT_TOKEN`, and `CODEFLY_RELEASE_TRANSIT_KEY`.
`CODEFLY_RELEASE_TRANSIT_MOUNT` defaults to `transit`. The address requires
HTTPS, with HTTP admitted only for loopback. The CLI uses the supplied Vault
token; it performs no login and sends that token only in `X-Vault-Token`.
It reads the latest public key version and signs using that exact version.
Only Ed25519 and ECDSA P-256 are supported. The derived ID is `<name>-v<version>`;
a mismatch with `host.release.key_id` refuses before signing and names both.
Rotation is: pin the new public key on the host, deliver with its new key ID,
then remove the old pin.

Developer custody is one Ed25519 key per machine. `codefly delivery key`
creates or loads it and prints only its ID and public PEM. Its ID is `dev-`
plus the first 16 lowercase hex characters of SHA-256 over the public key's
PKIX DER. macOS uses the OS keychain (`codefly-delivery-signing`, account equal
to the ID); Linux uses `secret-tool` when available. Otherwise a 0600 PKCS#8
PEM file lives in the 0700 `~/.codefly/delivery-keys` directory. The 0600
`current` index selects that one key; broader permissions and symlinks are
refused. A failed keychain lookup never silently replaces an indexed key.

A local publish uses that key only when `host.developers` lists its ID. A
native run needs no such entry: it writes the host's local profile itself,
bound to the run's temporary CA and admitting this machine's key for the
host's domain (`NativeVerificationPolicy`), so a shared composition never
carries a per-machine key, and `host.developers` stays a declared local k3d
cluster's allowlist.
An unlisted key is reported with the exact declaration to add, before any
signature. A release publish never loads developer custody. A local host with
no developers retains unsigned qualification delivery; one listing developers
must sign successfully or refuse.

Fresh and reused carriers pass the same core verifier over a pinned set holding
the custody signer's own public key, its kind (`release` or `person`), and the
environment's ownership domain. A changed declaration between planning and
execution therefore reaches both fresh and previously delivered carriers.
The host performs its own offline check using pins provisioned by its operator.

The carrier beside each document (data key
`solution-host-binding.signed.codefly.yaml` or
`solution-authority.signed.codefly.yaml`) is core's `Signed`:
`{document, envelope}`. The envelope carries exactly one signature and its key
ID; public keys and admission rules come from the host's own anchor. An
unsigned render says so, and unsigned qualification delivery renders no Job
to submit a document the host would refuse.

## The carrier Jobs

The ConfigMaps are the delivery-side durable desired state Argo keeps true in
the cluster, surviving a host database restore. The host learns of a generation
when its delivery API is POSTed the carrier: one Job per document type per
module **per settled set**, rendered at publish once the carriers exist — the
presence Job in the module's namespace, as the `delivery` ServiceAccount the
render creates there; the authority Job in the authority namespace, as the
`delivery` account the platform provisions. A Job is named
`deliver-<kind>-<module>-<environment>-<digest of the carriers it posts>`:
a changed set is a new Job, an unchanged set the same one, and two
environments of one workspace on one cluster never replace each other's Job
in the authority namespace they share. The file it is written to
(`deliver-<kind>.yaml`) is stable.

The overlays are part of the **immutable service snapshot**: every
Application the bootstrap stamps reads the one snapshot revision, the delivery
Applications included, so the overlays they read are settled and signed
*before* the snapshot is committed and live in it beside the units. (They were
once staged after the snapshot, which left every delivery Application pointing
at a revision where its path did not exist — nothing delivered, and no gate
said so until a test ran `ls-tree` on the snapshot.) A rollback is a publish
whose render is the restored tree: it is snapshotted, settled and signed the
same way, and the rollback mutation advertises its snapshot ref as a publish
does.

Each Job is an Argo CD **Sync hook** (`argocd.argoproj.io/hook: Sync`,
`hook-delete-policy: BeforeHookCreation` — which deletes a hook of the *same
name* before creating it again; the Job's name carries its set's content, so
a new generation is a new name and completed Jobs of earlier generations
accumulate until an operator prunes them, which is operational work this
renderer does not do), not a tracked resource: a tracked
Job is applied once and, being complete, never re-synced, so after a restore
nothing would re-POST. A hook runs on every sync of the Application from state
that still exists, so a replay after a restore is an ordinary sync, and a sync
that changes nothing else re-POSTs a generation the host answers "current" to.
Its token is projected for the host's audience and re-read per request; it
runs a digest-pinned `curl` image, read-only, as a non-root user with every
capability dropped. The script spells its variables `$NAME`, never `${NAME}`
— the promotable ruleset refuses a manifest carrying `${…}` as an unresolved
placeholder, and the script is a manifest — and the file the projected
identity is read from is named `DELIVERY_IDENTITY_FILE`: a variable named
`*_TOKEN` carrying a value is a credential to that ruleset, and the Job was
once refused at publish for both.

The Job dials `<service>.<namespace>.svc.cluster.local:<port>` where the port
is the **Service port the CLI allocates** for the endpoint `host.delivery`
names (core's `network.DeployedEndpointPorts`, the same allocation the agent
renders its Service with) — not the container port the service declares, which
is the pod's and may differ. The scheme is `http` unless the endpoint is
`secured`: inside the cluster the mesh carries the request over mTLS between
the Job's pod and the host's, and the bearer token never leaves that tunnel.

Reachability is a retry, not a sync wave: a per-module Application cannot
express "after the host's delivery API is reachable" (the host is another
module's Application), so the Job retries transport failures, 429, 5xx and a
404 from a host a version behind, with backoff and `--max-time` per request,
for about half an hour. The Applications an ApplicationSet stamps sync
independently of each other, so the sync wave orders them only where a
parent syncs them by wave; there, delivery goes **beside the module's
consumers**, in their wave, after its bootstrap units — not after them: a
consumer whose readiness waits on its activation would otherwise wait on a
declaration that waits on it. Neither waits for the other; the Job retries
until the host answers and the consumer becomes ready once activated. The
one exception is the module that serves the delivery API itself (its
inventory records `hostsDelivery`): its delivery follows its units, or on a
cold bootstrap it would wait on a service ordered after it. What is not
verified: no parent application has run this ordering against a real
readiness model; the waves are emitted, not exercised. The host's verdicts — 400, 401 (the token
review ran and refused this identity), 403, 409 (stale or rewritten
generation), 422 (invalid, or outside the envelope) — refuse the document;
the Job goes on to post every other document, a tombstone included, and fails
at the end if any was refused, so a refused delivery is visible where it
happened without withholding the rest. A 2xx is committed and durable; the
host answers `awaiting_match` when the peer document of the tuple has not
arrived, which is terminal for the Job and visible to an operator.

The Job posts in **rounds**: every pending document once per round, terminal
refusals (400, 401, 403, 409, 422) recorded and never retried, rounds until
each document is answered or the budget is spent (`DELIVERY_BUDGET_SECONDS`,
1500 of the Job's 1800-second deadline), with a pause between rounds that
doubles up to `DELIVERY_MAX_PAUSE_SECONDS`. The budget is **enforced by the
clock, not promised by the constants**: the budget left is read before every
request and every sleep, each request is clamped to `DELIVERY_REQUEST_SECONDS`
(30) and to what is left, each sleep to what is left, and the set a Job
carries is bounded at publish to `budget ÷ request` documents (50), so the
first round's attempts fit the budget even when the host runs every one of
them to its timeout — a set past the bound is refused at publish by name,
and it shrinks only when the host acknowledges tombstones, which the delivery
API does not offer yet. A document the host keeps failing never keeps a later
one — a tombstone among them — from its first attempt, and the Job fails at
the end for whatever never landed.

## Workspace configuration groups a render bakes in

A render records, per workspace configuration group its services and its
contract's slots consume, a digest of the group as the environment provided
it, and **reports** the sibling modules rendered for the same environment that
record another digest for a group they both consume. The rule is enforced at
**publish**, where every refusal has one action that satisfies it: a module
whose recorded digest is not the composition's current value was rendered
before the group changed and is refused until rendered again — and the groups
held against are the ones its services and contract consume **now**, derived
from the composition at publish, so a group consumed since the render is a
stale render too (a packaged solution, which has no sources in the workspace,
is held to the groups its render recorded: the one case a group consumed
since cannot be found at publish); a sibling
consumer whose local tree records another digest has not been rendered since
and the publish is refused until it is, by name. A render never refuses on a
sibling's account — it did once, symmetrically, and deadlocked: with A and B
both rendered against the old value, rendering A was refused because B's tree
was stale and rendering B because A's still was. The delivery base is held
the same way, without the deadlock: a consumer the base branch delivers at
the old value is refused **unless this workspace holds its current render** —
the render step already names it as needed — so the base branch never
carries a consumer of a value its provider no longer gives with nothing
queued to replace it, and the way through is always the same one action:
render the named module beside this one and publish both. A consumer current
in this workspace but still stale on the base is named in the plan as the
next publish to run. Absent history and unreadable history are told
apart: no modules delivered yet on the base branch is a first publish, while
a module directory — local or delivered — whose inventory is missing or
cannot be read is an error, never agreement. A rollback is held to the same
rules as a render: a restored tree that would resurrect configuration the
composition no longer provides is refused until re-rendered. The inventory
schema a tree must carry is the current one — an older tree records no
digests, and is refused by number.

## CI wiring

The signing key is the platform's Vault Transit release key, pinned by the
host and declared as `host.release.key_id`. Publisher and host both hold the
signed document to its ownership domain.

```yaml
# .github/workflows/release.yml of the composition repository
on:
  push:
    tags: ["v*"]
jobs:
  deliver:
    runs-on: ubuntu-latest
    permissions:
      contents: read
    steps:
      - uses: actions/checkout@<pinned>
      - run: codefly deploy gitops render --env production
      # VAULT_TOKEN is supplied by the pipeline's own Vault authentication.
      - run: codefly deploy gitops publish --env production
        env:
          VAULT_ADDR: ${{ vars.VAULT_ADDR }}
          VAULT_TOKEN: ${{ secrets.VAULT_TOKEN }}
          CODEFLY_RELEASE_TRANSIT_KEY: ${{ vars.CODEFLY_RELEASE_TRANSIT_KEY }}
```

A publish with another custody key is refused before signing. The host also
refuses every carrier whose key ID is absent from its pins.

## Local runs

`codefly run` on a laptop has no Kubernetes today: no TokenReview, no pod to
read an image from, no mesh, no delivery repository and no signing identity.
The host owns the verification policy; the CLI owns what a local run emits.
What follows is the shape agreed with the host session on 2026-10-02, and it is
not what the first proposal asked for: there is **no `local` trust policy**, no
second document schema, no second carrier and no policy switch. Local and
deployed differ in data — the host's coordinate and its identity allowlist —
never in a code path.

**A process run cannot be described honestly, so a local run runs pods.** A
present generation of the presence schema declares at least one rendered
artifact and, for a backend artifact, workloads pinned to an OCI image manifest
digest with a mesh principal. A laptop run that executes processes renders nothing
and runs no image; a presence document for it could only be produced by
inventing digests, and neither the CLI nor the host accepts an invented one.
The host's decision is that `codefly run` provisions or attaches a **local
cluster** and runs each service as a Pod from a locally built image. Then
nothing is invented: the manifests rendered into the local tree are real bytes
with a real digest (no artifact URI is claimed, so the document never says
they live in a delivery repository); the locally built image's manifest digest
is real and moves on every rebuild, which moves the generation; and the mesh
principal is the account-derived identity the pod runs as — core checks that
field for well-formedness only, and a local cluster with no mesh verifies
nothing on the wire, so no local issuer exists or is required.

**One trust model.** Local and deployed documents use the same envelope and
pinned-key verification. The local host admits a machine's developer key as
`person`, bound to its cluster CA; a deployed host pins platform release keys.
The host accepts no request field that can change those pins or the trust policy.

**Same delivery, same mint.** The same two routes, the same `{document,
envelope}` carrier, the same idempotency on generation and content hash, the
same receipt. A local pod mints with its projected ServiceAccount token, and
the host runs TokenReview and reads the pod's image locally too — `codefly run`
exercises the real path rather than a simulation of it.

**Removal is a tombstone, locally too.** The host treats an empty or
unreadable desired set as removing nothing, under every circumstance; it has
declined to invert that under any policy, because one reconciler holding two
opposite answers to "the desired set is empty" is the failure its tests exist
to prevent. Stopping a run feels like removal and is not: withdrawing a binding
is a POSTed tombstone generation.

**The ceiling locally** is the envelope record the local host holds, with the
same schema as the reviewed envelope, written the way the host writes it
everywhere — the host is moving the envelope from a boot-time file to a record
a platform administrator writes at runtime (saas#1055), and the CLI names no
file for it. The render never reads it, locally or not; it only stamps the
revision the k3d environment's `host.envelope_revision` declares, and the host
refuses a document naming another. The attestation's
own authority is deliberately not the ceiling: effective authority is the
intersection of binding, envelope and installation, and collapsing two of the
three locally would have a developer test a different authorisation shape than
production computes.

**One command (cli#918).** A developer runs the composition on their k3d
cluster with:

```bash
codefly run cluster --env local --app-project <project>           # every composed module
codefly run cluster documents billing --env local --app-project <p>  # or the ones named
```

It takes each step a cell takes, in order, and reports each delivery Job's
receipts:

1. **The platform's prerequisites, locally.** Argo CD, installed into the
   cluster when it does not serve the ApplicationSet API yet — the one
   installer (`deployments.InstallArgoCD`) the k3d qualification uses too, of
   v3.4.1 pinned by what cannot move: the manifest at the commit the tag names,
   its SHA-256 verified before anything is applied, and every image it runs
   rewritten to its digest (an image no digest pins refuses the install). An
   Argo CD already there is used as it is. No workload identity issuer: a
   workload's transport identity is the cell's mesh's, and a local cluster
   with no mesh issues nothing in its place. And the `platform-authority`
   namespace with its `delivery`
   ServiceAccount (no default token mounted), which a cell's platform
   provides and the rendered tree cannot: the authority AppProject admits
   ConfigMaps and Jobs there and nothing else. The modules' namespaces, which
   every Application expects provisioned (`CreateNamespace=false`). And **the
   local host's trust anchor** (below), before any module syncs, because the
   host reads it at boot.
2. **Each module rendered**, exactly as `deploy gitops render` renders it: its
   images built and pushed to the environment's `registry.url` (a registry the
   cluster pulls from, such as the one `k3d cluster create --registry-create`
   makes — the tree pins images by registry digest, so an image imported into
   the cluster by tag would not be the one the documents name), and presence
   and authority derived with the one derivation a cell's render uses
   (`presenceInstanceOf`, `authorityInstancesOf`, `authorityDocument`) from the
   real bytes the build produced: manifest digests, image manifest digests, the
   account-derived mesh principal.
3. **Each module published and promoted**: settled against the local delivery
   repository, signed as the developer, every carrier checked as the local
   host will check it, the same `{document, envelope}` carriers and delivery
   Jobs a cell receives written — then promoted (below). The plan is shown and
   confirmed unless `--yes`.
4. **The latest promotion served**: the fetch remote brought up on the base
   branch tip, the cluster's Argo CD made to trust the remote's CA
   (`argocd-tls-certs-cm`, keyed by its name), and each published module's
   Argo bootstrap (its AppProjects and ApplicationSet) applied from the
   promoted revision.
5. **The deliveries awaited**: Argo CD syncs each module's delivery
   Applications and runs their Jobs as Sync hooks — the presence Job in the
   module's namespace and the authority Job in `platform-authority`, each as
   the `delivery` ServiceAccount, each POSTing to
   `/platform/_delivery/{presence,authority}` with a projected token for the
   host's audience, so the host's TokenReview, signer policy and idempotency
   run locally exactly as in a cell. The run reads **its own** attempt of each
   Job. An unchanged carrier set keeps the Job's name, so the attempt that
   existed before the run (an earlier run's — finished, failed or deleted) is
   recorded first; then the run acquires a sync of every delivery Application,
   always: the Application is the one the CLI rendered, held to its full
   identity (the name the module's ApplicationSet gives the in-cluster
   tenant's component, its repository, revision and path, the in-cluster
   server and the namespace, plus the ApplicationSet controller kind/name/UID
   returned by bootstrap apply). Unowned Applications, foreign owners and
   multi-source overrides are refused. It is never found by path, which every tenant's
   Application of an overlay shares; the run waits for its operation slot to
   be idle, requests a sync carrying a nonce of its own conditionally on the
   version it read, and holds out until Argo CD's operation state carries that
   nonce and its per-operation Kustomize source override, re-requesting one
   another operation consumed. That override stamps the hook with
   `codefly.dev/delivery-attempt`; the awaited Job must carry that accepted
   nonce, have a fresh UID, and be the single matching Job. A creation time
   alone cannot identify an operation. Receipts come from that attempt's own pods. It prints each
   document's answer (delivered with the host's receipt, refused with its
   verdict, given up on) and fails when any was refused; an attempt that never
   appears is a timeout, never an earlier success.

The environment declares `cluster.kind: k3d` and `cluster.context` (the legacy
reading that an undeclared local environment is k3d never selects a cluster to
install into), a `host` block, `host.developers`, and the gitops
`fetch-repo-url` naming the fetch remote. The same steps stay available one by
one: `deploy gitops render`, `deploy gitops publish --local`, and
`deploy gitops remote up` with no module (the latest local promotion).

**Mesh principals and module authority.** Nothing is issued or mounted for a
workload's transport identity: inside the cell every hop is plain HTTP, and
the mesh carries mTLS and destination identity under the workload's
**mesh principal**, the SPIFFE ID of its ServiceAccount
(`EnvironmentHost.SPIFFEID`, the one function presence, module authority and
the mesh edges all name it with). The environment names `host.gateway:
<module>/<service>`, the composed service that fronts solutions and serves the
mint on its `workload` endpoint; the render gives it Core's resolved
ServiceAccount, so the principal every edge admits is the account its pod
really runs as. A service declaring `workspace-configuration-dependencies:
[module-authority]` is given the same account and this group per service,
with the same resolved identity and composed module name as its presence:

| Key in `module-authority` | Rendered source |
|---|---|
| `PRINCIPAL` | The workload's mesh principal |
| `AUDIENCE` | The environment's `host.module_audience`: the audience the host stamps on a module's own capability, as the host names it. A module refuses a credential addressed to anything else |
| `PROJECTION_AUDIENCE` | The environment's `host.audience` |
| Secret `INTERNAL_TOKEN` | A reference to the existing `internal-auth` workspace secret `CODEFLY_INTERNAL_TOKEN` |

Public values use `CODEFLY__WORKSPACE_CONFIGURATION__MODULE_AUTHORITY__<KEY>`
in the service's bound ConfigMap. The secret uses
`CODEFLY__WORKSPACE_SECRET_CONFIGURATION__MODULE_AUTHORITY__INTERNAL_TOKEN`
with a `secretKeyRef` to
`CODEFLY__WORKSPACE_SECRET_CONFIGURATION__INTERNAL_AUTH__CODEFLY_INTERNAL_TOKEN`
in `secret-<service>`. The existing ExternalSecret projection delivers that
key from the environment's service secret store; no token value is copied into
the render. A missing host audience, module audience, namespace or service
secret store refuses the render. The group bypasses the configuration
manager, including composition-root inheritance in native runs: its values
are projected per workload after the agent renders, on any cluster kind. An
authored file for it anywhere in the selected composition is refused, even
when no service declares the group.

Promotion builds every applied overlay and requires each `(apiGroup, kind,
namespace, name)` to occur exactly once across the whole tree. Missing or empty
namespaces take the overlay namespace, then the environment's target namespace,
before comparison. A conflicting occurrence reports both source paths, even when
the two objects are identical or a Kustomize prefix/suffix created the collision.

Filesystem access retains the staged render root, resolved once with
`filepath.EvalSymlinks`. Every path component below it is checked with `Lstat`;
symlinks in service directories, their parents, overlays or files refuse access.
Kustomize uses the same confined filesystem for every read. Manifest updates replace the
file atomically, so hard links cannot cause writes into another tree.

## Live endpoint delivery

Presence names endpoints; it never supplies their addresses. Core's
`codefly/endpoint-delivery/v1` document carries the CLI-resolved carriers of
the endpoints a presence names, bound to the applied presence it joins: host,
binding, generation, ownership domain and the canonical presence digest. It is
signed exactly as the presence is (`SignEnvelope`, `Carrier`, `MarshalSigned`)
and delivered through the same inbox, `POST /platform/_delivery/endpoints`,
with the same credentials. The host verifies it, joins it to the APPLIED
presence generation and keeps the canonical bytes append-only by identity and
generation; its read projection hands them to the gateway's SDK, which refuses
a digest or generation mismatch without a startup-carrier fallback. No
directory, mount or file carries an endpoint anywhere: a configuration or
credential mount is outside the security posture.

A native run delivers the endpoints right after each presence generation
settles, from the same resolution (`Flow.EndpointDelivery`, reusing
`hostRoutes`, visibility selection and `hostRouteCarrier`). The host answers
503 while the generation is not applied yet and the run retries within a
bound; a stale, mismatched or tombstoned delivery (422) and a different
document for an applied generation (409) are refused for good. A changed
address is a new presence generation, never a rewrite, and a tombstone
delivers no endpoints: the host withdraws them with the presence. Resolving one
presence never waits for unrelated producers to allocate their endpoints.

A publication delivers the same document. The render records, per module, the
in-cluster carriers of its endpoints the host routes (`Flow.EndpointCarriers`,
the same routes, visibility and address derivation; `endpointCarriers` in
`.codefly-render.json`, address-only and unsigned), and publish, once the
presence generation is settled, marshals the delivery over the carriers of the
endpoints that presence names, signs it under the same reuse and self-check
rules as the presence, writes it beside the presence's carrier in its ConfigMap
(`endpoint-delivery.signed.codefly.yaml`) and renders a `deliver-endpoints`
Job that posts it to the same inbox through the same retry loop, which tries a
503 again until the presence is applied. A withdrawal carries none, and a
presence naming no endpoint delivers none. The addresses are settled with the
generation: an unchanged publish reuses the delivered endpoint carrier byte for
byte, as it reuses the presence's, and an address that moved under an
unchanged presence settles both at the next generation, since the host keeps
one endpoint delivery per applied generation. A delivered endpoint carrier
that signs other bytes than the delivery being reused is refused, as a
presence carrier is. A local cluster run awaits the `deliver-endpoints` Job
as it awaits the others.

This is the delivery contract, and it is how a component
composed after the run started reaches the gateway: every `codefly run` (a
service, a solution or the workspace) admits the components a reloaded
composition adds when it receives `SIGUSR1` (`kill -USR1 <pid>`). The run
reads the workspace again, compares its roots with the ones it starts and the
ones the composition named when it began, admits each new module-identity service into the
native cluster (its identity is planned by the rule attach used; a known
component may neither change nor disappear, since a replaced or withdrawn
principal is a narrowing the host refuses from a development loop, and the
run's authority, its CA, API server, host token and trust anchor, is
untouched), and starts the new root's closure in the running flow, in
dependency order, through the same Running hook that delivers every other
workload: presence, authority, the envelope (which only ever widens) and the
signed endpoint delivery. Nothing already running is restarted, and the host
keeps its process. One rule of core's namespacing is pinned by test: a
workspace composing one module gives it the environment's namespace, and
composing a second moves it to its own namespace and mesh principal, so that
addition is refused as a replacement; a composition that must grow live starts
with more than one module.

The signal also refreshes the native delivery's composition snapshot when no
root was added. After changing a solution group's title, order or membership,
signal the CLI before reloading the affected component through its agent. The
next Running hook delivers the updated declaration through the same signed
generation path; the signal alone does not restart or re-deliver existing
components. A no-root refresh still refuses any changed workload identity.

If the native run's fixture administrator session expires, the next envelope
read or conditional write signs in again after HTTP 401 and retries once with
the same request and expected revision. A forbidden response or transport
failure is not retried. This is demand-driven administrative delivery, with no
idle sign-in or workload-credential mint loop.

A root the reloaded composition no longer names is withdrawn by the same
signal: the flow stops what only it needed, and the run delivers, for each
stopped workload it had delivered, the generation that withdraws it
(`WithdrawnDocuments`: the presence's tombstone and its authority's, the same
withdrawal a publication settles), then forgets the workload. The host
withdraws its endpoints with the presence, keeps its process and its
authority, and the envelope is not narrowed. The withdrawal is terminal for
the binding at the host.

The tombstone retains the original delivery namespace as transport context;
its signed presence has no workloads, and its removed authority has no effective
generation. If delivery fails, the stopped workload remains pending in this
run. The next `SIGUSR1` retries that same withdrawal even though the root has
already left the flow. Additions wait until pending withdrawals succeed. A successful POST is only
an inbox receipt: completion requires the exact removed generation, digest,
release and host from the existing APPLIED observer. The run uses its separate
read-only endpoint-publisher identity, waits for up to one minute, and retains
pending state if the host cannot apply. Ordinary hot-reload delivery is refused
while removal is pending, keeping the envelope and tombstone generation stable;
reload that workload again after a successful composition refresh.
Accepted tombstones are observed again
without reposting on the next signal. The host must accept the narrowing through
its policy witness; a pending or refused apply is not successful removal. Pending retries survive within this process,
not across a CLI restart.

## Mesh edges

The cell's mesh decides who reaches a workload's port, and its protected
applier compiles that into default-deny `AuthorizationPolicy` objects. **The
CLI never writes an AuthorizationPolicy.** What it writes is the data the
applier compiles: every edge the composition declares, delivered with the
rendered tree in the render record at the tree root, `.codefly-render.json`,
under `meshEdges`, versioned by its own `schemaVersion` (1). Each module's
tree carries the edges INTO that module's workloads; the applier reads every
tree it applies.

```json
"meshEdges": {
  "schemaVersion": 1,
  "trustDomain": "cluster.example",
  "gateway": "saas/auth-gateway",
  "edges": [
    {
      "module": "documents", "service": "api", "namespace": "cell-documents",
      "account": "api", "principal": "spiffe://cluster.example/ns/cell-documents/sa/api",
      "endpoint": "grpc", "port": 9090, "targetPort": 9090,
      "principals": ["spiffe://cluster.example/ns/cell-saas/sa/auth-gateway", "spiffe://cluster.example/ns/cell-wiki/sa/backend"]
    },
    {
      "module": "saas", "service": "auth-gateway", "namespace": "cell-saas",
      "account": "auth-gateway", "principal": "spiffe://cluster.example/ns/cell-saas/sa/auth-gateway",
      "endpoint": "rest", "port": 8080, "targetPort": 3000, "ingress": true,
      "renderedService": {
        "name": "auth-gateway", "selector": {"app": "auth-gateway"},
        "portName": "rest", "protocol": "TCP", "appProtocol": "http"
      }
    }
  ]
}
```

The `gateway` reference is the composition's explicit host gateway. Each edge
carries both `port` (the Service port) and `targetPort` (the resolved container
port). Target ports come from the effective rendered Service, including its
environment overlay and named-port resolution. Missing or ambiguous targets
refuse the render. L4 policy uses the container port; Service-attached policy
uses the Service port.

Gateway edges additionally carry `renderedService`: actual Kubernetes name,
selector, port name, protocol and `appProtocol`. Both `rest` and `workload`
require a named TCP port with `appProtocol: http`, a non-empty selector and
distinct container ports. An alternate Service publishing the same endpoint
refuses the render. The CLI reads these facts from the effective Service;
it does not derive a Kubernetes name from a logical reference. The protected
cell reader compares them against its inventory and owns enrollment,
HTTP presenter sanitization and bypass prevention. HTTP metadata alone is
not authenticated presenter evidence.

One edge per declared endpoint of each workload of the module, at the
endpoint's in-cluster port (the one the render hands the service's agent to
publish as its Kubernetes Service port), naming the destination's namespace,
ServiceAccount and mesh principal, and the principals admitted, sorted and
each once. The sources are the composition's own declarations, each derived
once and nowhere else:

- a module endpoint admits the modules whose declarers ask for it — core's
  derived allow-list (`Workspace.DeriveAllowModules`), at module granularity:
  every workload of a consumer module — and, when composed solutions consume
  it through the host, the gateway's principal (`orchestration.HostRouteTargets`,
  the same derivation that hands the gateway its route carriers);
- a solution's backend endpoint admits the gateway's principal: the gateway
  fronts the solution (the same targets);
- the gateway's `workload` endpoint admits every composed workload's
  principal: every module mints on it;
- a public endpoint — the gateway's `rest` — is marked `ingress`: the cell's
  ingress gateway reaches it, under a principal the cell names, not the
  composition.

One source is not in the composition at all. A service whose render includes a
workload that does **not** serve its endpoints — an agent's bootstrap Job,
which runs under the service's own ServiceAccount and so under the service's
own mesh principal — reaches the endpoints of the very service it belongs to.
Nothing declares it, because it is not a dependency between services but one
rendered workload reaching another, and the rendered tree is the only place it
is visible. So a service that renders one admits itself to each of its own
endpoints, once. Without it the cell's default-deny mesh refuses a Postgres
bootstrap Job the database it exists to initialize.

An endpoint nobody is declared to reach has no edge (default deny is what the
applier compiles, and the absence is the declaration); an endpoint with no
in-cluster port (an external one) is not in the cell; a managed service has
no workload, so it holds no principal and no edge; an environment that
declares no host has no mesh and records no edges. A reader refuses a
`schemaVersion` it does not know rather than compile edges it misreads.

**Local promotion is the developer's signed publish.** On a k3d environment
whose host lists developers there is no reviewer: the developer is the author
and the approver, and the key signature the publish makes — checked
against `host.developers` before it is written — is the approval. So
`publish --local` there **advances the environment's local base branch
itself**, a fast-forward of the local delivery repository the CLI owns, as the
last step of a publish whose every carrier passed its check (a failed check
stops the publish before any commit lands, and nothing is promoted). The
reason is the generation: a publish settles generations against the base
branch, the one record of what was delivered, and a base branch that never
moves would settle every rebuild at generation 1 with other content — a
rewrite the host refuses. Promoted, a rebuilt module lands at generation 2
with its new digest, the next at 3. The promotion acts on the declared local
repository **directly**, never through a push: the environment's `file://`
repository is resolved to its path — following every symbolic link the way
the filesystem traverses it (`EvalSymlinks`), never cleaned lexically, since
`alias/../manifests.git` through a directory link names the link target's
sibling — and must be a bare repository there. Planning records that resolved
path and the directory's device/inode in the plan ID. A retargeted link or
replacement directory invalidates the plan even with identical Git history.
Every local snapshot, review, promotion and base-ref access verifies that
binding before and after it runs. The repository is changed by a git that
reads no
configuration but that repository's own (no global or system configuration,
no `GIT_*` variable of the process, the file transport only), so no
`insteadOf` or `pushInsteadOf` can send it elsewhere. The signed commit is
fetched into it from the publication checkout, held to descend from the base
the plan settled against, and the base ref is moved by a compare-and-swap on
that base — a promotion that landed in between refuses this one rather than
being overwritten — then read back from that repository. It is never forced
and never reaches a hosted repository: a promotion branch the base has moved past
(another module's promotion since) starts again from the base, one that
diverged from it is refused by name, and the base branch of a non-k3d
environment — or of a local one whose host lists no developer — moves only by
a merge after review, as before. `remote up` with no module serves the latest
promotion, and re-running it after a new one moves Argo CD to it.

**How a local pod's identity reaches the host.** Not through anything the run
projects: there is no `MODULE_IDENTITY_PREFIX`, no secret and no
`MODULE_PRINCIPALS` line, and nothing self-registers (cli#855 removed that on
purpose). A module's pod runs as its own ServiceAccount; the presence document
names that account's SPIFFE ID and the image digest it must run, and the
authority document approves that build for the module's principal. The pod
presents its projected ServiceAccount token, minted for the host's audience;
the host reviews it (TokenReview), reads the pod's running image, and
compares both to what the delivered documents approve — the mint saas#1056
wires into every module credential. A module that still requires a projected
`MODULE_IDENTITY_PREFIX` at boot (annotations 0.2.14) fails on this path by
design: its SDK must read the projected token instead.

**The local host's trust anchor** (module-saas-starter#1066, its
`SOLUTION_REGISTRATION.md` §8 "The local profile's trust anchor"). A host
reads one public policy file from its constant anchor path. On a local cluster
the run provides it, as a cell's platform does:

- **`verification_policy.json`** has a `local` block holding
  `clusterCertificateAuthoritySha256`, the lowercase hex SHA-256 of the CA DER
  from `kube-root-ca.crt`. A multi-CA bundle is refused. Its `signers` array
  contains only this machine's held developer key when the host lists its ID:
  `{"name": <key ID>, "kind": "person", "keyId": <key ID>,
  "publicKey": <public PEM>, "domains": [<host.domain>]}`.
  Another developer's public key cannot be inferred from an ID.

The policy is one ConfigMap named `codefly-delivery-trust` in the namespace
of the module `host.delivery` names. On k3d, `host.trust_anchor` makes the
render mount that ConfigMap alone, read-only with keys unchanged, into the
verifier's authenticating container. Provisioning fetches no trust material
and writes no Secret. The host's `SOLUTION_HOST_TRUST_POLICY` is `keys`.

**On a hosted cell** (cli#1036). The platform renders the trust ConfigMap,
`<host.component>-delivery-trust`, in the verifier's namespace, and its
contract says the composition's Deployment carries the mount — which is this
render's output. So on any environment that declares a host and is not a
local k3d one, the render mounts that ConfigMap into the verifier's
authenticating container at the host's constant path,
`/etc/obin/delivery-trust` (`gitops.HostedTrustAnchorPath`, the other half of
the host's own constant, which it refuses to relocate from inside a pod), as
one projected volume with one ConfigMap source, no `items`, read-only — the
shape the platform's admission pins (obin-ai/infra-base#1366); a pod
mounting the anchor any other way is refused by the cell. The render declares
nothing about the ConfigMap's content: the platform owns what the anchor
says, the render owns only that the pod reads it where the host looks. The
path is never a workspace value — a composition that could name it could
point the verifier at a policy it wrote itself — so `host.trust_anchor` stays
refused on every environment that is not k3d.

The host proves the binding at boot (the CA in its projected bundle, and a TLS
handshake to its API server with that CA alone), so a profile copied to any
other cluster refuses the boot. Every run recomputes the fingerprint,
re-writes the anchor, and **reconciles** every Deployment, StatefulSet and
DaemonSet that READS it — any configMap volume, projected source,
`envFrom` or `valueFrom` naming the anchor, whatever the volume is called:
the digest of the policy's contents is stamped on its pod template
(`codefly.dev/trust-anchor`) when it differs, its rollout is awaited, its
template is read back (a competing rollout that put the old one back is
caught), and **every live pod** it selects must carry the digest. An update
strategy that keeps old pods by design — `OnDelete`, a partitioned
StatefulSet — cannot be proven and is refused rather than reported done. The
annotation is the run's: the render refuses a workload that carries it in
desired state, so Argo CD never owns a value that would revert the stamp.
Convergence is read from the cluster, never from what an apply reported, so a
stamp, rollout or pod that failed — after a developer was removed, say — is
retried by the next run until every running pod started on the anchor in
force.

**What the host itself must still be configured with** (its own local
configuration, not the trust anchor): its solution-host coordinate equal to
`host.coordinate`; its accepted ownership domains including `host.domain`;
its delivery API being the service `host.delivery` names; permission to
create TokenReviews and to read the pods of the module namespaces; and the
envelope record at `host.envelope_revision` holding the binding IDs the local
contracts ask for (saas#1055).

**The cost, and the alternative the host would accept.** Requiring a local
cluster changes the laptop story from a container runtime to a container
runtime plus a cluster, on every developer's machine. The host names the
alternative it would accept instead — not a second trust model but **no
capability surface locally**: presence and routing so a developer sees their
UI, and no module credential minted at all. Which of the two holds is the
owner's call; this document records the first, as agreed, until the owner
decides otherwise.

### A native run: the run answers as the cluster

The section above ends at "a local run runs pods", and `codefly run cluster`
is that run. A run of processes is still the first loop a developer has, so a
**native** `codefly run` of a composition that declares a host stands in for
what the host reads from Kubernetes, instead of the host learning a second
path. The host's code is the same in both.

For the lifetime of one run, the CLI is:

- **the API server** — TokenReview and pod reads over TLS on a loopback port,
  under a CA created for that run. It issues each module-identity workload's
  projected token for the host's audience, living **600 s** and reissued at
  80 % of that (480 s) as the kubelet rotates a projected token, and reports
  each workload's pod. Each token carries a JTI of its own, and its review
  reports it as `authentication.kubernetes.io/credential-id` (`JTI=<jti>`),
  the value the host records to refuse a second mint with the same token: a
  mint client that re-reads the projection before every mint always presents
  a token the host has not seen, and a replayed one is refused. The
  `module-authority` group is supplied to the process the way the render
  supplies it to a pod, with the workload's mesh principal.
- **not the mesh** — nothing issues or serves a certificate: every hop is
  plain HTTP, and the laptop's stand-in for the mesh's reachability rule is
  loopback. The address the run hands every service to listen on is a
  loopback address, and only the host's gateway may be reachable off it; a
  service that answers on one of the machine's other addresses is warned
  about, naming it (`pkg/orchestration/loopback.go`). Where a workload
  reaches the gateway is the workspace value the composition points at the
  gateway's `workload` endpoint, resolved like every other `${endpoint:…}`
  reference at the address the run allocated.
- **the delivery pipeline** — once a workload is running, and once the host
  is ready to take it, it signs that workload's presence and authority with
  the developer key and POSTs them to the host's own delivery endpoints, and
  writes the module envelope through the fixture's platform administrator.

The host is pointed at the stand-in by two settings the run supplies,
`SOLUTION_HOST_LOCAL_CLUSTER` and `SOLUTION_HOST_LOCAL_TRUST_ANCHOR`, both
under `.codefly/native-cluster/<environment>/` in the workspace. The host
refuses both in a pod, and the run never makes the host believe it is in one.
The host turns its delivery surface on from three keys of its `federation`
workspace configuration, which a cell's composition declares for the
environment: `SOLUTION_HOST_COORDINATE`, `SOLUTION_HOST_OWNERSHIP_DOMAINS` and
`SOLUTION_HOST_TRUST_POLICY`. A native run derives them for the delivery host
alone (the service `host.delivery` names, when it declares `federation`) from
the environment's host block: the coordinate is `host.coordinate`, the
ownership domain is `host.domain` (the one the run's own policy admits the
run's signer for) and the trust policy is `keys`, handed after the host's
authored group so they settle over what it leaves empty; the host's
registration secrets stay in that authored group, untouched.

Three things it does, and why:

- **The envelope is written before the documents that name it.** A document
  naming a revision the host does not hold is outside its ceiling, and the
  host refuses the whole reconcile pass on it.
- **A run only ever adds to the envelope.** It carries every principal the
  host already grants and every build it already approves. Withdrawing either
  is a narrowing, which the host witnesses and a development loop does not
  perform. A principal whose own section changes — a solution whose contract
  moved — is refused by a host with no witness: reset the local store.
- **A module-identity service is told where its token is.** Its contract
  fixes the path a pod mounts (`/var/run/secrets/codefly/token`), which a
  process cannot have; the run projects the token under its own state and sets
  `CODEFLY__WORKLOAD_TOKEN_FILE`. A pod never sets it.
- **What the run delivered is remembered across runs**, under
  `.codefly/native-cluster/<environment>/delivered/`, so a new envelope
  revision re-delivers the documents of workloads this run has not started yet;
  a document naming an old revision would otherwise stop the host's reconcile.
- **A solution's token is withheld until its documents are delivered.** A
  workload starts before the host has been told about it, because its build is
  known only once it runs. A cell answers that with a restart; the run answers
  it by projecting the token only after delivery. The workload waits for the
  token, and from then on the host answers "not established yet" until its
  registry has read the documents.
- **A delivery waits for the host, within a deadline.** A workload reports
  Running on its own schedule: a module the gateway does not depend on can be
  Running before `host.gateway` has started, and a gateway that listens can
  still answer 503 until it has configured itself. So a workload's delivery
  starts only once the gateway and the `host.delivery` service are ready by
  the readiness the run gates on (Start completed, consumed endpoints
  answering), and a refusal that only says the host is not serving yet is
  asked again with a backoff from 0.5 s doubling to 10 s, for at most
  **10 minutes** from Running: a 5xx; a 404 from the gateway (sign-in or
  envelope) before it exposes the route; a 409 on the envelope (a lost
  revision race); no answer at all (refused, reset, timed out). A refusal
  that says no fails at once with its reason: any other 4xx, including a 404
  from the host's own delivery endpoint (a wrong `host.delivery`), a policy
  refusal, an address that is not a URL, an agent that reports no started
  artifact. The first wait and every change of its reason are logged at info
  with the service, its module and the reason. A workload not delivered by
  the deadline is reported as an error naming it, why the host is still not
  serving, the last refusal it answered, and the consequence: every gateway
  route to its module answers 404 until it is delivered, which a restart of
  the workload attempts again. A workload has one delivery in flight: a
  restart replaces the waiting one, and a workload the composition withdraws
  while it waits is not delivered.
- **The build is what the agent says it started.** The run asks no question of
  the filesystem about where a workload's build lives, because the answer
  differs per agent family and only the agent that started the process knows
  it. See below.

### The executable the run started

A cell pins an OCI image manifest. A native run has no registry, so it pins
what actually ran — and gets that from the agent, over
`StartStatus.started_artifact` (`runtime-started-artifact/v1`): the program the
agent exec'd, and the files it handed that program.

- **A compiled runtime starts its own build.** The Go agents build a binary and
  run it; that one file is the whole of what the process is. The build is its
  SHA-256, which is what this has always been.
- **An interpreted runtime starts an interpreter.** A `python-fastapi` service
  runs `uv`/`python` over its own sources; hashing the interpreter would give
  every python service on the machine the same build, and tell you nothing when
  one of them changes. So the agent names the closure beside the interpreter —
  the entrypoint, the modules it loads, the lock file pinning its dependencies
  — and the build is the SHA-256 of a manifest listing each of those files with
  its own SHA-256. Editing any of them is a different build.

  Three things make that manifest say one thing only. Each entry is
  **length-prefixed** (`<len> <name> <sha256>`), so a file whose name contains
  a newline cannot spell out entries of its own and let two different closures
  share one build. The manifest is opened with a **domain tag**, so an
  executable whose bytes happen to be a manifest is not read as that closure —
  the two shapes never meet by accident. (A deliberately crafted executable
  still can: closing that would mean tagging the single-file digest too, and
  then a compiled runtime's build would stop being the SHA-256 of its own
  file. On a developer's own machine that is authority they already have.) And
  a name is taken **relative to the workspace**, with the workspace and the
  reported path's *directory* resolved through their symlinks, so a tree known
  as `/var/...` and reported as `/private/var/...` is one build and the same
  checkout digests the same wherever it sits. A file's own link is not
  followed: `.venv/bin/python` points at whatever interpreter uv or pyenv
  installed, and naming the build after that target would make a pyenv rehash
  or another developer's machine a new build for an unchanged tree — the link
  is what the workspace holds at a stable name, and its content is still read
  through it. A file outside the workspace, such as an interpreter in `/usr`,
  is named by its absolute path — the only name it has, and part of the
  build. The list is sorted and de-duplicated, so the agent's enumeration
  order is not part of the build, and an agent that names the executable among
  its own sources describes the same process as one that leaves the list
  empty.
- **An agent that reports nothing is a refusal, not a guess.** The run names
  the agent and the contract it must implement, and that workload is not
  delivered — so a solution keeps waiting for its token rather than minting
  against a build nobody vouched for. A reported path that is not absolute is
  refused the same way: resolving it would open whatever file sat in the
  terminal's working directory. The run used to read
  `<service>/.cache/native/main` for every agent, which is the Go agents'
  layout; a service on any other agent failed with a missing file and was never
  delivered at all ([#989](https://github.com/codefly-dev/cli/issues/989)).
  Until an agent adopts the contract, its workloads are refused by name.
- **A workload with no host file has no shape here yet.** A service the run
  starts in a container is still a peer the run would deliver, and its build is
  an image digest rather than files on the developer's disk. The contract has
  no form for that, so such a workload is refused like any other that reports
  no artifact. It was never delivered before either — it has no
  `.cache/native/main` — so nothing regresses, but the refusal names a
  contract its agent cannot honestly satisfy.

What is weaker than a cluster, stated rather than implied: the build a pod is
said to run is the SHA-256 of what the agent reported starting, and the run is
both the party that says so and the party that signs the presence naming it.
No kubelet is a second witness — and the agent is not one either: it is the
same developer's machine reporting on itself. That is the authority a developer
already has over their own machine. The profile the host reads admits the
developer's key only for the one CA it names, and that CA does not outlive the
run.

## Dev deployments

`codefly deploy dev` re-pins a service's image inside an already-rendered
tree. On a module that declares presence or authority it is **refused** and
sent to a render: the documents pin, in bytes publish signs, the exact build
each workload runs, and a re-pin would leave them describing bytes nobody
runs — the host refuses the new pod, and a publish would sign a false
declaration. A render re-derives the declaration with the new build. On a
module declaring neither, a dev deployment is what it was.

## What is not verified

No cluster and no host have applied these documents: the shapes are agreed
with the host's reconciler and the platform's issuer, but agreement is not a
running system. Vault custody is exercised with a loopback HTTP server;
cryptographic verification uses core's real verifier. Real Vault, OS keychains,
and cluster delivery require integration verification. The packaged solution path
(`RenderSolution`) runs through an in-process executor.

Named and not built: a **withdrawal path for a whole module** — a module
removed from the composition has no render and so no publish, and its
presence and authority stay applied on the host until a publish of that
module's path with no instances writes their tombstones; pruning gated on the
host acknowledging them needs the host's acknowledgement, which the delivery
API does not yet answer. The **release digest** is the content digest of the
module package as the composition materialized it, computed over its files:
for a module that is a checkout, the files its repository tracks or does not
ignore (so dependency caches, built bundles and local secrets the repository
ignores are not the release), and for a materialized package outside a
checkout, its whole tree; version control metadata, render output and the
CLI's own state root (`.codefly`) are never part of it. It ties the
declaration to those bytes and to nothing a registry attested, and
nothing attests that the pinned image was built from them. The **module
contract** (`codefly/module-contract/v1`) has ONE implementation, in core,
beside the presence and authority documents: `contracts/module` (the model,
strict decoding, every refusal, slot resolution). This repository holds no
copy: the render derives authority through core's reader, and the kits core
ships — the document kit (`Fixtures()` and `Run(t, read)`, every accepted and
refused document with its sentinel and message) and the resolution kit
(`RunResolution`, driving the provider the render resolves slots from) — run
through this repository's own entrypoints
(`TestTheModuleContractKitRunsThroughTheRender`,
`TestTheResolutionKitRunsThroughTheRendersProvider`). The runtimes publishing
contracts run the same kits through theirs. **No cell inventory is written
here**: core's owner ruled an environment-wide Kubernetes model out of core,
and the Lane 3 registry specification replaces it with an inventory of the
keys the cluster's admission policy compares, written by the CLI once that
specification is an implementation contract — a PR of its own.

### Solutions with no consumed permissions

A declared solution with no scoped consumption still emits a signed authority
document with an explicit empty binding list. Its principal, presence binding,
workload and approved build remain admitted through the ordinary delivery path.
The native envelope carries that exact presence binding in `admitted_bindings`;
this admits installation without inventing a consumed permission. A deployed
operator envelope must provide the same explicit admission.

A module contract takes precedence over an empty consumption declaration. Two
nonempty authority sources remain refused. Exactly one service must declare
`module-identity`, including for an empty permission set.
