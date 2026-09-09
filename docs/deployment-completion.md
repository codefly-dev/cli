# Deployment completion stages

A deployment can succeed in four different senses, and conflating them is how a
green `codefly deploy` ends up meaning "kubectl accepted some YAML" while a
caller reads it as "the service works". `pkg/deployments` names the four stages
explicitly and every deployment result reports the one it actually established.

| Stage | What it means | What establishes it |
|-------|---------------|---------------------|
| `rendered` | Manifests were produced from the deployment tree. For a render, no cluster is contacted; for a direct apply that failed its bootstrap barrier, see **Partial revisions** below. | `RenderManager`, `--render-only`, `--dry-run` |
| `applied` | `kubectl apply` accepted every manifest against the verified target. Nothing has been observed running. | `kubectl apply` exiting zero |
| `bootstrapped` | Every schema-preparation Job the applied manifests own ran to completion. | Observing the owned bootstrap Jobs |
| `healthy` | Every workload the applied manifests own finished its rollout with its replicas ready. | Observing the owned Deployments/StatefulSets/DaemonSets |

The stages are cumulative: reaching one means every earlier one holds.

## Choosing a completion condition

`codefly deploy service` and `codefly deploy module` take:

```bash
codefly deploy service api --wait-for healthy --wait-timeout 5m
```

- `--wait-for` defaults to **`applied`**. That is exactly what a direct apply has
  always established, so an existing deploy keeps its meaning rather than being
  silently relabelled as healthy. Opt in to `bootstrapped` or `healthy` when the
  caller genuinely needs the deployment to be running — an integration
  deployment test should.
- `--wait-timeout` (default 10m) is the **total** observation budget for the
  command, not a fresh allowance per service or per stage. A module deploy
  spends it across every service it observes; once it is gone the next
  observation fails immediately rather than silently starting a new clock.
  Exceeding it is a failure naming every resource that never finished, not a
  hang.
- `--wait-for rendered` is rejected: use `--render-only` or `--dry-run`, which
  contact no cluster at all.

Through the control plane the same choice is `DeployRequest.Completion` and
`DeployRequest.CompletionTimeout`; an unset `Completion` means `applied`.
`DeployResult` carries `Required` and `Reached`, and a result that reached less
than it required is never reported as a success.

## What observation reads

Observation is bound to the **exact deployment evidence**: the Jobs and workloads
the rendered manifests declare, by kind, namespace and name. Nothing else in the
namespace is read, so an unrelated broken Deployment next door cannot fail a
deploy and a healthy one next door cannot rescue it.

A manifest that declares no namespace is observed in the namespace the verified
kubeconfig context selects, because that is where `kubectl apply` puts it —
apply passes no `--namespace`.

Pod-level diagnosis follows `ownerReferences` (through the ReplicaSet, for a
Deployment), never the label selector. kustomize `commonLabels` routinely stamp
a workload's selector labels onto resources it does not own, and a sibling's
failing pod must never be reported as this workload's failure.

A resource that reaches its stage is **settled**: it is never read again. A Job
with `ttlSecondsAfterFinished` disappears shortly after completing, and
re-reading it while a rollout finishes would fail a deployment that succeeded.
Conversely, an owned resource that stops reporting for several consecutive
sweeps is called failed rather than waited out — it was applied before
observation began, so a persistent `NotFound` means it is gone.

Terminal conditions are reported as themselves instead of burning the timeout:

- a Job with a `Failed` condition (`BackoffLimitExceeded`, `DeadlineExceeded`, …)
- a pod of an owned workload waiting on `ImagePullBackOff` or `InvalidImageName`

`ProgressDeadlineExceeded` and `ReplicaFailure` are **not** treated as terminal.
Kubernetes keeps reconciling through both, and they clear on their own when the
slow image pull or the exhausted quota that caused them resolves — the default
`progressDeadlineSeconds` is 600s, the same order as the default budget, so
failing on them would kill rollouts that were about to succeed. They are
attached to the pending message instead, and the caller's budget bounds the
wait.

Receipts and logs carry kind, namespace, name and the cluster's own status
reason and message — never manifest content, so no secret value reaches them.

## Bootstrap ordering

Schema preparation must finish before the consumers that depend on it start.
Position in a rendered stream or a YAML list is not a dependency barrier, so
ordering is expressed twice, once per reconciliation mechanism:

- **Direct local apply.** When — and only when — the caller requires
  `bootstrapped` or more, `LocalApplyManager` splits the rendered documents into
  preparation (namespaces, config, secrets, services, and the **schema Jobs**)
  and rollouts (Deployments, StatefulSets, DaemonSets, and ordinary Jobs).
  Preparation is applied first and its Jobs must complete before any rollout
  document is applied. A caller that requires only `applied` gets its documents
  applied exactly as rendered: reordering resources changes which of them exist
  when, and that is not a change to make on a caller's behalf.

  A Job joins the barrier only if it carries the
  `codefly.dev/bootstrap-service` label — the same marker the GitOps module
  bundle uses. An ordinary Job (a smoke test that talks to its own Service)
  stays with the rollout: hoisting it ahead of the workload it exercises would
  fail it, and waiting on it there would deadlock against a dependency that has
  not been applied yet.

  Across services, the module deploy loop walks services in dependency order, so
  a completed schema service gates the next one.
- **GitOps.** The generated ApplicationSet stamps Argo CD sync waves: module
  resources reconcile first, then units that prepare schema
  (`InventoryUnit.Bootstrap`), then their consumers. Argo CD only starts a wave
  once the previous one is healthy.

The publish/reconcile boundary is unchanged. GitOps still reaches `healthy`
through Argo CD reconciliation and reports it on its own evidence receipt; the
CLI gains no direct mutation access to a remote cluster. Direct apply remains
restricted to an exact, verified local k3d target.

## Partial revisions

A bootstrap barrier that fails has already applied the preparation resources and
deliberately withheld the rollout, so the target holds part of the new revision
next to the old workloads. The tree reports `rendered` — it never became
`applied` — and sets `Mutated`, which is what says the cluster was changed. The
CLI prints it, and `DeployResult.RenderedTrees[].Mutated` carries it to the
control plane. Treat a `Mutated` tree short of `applied` as needing
reconciliation, not as a no-op.

## Expand/contract schema rollout

Gating a rollout on a schema Job is only safe when the schema change is
backward-compatible with the running consumer, because during the wave the old
pods are still serving. Roll schema out in two deployments:

1. **Expand.** The schema Job only adds — a nullable column, a new table, a new
   index, a view. Old and new consumer code both work against it. Deploy this
   with `--wait-for bootstrapped` (or `healthy`), so the Job is proven complete
   before the consumer that queries the new table rolls out. The Job must carry
   `codefly.dev/bootstrap-service` to be part of the barrier.
2. **Contract.** Once every consumer runs the new code, a second deployment
   drops what is no longer read — the old column, the compatibility view.

A destructive change shipped in a single step will break the pods that are still
running while the wave completes, no matter which completion stage is required.

## Reporting

Failures name the stage that failed and the resource responsible, for example:

```
deployment cannot reach bootstrapped: Job payments/schema-migrate: Job failed:
BackoffLimitExceeded: Job has reached the specified backoff limit
```

```
deployment did not reach healthy within 5m0s: Deployment payments/api:
0/2 replicas ready, 2 updated; pod api-7f9 container api is ImagePullBackOff
```

The stage and its diagnostics are reported on the failure path too, not only on
success — that is when they matter.

## Testing

```bash
go test ./pkg/deployments ./pkg/gitops
CODEFLY_DEPLOYMENTS_K3D_QUALIFY=1 go test ./pkg/deployments -run DisposableK3d
```

The qualification suite creates and deletes a disposable k3d cluster; it needs
`docker`, `k3d` and `kubectl` on `PATH`.
