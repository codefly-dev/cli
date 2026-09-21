# Independent upgrade coverage

This is the implementation boundary for [CLI #753](https://github.com/codefly-dev/cli/issues/753)
and its agent-admission work in [CLI #752](https://github.com/codefly-dev/cli/pull/752),
coordinated with [Core #589](https://github.com/codefly-dev/core/pull/589).
It is not a claim that the complete product workflow is available.
At the owner's subsequent direction, generation #751 and checkout diagnostics
#733 and managed-identity #756 are consolidated into #752 with their original
commits. Issues #754, #755 and #757 are transferred to #753 and closed as
superseded, not completed. #753 remains the
single tracker. Consolidating branches does not authorize merging to main,
releasing or rebuilding the fleet.

The owner's instruction to leave only one open CLI PR also consolidates the
previously separate dashboard dependency update #637 into #752. Its eight npm
updates and generated embedded assets are preserved; `npm ci` and
`npm run build` reproduce those assets. This is additional dashboard scope,
not an agent-compatibility requirement.

The required workflow is: select a nested replacement, inspect differences,
check compatibility, test the effective combination, approve, deploy those
exact inputs, and inspect the actual deployment.

## What #752 implements

- Runtime protocol/capability admission, including the direct gateway supervisor
  and the container-recovery operation boundaries. Missing declarations and
  wrong run acknowledgements are errors, not legacy compatibility.
- Installed-agent discovery from live metadata; no production compatibility
  roster, concrete-agent aliases or named bootstrap compiler. Explicit choices
  are preserved, and an incompatible choice is not silently replaced.
- Candidate inspection before `update agent` persists a version. Discovery or
  protocol failure preserves both the service file and its in-memory selection.
  This command edits a service declaration; it does not implement product-owned
  nested replacements or qualify a deployment.
- Runnable protocol admission before `Builder.Load`, separately from advertised
  Builder support; solution live-declaration admission before exposing the
  Package/Render client. Real-process tests cover missing/future protocols,
  discovery failure and compatible peers. The Runnable regression records that
  rejected peers never receive Load.
- Contract publication/comparison and documentation that an unchanged contract
  does not require rebuilding or repinning the fleet. The release runbook and
  skill now target affected repositories instead of recommending `--all` or
  requiring every ancestor to publish in a fixed chain.
- Companion-only protobuf generation and checkout drift diagnostics from #751
  and #733. The consumed Core release includes `proto.FormatGoOutputs`,
  and the combined branch builds. Drift diagnostics are advisory, not deployment
  admission or proof of verified artifact provenance.
- Removed the product-specific PostgreSQL IPC sweep from both `run` and `clear`,
  matching the Core removal. Generic owned-process/container cleanup remains;
  no named replacement hook or product-resource heuristic is added to the CLI.
- Product-owned nested selections, independent local checkout records and dirty
  content inspection, selective HTTPS acquisition, authenticated consumer
  compatibility, exact-runtime-file admission inspection, owner-scoped adoption
  facts and Core-proven override-removal proposals. See
  [the command contract and limits](composition-selections.md).
- Selected source-build staging invokes Core-bound Builder Build operations on
  exact acquired executors, requires checked registered-group shutdown, and
  rehashes every output before batch completion. Typed build/render payloads
  share one configuration identity. Real TLS source-packaging tests cover two
  instances, refused source/payload drift, partial failure and CLI invocation.
  This stages bytes only: functional or stateful qualification is not delivered
  by a build receipt. Separate `publish-build` verifies and privately snapshots
  outputs, uses package-scoped owner signing keys for Core derived statements,
  uploads exact digest-addressed objects to an explicitly selected OCI repository,
  reads back their bytes, and retains an exclusive complete record. It uses the
  existing local prepared mutation gate, creates no module/CLI release and does not grant
  deployment authority. A digest-named OCI retention manifest protects the outputs
  against ordinary garbage collection, including untagged-manifest removal; actual
  Distribution GC/readback is tested. Explicit retention-root deletion or expiration
  remains under registry operator control.
  OCI acquisition likewise preserves the signed manifest/blob representation
  without recursively downloading layers or sources. Credential and token requests
  require HTTPS. Cache reads use nonblocking validated regular descriptors; cache
  writes and creation are root-relative and reject escaping ancestor symlinks.
  Build and render both reject replaceable output ancestry and inherited macOS
  allow ACLs before invoking an executor. The render ACL regression fails with
  the old mode-only boundary and passes with the shared storage check.
  Command JSON/configuration-key reads and approval signing-key opens reuse the
  nonblocking regular-file boundary; real FIFO regressions ensure malformed
  command inputs cannot wait indefinitely for a peer before staging/admission.
- Exclusive durable Core admission records and rechecking against an independently
  retained admission identity, current policy and freshly verified runtime/render
  files. This includes qualification expiry, signer revocation and record/output
  tampering checks. It does not issue functional evidence or authorize deployment;
  the effect-path rejection guards are unchanged.
- Trusted-local, product-scoped host approval authority and signed exact-admission
  approvals using Core scoped authorization. Candidate files cannot choose policy,
  verifier keys, audiences or allowed targets. Authority replacement requires its
  prior digest; approvals bind the full normalized policy and signer keys, exact
  admission and target bindings. Fresh checks enforce literal expiry and drift.
  Host storage validates root/effective-UID ownership, protected ancestry and
  opened handles, including home aliases and macOS ACL mutation grants. Trusted
  sticky temporary ancestors remain supported. Reads and atomic replacement use
  validated directory handles; this is not same-UID/root compromise protection.
  Approval inspection does not consume authorization or integrate an effect path.
- Host-owned durable single-use approval consumption is available to effect
  owners through `ReserveApproval`. It re-admits exact current inputs, uses the
  verified signer/token ID across policy rotation, and exclusively publishes
  synced use records. Concurrent processes and lost replies cannot refund use;
  historical inspection does not assert deployment. No consuming deployment
  adapter, cross-host/target-wide fence or reset/retry command is delivered.
- Private approved-input preparation copies runtime bytes and Core-declared
  render outputs, re-admits the copies, and retains independent evidence. Named
  read-only handles survive source cache/staging removal; reservation freshly
  rechecks those private bytes, current selection and host authority. macOS
  inherited allow ACLs are refused before copying. Deterministic real-filesystem
  post-link cancellation/cleanup-denial tests assert uncertain consumption is
  never refunded. Snapshots are ephemeral, not crash-resumable deployments; no
  production effect caller or guard relaxation is delivered by this increment.
  Shared runtime admission and both copy paths use nonblocking descriptor-validated
  opens; real FIFO and post-admission replacement tests cover cancellation, lock
  release, no consumption and partial-snapshot cleanup.
- Read-only local Kubernetes target inspection/rechecking binds verified cluster
  routing/CA identity to live `kube-system` and selected namespace UIDs. Explicit
  namespaces, TLS verification, missing/deleting targets and retained-identity
  mismatch are enforced. Namespace recreation invalidates the binding; ordinary
  metadata updates do not. This is an observation primitive for the existing local
  adapter, not target fencing, qualification execution or positive effect admission.
- Daemon monitoring uses Core's authenticated read-only group ownership API;
  executable names are display-only, and errors do not imply orphanhood.
- Dependabot may propose Core build-dependency updates. Runtime admission and
  qualification remain required; there is no fleet-linked-Core pin policy.
- Generic managed-service identity projection from #756 uses Core's atomic
  workload projection at module and service rendering boundaries. Conflicting
  identities fail before publication; build-only and non-consuming services
  remain unchanged. Endpoint/port and secret references retain their existing
  explicit configuration owners; the CLI does not rewrite endpoint addresses.

The #756 merge preserves the newer Core contract, not its superseded cell/v1
implementation. Core deliberately removed proxy transport/image/args, inferred
loopback routing and audit sinks in ac8b363f/186d2403. Import tests reject those
unsupported declarations without changing workspace files; existing endpoint,
secret-reference, identity and deep-copy regressions remain. The retained cell/v2
wrapper is under owner review, not an approved final primitive contract.

The incoming namespace-wide NetworkPolicy is not retained: with no existing
egress policy, its empty pod selector would isolate every pod while allowing only
the endpoint CIDR/port, blocking DNS and unrelated destinations. Egress CIDRs
remain explicit configuration facts. Safe policy generation and real-cluster
qualification remain tracked in #753; this merge does not claim that policy
generation is implemented or that imported CIDRs alone enforce isolation.

These are agent-admission guarantees, not proof of service behavior, retained
data recovery, authorized production artifacts or approval of a composition.
An agent used to build an artifact is not necessarily present in production.

## What remains under #753

1. **Product execution.** Connect the implemented Core selections to typed
   build/render inputs using Core's now-published execution binding API.
   Batch request preparation, exact selected-executor loading, typed render
   and build invocation, staged-output verification and derived publication/signing
   are implemented. Qualified effect-boundary integration remains incomplete. Staging now
   requires checked registered-group shutdown, with the limits detailed below.
2. **Local development execution.** Local records preserve release choices and
   bind identity to actual bytes. Real two-checkout source builds, changed-input
   rejection, release restoration and macOS sandbox/UDS execution with network
   denied are tested. Production builder adoption and exact-input functional/stateful
   test execution remain incomplete; local output cannot be deployed as a release.
3. **Selective acquisition.** HTTPS and digest-addressed OCI requirements are
   acquired and authenticated, without implicit source/layer collection. Declared
   source-build requirements can be staged and owner-authorized outputs published.
   Content-addressed retention roots survive ordinary registry GC. Operator deletion
   policy and truthful production executor adoption still govern actual availability.
4. **Inspection.** Project the shared effective record into human/structured
   output: inherited references, each selection/artifact difference, reasons,
   evidence and approved versus observed deployment. Keep private configuration
   and credentials out of output and upstream requests.
5. **Compatibility.** Use Core's evaluator with authenticated actual-consumer
   usage. Present SAFE / NEW CAPABILITY / BREAKING and separate undetermined
   evidence from a demonstrated break. Positively admit permitted verdicts;
   an unknown enum or evidence gap must not become SAFE. Protocol compatibility
   alone cannot establish functional readiness.
6. **Qualification and deployment.** Bind tests, build/render outputs and
   approval to the same effective inputs and target bindings. Call Core's
   deployment-admission API before effects on every applicable path, reject
   private patches and absent required functional/stateful evidence, and never
   silently replace approved inputs. Retain existing mutation authorization.
   Persisting/rechecking admission and trusted-local host-policy/signed approval
   decisions are implemented. Qualification execution, remote approval delegation,
   target-wide/cross-host fencing, positive effect-time integration and actual
   observed/rollback comparison remain incomplete.
7. **Upstream loop.** Prepare a scoped request to the owner of the inherited
   default, reusing existing records and including exact replacements and
   shareable evidence. Submission needs authorization; deployment need not
   wait for adoption. Only propose removing an override after Core proves full
   effective equivalence, not because a version string caught up.
8. **End-to-end qualification.** From a clean consumer environment, prove the
   Team A/X and Team B/Y replacements within the same Foo release and a nested
   PostgreSQL-agent replacement. Include breaking candidates, private-patch
   refusal, local development, interruption, cache identity, observed state and
   upstream catch-up. Exercise retained data/recovery on real infrastructure.

## Concrete integration points and blockers

The CLI consumes published Core `v0.3.41` (merge `fda66313eb54`), with
`GOWORK=off` and no replacement. Uncommitted Core files are not a dependency.
This includes Core's nonblocking render-output opener and descriptor validation,
fixing the reproduced regular-to-FIFO replacement hang in the shared verifier.
CLI runtime admission and snapshot copy opens have their own nonblocking checks;
neither fix establishes functional qualification or permits deployment effects.
The acquisition cache and subsequent compatibility snapshot read now share that
descriptor boundary too. Real FIFO replacements reproduce the previous hangs;
the fixed paths reject/reacquire without holding the selection lock past cancellation.
Root-relative cache creation and writes also reject escaped symlink ancestors and
preserve unrelated external files when an opened directory is renamed.

Core source reconciliation confirms an additional owning contract gap: the
artifact-execution API and exact-executor loader support build/render, not an
exact-combination functional/stateful test invocation. Runtime test `selection_id`
acknowledges test selectors only. Core #584 must supply the missing invocation and
acknowledgement binding before CLI qualification can honestly produce signed
evidence for runtime/render/target identities. Existing qualification verification
is not that invocation capability; no parallel CLI wire contract is substituted.
The release also includes Linux retained-projection-symlink activation
(`f05de63c`), preserving concurrent readers during projection replacement.
Lifecycle v1 and startup v2 declarations are unchanged from Core v0.3.40;
`artifact-execution/v1` still requires truthful adoption by each selected executor.
Consuming this release does not authorize CLI, fleet or module publication.

Release-adoption verification used the actual CLI `go.mod`: module verification,
full build, composition/command/deployments/conformance/orchestration/environment
race tests, real macOS sandbox/UDS staging and disposable-k3d target inspection
passed. Downloaded Core FIFO/containment and projection-activation regressions
passed five race repetitions on both native macOS and a Linux container.
Full CLI normal and race suites retain exactly the same eight engine/gateway
failures from published Python, Next.js and generic artifacts missing protocol
declarations; no race reports occurred. Full lint retains 315 baseline findings,
with zero new findings against `973e46d4`. This is dependency-integration evidence,
not qualified production executors, functional/stateful delivery or release clearance.

Build-staging verification uses the same published Core dependency. Full build,
composition/command race, real macOS sandbox/UDS build and render, and focused
Linux build/command race tests pass. The final full normal and race suites retain
the same eight missing-protocol failures; neither produced race reports. Full
lint remains at 315 findings and changed-line lint against `73353171` is clean.
The Linux container requires an init/reaper: without it, a real executor child
became a PID-1-owned zombie and checked shutdown correctly refused completion.
The repeated run with Docker `--init` passed; no shutdown check was weakened.
These tests package real source bytes over TLS and verify signed-derived handoff,
but do not qualify production executors or retained-data recovery.

`LoadModuleTrust` uses package-scoped release and build signers with no global-key
fallback. Selection checks use authenticated consumer usage and Core's evaluator;
UNDETERMINED is explicit and exits nonzero, not a demonstrated break or SAFE.
Semantic source-supported comparison and real packaged consumer evidence remain
qualification work, not something a matching protocol alone proves.

Core's typed selection-to-build/render binding is now available. The CLI uses
`PrepareArtifactExecutions`, `manager.LoadArtifact` and
`VerifyArtifactExecutionDirectory` for selected-executor staging and inspection;
Core admission requires exact staged render outputs and qualification over
`ExecutionIdentity`. Independent gRPC renderer tests produce the inspected
files and exercise missing capability, changed/missing/extra/symlinked evidence,
multiple services and stale qualification. This is not published-executor
adoption or deployment qualification. `stage-render` binds actual typed RPC
payloads to configuration identity and invokes exact acquired native executables
through authenticated Core lifecycle/operation admission. Builder and Solution
subprocess regressions cover partial failure, cancellation and concurrent batches;
the sandbox integration test exercises UDS with network denied. No URI/name
inference, install mutation or parallel CLI loader is substituted. Explicit rejection guards
remain; positive selection-bound deployment and observation are incomplete.

Cross-repo review reproduced a shutdown defect in previously consumed Core
`5fe990d2c3a1`: `AgentConn.Close` can return after the leader exits while a child
in the same tracked group continues writing. The owning real-process regression
is recorded in `/tmp/core584-shutdown-repro.log`. The earlier CLI tests missed it.
Core's `CloseAndWait`, introduced in `3264a63d712e`, remains required. The CLI
requires authenticated group shutdown on a fresh ten-second cleanup context, joins
shutdown and render errors, discards the receipt on failure, and only then
verifies output bytes and publishes completion. Real CLI regressions cover
same-group writers, cancellation, filesystem cleanup denial on both protocols,
combined operation/cleanup errors and absence of a completion record on failure.
The cleanup-denial regression failed against the old CLI defer even with the new
Core dependency; merely changing the dependency would have hidden the error.
This is registered-group shutdown evidence, not an attestation about intentionally
detached processes. Older records are not retroactively qualified, supplied
receipts are not invocation/shutdown proof, and deployment effects remain guarded.

The deployment integration must cover at least these effect-owning boundaries,
not only the top-level `deploy` command:

- `pkg/deployments/manager.go`: service apply, module kustomize apply and image
  import, reached through `cmd/deploy/service.go` and `cmd/deploy/module.go`.
- `pkg/platform/deploy.go` and `cmd/ci/deploy.go`: the older platform/CI route.
- `pkg/gitops/publish.go`: publication and rollback behind mutation permits;
  `pkg/gitops/orchestrate.go` and `solution.go`: build/package/render inputs.
- `pkg/gitops/observe.go` and `pkg/deployments/observe.go`: observed target
  evidence must be compared with the approved effective inputs, not just
  successful application or an artifact version.

This inventory is not a completed all-path admission audit. `composition
check-deployment` calls Core admission for inspection; it does not authorize these
legacy effects or imply that anything is running.

The exact-artifact Solution path no longer uses the installed-provider verifier:
`LoadArtifact` authenticates raw executable bytes and the live generic/operation
declarations, then carries the returned Solution identity into Render. Legacy
coordinate-based solution installation is a different path; staging does not
establish that path's production qualification.

Daemon monitoring now consumes `TrackedProcessGroup.InspectOwnership`, including
Core's leaderless-member authentication and recorded-owner birth checks. The CLI
does not duplicate registry/reaper authentication. PostgreSQL IPC cleanup is no
longer a host migration prerequisite:
the owner directed its removal from Core/CLI. The CLI no longer scavenges native
PostgreSQL shared memory or semaphores; native retained-data/recovery behavior
has not been requalified by removing those calls.

Published-agent qualification remains a release blocker. Core #589 records 17
official service artifacts rejected for missing lifecycle declarations,
including Go 0.0.52, Next.js 0.0.153 and Redis 0.0.89. Known owner work includes
service-go#81, service-python#86, service-nextjs#129, service-generic#59 and
service-redis#62. Missing declarations require truthful
owner adoption and authorized publication, not a blanket fleet rebuild.
The supported-set inventory and non-service qualification are not complete.

Authentic packaged contract/usage evidence and real consumer qualification also
depend on module-saas-starter#852 and obin-ai/platform-obin#25, as tracked in
#753. Fixtures and source-built agent tests do not replace that evidence.

Runtime module/solution onboarding without restarting the host is not delivered
by these changes. Configuration delivery, registration and actual host
acceptance require their own evidence.
