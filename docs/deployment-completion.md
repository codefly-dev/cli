# Deployment completion stages

A deployment can succeed in four different senses, and conflating them is how a
green `codefly deploy` ends up meaning "kubectl accepted some YAML" while a
caller reads it as "the service works". `pkg/deployments` names the four stages
explicitly and every deployment result reports the one it actually established.

| Stage | What it means | What establishes it |
|-------|---------------|---------------------|
| `rendered` | Manifests were produced from the deployment tree. **No cluster is contacted.** | `RenderManager`, `--render-only`, `--dry-run` |
| `applied` | `kubectl apply` accepted every manifest against the verified target. Nothing has been observed running. | `kubectl apply` exiting zero |
| `bootstrapped` | Every Job the applied manifests own ran to completion. | Observing the owned Jobs |
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
- `--wait-timeout` (default 10m) bounds observation. Exceeding it is a failure
  naming every resource that never finished, not a hang.
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

Terminal conditions are reported as themselves instead of burning the timeout:

- a Job with a `Failed` condition (`BackoffLimitExceeded`, `DeadlineExceeded`, …)
- a Deployment whose `Progressing` condition went false
  (`ProgressDeadlineExceeded`) or that carries `ReplicaFailure`
- a pod of an owned workload waiting on `ImagePullBackOff` or `InvalidImageName`

Receipts and logs carry kind, namespace, name and the cluster's own status
reason and message — never manifest content, so no secret value reaches them.

## Bootstrap ordering

Schema preparation must finish before the consumers that depend on it start.
Position in a rendered stream or a YAML list is not a dependency barrier, so
ordering is expressed twice, once per reconciliation mechanism:

- **Direct local apply.** `LocalApplyManager` splits the rendered documents by
  kind into preparation (namespaces, config, secrets, services, **Jobs**) and
  rollouts (Deployments, StatefulSets, DaemonSets). Preparation is applied first;
  when the caller requires `bootstrapped` or more, the owned Jobs must complete
  before any rollout document is applied. Across services, the module deploy
  loop walks services in dependency order, so a completed schema service gates
  the next one.
- **GitOps.** The generated ApplicationSet stamps Argo CD sync waves: module
  resources reconcile first, then units that prepare schema
  (`InventoryUnit.Bootstrap`), then their consumers. Argo CD only starts a wave
  once the previous one is healthy.

The publish/reconcile boundary is unchanged. GitOps still reaches `healthy`
through Argo CD reconciliation and reports it on its evidence receipt in this
same vocabulary; the CLI gains no direct mutation access to a remote cluster.
Direct apply remains restricted to an exact, verified local k3d target.

## Expand/contract schema rollout

Gating a rollout on a schema Job is only safe when the schema change is
backward-compatible with the running consumer, because during the wave the old
pods are still serving. Roll schema out in two deployments:

1. **Expand.** The schema Job only adds — a nullable column, a new table, a new
   index, a view. Old and new consumer code both work against it. Deploy this
   with `--wait-for bootstrapped` (or `healthy`), so the Job is proven complete
   before the consumer that queries the new table rolls out.
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

## Testing

```bash
go test ./pkg/deployments ./pkg/gitops
CODEFLY_DEPLOYMENTS_K3D_QUALIFY=1 go test ./pkg/deployments -run DisposableK3d
```

The qualification suite creates and deletes a disposable k3d cluster; it needs
`docker`, `k3d` and `kubectl` on `PATH`.
