# Orchestration Engine

The orchestration engine is the core of the codefly CLI. It coordinates multi-service lifecycles by building a dependency DAG and executing actions through a policy-driven playbook.

## Architecture Overview

```
                    ┌──────────────────────────────────────────────────────┐
                    │                      Flow                           │
                    │                                                      │
                    │  ┌─────────┐   ┌──────────┐   ┌──────────────────┐  │
                    │  │  World  │   │ Playbook │   │  StateManager    │  │
                    │  │         │   │          │   │                  │  │
                    │  │ - Env   │   │ - Policy │   │ - Endpoints      │  │
                    │  │ - Mode  │   │ - DAG    │   │ - NetworkMappings│  │
                    │  │ - Deps  │   │ - Actions│   │ - Configurations │  │
                    │  └─────────┘   └──────────┘   └──────────────────┘  │
                    │                     │                                │
                    │         ┌───────────┴──────────┐                    │
                    │         │         Hub           │                    │
                    │         │  ┌───────┐ ┌───────┐ │                    │
                    │         │  │Manager│ │Manager│ │                    │
                    │         │  │(svc A)│ │(svc B)│ │                    │
                    │         │  └───┬───┘ └───┬───┘ │                    │
                    │         └─────┼──────────┼─────┘                    │
                    └───────────────┼──────────┼──────────────────────────┘
                                    │          │
                              ┌─────┴──┐  ┌───┴────┐
                              │ Runner │  │ Builder│
                              │        │  │        │
                              │ Agent  │  │ Agent  │
                              │ (gRPC) │  │ (gRPC) │
                              └────────┘  └────────┘
```

## Flow

The `Flow` is the top-level coordinator. It holds the workspace context, the service dependency graph, and orchestrates everything.

**Creation:**

```go
flow, err := orchestration.NewFlow(ctx, workspace, module, service, env, mode, opts...)
```

**Modes:**

| Mode | Description | Terminal Action |
|------|-------------|-----------------|
| `RunMode` | Start services locally | `RuntimeStart` |
| `TestMode` | Start services, then run tests | `RuntimeTest` |
| `BuildMode` | Build container images | `BuilderBuild` |
| `SyncMode` | Sync service configs | `BuilderSync` |
| `DeployMode` | Deploy to environment | `BuilderDeploy` |

**Lifecycle:**

1. `NewFlow()` -- creates the flow with dependency graph, network manager, configuration manager
2. `InitManagers()` -- creates a Manager (Runner + Builder) for each service in dependency order
3. `Load()` -- loads configurations, creates the Playbook with the appropriate Policy
4. `Start()` / `Test()` / `Build()` / `Deploy()` -- executes the playbook
5. `Stop()` / `Shutdown()` -- graceful teardown, in reverse topological layers (see [Teardown](#teardown))

**Key options:**

```go
flow.WithStandAlone(true)           // Don't start dependencies
flow.WithExcludeRoot(true)          // Start dependencies only, skip target
flow.WithCoRoots("wiki/backend")    // Start more roots against this same graph
flow.WithRuntimeContext("nix")      // Set runtime context
flow.WithFixture("seed")            // Use named test fixture
flow.WithRemotes([]*Remote{...})    // Use remote services for some deps
flow.WithOutputEnv(".env")          // Export the origin service's runtime environment
flow.WithOutputEnvService("api/web") // Or select one dependency explicitly
flow.WithLoadOnly(true)             // Stop after Load phase
flow.WithInitOnly(true)             // Stop after Init phase
```

### Roots

A flow starts from its **origin** — the service it is named, reported and
attributed failures by. `WithCoRoots` adds further roots to that same flow, which
is what lets a composition run several solutions against one host graph instead
of one `codefly run` per solution.

The run's service set is the union of every root's dependency closure plus the
co-roots themselves, so a service two roots share is started once. Three
consequences are worth knowing when changing this code:

- The playbook is seeded once per root, in one action group. Shared dependencies
  are de-duplicated by the action manager, not by the seeding.
- `Playbook.Begin` narrows the policy's graph to the root's closure only when
  there is exactly one root. With several, narrowing to any one of them would
  make the others unresolvable, so `InitManagers` scopes `world.Dependencies` to
  the run's own service set up front instead. That scoping is what keeps
  propagation to a service's dependents from naming a service the flow has no
  manager for.
- Barriers that stop the playbook (`--load-only`, `--init-only`) wait for *every*
  root to reach them; stopping at the first would leave the others unstarted.

`--stand-alone` and `--exclude-root` carve the origin out of its own graph, so
the run command rejects them alongside more than one root.

### Module closure

`codefly run` passes `WithRunModuleClosure(<module per root>)`, which derives the
modules of the run from its roots and the dependencies each service declares,
resolved against the workspace's module list as a **pin set** — where a module
named X comes from, not which modules take part. A module nothing reaches is
absent from the graph, and a declaration reaching a module the workspace does not
pin is refused up front, naming the service that asked, instead of coming up with
no endpoints.

The closure is also where a run's endpoint visibility is judged, through the same
core implementation `Workspace.ValidateServiceDependencies` uses, so a graph that
runs is a graph that validates. The verdict is scoped to the closure: a violation
no root reaches belongs to the runs that do reach it, and to the workspace-wide
pass.

Only the graph is narrowed. Workspace configurations and run profiles are
declared by the composition, so they stay resolved against all of it. Every other
entry point (build, test, deploy, sync) names no seeds and keeps the whole pin
set.

## World

The `World` holds shared state that all managers can access:

- `Env` -- target environment (local, staging, production)
- `Mode` -- current execution mode
- `Workspace` -- the workspace resource
- `Dependencies` -- the service dependency DAG (`architecture.ServiceDependencies`)
- `SharedState` -- the `StateManager` for cross-service state
- `LocalNetworkManager` -- assigns ports and network mappings for local runs
- `RemoteNetworkManager` -- manages port-forwarding for remote services
- `ConfigurationManager` -- loads and distributes service configurations

### Workspace configuration groups

Every path that delivers workspace configuration groups to a service resolves
them in one place, `pkg/orchestration/workspace_configurations.go`, by one rule:

    the groups a service receives = the groups it declares
        (`workspace-configuration-dependencies`)
      ∪ the groups the composition root provides run-wide
      − the groups the selected run profile excludes

The composition root's own `configurations/<profile>/*` (and any
invocation-scoped override) reach **every** service of the composition, so a
composed module's service reads a root-provided value without redeclaring it. A
group a composed module ships reaches only the services that declare it.

**The rendered deployment is the source of truth for that set, and the local run
resolves the identical one.** For a run of the whole composition, `codefly run`
and `codefly deploy gitops render` hand a given service the same group names, and
the same keys inside them *except where a rule below removes a key from both* —
a credential the service does not declare is withheld in each path, not in one.
Where a key reaches both, only the address family differs: a value naming
`${endpoint:<module>/<service>/<endpoint>}` resolves to the producer's loopback
address for a local run and to its in-cluster address in a render.

The claim is about the two paths agreeing, not about every key arriving. An
earlier wording said "the same keys inside them" without that qualification,
which the credential rule and the three asymmetries both contradict.

Three named asymmetries remain, below, and they are the reason that sentence says
*a run of the whole composition*. **Every one of them leaves the run with fewer
values than the render, never the reverse** — the direction that cannot produce
"works locally, dials something unconfigured once deployed". A deployed workload
never loses a value because of how someone ran things locally.

A render that resolved fewer groups than a run would make "works locally, dials
something unconfigured once deployed" a property of the pipeline rather than a
mistake anyone made — the root supplies a value every service can read, the run
delivers it, and the deployed service starts without it, naming a key it never
saw or, worse, not noticing. A run resolving fewer would hide the mirror-image
fault equally well. `TestRenderAndRunDeliverTheSameWorkspaceConfigurationGroups`
renders a fixture composition both ways and diffs the per-service group set *and
the keys of each group*; both diffs must be empty.

#### The set is selected before anything is resolved

The whole effective set drives producer discovery, and that ordering is the
correctness condition rather than a tidiness one.

Core resolves the root's run-wide groups **leniently**: a value whose
`${endpoint:…}` a given consumer cannot resolve is dropped for that consumer
rather than failing it, and an information block whose every value was dropped
goes with them (core #393 — the leniency exists because a run-wide group reaches
leaf services that never declared the producer). So a resolution that discovered
producers from the declared groups alone deleted a root group's endpoint values
from every service that did not declare the group, and a root group holding
nothing but references disappeared whole, with no error anywhere. A group holding
one literal beside its references keeps its *name*, which is why a parity
assertion over group names cannot see the loss.

Two things prevent it, and both are needed:

- `effectiveWorkspaceConfigurationGroups` selects declared ∪ root − excluded
  **before** `referencedProducerMappings` runs, so a reference carried by a group
  the service never declared still gets its producer's addresses bound.
- `refuseDroppedWorkspaceConfigurationValues` runs **after** resolution and
  compares what was asked for with what arrived: every value whose loaded form
  carried a reference must still be there. It reads the outcome rather than
  predicting it, and that distinction is load-bearing — an earlier revision
  predicted, asking whether the bound mappings held an endpoint of that identity
  and treating yes as resolution, so a mapping with no instance for the
  consumer's network access, an instance with an empty address, and a reference
  missing its endpoint component all read as resolved while core dropped the
  value and returned no error. Core's interpolation is the only thing that knows
  whether it succeeded.

  What a missing value means depends on the path, and in a render it does **not**
  depend on run membership:

  - A **render** refuses whenever the producer is a service of the workspace. A
    deployed address is a pure function of identity and namespace, so it exists
    for any service the workspace has — including one this render's flow does not
    cover, which is the normal case: a render flow covers one root service's
    build closure (`Flow.managerDependencies` → `Dependencies.Restrict`), and a
    root group's reference adds no edge to it. Judging a render by run membership
    exempted exactly the #882 case. There is no excused drop left here: a
    producer the workspace does not have was the last one, and it is refused
    before the outcome is examined at all.
  - A **run** drops, with a WARN. A local address exists only for a service of
    this run, so a producer outside it could never resolve here, and one inside
    it may simply have no address yet.

#### A composition-root group is not a way around an export boundary

Widening the set that resolves widens what must be *checked*, and the check did
not follow at first. Core's plan-time check
(`configurations.CheckEndpointReferences`) validates every reference a consumer
receives — it is well formed, the producer is a service of the workspace, that
producer declares the endpoint named, and the endpoint is visible to the
consumer's module — but it iterates the consumer's
`workspace-configuration-dependencies`, **the declared groups only**. Its own
comment gives the reason the visibility rule is there at all: without it,
*declaring the group instead of the dependency would be the way around
visibility*.

While the render resolved declared groups only, a service that did not declare a
root group never had the producer's mappings bound, so the boundary held by
accident. Once every root-referenced producer is bound for every service, two
things would otherwise follow:

- a root group carrying `${endpoint:platform/authority/admin}` where that
  endpoint's visibility is `private` (or `internal` without the consumer's module
  in `allow-modules`) would hand its address to **every** service of the
  composition, with nothing refusing it;
- a typo'd producer in a root group — the one kind of group no service declares,
  so the plan check never saw it — would be dropped from **every** service in
  silence, which is the cli#882 fault itself.

Both are checked now by handing core's own check the **effective** set in place
of the consumer's declared one — core's rule reaching core's verdict over the
wider set. Where each is *refused* differs, deliberately:

- **A visibility violation is refused everywhere**: at the plan gate
  (`CheckConfigurationReferences`, before anything is built or started) and
  again when the value resolves
  (`checkEffectiveWorkspaceConfigurationReferences`).
- **A producer the workspace does not have is refused in both places**, by
  core's own verdict. It used to be refused at the gate and merely dropped by
  the resolution — the division this package has for declared groups — until a
  dynamic review showed that to be a fail-open rather than a division: a typo
  supplied through an invocation-scoped override reached the resolution while the
  gate was still reading the configurations off disk, so nothing refused it
  anywhere and the value was quietly absent. A guard that is only correct while a second guard is
  also correct is not a guard.

The gate reads the configurations **the run will actually resolve**, through
`WorkspaceConfigurationsForChecking`, which loads a configuration reader rather
than reading the directory. That is the only way to see an invocation-scoped
override from outside core, because core applies them inside the loader
(`applyWorkspaceConfigurationOverrides`), and it matters twice over: a
`${endpoint:…}` an override supplies is checked like any other, and a group an
override makes composition-root — the run itself is supplying the value, so it
reaches every service — is recognised as one.

**A render or a deploy refuses to start while an invocation-scoped override is
set.** The loader applies it before anything resolves, so its values would be
baked into the committed manifests, and an override makes its group
composition-root even on a name a composed module provides — so it reaches every
rendered service, not only the ones that declared the module's group. The carrier
belongs to integration harnesses running against a throwaway workspace, so
`NewFlow` refuses it in snapshot and deploy modes and a local run is untouched
(`refuseInvocationOverridesInARender`). Asking an operator not to set it was
advice, not a guard.

**An invocation-scoped workspace configuration override is not `--set`.** It is
carried in `CODEFLY__WORKSPACE_CONFIGURATION_OVERRIDES`, core's private
SDK-to-CLI carrier, which an integration harness sets; the CLI itself sets it
nowhere and has no flag for it. `codefly run --set <service>:KEY=VAL` is a
different mechanism — a per-service runtime environment override, documented in
`docs/commands.md` — and a `${endpoint:…}` written in one of those is neither
checked nor interpolated. An earlier revision of these docs named `--set` here;
that was wrong, and an operator reading it would have believed a reference they
typed on the command line was being checked.

**One snapshot orders the run, checks it, and resolves it.** `NewFlow` takes that
same invocation-aware read once and uses it for the dependency graph's ordering
edges as well, because two reads were a fault: the graph used to be ordered from
the directory while the gate validated the overridden values, so an override
that redirects a reference to a *different* producer left the run's closure
holding the producer named on disk. The gate then accepted the new reference —
nothing is wrong with it — and the resolution dropped the value, because core
drops a reference to a producer the run does not contain rather than failing the
consumer. Delivered false, error nil: cli#882's own shape, reached through the
mechanism added to check for it. A snapshot that cannot be taken is not fatal in
`NewFlow` itself — every mode that resolves a configuration passes the gate,
which fails closed, and the modes that skip the gate (build, sync) resolve none.

Loading a reader is also what fixes the lifecycle: this gate runs inside
`InitManagers`, and the loader the flow registers only populates its
composition-root names in `Load`, which runs afterwards. Reading that loader
here returned nothing, so the gate quietly checked declared groups only. The
same call is the single place the composition-root names are derived, so there
is no CLI-side "not `ComposedBy` ⇒ root" rule any more: it asks core's loader.

Root groups are judged against the **whole workspace**, not the plan's graph. A
root group reaches every service of every run, including a module-closure run
whose graph is a slice of the workspace, and judging it against that slice would
report a real producer as "not a service of this workspace" and refuse the run.
Whether a producer is in *this* run is not a plan-time question.

##### What this refuses that used to be tolerated

This is a **behaviour change for existing compositions**, and it can refuse a
render or a run that worked before. Two shapes:

- An endpoint with **no `visibility:` at all defaults to `private`** (core's
  `Endpoint` post-load). So a composition-root group referencing such an
  endpoint used to have that one value quietly dropped for every service outside
  the producer's module, and now fails them by name — in `codefly run`, in a
  render, in `codefly doctor` and in CI. The value was never reaching those
  services; what changes is that they say so instead of starting without it,
  which is the whole point of #882. The fix in the composition is to declare the
  visibility the reference needs (`public`, or `internal` with the consuming
  module in `allow-modules`), or to stop referencing a private endpoint from a
  group every service receives.
- Core's deprecated `visibility: module` **permits** rather than refuses — it is
  an alias for `internal` with every module allowed
  (`resources.Endpoint.AllowsModule`) — so a reference to one of those is
  unaffected.

A root group's reference to a producer the workspace does not have is also
refused at the plan gate now, where before it was dropped for every service in
silence.

##### Checking a reference and resolving it are two different selections

Handing core's check the effective set closes the group-as-a-way-around-visibility
hole. It does **not** make the check and the resolution agree about *which*
endpoint a reference names, and an earlier revision of this document claimed it
did:

- `configurations.checkEndpointReference` walks the producer's **manifest
  endpoints** and returns on the first one the reference matches.
- `resources.resolveEndpointReference` walks the **bound mappings** and returns
  the first one that matches *and* has an instance for the consumer's network
  access, falling through to the next match otherwise.

A reference may name an API rather than an endpoint
(`${endpoint:platform/authority/rest}` where the producer declares two `rest`
endpoints), in which case it matches both. The check then passes on whichever
comes first in the manifest, and the resolution can pick the other one — because
the agent published the mappings in a different order, or because the endpoint
the check approved has no instance for this consumer's access (an agent may drop
a view it cannot serve). With a public and a private endpoint sharing an API,
that hands a private endpoint's address to a consumer whose module may not reach
it, with no error anywhere.

So the bound set is filtered as well: `World.exportableTo` keeps only the
mappings whose endpoint passes `resources.ValidateEndpointVisibility` for the
consumer's module — core's rule again, applied to what the resolution walks
rather than to what the check reads. A private sibling is then not in the set to
fall through to. It narrows only, and never within a module:
`ValidateEndpointVisibility` returns nil when the consumer and the producer share
one, so a service reading its own module's endpoints is untouched.

Two further rules make the choice disappear rather than constrain it:

- **A reference more than one permitted endpoint satisfies is refused by name.**
  Only the matches the consumer may *reach* are counted, so the common case still
  works — with a public `api` and a private `admin` both on api `rest`, a
  cross-module consumer has exactly one legal endpoint and the reference is
  unambiguous for it, while a consumer in the producer's own module is told to
  name the one it means. **An endpoint named exactly as the reference wins**, so
  a producer declaring `grpc` (api grpc) and `admin` (api grpc) does not
  make `${endpoint:…/grpc}` unresolvable — core's matcher is
  `Name == token || API == token`, and naming the api explicitly does not narrow
  the name branch. That exemption comes with **precedence**, not only tolerance:
  the mappings bound for a reference that names its endpoint exactly exclude the
  siblings that merely share its API, so the value cannot silently address one of
  them — by ordering, or because the named endpoint has no instance for this
  consumer's access and core's interpolation would otherwise fall through.
  **Two references into one producer** are handled by ORDER rather than removal,
  because each one's exact answer is the other's wrong answer and neither can be
  dropped: the list is arranged so every reference meets the endpoint it names
  first. Two cases no list can serve are **refused** instead of guessed — a
  reference whose named endpoint has no address for this consumer's access while
  a sibling it also matches does (core falls through rather than stopping), and
  references whose orders contradict each other, which endpoints
  (name `grpc`, api `rest`) and (name `rest`, api `grpc`) produce when both are
  referenced by name.
  Refused references arrive as core's own structured findings, so they carry a
  consumer, a group, a key and a reason rather than one opaque message. They are
  reported **after** the other reference faults rather than merged with them:
  `CheckConfigurationReferences` returns core's findings first and reaches the
  ambiguity rule only when there are none, so a plan with both kinds is fixed in
  two passes. The `codefly doctor` line for an ambiguous finding also falls
  through to the generic remediation. Both are follow-ups, named in the PR.
- **A mapping that does not say which endpoint it is does not get bound.** The
  filter identifies a mapping by its endpoint's name, with the mapping's API
  checked for consistency against the manifest. Matching a nameless mapping by
  API cannot establish which endpoint carries the address: a mapping of
  `{Name: "", Api: "rest"}` holding the private endpoint's address was judged
  against the first API match, approved as public, and then returned by core's
  interpolator, which matches the retained mapping by API.

Together they leave one identified endpoint per mapping, and — for a reference
whose exact name the consumer may reach — one candidate for the resolution to
pick. **What they do not cover:** an exact name the consumer may *not* reach
loses to a permitted API sibling, and manifest order then decides which address
the value gets, with no error. That case is a follow-up, named in the PR, and the
honest form of it is to refuse with the visibility reason rather than resolve to
something else. The durable fix is still core's: judge visibility for **every**
endpoint a reference can match, prefer an exact name, refuse an ambiguous one,
and resolve only to the endpoint that was judged.

##### A composition root's credentials are not run-wide

A root group's non-secret values reach every service. **Its credentials reach
only the services that declare the group** (`workspace-configuration-dependencies`),
in a run and in a render alike — `withheldCredentials`, decided once per service
and used by the reference check, producer discovery, the plan gate and delivery.

The reason is that making the render resolve root groups, which is the #882 fix,
would otherwise have widened least privilege as a side effect. Before it, a
render delivered declared groups only, so a root group's credential reached the
services that declared it. Afterwards every workload of the composition would
receive every root credential as a mandatory `secretKeyRef`: compromising any one
service would yield all of them, and the environment's store would hold a copy of
each credential per service. Nothing about #882 asks for that.

Both delivery paths withhold, so the parity this section is about is intact — the
run does not give a service a credential its deployment lacks, and the render
does not give it one the run lacked.

A **structured** secret — `configurations/<profile>/<name>.secret.yaml`, which
core loads as an information block with no key/value entries — is withheld whole,
by group. It also has to be: a promotable render refuses a structured secret
outright ("requires typed Kubernetes key references"), so while the root's
`.secret.yaml` reached every service, no service of the composition could be
rendered at all.

And the decision is made **before** anything is checked or resolved, in the
resolution **and at the plan gate**. A value a service does not receive imposes no
obligation on it: a withheld credential whose `${endpoint:…}` names an endpoint
private to the producer's module, or a producer whose address nothing can derive,
used to refuse the render of every service that would never read it — and, while
only the resolver was fixed, still refused their *plan*, which stops `codefly
run`, a render, a dev deploy and `codefly doctor` alike. A guard that refuses
what the thing it guards would have allowed is a contradiction, not a division of
labour. The same reference still refuses the service that *declares* the group,
which is where the obligation belongs.

One step is **not** fixed here: core resolves every root group's secrets inside
`GetCompositionRootWorkspaceConfigurations`, before the CLI can remove what this
service does not receive. So a withheld credential is still read from the secret
backend for a consumer that will never get it, and a missing backend or a failed
authentication fails that consumer's resolution. The value is discarded before
delivery — it reaches no workload, no manifest and no store plan — but the read
happens. Fixing it needs a core API that resolves a named subset; it is listed
with the other core changes below. What an operator meets is that a service
needing a root credential must say so, which is the same thing a declared group
has always required. "Credential" is the render's own classifier: an explicit
`secret:` flag, or a credential-named key (`resources.IsSensitiveKey`) — exactly
what promotes a value to a `secretKeyRef` and what `codefly deploy secrets` plans
a store entry for.

**The durable fixes are in core,** and the above are shims until they land:

- let `configurations.CheckEndpointReferences` take the effective group set, so
  the plan-time check and this resolution stop being two selections of the same
  thing;
- make `configurations.checkEndpointReference` and
  `resources.resolveEndpointReference` agree on which endpoint a reference names
  — judge every endpoint a reference can match, or refuse an ambiguous reference,
  and resolve only to the endpoint that was judged;
- give `configurations.Manager` an accessor for the composition-root group names
  it accumulates across **all** its loaders (in `LoadConfigurations`), so the CLI
  can read the manager's union instead of binding the capability on the one
  loader it registers;
- let a run profile exclude a composition-root group that no service declares:
  `resources.ResolveRunProfile` builds its inventory of known group names from
  service declarations, so such a name is rejected as unknown and the group
  cannot be excluded at all;
- let the dependency mappings a consumer is handed be what
  `PermittedDependencyEndpoints` grants. A **bare** service dependency — one
  naming no endpoints — is handed every mapping its producer published
  (`StateManager.GetDependenciesNetworkMappings` narrows by the dependency's
  endpoint list, never by visibility), so a cross-module private endpoint's
  address was in the set a `${endpoint:…}` resolves against. The CLI filters that
  set now (`World.exportableTo`), which is the consumer-side half;
- refuse `CODEFLY__WORKSPACE_CONFIGURATION_OVERRIDES` outside a local
  environment in the loader itself, so the guard does not depend on every caller
  having one;
- let a caller resolve a NAMED SUBSET of the composition root's groups, or
  filter before secret resolution:
  `Manager.GetCompositionRootWorkspaceConfigurations` resolves every root group's
  secrets before interpolation (`configurations/manager.go`,
  `configurations/secrets.go`), so a credential the CLI is about to withhold is
  read from the backend anyway and a backend failure fails a consumer that does
  not receive it;
- expose the manager's authoritative inventory of composition-root group names
  (the same gap as the accessor above), so a World cannot resolve groups it
  cannot classify — the CLI refuses that construction now, which is a guard on
  the binding rather than on the inventory;
- make the shared configuration resolution safe for simultaneous readers.
  `Manager.resolveWorkspaceConfiguration` mutates the loader's `Info` protos in
  place and publishes into `resolvedWorkspace` without a lock
  (`configurations/manager.go`), and the secret cache and value publication in
  `configurations/secrets.go` do the same, so two concurrent cold reads of one
  manager race. The CLI reaches it through this resolution and through
  `pkg/control/checks.go`'s `Configurations()`; the normal playbook executes
  serially, so no CLI path is known to trigger it today.

The root's group names come from the manager's own loaders (core's
`configurations.Loader.CompositionRootWorkspaceConfigurationNames`) and are bound
on the `World` at the one place a loader is registered. A `World` that cannot
name a root group would plan producer discovery without it, so
`requireKnownRootGroup` refuses such a group rather than delivering it with its
references resolved against nothing.

#### The three asymmetries that remain, and why none is the fault above

- A [run profile](commands.md#codefly-run-service)'s
  `exclude-workspace-configurations` trims the **run** only — build and
  deployment operations ignore profiles.

- **A root group's `${endpoint:…}` does not order the run**, so under
  `--temporary-ports` its producer may have no address yet. Core's dependency
  graph orders a consumer after the producers of the groups it *declares*
  (`ServiceDependencies.addConfigurationReferenceEdges`, core
  `architecture/service_dependencies.go`, reads a consumer's
  `workspace-configuration-dependencies` and no more), so nothing puts the
  producer of a root group's reference first.

  Note what this does **not** mean, because the earlier wording here had the
  mechanism wrong: with deterministic ports a consumer that merely starts early
  still resolves, because a producer's address is derived from the endpoints it
  recorded at **Load**, and every service of a run loads before any initializes.
  The value is lost only where no address can exist yet — under
  `--temporary-ports`, where each address is allocated at initialization
  (`codefly ci`, `test --temporary-ports`), or where the producer recorded no
  endpoint at all. There it is dropped; a *declared* group's reference is
  ordered, so the same failure there stays a hard error.
  `TestARunWithTemporaryPortsDropsARootReferenceItCannotPlaceYet` pins the drop,
  and `TestARenderRefusesARootReferenceNoAddressCanBeDerivedFor` pins that the
  render refuses rather than emitting a manifest with the value missing.

- **A run of fewer services than the composition does not contain the producer**
  a root group references. `codefly run payments/worker` runs the worker and its
  dependency closure, and a root group's reference adds no edge to that closure,
  so the producer is not in the run and has no local address: the value is
  dropped. The render of that same one service derives the producer's address
  anyway — a deployed address is a function of identity and namespace rather than
  of run membership — so it delivers the value.
  `TestASingleServiceRunDropsARootReferenceTheRenderResolves` pins both halves.

All three leave the **run** with less than the render, never the reverse, which
is the direction that cannot produce "works locally, unconfigured once
deployed". None of them is a reason the **render** may lose a value: a render
refuses a lost value whenever the workspace has the producer, whatever this
render's flow happens to cover.

A value dropped for any of these reasons is logged at **WARN**, naming the
consumer, the producer and the reference. Core's own drop is a DEBUG line, and a
configuration value going missing without a word is the fault this section exists
to remove.

#### A partial root override of a composed module's group replaces it whole

This is **core's** behaviour, not this package's, and it is a trap worth knowing
about because it interacts with the rule above.

A consuming workspace that declares a group a composed module also provides wins
the name — that is the intended rule, so a solution can override a composed
configuration without redeclaring everything the host brings. But the override is
**wholesale, not per key**: `configurations.composeModuleWorkspaceConfigurations`
(core `configurations/local_reader.go`) skips the module's information outright
once the workspace declares the name, so

- a key the root did not supply loses the module's default, and
- a key the module declared `${profile}` loses its requirement with it, so
  nothing reports the omission and `Load` succeeds — including when the root
  supplies an **empty** value for such a key, which is precisely what the
  `${profile}` marker exists to refuse.

It also stops being composed, so it is reclassified as a composition-root group
and this package then injects the truncated group into **every** service of the
composition rather than only the ones that declared it. A partial override
narrows a group's contents and widens its delivery at the same time.

This is filed as [codefly-dev/core#693](https://github.com/codefly-dev/core/issues/693)
and **fixed in [core#694](https://github.com/codefly-dev/core/pull/694)**, which
is open and green but not yet released, so the CLI still pins `v0.7.1` and the
behaviour above is what a composition meets today.

Until that release is pinned here: declare **every** key of a composed module's
group when overriding it, or override none of them.

When it is pinned, three of the four statements above stop being true for a
**module's** group — an override becomes exactly a partial override, an
undischarged `${profile}` is reported rather than discarded, and the group stays
composed, so its delivery stays scoped to the services that declared it instead
of widening to every service of the composition. The interim rule then narrows
rather than disappearing: core#694 deliberately leaves a group inherited from a
composed **workspace** (the product model) replacing whole, because the
precedence there has to be decided per key for the product model first. An
invocation-scoped override is unchanged either way — still attributed to the run,
still composition-root even on a composed name.
`TestAPartialRootOverrideReplacesAComposedModuleGroupWhole`
(`pkg/orchestration`) pins the current behaviour and names the core function, so
the CLI's expectations move when core's do. The semantics it should get already
exist one function away — `profileOverlay.add` in core's
`configurations/profile.go` overlays per key across profile derivation layers,
keeps a `${profile}` marker an override did not discharge, and refuses a key the
layer below never declared — they are simply not applied across the
workspace/module boundary.

#### The consequence an operator meets

A root group's values are subject to the render's own rules, which is the point
of there being one resolved set: a credential-named value (or one declared
`Secret`) is promoted to a `secretKeyRef` on the service's own Secret rather than
rendered inline, so the environment's `service-secrets` store must hold that key
for the projected ExternalSecret to materialize it.

`codefly deploy secrets` discovers that key from the render. It does **not**
necessarily supply the value: see [the render's secret
consequence](commands.md#codefly-deploy-gitops-render-secret-consequence) for the sources its
planner can reach and the refusal it reports when none of them covers the key.

## Playbook

The `Playbook` is the execution engine. It receives actions, executes them through a policy, and produces follow-up actions.

```go
playbook, err := NewPlaybook(ctx, world)
playbook.WithPolicy(policy)
playbook.WithStoppingAfter(func(ctx, action) bool { ... })  // When to stop
playbook.WithIgnore(func(ctx, action) bool { ... })          // What to skip
```

**How it works:**

1. `Begin()` restricts the dependency graph to the target service, then seeds the initial action
2. `Work()` runs the main loop: receive action groups from a channel, execute each through the policy
3. The policy returns follow-up actions, which are sent back into the channel
4. Execution stops when the `StopAfterFunc` returns true, or the context is cancelled

**Action flow through Work():**

```
Receive ActionGroup
    │
    ├── Check PauseManager (service failing? skip)
    ├── Check IgnoreFunc (standalone mode? skip non-target)
    ├── Check previously executed (idempotency)
    │
    ├── Record action
    ├── policy.Execute(action) → next actions
    │
    ├── If pause returned → log warning, signal, continue
    ├── Add next actions to plan
    ├── Signal completion
    ├── Check StopAfterFunc → return if done
    │
    └── Send plan's actions back to channel
```

**Concurrency:** Actions at the same DAG level are grouped and sent together. The policy determines which can execute in parallel based on the dependency graph.

## Actions

Actions are the atomic units of work. Each has a `Type` and targets a specific `Service`.

**Runtime actions (for `run` and `test`):**

```
RuntimeBegin → RuntimeLoad → RuntimeInit → RuntimeStart → [RuntimeTest]
```

**Builder actions (for `build`, `sync`, `deploy`):**

```
BuilderBegin → BuilderLoad → BuilderInit → BuilderBuild → [BuilderSync] → [BuilderDeploy]
```

**Action structure:**

```go
type Action struct {
    Type    ActionType  // e.g. RuntimeLoad, BuilderBuild
    Service string      // unique service identifier: "module/service"
    Failed  bool        // marks a failing action (triggers pause/retry)
    Round   int         // execution round for ordering
}
```

## Policies

Policies implement the `PlaybookPolicy` interface and define the state machine transitions.

```go
type PlaybookPolicy interface {
    ExecutorManager                                           // Maps action → executor function
    Execute(ctx context.Context, action Action) ([]Action, error)  // Run action, return next actions
    Restrict(ctx context.Context, service string) error       // Scope the dependency graph
}
```

### RuntimeStartPolicy

Used for `codefly run service`. State machine:

```
RuntimeBegin → RuntimeLoad (for all services in dependency order)
RuntimeLoad  → RuntimeInit (once load completes)
RuntimeInit  → RuntimeStart (once init completes)
RuntimeStart → done (service is running)
```

At each transition, the policy checks the dependency graph: if service B depends on service A, then B's `RuntimeLoad` won't fire until A's `RuntimeLoad` completes. Services at the same DAG level transition in parallel.

If an action fails (executor returns `Wait`), the action is marked as `Failing` and the PauseManager handles retry.

### RuntimeTestPolicy

Same as RuntimeStartPolicy, but adds `RuntimeTest` after `RuntimeStart` for the target service. The playbook stops after the test action completes.

### BuilderBuildPolicy (BuildPolicy)

Used for `codefly build service`. State machine:

```
BuilderBegin → BuilderLoad → BuilderInit → BuilderBuild → done
```

### BuilderDeployPolicy (DeployPolicy)

Used for `codefly deploy service`:

```
BuilderBegin → BuilderLoad → BuilderInit → BuilderBuild → BuilderDeploy → done
```

For a full module GitOps render (`codefly deploy gitops render`), the render
inventory's contract provenance — a service's exposed API contracts (from the
module's `contracts/api/catalog.codefly.json`) and its package identity (from
`module.package.codefly.yaml`) — is assembled outside this Flow, in
`pkg/gitops.renderModuleTree` (`pkg/gitops/orchestrate.go`), once per module
render, after each service's `Flow` has produced its deployment output.

### SyncPolicy

Used for `codefly sync service`:

```
BuilderBegin → BuilderLoad → BuilderInit → BuilderSync → done
```

## StateManager

The `StateManager` tracks shared state across services during orchestration:

- **Endpoints** -- each service registers its endpoints after `Load`
- **NetworkMappings** -- each service registers the port assignments its agent accepted at `Init`
- **Configurations** -- runtime configurations exposed by services for their dependents

```go
type StateManager struct {
    endpoints       map[string][]*basev0.Endpoint
    networkMappings map[string][]*basev0.NetworkMapping
    configurationManager *providers.Manager
    dependencies         *architecture.ServiceDependencies
}
```

Key methods:

- `RecordEndpoints()` -- called after a service loads, stores its API endpoints
- `RecordNetworkMappings()` -- called after init, stores the agent-accepted host:port assignments
- `GetDependenciesEndpoints()` -- returns endpoints of a service's direct dependencies
- `GetDependenciesNetworkMappings()` -- returns network addresses of dependencies
- `GetDependentConfigurationsFor()` -- returns configurations from dependency services

## Runner

The `Runner` manages a single service's runtime lifecycle via its agent (a gRPC plugin process).

**Lifecycle phases:**

| Phase | What happens | Agent gRPC call |
|-------|-------------|-----------------|
| **Load** | Agent starts, reports endpoints | `Runtime.Load()` |
| **Init** | Receives dependency info, network mappings, configurations. Returns its own mappings. | `Runtime.Init()` |
| **Start** | Receives dependency network addresses. Service starts listening. | `Runtime.Start()` |
| **Test** | Runs the service's test suite | `Runtime.Test()` |
| **Follow** | Polls agent for desired state changes (hot reload) | `Runtime.Information()` |
| **Stop** | Graceful shutdown (10s timeout, then SIGKILL) | `Runtime.Stop()` |
| **Destroy** | Clean up resources | `Runtime.Destroy()` |

**Accepted network mappings:** `Init` proposes an address per endpoint
(`InitRequest.proposed_network_mappings`) and the agent answers with the ones it
will actually serve (`InitResponse.network_mappings`) — it may bind a different
port. The answer is validated and then becomes the single published set: the
runner, the shared state every consumer reads (dependents' `Start` requests,
readiness probes, the dashboard) and the exported environment all take it.

The answer is judged against the proposal, not in the absolute: an address the
agent echoed back unchanged is acceptable whatever shape it has, because the CLI
generated it. Only what the agent added or altered has to stand on its own. It
must name exactly the proposed endpoints — membership in the proposal is what
establishes ownership — answer each access view at most once, leave every
endpoint at least one address, keep a public address off an endpoint the CLI
would not have proposed one for, and neither blank a field nor move a container
address onto loopback. It may drop a view it cannot serve and add one the CLI
would itself have proposed. Anything else fails `Init` before a single value is
published.

The accepted set is re-ordered onto the proposal's endpoint order and the
canonical access order (container, native, public). `NetworkMappingHash` is
order-sensitive and gates propagation, so without this an agent that merely
re-orders its answer between Inits would re-Init and re-Start every dependent.

An agent that returns no mappings at all is taken to accept the proposal
unchanged (the legacy contract, from before `InitResponse.network_mappings`),
and says so at `WARN` naming the addresses assumed; a partial answer is never
merged with the proposal endpoint by endpoint.

**Hot reload:** The `Follow` phase polls the agent every second. If the agent signals `DesiredState.LOAD`, `INIT`, or `START`, the runner sends a callback action to the playbook, triggering a re-execution of that phase.

## Builder

The `Builder` manages build/deploy lifecycle via the agent's builder API.

**Lifecycle phases:**

| Phase | What happens | Agent gRPC call |
|-------|-------------|-----------------|
| **Load** | Builder starts, reports endpoints | `Builder.Load()` |
| **Init** | Receives dependency endpoints | `Builder.Init()` |
| **Sync** | Synchronizes service configuration | `Builder.Sync()` |
| **Build** | Builds container image (Docker context) | `Builder.Build()` |
| **Deploy** | Deploys to target environment | `Builder.Deploy()` |

### Image reuse

`Builder.Build` executes the recipe the plan phase emitted, and before it runs
`docker buildx` it asks whether the image it would produce already exists. The
question is answered by a digest over the image's inputs — every byte of the
build context Docker would send, the verified recipe tree, and the normalized
buildx invocation — held in `pkg/orchestration/image_cache.go`. A key that has
an entry, whose recorded image the registry or the daemon can still produce,
stands in for the build: the digest a snapshot pins and the resolved images
every SBOM subject binds to come from the entry, so nothing downstream can tell
a reused image from a freshly built one.

The identity is taken over bytes and never over a declaration about them — a
dependency manifest, a lock file, a recorded tree digest. Keying on a manifest is
what makes a source-only edit in a language whose manifest did not move
invisible to a cache, which serves a stale binary while every digest looks
correct. Anything the digest cannot account for declines rather than guesses, and
`World.RebuildImages` (`--rebuild`) bypasses reuse entirely.

Two inputs a build reads without reading the context are bound separately: the
`go.mod`/`go.sum` of every declared Go module root, hashed by path so no ignore
policy can drop what the prefetch acts on, and the resolved manifest digest of
every base image, because a `FROM` tag is re-pushed whenever the base is patched.
The identity also carries the context digest it was taken over, so the tree can
be proven unchanged after the build; a context that moved mid-build records
nothing, since the entry would claim inputs the image does not match.

**The identity binds what the agent emitted.** For a service whose module
replaces a sibling by filesystem path, the agent assembles a build context
carrying that sibling and the identity is taken over the assembled tree — so an
edit to the shared source reaches the key through the agent's re-emission. An
agent that cached its own assembly would hand over stale bytes that this hashes
and confirms, which is the one way the guarantee can be defeated from outside.

Two costs a reused image still pays: the agent's plan phase still runs, so the
recipe is re-emitted and any Go module graph the recipe declares is still
prefetched on the host before the reuse decision is taken. Computing the identity
earlier would avoid the prefetch, and is achievable with a single construction of
the key cached on the planned build; it is left as a trade-off taken, not a
constraint.

## Service Dependency Resolution

The DAG is built from `service.codefly.yaml` files. Each service declares its dependencies:

```yaml
service_dependencies:
  - name: db
    module: backend
    endpoints:
      - postgres
```

The `architecture.ServiceDependencies` package resolves these into a topological order. For example:

```
frontend → api → db
                → cache
```

Results in execution order: `[db, cache]` (parallel) → `[api]` → `[frontend]`.

The `Restrict()` method scopes the graph to only the services needed for a given target.

## Readiness

`Flow.Readiness(ctx)` evaluates the gate and returns the first requirement that does
not hold (`*ReadinessFailure`: service, endpoint, predicate, last error).
`control.WaitReady` is the shared waiting seam used by interactive and headless runs:
it preserves the last complete diagnosis, retries with exponential backoff (150ms up
to 5s), and stops on the caller's deadline. `codefly run service` and `codefly run
solution` apply a five-minute deadline by default; `--readiness-timeout` changes it.
`Flow.Ready(ctx)` remains the bool projection for one-shot status surfaces.

A run is ready when every one of these holds:

| Requirement | Predicate | Holds when |
|-------------|-----------|------------|
| Lifecycle | `lifecycle` | The service's `RuntimeStart` returned successfully (the run emitted `StateRunning`). A dependency with no endpoints -- a one-shot job -- is satisfied by this alone. |
| Runner | `runner` | No started runner has reported a failure. A process dying after start revokes readiness. |
| Endpoint mapping | `endpoint-mapping` | A consumed endpoint has a recorded network mapping with an address. |
| Transport | `tcp-connect` | The endpoint accepts a TCP connection. HTTP endpoints use this unless they declare a health path, so readiness never renders an application page. |
| gRPC health | `grpc-health` | The endpoint's gRPC health service reports `SERVING`. A server that answers `Unimplemented` declares transport-only readiness and is accepted as such -- Health is never assumed on an arbitrary server. |
| HTTP status | `http-status` | An explicitly declared health URL answers `HEAD` without a server-side failure. Redirects are not followed. The current endpoint schema does not yet declare such a URL. |

The endpoints that must be healthy are the ones the run's services actually
declare (`service-dependencies[].endpoints`, empty meaning all of them), across
the whole resolved graph -- not the target's direct socket list. An endpoint no
consumer declares cannot stand in for a required one, and an open admin port
cannot mask a closed gRPC endpoint. The excluded target of an exclude-root run
is still what selects those endpoints, but its own lifecycle is not required.

The predicate comes from the endpoint's declared capability. `grpc` gets gRPC
health; HTTP and everything else (`rest`, `tcp`, `connect`, undeclared) stay
transport-only until the endpoint can declare a health URL. In particular, the HTTP
API label is not permission to probe `/`: for a frontend that route may compile and
server-render the full page.
gRPC health asks about the protobuf services the endpoint declares
(`api_details.grpc.rpcs[].service_name`, package-qualified) and falls back to
the server-wide status when it declares none; TLS comes from the endpoint's
`secured` declaration, never from guessing at its address.

When a run stays "Starting", `control.WaitReady` reports the requirement that held it
when the readiness deadline expires. Every predicate is satisfied by what the
endpoint already advertises, so the fix is to make the service serve what it
declares, never to relabel its `api` -- that field also drives address schemes,
environment variable interpolation and client generation.

Until [core#418](https://github.com/codefly-dev/core/issues/418) lands there is
no place to declare a health route, a success predicate or a body predicate, so
HTTP readiness deliberately stops at TCP reachability. Probing `/` would invent a
health contract and can itself keep a development server unready.
## Teardown

`Stop()` and `Shutdown()` do not fan every manager out at once. Launch order is
not completion order, so stopping a database concurrently with the API draining
into it can cut off that API's last write. Both walk **reverse topological
layers** instead:

1. Layer 0 is every participating service no other participating service depends
   on. They are stopped concurrently.
2. The flow waits for that whole layer to settle -- a barrier, not just a launch
   order -- before touching the layer it depends on.
3. A shared dependency is stopped once, after every consumer of it has finished.

Managers carrying no edges among the participants -- an excluded root's
`NoOpManager`, or a dependency whose consumer was never created because init
failed partway -- land in layer 0. A dependency cycle (which the DAG rejects
before it gets here) would tear the remainder down in one final layer rather
than leaking it.

**Budget.** Each layer is bounded by `defaultTeardownPhaseBudget` (15s), so a
whole teardown is bounded by that budget times the number of layers. The budget
is per layer, not aggregate, because the resources that hold state are in the
*last* layer: an aggregate deadline burned by slow consumers would leave nothing
for exactly the resources whose clean stop matters most. A layer that overruns
its budget is cancelled and the next layer proceeds -- best-effort cleanup of the
remaining owned resources rather than an indefinite hang.

**Receipt.** Each teardown records a per-resource receipt: the operation, the
number of layers, and one entry per resource with its layer, duration, and
outcome -- `stopped` (drained gracefully), `failed` (the agent answered with an
error) or `timed-out` (the drain was cut short by the budget, so the resource may
still be running). Entries are ordered by layer, then by hub registration order,
regardless of which goroutine finished first. The receipt is internal: what
reaches callers is the aggregated `go-multierror` (which names each failing
resource) plus an `OutputSink` error line for every resource the budget forced,
because a forced resource may still be running and some callers -- `pkg/control`'s
`stopFlow` -- discard the returned error.

Builder-only flows (`BuildMode`, `SyncMode`, `DeployMode`, `SnapshotMode`) have
no runtime to stop; `Stop()` returns immediately for them.

## Error Handling

- **Agent load failure:** The `RunnerLoadManager` captures the error. The policy returns a `Failing` action, which the `PauseManager` handles. The service enters a wait-and-retry loop.
- **Context cancellation:** All gRPC calls check for `codes.Canceled` and return gracefully.
- **Partial failures:** Every manager is torn down during `Flow.Stop()`, including ones a partial `InitManagers()` created but never started. Errors are collected via `go-multierror` and returned together; a resource the teardown budget forced is additionally narrated through the flow's `OutputSink`, since it may still be running and some callers discard the returned error.
- **Init failure:** If `Init` returns a non-READY status, the output manager marks the result as failing, triggering a pause and retry from the Load phase.

## Validation capability preflight

Lint, native compile and test inspect the origin agent's authoritative validation
advertisement before initializing its runtime or any runtime dependency. An
explicitly unsupported operation stops after discovery: no Load, Init, validation
RPC or disposable Destroy is sent, and no Docker backend is required. The flow
still owns and closes the discovered agent connection.

Supported operations retain their initialization and dependency requirements.
Agents without a validation advertisement retain compatibility probing; an absent
advertisement never grants a skip. A supported operation that returns Unimplemented
remains a contract failure. CI records unsupported operations as skipped with
`agent_no_lint_capability`, `agent_no_compile_capability`, or
`agent_no_test_capability`, rather than reporting a successful check.
