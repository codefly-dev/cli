# What a deployed render decides

A render that targets a cluster other people reach — anything but a local k3d
cluster (`environments.Environment.Deployed`) — decides three things a local
render leaves alone: the public origin of each **exposed** endpoint, the identity
each workload runs under, and the surface a composed solution may reach
directly.

Each is a property of the composition plus the environment's own declarations,
which is why it is decided here: the render is the last place an operator can
still supply the answer, and the only place that sees the whole composition at
once.

Three properties hold for all three rules.

**They resolve, they do not recognise.** A declaration is followed to a result —
an origin, a principal, a permitted service — and what the result is decides the
outcome. None of them keeps a list of strings to reject.

**Absence refuses, by name.** A declaration that is missing, and one that is
present but resolves to nothing, are the same answer: the render refuses, says
which service and endpoint it is about and which declaration would supply it, and
installs nothing. There is no silent default, because what a default produces is
invisible — a workload that boots, serves, and is wrong.

**They are decided on the effective render.** What a cluster would receive after
the environment's overlay is built, not the files underneath it. A file's name,
the directory it sits in and a patch applied after the render all count.

A local cluster is exempt throughout. These rules exist so that rules someone
else writes can hold — an edge that binds a host, a policy that names a principal
— and a developer's own cluster has none of them. A local render is left
byte-for-byte as its agents wrote it.

## 1. Every exposed endpoint has an origin an operator fixed

A service the outside world **addresses** has to be able to name the origin it is
reached by: that origin is what binds a sign-in redirect, an authenticator's
relying party, and a link the service sends to someone. It is also the one thing
a workload cannot work out for itself, because it is a fact about the cell and
not about the service — so an operator declares it, and the render carries it.

Which endpoints those are is core's **exposure**, not a visibility. Since core
v0.14.0 they are three separate axes — reach, addressing, and a derived
allow-list — and the first two answer different questions:

| Declaration | Question it answers |
| --- | --- |
| `visibility` | **reach** — who may call the endpoint |
| `exposure` | **addressing** — whether an address reachable from outside the workspace is allocated |

So `visibility: public` with `exposure: none` is an endpoint any module may call
and nothing outside addresses — a module's REST surface reached through a host's
gateway, say. It has no origin of its own and this rule never asks it for one.
`exposure: public` is the endpoint that does, and a public endpoint states its
exposure rather than omitting it — core refuses one that omits it, so a manifest
written when "public" meant an address fails to load instead of quietly losing
it.

An endpoint that lives outside the system (`location: external`) is not asked
either: it is addressed where it lives, its DNS-backed address is already its
public one, and core refuses `exposure: public` beside it.

An environment fixes the origin of each exposed endpoint, in one of two forms:

```yaml
environments:
  - name: staging
    ingress:
      - name: product
        service: shop/web          # module/service, or a bare service name
        endpoint: http             # omit for a service-wide route
        hosts:
          - app.example.com        # canonical: the origin the product answers as
          - www.example.com        # aliases after it
    dns:
      # Or: every service's host is derived from one suffix.
      app-host-suffix: cell.example.com
```

The resolved origin reaches the workload in the ConfigMap its container already
loads its configuration from:

```
CODEFLY__PUBLIC_ORIGIN__<MODULE>__<SERVICE>__<ENDPOINT>__<API>=https://app.example.com
```

This is the third of the three addresses one endpoint has, and the names are kept
apart on purpose:

| Carrier | Address |
| --- | --- |
| `CODEFLY__ENDPOINT__…` | what the service **listens** on (a deployment localizes it) |
| `CODEFLY__SELF_ENDPOINT__…` | the in-cluster address its **peers** reach it at |
| `CODEFLY__PUBLIC_ORIGIN__…` | the origin the **outside world** names it by |

### What is resolved, and what is refused

A declared host is normalised the way the client that dials it normalises one,
and then classified: it is an **address**, a **public DNS name**, or **neither**.
Neither is a refusal.

- The scheme must be http or https; the value must be a bare origin, so a path,
  a query or a fragment is refused.
- Case is folded and one trailing dot (the DNS root) is dropped before anything
  is decided.
- A host that **ends in a number** is an address, in every form a resolver
  accepts — dotted or not, decimal, octal or hexadecimal. That is what makes
  `127.1`, `2130706433`, `0x7f000001` and `0177.0.0.1` one address rather than
  four domain names.
- An address that is loopback, unspecified, link-local or multicast is refused on
  a cell: it is no origin anybody else can use. `localhost` and anything under it
  are refused as the reserved names they are.
- A name must be a DNS name with more than one label, ASCII, with labels of 1–63
  characters — a single label resolves only inside whatever namespace the client
  sits in.
- The canonical origin drops the scheme's default port and an IPv6 zone, so two
  declarations of one origin compare equal.

"Is this a well-formed origin" and "is this a **public** origin" are two
questions, and only the second is a cell's. A local cluster is reached at a
loopback origin, so it carries one and is refused nothing; a cell is not, so
there it refuses.

A refusal names the endpoint and what is wrong with the declaration, and **never
repeats the declared value**: a declaration is operator input, it can carry a
credential, and a render's diagnostics are written to a terminal and a log.
Userinfo in a declared URL is refused before any message exists.

### Required, not offered

A declared origin must reach the workload. The render checks the effective
render for it, so an overlay that unbinds the configuration a container loads, or
a container that loads none at all, is a refusal naming the service — a
declaration and a workload that disagree is exactly the state this rule exists to
end.

## 2. Every workload runs under an identity of its own

The account a workload runs under is what everything downstream has to name to
tell two workloads apart — an authorization rule, a network identity and a cloud
identity binding all name a principal — and a principal shared by two workloads
grants each of them whatever either was granted. The account Kubernetes gives a
pod that names none is shared by every pod that names none, so it is never an
identity.

The render binds every deployed workload to a `ServiceAccount` named after its
service, rendered into the service's kustomize base. Where the environment
declares a runtime identity for a service — its own `service-identity` entry, or
one its managed dependency declares — that identity's annotations and pod labels
go on the same account, as before.

**The name is derived, never accepted.** A workload already bound to some other
name is not evidence of a distinct identity: two services bound to one name hold
each other's grants. So a workload the effective render binds to anything but its
own service's account is refused, with the workload and both names given. A
service whose agent already names the account after the service passes unchanged,
which is what every agent that renders one already does.

**And no two services may claim one principal.** Each service's account is its
own name, which is distinct within a module by construction; two modules
rendering into one namespace is the case only the whole render can see, and the
render checks it there.

A unit whose effective render carries no workload gets neither an account nor a
refusal: there is no pod to tell apart from another's.

### What this does not decide

The account is per **service**, not per pod template. A service that renders two
workloads — a StatefulSet and its bootstrap Job, say — binds both to the one
account named after it. That is the granularity every other identity declaration
in this model already has: `service-identity` is keyed by service, a service's
own `spec.service-account` names one account, and a managed dependency's
principal is resolved for the service that dials it. Splitting a service's own
workloads apart would need a declaration saying which is which, and there is
none.

## 3. A solution reaches another module only through its host

A composed solution is a guest: it arrives through the host it is composed
against, and the host is what decides, per request, what it may see. A declared
`service-dependencies` edge is how a service asks for an address, and the render
answers with that address — so the render is what decides what a guest can
reach, and it decides it from one declaration rather than from inference.

```yaml
environments:
  - name: staging
    solution-boundary:
      host-surface:
        - service: shop/web          # module-qualified; a bare name is refused
          endpoint: http             # omit to permit the service's endpoints
        - service: shop/api-gateway
          endpoint: rest
```

A solution's service may depend on its own module's services, and on the surface
above. Every other cross-module dependency is refused, naming the edge — and an
environment that declares no surface permits nothing, because a composition that
has not said what a guest may reach has not said that it may reach anything.

A module is a solution because it ships a `solution.codefly.yaml`, and for no
other reason; every service it declares is checked, not only a default runnable
entry. What the solution declares it consumes (`api.consumes`) explains a
refusal — the api is reached through the host — but never decides one. Where a
configuration group happens to be declared decides nothing either.

Build and schema edges are left alone: a toolchain reads them, no workload opens
them, so they are no path in a cell.

A declaration this rule is judged against that cannot be read is a refusal, not a
pass: a solution service whose dependencies cannot be parsed is not a service with
no dependencies.
