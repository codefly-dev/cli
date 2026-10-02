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
revisions named. `trust_domain` is the mesh's — `cluster.local` for a mesh
that derives a workload's identity from (trust domain, namespace,
ServiceAccount) — and the platform's loader refuses a SPIFFE ID it does not
derive the same way, so a document naming any other value gets an identity the
mesh never presents.

The environment also declares the external reach of each service, keyed by
module-qualified identity like `managed-services`:

```yaml
    egress:
      platform/accounts: {hosts: [identity.example.test, api.github.com]}
```

A declaration the composition states and the render carries into the cell
file, never derived: the module author knows a service reaches an identity
provider or a code host, only the composition knows which, and a host left out
here is a workload that cannot reach it. The render validates a bare host name
and never parses one out of configuration. `delivery` names the host's delivery API the way
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
`codefly/module-contract/v1`) and declares `module-identity: true` on a service
gets one authority document per such service, into
`solution-authority/overlays/<environment>/`, delivered to the platform's
authority namespace (`platform-authority`) — the namespace only the delivery
pipeline may create Jobs in.

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

The derivation:

- **authority ID** `<binding-id>:<service>`;
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
absence. Publish writes the tombstone (`removed: true`, prior + 1), carries an
existing tombstone forward verbatim, and re-presenting a withdrawn binding
starts a new generation after the tombstone. Nothing is ever withdrawn by a
document disappearing: the host treats an empty or unreadable desired set as
removing nothing. A render that names no host over a delivery that declared
bindings is refused, not tombstoned — withdrawing everything the moment a host
declaration is removed is not a publish to make silently.

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
identity, recorded in the transparency log, packaged as a Sigstore bundle. The
`codefly` binary holds no key, and there is no option to supply one
(`pkg/delivery/signing`).

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
than from a hand-written set. One namespace per module rendered for the
environment; under it every pod-producing workload of every unit — Deployment,
StatefulSet, DaemonSet, Job, CronJob — with its kind, the exact label set that
**selects its pods** (the selector of a Deployment, the template labels of a
Job: a bootstrap Job carries no `app` label, and a policy assuming one selects
its pods with nothing), the account it runs as and its SPIFFE ID, its pinned
containers and init containers, the artifact and release it comes from, the
endpoints it serves with their **container** ports (the port a connection
lands on after Service resolution, as the service declares and the render
verifies), their visibility and `allow_modules`, and their **consumers** —
every composed service whose `service-dependencies` reaches the endpoint, so a
port no declared edge reaches is visible in the file rather than found by an
audit. Per namespace, the egress each service is declared to need: the hosts
from the environment's `egress` declaration and the CIDRs of a managed service.

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
