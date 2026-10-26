# Module manifests: what the CLI delivers to the host, and how

A module (or a composed solution) is **present** on a host because a signed
**module manifest** declared it — never because its process announced itself.
One manifest per module instance says which workloads run as the module,
where the host may route to, and what the module asks for. `codefly` builds it
from the composition, settles and signs it at publish, and delivers it through
the same Argo-managed tree the workloads travel in, plus one Job per module
that POSTs it to the host's delivery API. A native `codefly run` builds and
delivers the same manifest itself.

The document, its rules, its canonical bytes and its signature are core's
(`solutionhost`, schema `codefly/module-manifest/v1`; see core's
`docs/module-manifest.md`). This page is about what the CLI puts in it and how
it gets there. The presence (`codefly/solution-host-binding/*`), authority
(`codefly/solution-authority/*`) and endpoint-delivery
(`codefly/endpoint-delivery/*`) documents it replaced are gone, with no
compatibility path: a tree an older CLI rendered is rendered again (the render
inventory's schema moved to 8 for it).

## What a mint rests on, and what it does not

A host mints a module credential on four answers: **who is calling** (the
workload credential, attested — in a cell by TokenReview of a pod-bound
projected token and nothing else; natively by the run's own credential),
**which module it is** (the attested namespace and account match exactly one
workload of one manifest), **may it act for this organisation** (an active
installation), and **with which scopes** (the administrator's grant for the
module's principal). There is **no build approval at runtime**: no image,
release, artifact or started-artifact digest is approved, signed, compared or
reported. "Only approved images run" is deploy-time admission, outside this
pipeline. The administrator's grant — the **envelope** — is the host's record,
written by a platform super-admin (or, natively, by the run's fixture
administrator); the render never reads it.

## The manifest

| field | where the CLI takes it from |
|---|---|
| `binding` | `<workspace>.<environment>.<instance>`, derived from identity alone, bounded to 63 characters and to lowercase DNS labels because it names a Kubernetes object and is stamped as a label value |
| `alias` | the instance's name in the composition: one lowercase path segment (a solution is reached at `/<alias>/`, a module at `/v1/<alias>/*`) |
| `kind` | `solution` when the module ships a solution manifest, `module` otherwise |
| `generation` | settled at publish (below); a render writes a provisional one |
| `host` | the environment's `host.coordinate` and `host.domain` |
| `release` | the module's package manifest: `<publisher>/<name>` and its exact version |
| `group` | a solution's group in the composing workspace's `solution-groups` (generated, authored nowhere else) |
| `frontend` | the component's `solution.frontend.codefly.yaml`, with a generated `payload` (below) |
| `workloads` | **every** service of the module declaring `module-identity: true`: its service name and the container that authenticates — nothing of where it runs, which the cell spells by rule |
| `endpoints` | the module's endpoints the host routes, each with the address the composition resolved for it |
| `authority` | what the module asks for, always with `principal: <module name>` |

**Workloads.** A module runs as every service that declares
`module-identity` — a serving process and a worker, say — so its one manifest
lists each. A workload is `(service, container)` and carries no namespace,
account or SPIFFE ID (core's `solutionhost.Workload`, since core#812): a cell
runs it as the ServiceAccount named after the service, in the module's
delivered namespace — `<delivery>-<environment>-<module>` when the workspace
composes several modules (the environment's `namespace` being
`<delivery>-<environment>`), the environment's namespace when it composes one
(handbook decision
`a-delivered-namespace-is-named-by-its-delivery-environment-and-module`).
In a cell each is read off the unit that
renders it: the serving workloads (Deployments, StatefulSets, DaemonSets) of
the unit must agree on one container and one account; the container is the one
named like the service, or the only one; a workload running as the
namespace's `default` account, or as the `delivery` account the delivery Job
uses, is refused, and so is any account but the one named after the service.
A bootstrap Job or CronJob never authenticates as the service and is not a
workload. Its mesh identity is
`spiffe://<host.trust_domain>/ns/<namespace>/sa/<service>`. A module with no
`module-identity` service has nothing that runs as it and declares no manifest
(the render says why); one that publishes a contract or asks for scopes with
no such service is refused, since the request would otherwise be dropped. A
packaged solution (`deploy solution`) declares no service, so it declares no
manifest either.

**Endpoints.** The host routes a module's endpoint only when its service is a
workload of the manifest. The render records the in-cluster address of every
endpoint of the module the host routes (`Flow.EndpointCarriers`: the same
routes, visibility and address derivation as the gateway's own carriers) and
writes each into the manifest; a routed endpoint of a service that does not
declare `module-identity` is refused by name. A changed address is changed
content, so the next generation.

**A component's optional `frontend` block.** Modules and solutions both read
`solution.frontend.codefly.yaml` beside their entry service, through core's
`solutionhost.ParseFrontend`. A missing file (or no entry service) means a
headless component; an empty or malformed one refuses delivery. Its `payload`
is never authored: it is generated from the release's runtime artifact named
`frontend` in the package manifest (`release-artifacts`) — artifact name,
digest and media type, verbatim. A deployed render of a release declaring
none builds the bundle from the release's own source, as it builds the
release's images: `npm ci && npm run build` in the remote's npm project
(`services/fe-remote`, else the component root), inside a digest-pinned Node
image, on the committed tree (HEAD) of the repository the component is
released from: nothing untracked or ignored reaches the builder (nor do
uncommitted edits), and the module cache is never written. A component in no
repository builds from its own directory alone. It archives `services/fe-remote/dist` in core's
canonical form, publishes it by digest to
`<registry.url>/<workspace>/frontend-bundles` (rooted against registry garbage
collection and read back), and stamps that artifact as the payload. The same
render hands every service declaring the `artifact-cache` workspace
configuration `REPOSITORY`, that repository, which a host reads bundles from by
the digest it admits. A local render and a local k3d cluster's publish and
project nothing. Before native delivery, Codefly builds every declared page;
no manual bundle build is required. The build is keyed by exactly what it
reads, and reads a staged copy of exactly that: the files of its npm project
(`services/fe-remote`, or the module root when only that holds a
`package.json`, then without the services' code and every Go source) that its
repository tracks or does not ignore — but never an untracked dotenv file
(`.env`, `.env.*`), which the deployed build, reading the committed tree,
never sees — the same of every local package it depends on (a `file:` or
`link:` dependency naming a directory, transitively), every file such a
dependency names (a vendored `.tgz`, at the path npm resolves, so a cold npm
cache installs it from the copy; replacing it installs and builds again), and
the top-level regular files of each
directory above the project (a tsconfig base, an `.npmrc`). Unchanged sources
whose bundle is still retained build nothing, so a Go edit that restarts a
service never rebuilds the page, while a new file in the project or an edit
to a sibling package does; the record and the staged copy are kept under
`.codefly/run/<environment>/frontend/`. A module's staged copy, with its
`node_modules`, is removed when the module leaves the composition and every
copy when the run stops; the records stay, so a bundle still retained is
reused. Past 2 GiB of staged copies in an environment, the least recently
built copies of other modules are removed after a build. The build runs before the run-wide
delivery lock is taken, so it delays no other module's delivery. It runs the
frontend's own npm build natively in the staged copy (`npm ci` first when the
copy's dependencies were not installed from this lockfile, then `npm run
build`), with no variable of the shell but what finds npm and its registry, so
the component's own `node_modules` and files are never touched; a leftover
`dist` is never a serving claim. When the service that owns the page runs in a
container, the same copy builds in the pinned builder image as deployment
does. A symlink or special file among the sources is refused by name, since
the deployed build refuses it too. The bundle is archived in Core's canonical
form and retained in `.codefly/selection-artifacts` under its digest, and the
manifest names those exact bytes. A build failure refuses delivery with the
component name, build command and final 40 lines of build output; it never
falls back to a previous bundle. Headless components perform no frontend build.

## Declaring the host

The environment declares the host it delivers to. Every field is required,
together, because every manifest needs all of them at once:

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
      trust_domain: cluster.example             # the SPIFFE trust domain of the cell's mesh, which every workload's SPIFFE ID is under
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
with the domain named. A manifest **asserts** its own `host.domain`, so the host also
holds a policy saying which **signer identity** may make that assertion
(core's `Host.DomainsBySigner`, keyed by the key ID the host's envelope
verification names, such as `delivery-v3`).
That policy is the platform's, provisioned beside the identity allowlist and
never by a delivered document; the composition carries its domain (here, in
every manifest it renders) and nothing about who may
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
and the render resolves it to the in-cluster address the delivery Job POSTs to
(`<scheme>://<service>.<namespace>.svc.cluster.local:<port>`, the port being
the one the service declares under `spec.deployment.endpoint-ports`).

An environment that declares **no** `host` renders no manifest. A
`host.envelope_revision` is refused as an unknown field: no manifest names the
envelope's revision, which is the host's record alone.

Two environments that deliver to one repository, branch and path replace each
other's module trees whole, so a publish of one would turn the other's
delivered generations and withdrawals into absence — a replay would hold no
record of what was withdrawn. An environment that declares a host is refused,
at publish, while another environment of the workspace delivers to the same
path; each hosted environment needs a path of its own. What a path already
holds is kept whatever the configuration says now: a publish stages its own
environment's overlay beside every other environment's delivered documents
and removes nothing it did not author under its own environment — an
environment removed from the configuration keeps its delivered generations
and withdrawals where they are. And a document delivered under this
environment's name to another host coordinate refuses the publish: a host
change is a new environment, never a publish over another host's record.


## The workload credential, and the container that holds it

Every identity workload is projected a ServiceAccount token minted for the
host's audience, read-only, at `/var/run/secrets/codefly/token` (the path the
SDK's mint client reads), living **600 s** — and **only into the container its
manifest declares**. The host checks no container: Kubernetes attestation is a
TokenReview and nothing else, so which container holds the credential is
decided here, at deploy time, and a sidecar never does. The render writes the
projection as a patch in the unit's environment overlay. A container that
already provides exactly that projection (for the audience, at that file,
without `subPath`, living at most 600 s, hidden by no other mount) keeps it;
anything else at that path is refused with what is mounted there. A pod
template in which any other container — a sidecar or an init container —
mounts a token minted for an explicit audience is refused, keyed on the
audience as the platform's admission is (the token the ServiceAccount plugin
injects names none). A service that does not run as the module is projected
nothing.

The render also supplies `CODEFLY__WORKLOAD_TOKEN_FILE` to that authenticating
container, naming the validated projection. It replaces a stale literal or
`valueFrom` for this carrier while retaining unrelated environment entries.
An existing valid projection receives the carrier without another mount or
volume; sidecars and nonidentity workloads receive neither. Native runs use
the same carrier, so Authority's shared `hop.RunCredential` reads and rereads
the workload's credential without a consumer knowing a cell-specific path.

Before upgrading a consumer that removes a fixed-path fallback, re-render its
workload with this behavior and deliver the resulting pod template. The new
image and carrier may be delivered together. A generator pin or an image-only
upgrade does not update an already-rendered workload; verify the carrier in
each authenticating container. Render tests do not qualify an applied cell or
settle the separate handbook decision about mounted credential delivery.

## What a module asks for

The manifest's `authority` block is the module's **request**. Its principal is
always the module's name, so a module with workloads always runs as its
principal, even with nothing to ask (a module with no contract). What it asks
for comes from exactly one declaration:

- **A published contract**, `module.contract.codefly.yaml`
  (`codefly/module-contract/v1`): each operation binding becomes
  `{id, audience, operations: {invoke|lookup|headless: [<kind>:<action>, …]}}`
  with the scopes its resolved ceiling allows per operation; the module's own
  `queues` and `namespaces` are carried as declared, and `resource_kinds` are
  the kinds of its own scope ceilings. The contract's principal must be the
  module's name — a module claiming another's principal would claim its grant.
  Each destination it declares must name an endpoint one of the module's
  services declares.
- **A composed solution's scoped `api.consumes`** in `solution.codefly.yaml`
  (`codefly.solution-manifest/v1`, read through core's `LoadConsumption`, so
  only the schema and `api.consumes` are read, strictly): core's
  `ConsumptionContract` turns each entry carrying `scopes` and an `audience`
  slot into one invoke binding. A scoped entry for a module the workspace does
  not compose, or addressed to the solution itself, is refused.

Both at once is refused by name. The request is advisory: the host grants the
envelope's grant for the principal, reports requested bindings the grant lacks
as a reason on the binding, and never widens or narrows the grant from it.

**A module that installs prepared bindings also asks for each one.** When one
of its services declares the `runnable-bindings` workspace configuration group
([Runnable](runnable.md)), the module's request gains a binding for every
prepared binding the environment's group carries:
`{id: <the prepared key, MODULE__OPERATION>, audience, operations: {invoke, lookup}}`,
with the audience and the kinds and actions of the binding's resolved policy.
It carries **no resource id**: which resources an organization lets the module
reach is that organization's consent, written to the module's installation
scope, which narrows the binding there. A prepared key equal to a binding id
the module's own declaration uses is refused by name, and a value in the group
that is not a prepared binding fails the render by key. A module that asks for
nothing — no contract, no scoped consumption — gains nothing: the host grants
it its envelope whole, and a request would change that.

**A solution's roles are part of its contract.** A unit a person uses declares
the roles a person may hold on an installation of it in the same
`module.contract.codefly.yaml`, as `roles` beside its `scope_ceilings`, under
core's rules (an `installer` among them; each scope within the unit's own kinds,
or a permission at another module one of its bindings spells out for `invoke`
or `lookup`) — see core's `docs/module-contract.md`. The core manifest names the
kinds alone; for a composition-interface host the delivery carries each own
kind with its actions (`AuthorityManifestV1.scope_ceilings`, read once from the
contract's `scope_ceilings`) and the roles as declared
(`AuthorityManifestV1.roles`). A solution asking for scopes on `api.consumes`
declares roles by moving that consumption into the contract's bindings, since a
module asks for authority from one declaration. The CLI defines no roles schema
of its own: a unit still shipping the retired `roles.codefly.yaml`
(`codefly/solution-roles/v1`) fails the render and the delivery, naming the file
and the contract its kinds and roles now belong in.

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
a `*-binding` key, `audiences` from a `*-audiences` key and `resource_kinds`
from a `*-resource-kinds` key — in either key spelling core accepts, and refuses a slot
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

A binding is **mandatory** unless its contract marks it `optional: true`: every
composition fills a mandatory binding's slots or the render refuses. An optional
binding is for a capability the module degrades without, and core's resolution
decides it on the slots alone. A composition that fills **none** of them — each
key absent from its group, or supplied empty, as a module's declared
configuration supplies a key nobody set — renders the module without it: the
delivered manifest carries no such binding, so no credential can be issued
through it, its calls are never read (the audience need not be composed), and
the render logs it, `module <m>: optional binding <id> is omitted from the
delivered manifest:` followed by every unfilled slot. Filled, it renders exactly
as a mandatory binding. Filled **in part**, it is resolved whole and each
unfilled slot is refused by name, the refusal saying the binding is optional and
was filled in part, because a misspelt key must never silently drop a binding
(`TestAnOptionalBindingFilledInPartIsRefusedByName`). A secret-classified record
fills its slot, and is refused as a secret rather than omitted around.

A binding may be addressed to a **list of audiences** the composition writes —
for a generic module that reaches whichever modules a composition wires to it,
such as a store of comments reaching the owner of each kind of thing commented
on. It declares `audiences: {from: <group>/<key>-audiences}` in place of
`audience`, the value comma-separated with no space (`robin,documents`), and,
for a bare-action ceiling, `resource_kinds: {from: <group>/<key>-resource-kinds}`
whose value is `<audience>=<kind>` pairs. The render delivers it **once per
listed audience**, in core's resolution: binding `<binding>:<audience>`
addressed to that audience, its `calls` read from that audience's own published
contracts (so two owners declaring one protocol's methods differently get two
ceilings), its bare actions qualified by that audience's kind. A call one listed
audience does not serve is refused naming the binding, the call and the
audience; the audience that serves it is not blamed. The render logs
`module <m>: binding <id> is delivered once per listed audience, as` followed by
the delivered ids, since they are not the ones the contract spells. The issuer
holds each delivered binding to its one audience, exactly as a single-audience
binding, so a module is issued a context toward one owner by naming
`<binding>:<owner>`. An optional list composes with the rule above: the list
absent or empty omits the binding, and filled with its kinds missing is filled in
part, refused by name (`audience_list_binding_test.go`). A binding with
`audience` renders exactly as before, under its own id.

A binding may be addressed to **the module itself** — a module admitting work
into itself on someone's behalf, such as a scheduler issued the capability an
occurrence is admitted under. It writes `audience: {self: true}`, and core's
resolution delivers it with the contract's principal as its audience. The
render holds that principal to the module's name **before** anything resolves
(the same check that refuses a module claiming another's principal), so the
self binding is addressed to exactly the principal the manifest's `authority`
block names, read from no workspace configuration value: a configured value
carrying the module's own name would restate an identity the render already
resolves, and could drift from it. Core holds what such a binding may ask for —
no `resource_kind` slot, every scope spelled out within the module's own
`scope_ceilings`, `self` on an audience only — see core's
`docs/module-contract.md`. It composes with the rules above: an optional self
binding is omitted by its other slots alone (its `binding_key`, say), and an
audience-list binding beside it is delivered once per owner as before
(`self_audience_binding_test.go`).


## What publish settles

A render declares; publish settles. Two things are only knowable where the
delivery repository's base branch is in hand:

**The generation.** It is monotonic per binding across everything ever
delivered, and the only record of that is the delivered tree on the base
branch. An unchanged manifest keeps its generation (decided by core's
canonical digest, the encoding the host tells a replay from a rewrite by); a
changed one is prior + 1; a binding delivered for the first time is 1. The
render reports a provisional generation; the settled one is in the inventory's
`delivery` record.

**Removal.** Within this module's own delivery, a binding delivered before and
absent from this render is withdrawn — and a withdrawal is a generation, never
an absence: publish writes the manifest with `removed: true` at prior + 1,
carrying only its identity (schema, binding, alias, kind, generation, host,
release), and carries an existing withdrawal forward verbatim. **A withdrawal
is terminal**: installations and grants are keyed on the binding, so a later
generation would re-attach them to a new instance that reused the id. Core's
`Admit` refuses it (`ErrWithdrawn`), and publish runs the same decision over
the base branch's record first — the fix is a new instance name, which is a
new binding. A render that names no host over a delivery that declared
bindings is refused, not withdrawn. A withdrawal is authored by the domain that
delivered the binding: a manifest delivered under another ownership domain is
never withdrawn by this publish.

**Held to the host declared now.** Every manifest the render declares is held,
at publish, to the environment's host block as it is then — the ownership
domain and the host coordinate — and a
render made before the block changed is refused by name; a render for an
environment that names no host any more is refused outright.

**Unchanged is delivered as it was, once it is checked.** A manifest whose
settled generation is the delivered one keeps the carrier it was delivered as
— the same bytes, the same signature — and the Job that posts it keeps its
name, so a no-op promotion gives a re-sync nothing to do. A carrier is reused
only once it parses, its signed bytes are exactly the manifest's canonical
bytes, and on a release publish it still passes the self-check a host will
run; one the release policy no longer admits is signed again only with
`--resign`. The plan signs and hands its exact carriers to the publish that
executes it; execution never produces a signature its plan did not contain.

**Rollback re-settles.** A rollback restores the workloads an earlier revision
delivered and settles their manifests anew against the base branch, never the
carriers that revision signed; a rollback across a withdrawal is refused as
terminal.

**What publish reads, it reads honestly.** The base branch must exist in the
publication checkout; an overlay absent from it is told from an unreadable
one; a carrier ConfigMap that lost its manifest is an error, not another
object. An unreadable history read as "nothing delivered" would restart the
generation at 1 and lose every withdrawal.

## Signing

Delivery uses platform-owned keys. Core's `solutionhost.Signer` is the custody
boundary and `SignEnvelope` creates a DSSE envelope over the manifest's
canonical bytes; two document kinds are signed with the same custody — the
module manifest and the [module envelope](#the-module-envelope), each under
its own DSSE payload type, so neither signature verifies as the other. No
third-party service participates in signing or verification.

Release custody is the platform's Vault Transit, configured only for a release
publish with `VAULT_ADDR`, `VAULT_TOKEN`, and `CODEFLY_RELEASE_TRANSIT_KEY`
(`CODEFLY_RELEASE_TRANSIT_MOUNT` defaults to `transit`). The address requires
HTTPS, with HTTP admitted only for loopback; the CLI performs no login and
sends the token only in `X-Vault-Token`. Only Ed25519 and ECDSA P-256 are
supported. The derived key ID is `<name>-v<version>`; a mismatch with
`host.release.key_id` refuses before signing and names both.

### Cloud KMS custody

A cloud provider's KMS may sit behind the same custody interface as an
optional adapter, never a requirement: a hosted cell whose key custody is the
cloud's KMS has no Vault to hold a Transit key. Google Cloud KMS is configured
only for a release publish with `CODEFLY_RELEASE_KMS_KEY`, the full
cryptoKeyVersion resource name
(`projects/<project>/locations/<location>/keyRings/<ring>/cryptoKeys/<name>/cryptoKeyVersions/<n>`).
The derived ID is `<name>-v<n>`, so `host.release.key_id` keeps one spelling
across custodies. The key version must sign with `EC_SIGN_P256_SHA256` (core
verifies ECDSA P-256 and Cloud KMS offers no Ed25519); any other algorithm is
refused by name. The CLI reads the public key once, checks the derived ID
against `host.release.key_id` before signing (the public key itself is pinned
by the host), and signs the SHA-256 digest of each DSSE message through
`asymmetricSign` with the ambient Google credential (Application Default
Credentials), which it never reads from a file of its own choosing.
`CODEFLY_RELEASE_KMS_ENDPOINT` selects a regional or private endpoint
(default `https://cloudkms.googleapis.com`); the host must be under
`googleapis.com` or loopback, HTTPS is required, HTTP admitted only for
loopback. A response without its CRC32C checksum is refused as a failed one. Exactly one release custody is configured at a time: Cloud
KMS is selected when `CODEFLY_RELEASE_KMS_KEY` is set and no Transit variable
is, and a publish that sets both is refused naming each, never one silently
winning.

Developer custody is one Ed25519 key per machine, created or loaded by
`codefly delivery key` (ID `dev-` plus 16 lowercase hex characters of the
SHA-256 of its public key; the OS keychain where available, otherwise a 0600
PEM under `~/.codefly/delivery-keys`). A local publish uses it only when
`host.developers` lists its ID. A native run writes the host's local profile
itself, admitting this machine's key for the host's domain, so a shared
composition never carries a per-machine key.

Fresh and reused carriers pass the same core verifier (`VerifyDelivered`, or
`VerifyDeliveredEnvelope` for an envelope, which admits a `release` key only) over
a pinned set holding the custody signer's public key, its kind (`release` or
`person`) and the environment's ownership domain. The carrier is core's
`Signed` (`{document, envelope}`), held beside the manifest in its ConfigMap
under core's `SignedFileName` (`module-manifest.signed.codefly.yaml`). An
unsigned qualification render says so, and renders no Job.

## The delivery Job

The ConfigMap (`module-manifest-<binding>`, labelled
`codefly.dev/document: module-manifest` and `codefly.dev/binding`, in
`module-manifests/overlays/<environment>/`) is the delivery-side desired state
Argo keeps true, surviving a host database restore. The host learns of a
generation when its delivery API is POSTed the carrier: one Job per module per
settled set, rendered at publish, run **in the module's namespace as the
`delivery` ServiceAccount the render creates there**, posting every carrier —
withdrawals included — to `POST /platform/_delivery/manifest`. That is the
host's one carrier rule: the credential's account is `delivery` and its
namespace is the namespace of the manifest's workloads (for a withdrawal, of
the binding's previously applied manifest). A Job is named
`deliver-manifest-<module>-<environment>-<digest of its carriers>` in the file
`deliver-manifest.yaml`. The host answers 200
`{"disposition": "accepted"|"replayed", "generation": N}`, 409 for a stale or
rewritten generation or a withdrawn binding, 401/403 for the carrier, 400 for
an invalid manifest. There is no platform authority namespace, no authority
AppProject and no second manifest Job.

**One carrier per namespace, labelled as the carrier.** A cell admits exactly
one delivery carrier per namespace, by the exact labels the cell file
declares. Every delivery Job's pod — whatever it delivers — and the `delivery`
account carry `codefly.dev/delivery: manifest`, and the cell file's carrier
selector is rendered from the same constant, so the two cannot drift. The Job
itself also carries `codefly.dev/delivered-document: manifest` (or
`envelope`), naming the document kind; nothing selects on it — that is where
the kind lives, which is why the carrier label keeps a value that reads like
one kind.

**The carrier label's value cannot be renamed on its own.** A cell turns each
namespace's carrier selector into one entry of a closed execution-admission
set, matched on the exact label set, and a render regenerates the cell file
whole — every namespace at once — while a Job moves only when its own module
is published. So a renamed value flips every namespace's selector in one
commit and leaves every not-yet-republished module's delivered pod matching no
approved entry: the pod is refused, that module's delivery sync fails, and the
refusal names neither the label nor the rename. Renaming it means
re-publishing every module in the environment in the change that pins the new
cell.

The overlay is part of the **immutable service snapshot**: every Application
the bootstrap stamps reads the one snapshot revision, the delivery Application
included, so the overlay is settled and signed before the snapshot is
committed.

The Job is an Argo CD **Sync hook** (`argocd.argoproj.io/hook: Sync`,
`hook-delete-policy: BeforeHookCreation`), not a tracked resource: a hook runs
on every sync from state that still exists, so a replay after a restore is an
ordinary sync, and a sync that changes nothing re-POSTs a generation the host
replays. Its token is projected for the host's audience and re-read per
request; it runs codefly's own base companion image
(`ghcr.io/codefly-dev/codefly`, pinned by digest — never a Docker Hub image),
read-only, as a non-root user with every capability dropped. The cell file
declares that same image as the namespace's delivery carrier, so moving its
digest changes every cell file: re-render the cell and publish it with, or
before, the first Job that runs the new image. The script spells its variables `$NAME`, never
`${NAME}` (the promotable ruleset refuses `${…}` as an unresolved
placeholder), and the token file is named `DELIVERY_IDENTITY_FILE` (a variable
named `*_TOKEN` carrying a value is a credential to that ruleset).

The Job dials `<service>.<namespace>.svc.cluster.local:<port>`, the Service
port the CLI allocates for the endpoint `host.delivery` names. Reachability is
a retry, not a sync wave: transport failures, 429, 5xx and a 404 from a host a
version behind are retried with backoff in **rounds** — every pending carrier
once per round, a pause that doubles up to `DELIVERY_MAX_PAUSE_SECONDS`, until
each is answered or the budget (`DELIVERY_BUDGET_SECONDS`, 1500 of the Job's
1800-second deadline) is spent, every request and sleep clamped to what is left
— and the set a Job carries is bounded at publish to `budget ÷ request`
carriers (50). A verdict (400, 401, 403, 409, 422) refuses that carrier; the Job
posts every other one and fails at the end. Where a parent syncs Applications
by wave, the delivery goes beside the module's consumers, after its bootstrap
units; the module that serves the delivery API itself (`hostsDelivery`)
follows its units. What is not verified: no parent application has run this
ordering against a real readiness model.

## The module envelope

The module envelope is what a host grants each module principal: one
document per host, replaced whole at each revision. The composition declares
it beside itself, in `configurations/<environment>/module-envelope.json`
(`{"principals": {"<principal>": <grant>}}`, each grant in the host's own
shape), read across the composition exactly as a native run reads it — every
composed workspace's file first, then the consuming workspace's own, which
decides a principal it states. The file is resolved through the
environment's own configuration profile and the profiles it derives from
(`derives-from` in `profile.codefly.yaml`), the most derived copy winning, the
way a single-file declaration such as `dns.codefly.yaml` is: so every cell
whose profile derives from one shared profile (`blue` → `hosted-development` →
`hosted`) is delivered the ceiling `configurations/hosted/module-envelope.json`
states, and a new cell needs no envelope file of its own. Only the
environment's own profile is resolved, never a later profile of its chain: a
workspace holding no directory for that profile states no envelope for it,
rather than lending the envelope it states for `local`.

`codefly deploy gitops publish` delivers it the way it delivers a manifest,
and only in the publish of the module that serves the delivery API
(`hostsDelivery`), to an environment that declares a host, outside local
qualification. A local host pins developer keys and admits an envelope only
from a release key, so a local publish delivers none; a native run keeps
writing it as the fixture's administrator (below).

**The revision is settled against the base branch.** Publish reads the
envelope the base branch delivers in the host module's overlay. The same
canonical content apart from the revision keeps the delivered revision and
its carrier, nothing signed again; changed content is the delivered revision
+ 1; none delivered is revision 1. The file may also state a top-level
`"minimum_revision": N` (an integer of at least 1, beside `principals`): the
delivered revision is never below it, so a floor above the settled revision
is the revision delivered — signed anew, even for unchanged content — and an
unchanged envelope already delivered at or above it keeps its revision and
carrier. Across a composition the consuming workspace's floor decides;
composed workspaces stating different floors that the consuming workspace
does not decide are refused by name, never resolved to either. A native run
ignores the floor. Core's `AdmitEnvelope` runs over the base
branch's record before the host runs it over its own.

**Prepared bindings are listed for the module that installs them (a
stopgap).** The host reads a binding's delegation mode only from the
envelope's entry for it, so a requested binding the envelope does not list is
never granted. A native run and a publish therefore add, to the grant of each
module that asks for authority and installs prepared bindings, an
`operation_audiences` entry per prepared key the grant does not already state:
the binding's audience, its kinds and actions **without resource ids**, and
`"delegation_mode": "module_actor"`, so a call through it names the installing
module as the actor. An entry the composition states for that key is kept as
stated, and a principal the composition does not grant gains none. This exists
only because the mode is read from the envelope; when the mode travels with
the manifest's binding, it goes away.

**Stating nothing is not revoking.** A composition that delivered an envelope
and no longer states one — no `module-envelope.json` in any workspace of the
composition for the environment — is refused. Revoking every grant is a
statement: `{"principals": {}}`, delivered at the next revision. An envelope
delivered by a module that no longer hosts the delivery API, or to an
environment that no longer names a host, is refused the same way.

A file that exists states nothing by itself: one written for
`minimum_revision` alone, or whose `principals` key is dropped, misspelled or
`null`, is refused too, naming the two spellings. Only an authored
`{"principals": {}}` revokes — otherwise a dropped key would be signed under
release custody as the revocation of every grant on the cell, and every module
principal would stop minting.

**A delivered grant names no organization.** A grant carrying a non-empty
`tenant` is refused at publish, naming the principals: tenancy resolves
through each organization's installation, and pinning a module to one
organization is an administrator's interactive write on the host. The host
refuses the same grant; refusing it at publish saves a failed Job.

**It is signed by the release custody.** The canonical bytes are signed by
the same signer as the manifests (Vault Transit or Cloud KMS, checked against
`host.release.key_id`), and the carrier is held, right after signing, to the
verifier the host runs: a signature that verifies, from a key pinned as a
`release` key for the environment's ownership domain. A developer key is a
`person` key and never signs an envelope. Carrier reuse, `--resign` and the
plan's carriers work exactly as for manifests; the inventory's `delivery`
record lists it as kind `envelope`, ID `module-envelope`, its revision as the
generation, and its digest.

**What publish writes**, in `module-manifests/overlays/<environment>/` of the
host module's delivered path, listed in the overlay's kustomization:

- `module-envelope.yaml`: the ConfigMap `module-envelope` (labelled
  `codefly.dev/document: module-envelope`), holding the canonical document
  under `module-envelope.json` and the signed carrier under core's
  `EnvelopeSignedFileName` (`module-envelope.signed.codefly.yaml`);
- `delivery-service-account.yaml`, the `delivery` account, when the module's
  manifests did not already write it;
- `deliver-envelope.yaml`: a delivery Job named
  `deliver-envelope-<module>-<environment>-<digest of its carrier>`, the same
  Sync hook as a manifest's, posting the carrier to
  `POST /platform/_delivery/envelope` as the `delivery` account of the host
  module's namespace. Its pod is the one carrier the cell file declares for
  that namespace; the Job's own `codefly.dev/delivered-document: envelope`
  names the kind (see [the delivery Job](#the-delivery-job)). The cell
  declares the carrier for the host module whenever the composition states an
  envelope, even when that module renders no manifest of its own.

The Job's verdicts are the manifest Job's: 401/403 for the carrier, 422 for an
envelope the host refuses (a `tenant`, a grant it cannot read), 409 for a
revision the host will not apply, and each fails the Job.

**A 409 after an administrator's write is reconciled by hand.** A host also
holds an envelope its administrator wrote interactively, at a revision of its
own, and answers 409 to a delivered revision that does not exceed it. Publish
settles revisions against the base branch only — it never reads the host — so
the remedy is a change to the composition: state the administrator's change
in `module-envelope.json`, so the delivered envelope says what the host holds,
and set `minimum_revision` above the administrator's revision, read from the
host. The next publish delivers at that floor. Nothing reconciles the two
automatically.

## Workspace configuration groups a render bakes in

A render records, per workspace configuration group its services and its
contract's slots consume, a digest of the group as the environment provided
it, and **reports** the sibling modules rendered for the same environment that
record another digest for a group they both consume. `deploy gitops render <module>`
then renders those siblings in the same run, one at a time and each once —
the readers of the changed group and no other module, with one line per
reader naming the group that brought it in — so the publish below finds every
reader agreeing; `render --all` renders every module as before. The rule is enforced at
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

`codefly run cluster` takes a composition onto the developer's k3d cluster
through the same delivery a cell receives — there is no local trust mode and
no second path:

```bash
codefly run cluster --env local --app-project <project>           # every composed module
codefly run cluster documents billing --env local --app-project <p>  # or the ones named
```

1. **The platform's prerequisites, locally.** Argo CD (installed when absent,
   v3.4.1 pinned by its manifest's SHA-256 and every image by digest), the
   modules' namespaces (every Application syncs with `CreateNamespace=false`),
   and the local host's trust anchor (below), before any module syncs.
2. **Each module rendered**, exactly as `deploy gitops render` renders it: its
   images built and pushed to the environment's `registry.url`, and its module
   manifest built by the one derivation a cell's render uses.
3. **Each module published and promoted**: settled against the local delivery
   repository, signed as the developer, every carrier checked as the local
   host will check it, then promoted. The plan is shown and confirmed unless
   `--yes`.
4. **The latest promotion served**: the fetch remote brought up on the base
   branch tip, the cluster's Argo CD made to trust it, and each published
   module's Argo bootstrap applied from the promoted revision.
5. **The deliveries awaited**: Argo CD syncs each module's delivery
   Application and runs its Job in the module's namespace as `delivery`,
   POSTing to `/platform/_delivery/manifest` with a projected token for the
   host's audience. The run reads **its own** attempt of each Job: it requests a
   sync carrying a nonce of its own from the Application the CLI rendered,
   held to its full identity, and awaits the single Job stamped with that
   nonce. It prints each manifest's answer and fails when any was refused.

The environment declares `cluster.kind: k3d` and `cluster.context`, a `host`
block, `host.developers`, `host.trust_anchor`, and the gitops `fetch-repo-url`.
The same steps stay available one by one: `deploy gitops render`,
`deploy gitops publish --local`, and `deploy gitops remote up`. Stopping a run
is not removal: withdrawing a binding is a published withdrawal.

**Module authority.** A service declaring `workspace-configuration-dependencies:
[module-authority]` is given its workload's SPIFFE ID (`PRINCIPAL`), the
host's `module_audience` (`AUDIENCE`) and `audience` (`PROJECTION_AUDIENCE`),
and a reference to the `internal-auth` secret `CODEFLY_INTERNAL_TOKEN`
(`INTERNAL_TOKEN`), projected per workload after the agent renders.

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
  composition;
- the endpoint a module's **derived operation** is called on admits every
  service that installs prepared runnable bindings — the services declaring
  the `runnable-bindings` workspace-configuration group. An executor such as
  the runtime calls owners it does not and must not declare; the composition
  prepares those calls (`codefly generate runnable-bindings`), so the
  composition is where the edge comes from. The owner endpoint is the one the
  prepared call targets: the published endpoint for a REST publication, the
  service's connect endpoint for a gRPC or Connect one. Without this source
  the owner's connect endpoint had no consumer at all, and every fan-out
  operation on a cell settled `OWNER_UNAVAILABLE` at the mesh
  (codefly-dev/cli#1072).

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

The hosted counterpart of the same rule: a reviewed publish continues its
promotion branch only while that branch **contains the base**. A branch the
base does not contain — its pull request was squash-merged and the branch
kept, or another promotion moved the base past it — is stale, and a commit
stacked on it would fork from the old base and conflict add/add on every file
the base already holds. So the publish starts again from the base and replaces
the branch with a push leased on the stale tip it reviewed
(`--force-with-lease`), recorded in the plan as `replacesBranchRevision` and
part of its ID: a branch that moved after review refuses the publish. A stale
branch carrying anything outside the module's own path is someone else's work
and is refused by name, never overwritten.

**How a local pod's identity reaches the host.** Not through anything the run
projects: there is no `MODULE_IDENTITY_PREFIX`, no secret and no
`MODULE_PRINCIPALS` line, and nothing self-registers (cli#855 removed that on
purpose). A module's pod runs as its own ServiceAccount, and its manifest names
that account's SPIFFE ID. The pod presents its projected ServiceAccount token,
minted for the host's audience; the host reviews it (TokenReview, and nothing
else: no pod read, no image compare) and finds the one workload of one
manifest running as that (namespace, account). A module that still requires a projected
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
The same workloads, and no other, carry `automountServiceAccountToken: true`:
the verifier is the one workload with a Kubernetes API credential, for the
TokenReview that authorises a delivered carrier, read with the cluster CA at
the standard paths the ServiceAccount admission plugin injects them at — the
`kube-api-access-…` projection the cell's admission tolerates and its
`verifier_rbac` already expects — while every other workload keeps the
agents' `false` (the k3d render that mounts a local trust anchor sets it the
same way, since the host reads the same paths there).

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
create TokenReviews; and the envelope record holding a grant for each module
principal the local composition runs.

**The cost, and the alternative the host would accept.** Requiring a local
cluster changes the laptop story from a container runtime to a container
runtime plus a cluster, on every developer's machine. The host names the
alternative it would accept instead — not a second trust model but **no
capability surface locally**: manifests and routing so a developer sees their
UI, and no module credential minted at all. Which of the two holds is the
owner's call; this document records the first, as agreed, until the owner
decides otherwise.


## A native run: the run answers as the cluster

A run of processes is the first loop a developer has, so a **native**
`codefly run` of a composition that declares a host stands in for what a cell
gives a workload, and the host runs its one code path. For the lifetime of the
run, the CLI is:

- **the workload identity issuer** — core's `workloadidentity/native`: a key
  made for the run (never written), whose anchor the host reads from
  `CODEFLY_NATIVE_IDENTITY_ANCHOR`. Every `module-identity` workload is issued
  its credential **when it is projected**, before it starts — for the host's
  audience, naming the workload's namespace, account and declared container
  (the service), and nothing else: no build. It is written under
  `.codefly/run/<environment>/identities/` and handed to the
  process as `CODEFLY__WORKLOAD_TOKEN_FILE` (a pod reads the mounted path),
  reissued when at most 120 seconds of its 600-second lifetime remain. The
  run polls the actual wall clock every five seconds, including time spent
  asleep, so expired projections are renewed on the first poll after wake.
  A failed refresh logs the workload name and reason, leaves that projection
  due for retry on the next poll, and does not prevent other renewals.
  Nothing is withheld: until the host has applied the module's manifest a
  mint answers "not established yet", which the workload retries.
- **not the mesh** — every hop is plain HTTP on loopback, and only the host's
  gateway may be reachable off it.
- **the delivery pipeline** — once an identity workload reaches Running, and
  once the host is ready to take it (below), the run builds its module's manifest (the same `moduleInstanceOf` and
  `ModuleManifestOf` a cell's render uses), listing **every** identity
  workload of the module that runs now, with the addresses this run allocated
  for its routed endpoints, signs it with the developer key and carries it,
  with a native credential for account `delivery` in the module's namespace,
  to `ApplyManifest` of the `codefly.dev/composition` interface the
  `host.delivery` endpoint declares in its module's interface endpoints. A
  host whose delivery endpoint declares no such interface — the saas host on
  main — receives it through the legacy HTTP route instead, POSTed to
  `/platform/_delivery/manifest`, with the envelope written through its
  gateway (below); the run logs that reason on every delivery that takes it.
  A host whose module does not load receives nothing, with the reason; the
  run never guesses a transport. The legacy route is deleted once no
  composition delivers to a host without the interface. The run opens one
  connection to a composition endpoint — at the address its recorded instance answers,
  with TLS when the endpoint declares itself `secured`, exactly as readiness
  dials it — reuses it for every delivery, retry and withdrawal, dials again
  only when the host answers at another address, and closes it when the run
  stops. When the set of running identity workloads changes, the manifest is
  delivered again; its generation is settled from its content, which includes
  the captured method policies, scope ceilings and solution roles, including
  whether a policy document is present. The same captured declaration is
  signed and sent: a policy-only change advances the generation once.
  The run reserves that content and generation under
  `.codefly/run/<environment>/delivery/generations.json` before sending,
  using a cancellable shared file lock and Core's atomic writer. Neither signing
  nor network I/O holds the lock. A lost response or refusal keeps the reservation:
  after restart unchanged content replays it; changed content advances it.
  Entering paired settlement from an old live Core-only record advances once,
  because the old record cannot establish policy equality. Withdrawals keep their
  Core-only identity and read no policy source, so a withdrawal replays its
  exact generation. The record never refuses a delivery: a module re-added after
  its withdrawal is the next generation, and whether the withdrawal is terminal
  is the host's to say. Core's admission keeps it terminal for as long as the
  host holds it; the local authority holds applied deliveries in memory only, so
  its refusal says to stop the run and start it again, after which the host
  accepts the module at that generation. A withdrawal the host refuses because
  it already holds one for the binding is complete, over the composition
  interface (a `FailedPrecondition`) and over the legacy HTTP route (the saas
  host's 409) alike. Neither carries a typed signal for it yet, so the refusal
  is recognized by core's `ErrWithdrawn` wording, a stopgap until
  codefly-dev/authority#2. Deleting the record is not
  a recovery procedure; it loses the evidence needed to avoid rewriting a
  generation. These rules do not establish policy-file watch/reload behavior or
  power-loss durability beyond Core's atomic writer.

- **the administrator** — over the legacy HTTP delivery, before delivering, it
  makes sure the host holds the
  composition's envelope: `{"revision": N, "principals": {"<module>":
  {"grant": <grant>}}}`, each grant read from the `principals` of
  `configurations/<profile>/module-envelope.json`, resolved through the
  profile's derivation as above (already in the host's
  grant shape) — every composed workspace's file first, then the consuming
  workspace's own, which decides a principal it states: a platform base
  states its modules' grants, and a solution composed on it states only its
  own principal's. Two composed workspaces that grant one principal
  differently, which the consuming workspace does not decide, are refused by
  name. It is written through `PlatformAdminService/PutModuleEnvelope` as
  the fixture's `super_admin`, at the next revision and only when it changed.
  A composition stating no grant writes none.

**Authority admission precedes local Start.** The native launcher adds runtime
prerequisites through Core's dependency graph, retaining the delivery host when
module closure pruning would otherwise remove it. This ordering grants no
endpoint access and changes no build dependency. A composition-interface host
requires its delivery service; legacy delivery also requires its gateway.

Each attempt registers cleanup before stopping the prior process or preparing
its request. Immediately before an identity workload's Start RPC, the launcher
applies declarations
for the selected local runtime services and their binding targets. Remote
services, excluded origins, dormant pinned services and no-start test modes do
not acquire local workload membership. A binding targeting a module without a
selected declaration is refused by name before any declaration is sent. The
host's own module is not delivered through this path, so a binding targeting it
is also refused; host process readiness alone does not prove audience registration. Run, test service and test source use the
same launcher and completion boundary; a test that starts no services does not
load a native launcher.

Declared membership is distinct from readiness. The pre-start manifest lists
selected identity workloads but advertises only endpoints of successful starts.
Ready services are filtered before address resolution. Cold authority admission
can defer a frontend bundle; when this launcher already published a page, it
supplies that payload during unrelated partial startup rather than removing it.
Normal frontend publication requires the complete built payload.

Each attempt waits for host readiness and Applied, within **10 minutes**. Over
the composition interface, `Unavailable` (no answer at all among them),
`DeadlineExceeded` and `ResourceExhausted` use the existing bounded backoff;
every other code — `Unimplemented` (an endpoint that serves no composition
interface), `Internal`, `InvalidArgument`, `PermissionDenied`,
`FailedPrecondition` (a stale or rewritten generation, a held withdrawal) and
any code the run does not classify — fails at once with its reason. Over the
legacy HTTP delivery, a 5xx, a transport outage, a gateway-route 404 or an
envelope revision conflict uses the backoff; policy refusals and wrong delivery
endpoints fail at once. The deadline also bounds network calls and waiting for the delivery lock.
Stopping a run or superseding an attempt cancels its active delivery. A lost
response does not establish whether a host applied the manifest.

A request-preparation error, Start failure, missing response or cancellation stops refreshing that
attempt's projected credential and leaves its selected declaration unready.
It does **not** send a terminal withdrawal: that would prohibit a normal retry
with the same binding. Successful retry reprojects through the existing native
identity owner. Init receives the resolved credential path without issuing a credential; attempt
setup owns issuance and refresh membership. Attempt identity fences completion so an old failure cannot
forget the new projection and an old success cannot advertise stale endpoints.
Full flow teardown stops refresh even after partial manager initialization.
This is readiness cleanup and bounded credential expiry, not admission rollback
or immediate revocation. A copied native credential retains its existing lifetime
(up to 600 seconds after issuance); already-issued Work Contexts have their own
expiry and revocation rules. Process teardown remains required for partial starts.

The host is pointed at the stand-in by `CODEFLY_NATIVE_IDENTITY_ANCHOR` and
`SOLUTION_HOST_LOCAL_TRUST_ANCHOR`, both under
`.codefly/run/<environment>/host/`; it refuses both in a pod. The
delivery host turns its surface on from the `federation` keys
`SOLUTION_HOST_COORDINATE`, `SOLUTION_HOST_OWNERSHIP_DOMAINS` and
`SOLUTION_HOST_TRUST_POLICY`, which a native run derives from the host block.
The delivery service needs no authored `federation` file when these are its only
values. Authored registration secrets remain supported and pass the usual secret
and endpoint checks; ambiguous groups still fail. The generated fields reach only
the declaring delivery host. Other services, container runs and deployments retain
their authored requirements. `doctor workspace` reports this native-only supply as
a warning because it does not select the runtime.

**Growing and shrinking the run.** Every `codefly run` admits the components a
reloaded composition adds when it receives `SIGUSR1`: their identities are
planned by the rule the run started with (a known component may neither change
nor disappear), and they use the same pre-start admission and completion path. A root
the reloaded composition no longer names is stopped, and each stopped identity
workload is taken out of its module's manifest: a module that still has a
selected identity workload is delivered again listing what remains, and one
with none is **withdrawn** — its manifest at the next generation with
`removed: true`, which the host keeps terminal for the binding while it holds
it — and the run waits, up to a minute, until the applied record
`ApplyManifest` acknowledged (over the legacy HTTP delivery, the host's
read-only `GET /platform/_delivery/applied/<binding>`, read as the
endpoint-publisher) holds that exact withdrawal. A withdrawal not yet applied stays pending and is
redelivered unchanged (the host replays it) on the next signal.

## Dev deployments

`codefly deploy dev` re-pins a service's image inside an already-rendered
tree, and re-derives the environment's cell file when it declares a host. A
module manifest names no build, so a re-pin leaves it true and a delivering
module is re-pinned like any other.

## What is not verified

No cell has applied these manifests yet: the shape is the wire contract agreed
with the host's port, but agreement is not a running system. Vault custody is
exercised with a loopback HTTP server; cryptographic verification uses core's
real verifier. The packaged solution path (`RenderSolution`) runs through an
in-process executor.

No cell has received a delivered module envelope yet; the envelope route is the
host's parallel change, verified against core's verifier only. A native run
delivering through the composition interface writes no envelope, the manifest
being the ceiling; over the legacy HTTP delivery it still writes the envelope
as the fixture's administrator.

Named and not built: a **withdrawal path for a whole module** — a module
removed from the composition has no render and so no publish, and its manifest
stays applied on the host until a publish of that module's path with no
instance writes its withdrawal.

## Permission preflight

Before native CLI/control-plane runs construct their dependency graph, a preflight
reads policies from the selected module closure, checks declaration validity and
resolves authority bindings through the existing contract resolver. Local runs
log findings as warnings during adoption; fixture roles are diagnosed again by
the development authority when their modules are delivered. Refreshing a running
composition rechecks declarations even if no new root was added.

Render, direct deploy, development image deployment and source-module publication
reject invalid declarations before their build/apply effects. Hop warning mode
cannot bypass these checks. Unknown or contradictory method requirements are
invalid; an intentionally restricted business role is valid. Identical policies
published on multiple endpoints are accepted; conflicting policies for the same
method are refused rather than silently taking the first.

**A method that declares no policy is refused only where policies are
consumed.** The structural checks above hold everywhere: both `requires` and
`any_checked_caller` on one method, a requirement with no action, a kind or
action outside the vocabulary, colliding identifiers. A published method that
declares nothing at all is refused, by name and with the reason, in two cases:

- the environment's host (the service `host.delivery` names) declares that it
  implements `codefly.dev/composition`, so every method's policy travels with
  the manifest to the host's authority; or
- the module's `module.contract.codefly.yaml` sets
  `require_method_policies: true`, which holds it to a policy on every method
  wherever it is delivered.

The host's own module — the module of the `host.delivery` service, `saas` for
`delivery: saas/accounts/connect` whatever `host.component` says (`saas` on a
local environment, `platform-obin` on a cell) — is not behind the hop: its
methods are the host's, never fronted by the policies the host receives, so the
first case does not hold it. Only that one module is set aside; every other module in the
composition is still refused by name, and the host module's own
`require_method_policies: true` still holds it.

Toward any other host — the saas host every released composition delivers to —
an unmarked method passes, listed with neither field set exactly as
`MethodPolicies` reads it, and the hop in front of the module refuses it per
call. `codefly generate permissions` always requires a declaration: a table
names every method.

*Migrating a module to the hop vocabulary.* Mark each RPC with
`option (authority.hop.v1.policy)` (`requires` or `any_checked_caller: true`)
and each OpenAPI operation with `x-codefly-authority-policy`, then run
`codefly generate contracts --permissions`. Until every method is marked the
module still renders toward a host that does not consume policies; before it is
composed under a host implementing `codefly.dev/composition`, or once it sets
`require_method_policies: true`, every unmarked method is refused by name.

The checker validates published bytes and their package digests. For each
published protobuf endpoint resolved from local source (including service
overrides and materialized git clones), it also compares the service's `proto/`
tree with the snapshot recorded by `generate contracts`. Edits, added/deleted
files, and missing source or snapshots warn during local adoption and refuse
strict delivery. Module/service render checks the selected dependency closure
before effects, including producers reached through declared configuration groups
using the invocation-aware configuration reader; whole-composition render checks all modules before its first
build. Development image deployment validates the service selected by `--path`
even when replacing a packaged service. The existing ban on invocation configuration
overrides in delivery still applies. The diagnostic names the module, service, endpoint and changed
files, and directs the author to `codefly generate contracts <module>`.
Even comment-only changes require re-export before strict delivery.

Verified materialized packages retain artifact checks without requiring source;
the materializer's receipts distinguish them from checkouts. Unresolved or stale
materialization is reported, not silently treated as an artifact exemption.

This early guard covers existing published protobuf exports, not a newly added
endpoint with no export, OpenAPI source changes, external imports, descriptor
derivation or generated clients/permission constants. Modules with no package
manifest have no recorded source snapshot to compare. Run
`codefly generate contracts --permissions --check` in CI for full contract
regeneration and permission-output freshness. Static checks cannot prove resource visibility, active
installation, consent, provider availability or a successful runtime call.
Packaged solution publications without a source module and rollback of old
rendered artifacts still need artifact-level permission qualification; source
preflight alone does not validate those paths.

A dependency-only native run may declare its excluded root's workload identity
when that root opts into `module-identity`. The dependency client drives that
process outside the launcher. Admission still uses the signed manifest and must
complete before the SDK environment is ready; it does not mark the root started
or publish any of its endpoints. Credential projection and refresh belong to the
run, including normal teardown. See [orchestration](orchestration.md#identity-for-dependency-clients).
