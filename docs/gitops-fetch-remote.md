# Local read-only GitOps fetch remote

Argo CD runs inside the local k3d cluster and reconciles the reviewed Git
revision the promotion lifecycle produced. In a real environment it fetches
from the workspace's `gitops.repo-url`; locally there is no reachable Git host,
so the CLI owns a small, reproducible **read-only fetch remote** that serves the
reviewed revision to Argo over the private container network.

This replaces the ad-hoc qualification containers that used to be launched by
hand (wildcard host bindings, a mutable `nginx:alpine` tag, `/tmp` state, and a
throwaway self-signed certificate). The lifecycle is environment-scoped and
disposable: it owns nothing but the container and its generated trust material,
and never touches the publication repository, the registry, or the user's global
kubectl context.

## Lifecycle

```bash
# Inspect what would be created for the reviewed revision of a module.
codefly deploy gitops remote plan payments --env local

# Create or refresh the remote, serving the reviewed revision to Argo.
codefly deploy gitops remote up payments --env local

# Validate the running remote against its exact ownership and network identity.
codefly deploy gitops remote status --env local

# Tear it down (repository data is preserved).
codefly deploy gitops remote down --env local
```

`plan` and `up` take a module argument only to locate the reviewed revision from
its publication receipt (`codefly deploy gitops publish` must have run first).
`status` and `down` need only the environment.

## What `up` guarantees

- **Loopback-only host exposure.** The only host port is bound to `127.0.0.1`.
  Argo never uses it: it fetches over the private k3d network by container DNS.
- **Digest-pinned image.** The runtime image is pinned by `@sha256:` digest; a
  floating tag is rejected as a mutable image.
- **Immutable revision from a read-only mirror.** The remote serves an exact Git
  revision from a bare mirror mounted read-only.
- **Out-of-Git TLS with bounded rotation.** A CA and server certificate are
  generated under the workspace `.codefly/gitops/remote/<env>/tls` directory with
  owner-only (`0600`) private keys, exact DNS/IP SANs, and a bounded validity
  that rotates automatically before expiry. Private keys are never printed and
  never committed. The CA is the declarative material Argo trusts.
- **Exact ownership markers.** Owner, workspace, environment, cluster,
  repository, and role labels are stamped on the container.

## Teardown safety

`down` (and the replace step inside `up`) refuse to mutate a container whose
ownership labels or network membership have drifted from the expected identity.
This is the guard that turns a silent partial deletion into an explicit refusal.
The bare mirror, TLS material, and receipt under
`.codefly/gitops/remote/<env>/` are always preserved.

## Validation

`codefly doctor` sweeps every fetch remote it finds and reports wildcard host
bindings, mutable images, expired certificates, and stopped remotes.
`codefly deploy gitops remote status` additionally validates a remote against
its exact spec: ownership drift, network drift, missing CA trust, and a stale
served revision.

## Recovery

The lifecycle is idempotent and every durable artifact is preserved across
teardown, so recovery from a partial creation or deletion is always the same:

1. Run `codefly deploy gitops remote status --env <env>` to see the drift.
2. Run `codefly deploy gitops remote up <module> --env <env>` again. It reuses
   the existing mirror and still-valid TLS material, re-validates ownership
   before replacing a drifted container, and re-probes the loopback endpoint.
3. If teardown refused because ownership drifted, inspect the container
   (`docker inspect <name>`) and remove it manually only after confirming it is
   not owned by another workspace or environment, then re-run `up`.

## Disposable k3d qualification

`TestLocalFetchRemoteLifecycle` in `pkg/gitops` stands the remote up on a
throwaway k3d cluster and proves host loopback exposure, private reachability of
the exact revision over container DNS + TLS with CA trust, a private key that is
never leaked into the served repository, and a validated teardown that preserves
the mirror. It is disabled by default; enable it with real Docker and k3d:

```bash
CODEFLY_GITOPS_K3D_QUALIFY=1 go test ./pkg/gitops/ -run TestLocalFetchRemoteLifecycle -count=1
```
