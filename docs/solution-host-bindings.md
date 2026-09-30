# Solution host bindings

A solution is present on a host because delivery **declared** it, not because
its process announced itself. `codefly` renders one
`SolutionHostBinding` — core's `solutionhost` package — per solution instance,
into the same tree Argo already delivers. The host reconciles that record; the
runtime only reports health against a binding it never created.

Nothing reconciles these documents yet. Rendering them changes no runtime
behaviour: self-registration is untouched, and a binding delivered today is a
record nobody reads. What changes is that the record exists, and that it is
checked where it was authored.

The document, its invariants and its conformance fixtures live in
[codefly-dev/core `solutionhost`](https://github.com/codefly-dev/core). This
page is only about what the CLI renders into it.

## Declaring the host

A binding names the host it targets, and the CLI has no other durable record of
one — `codefly environment import` reads `coordinate` off a
`codefly/coordinate/v1` contract and keeps it as a provenance comment, nothing
more. The environment declares it:

```yaml
environments:
  - name: prod
    namespace: obin
    host:
      coordinate: obin/prod/eu-west-1      # matched by the host; a mismatch is refused
      component: saas-host                 # the component instance within it
      audience: https://saas-host.obin.example  # what a workload token must be bound to
```

All three are required together, because a binding needs all three at once. An
environment that declares **no** `host` renders **no** binding, and the render
says which solutions it could not declare — silence would be
indistinguishable from a composition with no solution in it.

Nothing derives a coordinate. A derived one would be a guess a host silently
refuses at reconcile time (`solutionhost.ErrWrongHost`), far from the render
that invented it.

## What is rendered, and where

One ConfigMap per solution instance, at
`solution-host-bindings/<binding-id>.yaml` in the module's owned tree:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: solution-host-binding-obin.prod.crm
  namespace: crm
  labels:
    app.kubernetes.io/managed-by: codefly
    codefly.dev/binding: obin.prod.crm
    codefly.dev/document: solution-host-binding
    codefly.dev/solution: crm
data:
  solution-host-binding.codefly.yaml: |
    schema: codefly/solution-host-binding/v1
    ...
```

A ConfigMap, and not a bare `solution-host-binding.codefly.yaml` file, because
the promotable render ruleset refuses any YAML in the owned tree that is not a
Kubernetes manifest or a Kustomization. A bare document cannot travel through
this pipeline at all, and a ConfigMap is what "configuration the host can
mount" means in the only delivery target the CLI has.

The data key is core's constant `solutionhost.FileName`, never the binding ID.
A per-binding key would read better in a projected directory, but a ConfigMap
data key is a *configuration name* and the render classifies those: an instance
named `auth-gateway` or `session-store` would produce a key core calls
credential-bearing and the render would refuse its own output.

Each solution renders into its own namespace, and each module render delivers
its own tree, so these ConfigMaps do not all arrive in the host's namespace
from one Argo Application. A host selects them by label across namespaces.

## Binding ID

`<workspace>.<environment>.<instance>` — for example `obin.prod.crm`.

Derived from identity alone, and from nothing that changes between renders: no
digest, no generation, no timestamp, no resolved address. That is what makes it
stable. The instance name is what distinguishes a second instance of the same
solution — two instances are two composed modules under two names. The
workspace and environment keep two deliveries that land on one host from
claiming the same binding.

It is bounded to 63 characters because it is stamped as a Kubernetes label
value, which is stricter than core's 128.

The binding ID is **not** the host's `solution_id`. The route alias is.

## Route alias

Exactly one route per binding: the instance's own name, as a single lowercase
segment (`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`). core's `namePattern` is wider and
admits dots and slashes; a host keys its registry on the alias as one URL path
segment, so the render refuses an alias a host would have to map.

The surface is `backend`, for the route and for every artifact. The CLI renders
Kubernetes workload artifacts and nothing else, and core refuses a route whose
surface the generation renders no artifact for, so `backend` is the only
honest surface today. A frontend or client surface arrives when the render
produces a frontend or client artifact.

The facade prefixes a solution federates consumed modules under
(`solution.codefly.yaml` `api.consumes[].as`) are deliberately **not** rendered
as routes, although they are aliases claimed on the same host: a prefix fronts
a consumed module this binding does not deliver and whose artifacts it does not
pin, which is exactly what core's route validation forbids.

## Generation

Monotonic per binding ID, and taken from the **previously delivered document**
for that binding — not from a counter the CLI stores, and not from a value
derived from the content:

| Source | Why not |
|---|---|
| A content-derived value | Deterministic, and a no-change re-render reproduces it — but it is not *ordered*. `Admit` refuses a generation lower than the applied one, and a hash is lower than its predecessor half the time. |
| A stored counter | Ordered, but a second source of truth about what was delivered. Two renderers (a developer and CI) keep two counters and both produce generation N+1 with different contents — `ErrRewrittenGeneration`. It also has to be seeded from the delivered document anyway. |
| The delivered document | Ordered, the only place the current generation is actually recorded, and it travels with the thing it describes. The render already reads the tree it replaces. |

A re-render with no change must not bump, so the candidate is compared at the
prior generation through core's own `Digest()` — the same canonical encoding
the host uses to tell a re-read from a rewrite. Equal keeps the generation and
delivers byte-identical output; different is prior + 1.

The failure mode this accepts: a render from a checkout that cannot see the
tree it replaces emits generation 1, and a host that has applied a higher
generation refuses it as stale. That failure is loud, it is at the host, and it
is fixed by rendering from the delivered tree. Inventing a higher number to
avoid it would be a renderer asserting a history it does not have.

An unreadable delivered document is an error, never a silent reset to 1.

The sharp edge: the render destination is per **module**, not per environment,
and a render replaces it whole. Rendering staging and then production again
finds no prior production document and starts over at 1. The render prints the
generation it declared for exactly this reason — a reset is otherwise
invisible until the host refuses the document. Render an environment from a
checkout that holds that environment's delivered tree.

## Release digest

Empty in v1, and left empty deliberately. Signed releases do not exist yet; a
digest invented here would pin nothing while looking like a pin. It becomes
required at a later schema version — a version step, not a stricter reading of
bytes already delivered.

## Admission

Every render runs core's `Host{}.Admit` over the **whole set** it is about to
write, and fails the render on any refusal. The zero `Host` is the renderer's
view: no coordinate and nothing applied, so every check that does not need host
state still runs and a route-alias collision is refused where it was authored
rather than leaving the host to guess which of two claimants meant it. A
document a host would reject is never written.

## Dev deployments

`codefly deploy dev` re-pins a service's image inside an already-rendered tree
to code no release describes. The binding is **not** re-rendered: it declares a
release and pins the digests of the artifacts that release produced, and
re-rendering it against a dev image would declare a release that does not
describe the running code — a worse record than a stale one.

So on a tree carrying a dev deployment, the declared artifact digests no longer
match the delivered bytes. A full `codefly deploy gitops render` re-derives
both and clears the dev deployments.

## Removal

No tombstone is rendered. An instance that leaves the composition leaves the
tree, and core is explicit that removal is a generation and never an absence —
so a host must keep the binding until a tombstone arrives, and a solution
genuinely withdrawn is not yet withdrawable through delivery.

**The document does not merely stop being refreshed — Argo deletes it.** When no
binding is rendered the inventory records no binding path, so the promotion
emits no ApplicationSet element for it; dropping an element deletes its
Application, and the Application template carries
`finalizers: [resources-finalizer.argocd.argoproj.io]` with
`syncPolicy.automated.prune: true`. The delivered ConfigMaps are cascade-deleted
from the cluster.

So a reconciler cannot treat "the document is gone" as "nothing to do". It has
to hold the binding across the document's disappearance, or removing a `host`
declaration — or a module ceasing to be a solution — silently withdraws every
binding it delivered. That is a requirement delivery places on the host, not
something the host can infer from what it observes.
