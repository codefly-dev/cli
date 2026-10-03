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
(`codefly/solution-authority/v1`) and the signed carrier
(`codefly/solution-host-signed/v1`). This page is about what the CLI puts in
them and how they get there.

## The static half, and what the render never reads

The lifecycle has a static, deploy-time half and a dynamic, runtime half, and
nothing straddles them. The render produces the static half:

| record | writer | what it carries |
|---|---|---|
| presence document | the render; settled and signed at publish | kind, binding ID, generation, ownership domain, envelope revision, host, release digest, rendered artifacts and their digests, the workloads with the build each runs and the identity each presents |
| authority document | derived from the module's published contract; signed at publish | the principal and the units of authority it holds, approved for one build, effective from one presence generation |
| cell file | the render, regenerated whole | the inventory of a cell: every namespace, workload, account, SPIFFE ID, image, endpoint, ingress and egress |

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
      trust_domain: cluster.example             # the SPIFFE trust domain of the host's identity issuer
      envelope_revision: 3                      # the envelope revision this composition was reviewed against
      delivery: platform/accounts/rest          # the host's delivery API, by composition identity
```

Nothing here is derived. A derived coordinate is a guess a host silently
refuses at reconcile time; an unlisted ownership domain is refused by the host
with the domain named; a wrong envelope revision is refused at apply with both
revisions named — and earlier, at publish, which holds every authority pair
against the revision this block declares *then*, so a render made before the
block was re-reviewed never leaves the publisher. A document **asserts** its own `domain`, so the host also
holds a policy saying which **signer identity** may make that assertion
(core's `Host.DomainsBySigner`, keyed by the certificate SAN the host's bundle
verification names — for a release workflow,
`https://github.com/<owner>/<repo>/.github/workflows/<file>@refs/tags/<tag>`).
That policy is the platform's, provisioned beside the identity allowlist and
never by a delivered document; the composition carries its domain (here, in
every document it renders, and in the cell file) and nothing about who may
sign for it. A document signed by an identity the host does not let speak for
its domain is refused with the signer and the domain named. `trust_domain` is the mesh's — `cluster.local` for a mesh
that derives a workload's identity from (trust domain, namespace,
ServiceAccount) — and the platform's loader refuses a SPIFFE ID it does not
derive the same way, so a document naming any other value gets an identity the
mesh never presents.

The environment also declares the external reach of each service, keyed by
module-qualified identity like `managed-services`, and what the cell must
grant it beyond the mesh edges the render derives:

```yaml
    egress:
      platform/accounts: {hosts: [identity.example.test, api.github.com, {name: smtp.example.test, port: 587}]}
    cell:
      platform/accounts: {bindings: [vault, audit]}
      platform/model: {bindings: [model_gateway], cloud-identity: true}
```

Both are declarations the composition states and the render carries into the
cell file, never derived. Egress: the module author knows a service reaches an
identity provider or a code host, only the composition knows which, and a host
left out here is a workload that cannot reach it. A bare host name means port
443; a host reached on another port says so, because the platform allows a
(host, port) and a port it cannot see is a denial that looks like an
application timeout. The render validates a bare host name and never parses
one out of configuration. `cell`: the cell-provided resources a service binds —
a vault, an object store, an audit sink; the vocabulary is the cell's and its
loader refuses a name it does not provide — and whether the workload mints a
cloud credential from the node's metadata server, a path no egress waypoint
carries. Only the composition knows which of its workloads holds a binding;
the cell provisions the resource and derives the grant, so a binding missing
here is a workload refused at the resource rather than one granted by guess.
`delivery` names the host's delivery API the way
`api.consumes` names a producing endpoint — by module, service and endpoint —
and the render resolves it to the in-cluster address the delivery Jobs POST to
(`<scheme>://<service>.<namespace>.svc.cluster.local:<port>`, the port being
the one the service declares under `spec.deployment.endpoint-ports`).

An environment that declares **no** `host` renders no document, and the render
says which modules it could not declare.

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
principal the environment declares for the service, and the SPIFFE ID
`spiffe://<trust_domain>/ns/<namespace>/sa/<service account>`), and every
other container, init containers included, as one that must never be accepted
as the workload. A unit with several containers and none named after the
service is refused: "whichever one presented the token" is how a sidecar ends
up holding a workload's authority. Every pod-producing object of a unit is a
workload — the bootstrap Jobs and CronJobs included, which run their own
images — so the host's approved set covers every pod it will see.

Two approved builds of one ServiceAccount share one SVID: a mesh derives the
identity from the account, not the image. Binding identity to the build is
therefore not something the SVID does; the host does it by comparing the pod's
resolved image to the approved build, and the platform by refusing a pod whose
image is not approved at admission. The identity a document names is the
account's.

## Authority

A module that publishes `module.contract.codefly.yaml` (schema
`codefly/module-contract/v1`) and declares `module-identity: true` on **one**
service gets one authority document, into
`solution-authority/overlays/<environment>/`, delivered to the platform's
authority namespace (`platform-authority`) — the namespace only the delivery
pipeline may create Jobs in. One service, because an authority document
approves one build and a host keeps one authority record per binding: two
services each claiming to be the module's identity would be two builds under
one principal, of which the host could activate at most one, so the render
refuses the pair and names both.

The contract declares the principal, the operation bindings the module redeems
(each with the operations it needs and a scope ceiling per operation), the
module's queues and namespaces, its own scope ceilings and the destinations it
exposes. Another module's vocabulary — a binding's audience, the resource kind
its scopes name — is never spelled in the module's repository: it is a **slot**,
`{from: <group>/<key>}`, resolved from the composition's workspace
configuration for the environment. A slot resolves public configuration only; a
slot pointing at a secret-classified value fails the render rather than inlining
it into a delivered document, and a bare string where a slot belongs is a schema
error.

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
  `<principal>:<binding>:<operation>`, with the operation's ceiling as a sorted,
  comma-joined list of `<resource kind>:<action>` and the binding's revision
  from the contract (1 when omitted); the module's queue and namespace when it
  declares exactly one of each — absence grants no queue- or namespace-scoped
  authority, never every queue;
- **effective from** the presence generation settled in the same publish.

The host's envelope must list the same binding IDs for core's exact-inclusion
check; the vocabulary of destination kinds (`module`, `host`,
`platform-internal`) is the envelope's.

### What the renderer checks before writing

A host admits **delivered** documents: core's `Host.Admit` takes the type only
its `VerifyDelivered` constructs, so no sequence of calls reaches a host's
admission without an attestation having held over exactly those bytes. The
renderer is the signer, and the set it is about to write is not signed yet —
signing happens at publish, and a `--local` qualification publish never signs.
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

A module with no contract has only the presence half, and core exports no
rendered fold for it (`AdmitRendered` carries no applied state), so publish
folds it itself against the base branch — generation, rewritten generation,
tombstone, and the ownership domain: a generation arriving under another
domain than the delivered one is refused with the host's own reason, since the
delivered domain is what says who may change the binding. That is the one
place this package restates a rule core holds; it goes the day core exports
the fold.

Verifying the carriers publish assembles — the host's own check, run early
against the signing identity — is not built yet.

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

**Authority follows presence.** Each authority document is made effective from
the presence generation settled for its module, so the two halves of a tuple
always travel together, and a binding whose authority content changed while its
revision did not is refused at publish: a credential seals the revision and is
refused when the live one moves, so a change that keeps the number keeps every
outstanding credential's old authority with nothing detecting it. Bump the
binding's revision in the contract.

## Signing

A release is signed only by CI, never by a person. Publish signs each
document's canonical bytes (`CanonicalBytes()`, the exact payload the host
re-canonicalizes) with the publishing workflow's **Sigstore keyless** identity
over GitHub Actions OIDC: an ephemeral key certified by Fulcio for the workflow
identity, recorded in the transparency log, packaged as a Sigstore bundle
(`application/vnd.dev.sigstore.bundle.v0.3+json`). The `codefly` binary holds
no key, and there is no option to supply one (`pkg/delivery/signing`).

The bundle is verifiable **offline**, which the host relies on: delivered
namespaces egress nothing, so a verifier that looked up Rekor or Fulcio would
hang rather than fail. The signer is the Sigstore public-good instance (Fulcio
and Rekor, no timestamp authority), and the bundle carries the log entry whole
— the signed entry timestamp and the inclusion proof with its checkpoint — so
a verifier needs only a mirrored trusted root holding the public-good Fulcio
chain, Rekor key and CT log keys (`LoadTrustedRoot`), never the log. A bundle
carrying no such evidence is refused by its own name (`ErrNoTransparency`),
distinct from a signature that fails (`ErrSignature`), on both sides of the
wire: a signer configured without a transparency log is fixed at the signer,
a bad signature is investigated, and the two must not reach an operator
looking alike.

The carrier written beside each document (data key
`solution-host-binding.signed.codefly.yaml` or
`solution-authority.signed.codefly.yaml`) is core's `Signed`: the canonical
bytes verbatim and the bundle, a JSON object the host verifies against its own
trust root and identity allowlist — issuer, repository, workflow path, ref
pattern; never a key. The bundle's certificate is evidence checked against the
host's allowlist, never read as authority, and neither document can nominate
its own identity policy. Rotation is a trust-root update; a disconnected
perimeter verifies offline from the bundle against a mirrored trust root.

A publish with no signing identity — a laptop — **refuses**, naming the
documents, unless it is a `--local` qualification publish, which delivers them
unsigned, records `signed: false` in the inventory, and renders no Job to POST
them, because the host refuses an unsigned document. A local render always
produces unsigned documents and says so.

## The carrier Jobs

The ConfigMaps are the delivery-side durable desired state Argo keeps true in
the cluster, surviving a host database restore. The host learns of a generation
when its delivery API is POSTed the carrier: one Job per document type per
module, rendered at publish once the carriers exist — the presence Job in the
module's namespace, as the `delivery` ServiceAccount the render creates there;
the authority Job in the authority namespace, as the `delivery` account the
platform provisions.

Each Job is an Argo CD **Sync hook** (`argocd.argoproj.io/hook: Sync`,
`hook-delete-policy: BeforeHookCreation`), not a tracked resource: a tracked
Job is applied once and, being complete, never re-synced, so after a restore
nothing would re-POST. A hook runs on every sync of the Application from state
that still exists, so a replay after a restore is an ordinary sync, and a sync
that changes nothing else re-POSTs a generation the host answers "current" to.
Its token is projected for the host's audience and re-read per request; it runs
a digest-pinned `curl` image.

Reachability is a retry, not a sync wave: a per-module Application cannot
express "after the host's delivery API is reachable" (the host is another
module's Application), so the Job retries transport failures, 429 and 5xx with
backoff and sits in the consumer-unit wave with the workloads whose presence it
declares. The host's verdicts — 400, 401, 403, 409 (stale or rewritten
generation), 422 (invalid, or outside the envelope) — fail the Job, and with it
the sync, so a refused delivery is visible where it happened. A 2xx is
committed and durable; the host answers `awaiting_match` when the peer document
of the tuple has not arrived, which is terminal for the Job and visible to an
operator.

## The cell file

`deployments/cells/<environment>/cell.yaml` (schema `codefly/cell/v1`) is the
inventory of the cell, from which the platform derives its mesh policy rather
than from a hand-written set. At the top, the host's coordinate, component,
the ownership `domain` the composition delivers under (so the platform can
hold its signer policy against what the composition declares at build time,
rather than have the host refuse the first delivery) and `trust_domain` —
carried as its own field as well as inside every SPIFFE ID,
so the platform re-derives each identity and refuses a mismatch instead of
parsing the domain out of the string it is checking. One namespace per module
rendered for the environment; under it every pod-producing workload of every
unit — Deployment, StatefulSet, DaemonSet, Job, CronJob — with its kind, the
exact label set that **selects its pods** (the selector of a Deployment, the
template labels of a Job: a bootstrap Job carries no `app` label, and a policy
assuming one selects its pods with nothing), the account it runs as and its
SPIFFE ID, the **authenticating** container (the one named after the service,
or the only one; several with none so named is refused, naming them — the
same designation the presence document carries, so an admission policy
compares that container's image to the approved build and treats every other
container as a closed set that never authenticates), its pinned containers
and init containers, the artifact and release it comes from, the endpoints it
serves with their **container** ports (the port a connection lands on after
Service resolution, as the service declares and the render verifies), their
visibility and `allow_modules`, and their **consumers** — every composed
service whose `service-dependencies` reaches the endpoint, so a port no
declared edge reaches is visible in the file rather than found by an audit.
Three more fields the platform derives grants from: `verifier: true` on the
serving workloads of the service `host.delivery` names (the one that verifies
delivered documents, from which the platform derives its token-review and
pod-read RBAC and the carrier's allow into it — a bootstrap Job of that
service is not marked), and the `bindings` and `cloud_identity` the
environment's `cell` declaration states for the service. Per namespace, the
egress each service is declared to need: the hosts from the environment's
`egress` declaration, each as `{name, port}` with the port always explicit,
and the CIDRs of a managed service.

It is regenerated whole on every render and carries no generation, no domain
and no tombstone — a workload absent from it is not delivered, which is the
opposite of the presence document's rule. Publish copies it into the delivery
repository at `<gitops path>/cells/<environment>/cell.yaml`, outside every
module path and matched by no Argo overlay, where the platform reads it at
build time.

## Workspace configuration groups a render bakes in

A render records, per workspace configuration group its services consume, a
digest of the group as the environment provided it, and refuses to replace its
tree while a sibling module rendered for the same environment records another
digest for a group they both consume. Change a group and render only one of its
consumers, and the refusal names the modules to render too, so the environment
never delivers two values of one group. The check runs at render, over the
module trees beside this one, and deliberately not at publish, where it would
refuse whichever consumer publishes first.

## CI wiring

The signing identity is the composition repository's **own release workflow,
running on a tag**. The host's allowlist pins three literals and one pattern —
the OIDC issuer, the repository, the workflow path (`.github/workflows/release.yml`)
and the ref (`refs/tags/v*`) — and refuses everything else: a branch build, a
dispatch off a branch, another workflow file in the same repository, and a
reusable workflow in another repository (whose certificate names the called
workflow, not the caller). So the publish step lives in that file, needs
`id-token: write` and nothing else, and no key or secret exists anywhere:

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
      id-token: write
    steps:
      - uses: actions/checkout@<pinned>
      - run: codefly deploy gitops render --env production
      - run: codefly deploy gitops publish --env production
```

A publish from any other identity produces documents the host refuses, by the
same check that refuses any unlisted identity.

## Local runs

`codefly run` on a laptop has no Kubernetes today: no TokenReview, no pod to
read an image from, no SVID, no delivery repository and no signing identity.
The host owns the verification policy; the CLI owns what a local run emits.
What follows is the shape agreed with the host session on 2026-10-02, and it is
not what the first proposal asked for: there is **no `local` trust policy**, no
second document schema, no second carrier and no policy switch. Local and
deployed differ in data — the host's coordinate and its identity allowlist —
never in a code path.

**A process run cannot be described honestly, so a local run runs pods.** A
present generation of the presence schema declares at least one rendered
artifact and, for a backend artifact, workloads pinned to an OCI image manifest
digest with a SPIFFE ID. A laptop run that executes processes renders nothing
and runs no image; a presence document for it could only be produced by
inventing digests, and neither the CLI nor the host accepts an invented one.
The host's decision is that `codefly run` provisions or attaches a **local
cluster** and runs each service as a Pod from a locally built image. Then
nothing is invented: the manifests rendered into the local tree are real bytes
with a real digest (no artifact URI is claimed, so the document never says
they live in a delivery repository); the locally built image's manifest digest
is real and moves on every rebuild, which moves the generation; and the SPIFFE
ID is the account-derived identity the pod runs as — core checks that field
for well-formedness only, and whether anything verifies it on the wire stays
conditional on a projected trust bundle, so no local issuer is required.

**One trust model.** A local document is signed through the same Sigstore
keyless flow as a deployed one, with the **developer's own OIDC identity**. A
deployed host's allowlist names the reviewed-change workflow identity and
nothing else, so a locally signed document is refused there by the ordinary
check that refuses any unlisted identity — not a mode, not a coordinate
comparison, not an environment variable. (A variable that selected a trust
model would be a variable that leaks into a deployment manifest once and is
never noticed.) The host accepts no `policy` field in a request: a caller that
could name its own trust model is what the allowlist exists to decide.

**Same delivery, same mint.** The same two routes, the same `{document,
bundle}` carrier, the same idempotency on generation and content hash, the
same receipt. A local pod mints with its projected ServiceAccount token, and
the host runs TokenReview and reads the pod's image locally too — `codefly run`
exercises the real path rather than a simulation of it.

**Removal is a tombstone, locally too.** The host treats an empty or
unreadable desired set as removing nothing, under every circumstance; it has
declined to invert that under any policy, because one reconciler holding two
opposite answers to "the desired set is empty" is the failure its tests exist
to prevent. Stopping a run feels like removal and is not: withdrawing a binding
is a POSTed tombstone generation.

**The ceiling locally** is a developer-authored envelope file in the host's
`local` configuration profile, with the same schema as the reviewed envelope,
at a path the host names when it builds the envelope table. The attestation's
own authority is deliberately not the ceiling: effective authority is the
intersection of binding, envelope and installation, and collapsing two of the
three locally would have a developer test a different authorisation shape than
production computes.

**What this PR does on a laptop, and what it does not.** A local render emits
unsigned documents and says so. `codefly deploy gitops publish --local` (a
k3d qualification environment) delivers them unsigned, records `signed: false`
in the inventory and renders no Job — a host refuses an unsigned document, and
the CLI says so rather than POST what will be refused. Not built here: the
local-cluster run path for `codefly run`, and signing with a developer OIDC
identity (the keyless flow with an interactive token instead of the workflow's).
Until both exist, a local run of a host and its solutions has no presence on
the host.

**The cost, and the alternative the host would accept.** Requiring a local
cluster changes the laptop story from a container runtime to a container
runtime plus a cluster, on every developer's machine. The host names the
alternative it would accept instead — not a second trust model but **no
capability surface locally**: presence and routing so a developer sees their
UI, and no module credential minted at all. Which of the two holds is the
owner's call; this document records the first, as agreed, until the owner
decides otherwise.

## Dev deployments

`codefly deploy dev` re-pins a service's image inside an already-rendered tree
to code no release describes. The documents are **not** re-rendered: declaring
a release that does not describe the running code is a worse record than a
stale digest. On such a tree the declared digests no longer match the delivered
bytes; a full render re-derives both and clears the dev deployments.

## What is not verified

No cluster and no host have applied these documents: the shapes are agreed
with the host's reconciler and the platform's issuer, but agreement is not a
running system. Keyless signing against the public Sigstore instance is
exercised only through an offline virtual Sigstore in tests. The packaged
solution path (`RenderSolution`) runs through an in-process executor.
