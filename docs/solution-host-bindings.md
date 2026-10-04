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
      release:                                  # the identity the host accepts delivered carriers from
        repository: example/payments            # the release workflow's repository
        workflow: .github/workflows/release.yml # its path in that repository
        refs:                                   # the refs it may run at (anchored regular expressions)
          - refs/tags/v[0-9]+[.][0-9]+[.][0-9]+
```

`release` is the host's **reviewed allowlist**, stated here so it is reviewed
with the composition and read by nothing else: a release publish holds the
workflow identity it runs under to it before it signs anything — another
repository, another workflow, a branch, a tag outside the pattern never sign —
and reuses a carrier delivered earlier only when the identity that signed it
is within it. The host publishes the same literal and enforces it
independently; a hosted environment that states no `release` cannot be
released to. It is optional in the schema only because a `--local` publish
never signs.

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

An environment that composes a module **declares the host it runs on**, or
the render is refused with the instances and the block's fields named.
Rendering the instances' workloads with no declaration delivered them present
on no host, with a warning the operator could miss; a composition with nothing
to declare is one with no solution instance, not one that forgot its host.

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
principal the environment declares for the service, and the SPIFFE ID
`spiffe://<trust_domain>/ns/<namespace>/sa/<service account>`), and every
other container, init containers included, as one that must never be accepted
as the workload. A unit with several containers and none named after the
service is refused: "whichever one presented the token" is how a sidecar ends
up holding a workload's authority. Presence names what **mints** under the
service's principal — its serving workloads — and not a unit's bootstrap Job
or CronJob: those run their own images and never authenticate as the service,
so they are declared to the cell for admission, where the closed approved set
covers every pod the platform will see, and not here, where their image would
be a build the host accepts a token for. A managed service's bootstrap bundle
is the cell's for the same reason and is never a presence artifact.

A serving workload names its own ServiceAccount: one running as the
namespace's `default` account — every pod's that names none — is refused,
because the identity a document names is the account's, and that one any pod
of the namespace can present.

The document cannot close the sidecar gap on its own: a projected
ServiceAccount token is the pod's, so a sidecar that mounts it presents it as
the workload and the host cannot tell which container asked. The cell's
admission policy closes it — a projected token minted for an explicit
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

Two approved builds of one ServiceAccount share one SVID: a mesh derives the
identity from the account, not the image. Binding identity to the build is
therefore not something the SVID does; the host does it by comparing the pod's
resolved image to the approved build, and the platform by refusing a pod whose
image is not approved at admission. The identity a document names is the
account's.

The `delivery` ServiceAccount is the delivery Job's and nothing else's: a
serving workload naming it is refused at render, in the presence document and
in the cell alike, since a deployment caller and a runtime caller under one
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

The contract's **principal is the module's own name** — the contract says so
of itself, and it is held to that at render: the contract is written in the
module's repository, so a principal it names is self-asserted, and a module
claiming another's principal would claim that principal's bindings. And the
contract is carried **whole or refused**: a declaration core's authority
document has no field for — the module's own `scope_ceilings`, its
`destinations`, a second queue or namespace, a binding's `binding_key` or
`lookup.method` — is refused by name, with the field the document would need,
rather than dropped between the contract and the signed document. A host
cannot enforce a declaration it never receives, and a declaration that changed
without the document changing would be enforced as before. A module declaring
any of them waits on core growing `solutionhost.AuthorityBinding`.

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
  publisher's own record under the repository's trust, like the cell;
  nothing signs it and no Job reads it; the module's queue and namespace
  when it declares one of each — absence grants no queue- or namespace-scoped
  authority, never every queue, and several are refused rather than granted
  none;
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
audience is re-rendered from the target declared now), and the rendered
cell is held to the same host before it is merged, and a render that declares documents for an
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
signed. A local qualification publish never reuses a signed carrier: it
delivers unsigned whatever the base branch holds, so `signed: false` holds
for the whole set.

**Signed once.** A keyless signature is fresh on every call — a new key, a
new log entry — so a publish that signed in the plan and again in the publish
could never match the plan it executes. The plan signs, hands its carriers to
the publish that executes it (`carriers` in the plan, keyed by kind, ID and
generation), and that publish delivers exactly those bytes after holding each
to its document the way any carrier signed before is held. A carrier signed
by an earlier release is reused under the **release policy** — the same
repository and workflow, any release tag — and one the policy no longer
admits (a rotated signing identity, a run that was never a release) is
signed again only **deliberately**: a plan made with `--resign` signs it
once, the publish executing that plan reuses that signature, and without
`--resign` the plan refuses and names the carrier. A publish never produces a
signature the plan it executes did not: it reuses the plan's — tried before
any signature, even where the delivered carrier was rejected — or refuses.

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

A release is signed only by CI, never by a person. Publish signs each
document's canonical bytes (`CanonicalBytes()`, the exact payload the host
re-canonicalizes) with the publishing workflow's **Sigstore keyless** identity
over GitHub Actions OIDC: an ephemeral key certified by Fulcio for the workflow
identity, recorded in the transparency log, packaged as a Sigstore bundle
(`application/vnd.dev.sigstore.bundle.v0.3+json`). The `codefly` binary holds
no key, and there is no option to supply one (`pkg/delivery/signing`). A
`--local` qualification publish **never signs** — not "signs when it can":
its signer is the one a process with no identity gets, so a workflow holding
an OIDC identity cannot deliver signed carriers to a qualification cluster
under the release identity.

A release publish **checks its own carriers**, right after signing each one,
as a host will: offline, against the public-good trusted root fetched through
TUF, under a policy pinning exactly this workflow's identity — repository,
workflow path and ref, as GitHub Actions states them to the job
(`GITHUB_REPOSITORY`, `GITHUB_WORKFLOW_REF`) — and that identity is held to
the environment's reviewed `host.release` policy **first**, so a workflow or
a ref the host would refuse never signs. A carrier delivered earlier is held
to the policy itself (its repository and workflow at any of its refs) before
it is reused — and so is the carrier an inspected plan carries, under the
policy **as it is at publish**: a plan records reused carriers as well as
fresh signatures, and a policy tightened between the plan and the publish
reaches both; a plan the policy no longer admits is stale and refused by
name, with nothing signed. The workflow identity exists only where those two variables are
set, local or not; a hosted release publish run without it (from a laptop, or
from a workflow that does not expose it) is **refused before it reads a
document**, rather than signing unchecked or reusing delivered carriers under
no policy at all. A publish to an environment with no host needs no identity
— it delivers nothing signed — and a `--local` publish never signs. A
carrier a host would refuse —
a certificate naming another workflow, a log entry the root cannot verify, a
bundle with no transparency evidence — is refused at publish, in front of
whoever ran the release, and nothing is written for a Job to deliver.

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

A managed service's bootstrap bundle is inventoried too — its Job is a pod
the closed admission set would refuse unless the cell names it — with no
endpoints of its own, and so is the module's **presence delivery Job**, under
`delivery` on the namespace: the labels its pods carry (its name holds the
settled set's digest and is decided at publish), the `delivery` account, the
one container and its pinned image. An ingress route reaches the
module-qualified service it names and no other module's service of that bare
name. A service's own bootstrap Job or
CronJob is inventoried the same way, with no endpoints, consumers or ingress:
a workload that serves nothing declares nothing, so no policy derived from
the cell grants a Job the service's reachability.

It is regenerated whole on every render from the module trees on disk, and
carries no generation and no tombstone — a workload absent from it is not
delivered, which is the opposite of the presence document's rule. Publish
stages **this module's contribution**: the namespace entry derived by the
render's own derivation applied to the tree it publishes — for a forward
publish and for a rollback alike, which contributes the restored tree's cell
— never read from a file that could describe another tree. What the tree
says is read off the tree: the workloads, their images, selectors, accounts,
identities, artifacts and release. What the composition says is **the
composition's declaration as it stands at publish**, not a record the tree
carries: the endpoints and their ports, ingress, bindings, cloud identity,
egress, and the consumer edges — so a rollback restores the workloads and
re-reads those declarations, and a declaration changed since the render is
what the cell carries. Persisting them in the render's inventory, so a publish
derives from immutable inputs and refuses drift, is owed and named. The
workspace's cell file, the render's output, is held on a forward publish to a
derivation over the inventory **as rendered** (the render cannot know the
tombstones a publish synthesizes) and refused when its entry does not match;
the delivered cell is then derived from the **settled** inventory, delivery
Job included, so a publish that withdraws the last declaration describes the
Job that delivers its tombstones. The consumer edges a publish reconciles
into other modules' entries come from the same dependency graph the cell
render builds (every composed service's `service-dependencies`, a dependency
naming no endpoint reaching every endpoint of its target), so the edges do
not depend on which module publishes first. The contribution is held to the
host declared now before it is merged into the cell the delivery repository
already holds at
`<gitops path>/cells/<environment>/cell.yaml` (outside every module path and
matched by no Argo overlay), every other module's entry kept as delivered.
Three rules keep the merge honest. The cell is **one host's record**: the
delivered cell's schema, coordinate, component, domain and trust domain are
what every other module's entry was written under, so a contribution made
under another declaration is refused rather than relabelling entries it does
not own. The publishing module's **edges follow its render**: in every other
module's delivered entry, the consumers that belong to this module are
dropped and the ones its inventory declares now (the contracts its units
consume) are added, so an edge the module grew or dropped reaches the
provider's entry even when the provider is not rendered in this workspace. And
a base branch with **no cell** for the environment is a first contribution,
while a cell that cannot be read is an error — the two are told apart by what
git says, never conflated into an empty baseline. A module is removed from the
cell by withdrawing it, never by another module's publish — copying the local
file whole let the last module published decide the platform's inventory for
the whole cell. A hosted environment publishes nothing
without it: a render that has no cell file is refused by name, and a cell
file that cannot be read is an error, never an absent one.

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
running system. Keyless signing against the public Sigstore instance, and the
publisher's self-check against the public-good root, are exercised only
through an offline virtual Sigstore in tests. The packaged solution path
(`RenderSolution`) runs through an in-process executor.

Named and not built: a **withdrawal path for a whole module** — a module
removed from the composition has no render and so no publish, and its
presence and authority stay applied on the host until a publish of that
module's path with no instances writes their tombstones; pruning gated on the
host acknowledging them needs the host's acknowledgement, which the delivery
API does not yet answer. The **release digest** is the content digest of the
module package as the composition materialized it, computed over its files;
it ties the declaration to those bytes and to nothing a registry attested, and
nothing attests that the pinned image was built from them. The **cell file is
unsigned**: it is read by the platform's own build from the delivery
repository, under the same repository trust as the manifests Argo applies,
and whether that is enough is the owner's call. The **module contract**
(`codefly/module-contract/v1`) and the **cell file** (`codefly/cell/v1`)
have ONE implementation each, in core, beside the presence and authority
documents: `solutionhost/modulecontract` (the model, strict decoding, every
refusal, slot resolution) and `solutionhost/cell` (the model, strict decoding,
every refusal). This repository holds no copy: the render derives authority
through core's reader, the publish reads every cell — the workspace's file and
the delivered one — through `cell.Parse` and holds the cell it writes to
`Validate` before writing it, and the two kits core ships (`Fixtures()` and
`Run(t, read)`, every accepted and refused document with its sentinel and
message) run through this repository's own entrypoints
(`TestTheModuleContractKitRunsThroughTheRender`,
`TestTheCellKitRunsThroughThePublisher`). The runtimes publishing contracts and
the platform loading cells run the same kits through theirs.
