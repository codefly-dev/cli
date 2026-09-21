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
  and #733. The consumed Core pseudo-version includes `proto.FormatGoOutputs`,
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
   invocation and staged-output verification are implemented. Build invocation
   and qualified effect-boundary integration remain incomplete.
2. **Local development execution.** Local records preserve release choices and
   bind identity to actual bytes; driving builds/tests from those records remains
   blocked by owner executor adoption and build/test execution wiring. Restoring
   releases is implemented.
3. **Selective acquisition.** HTTPS requirements are acquired and authenticated.
   OCI transport and execution of declared source-build requirements remain.
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

The CLI consumes pushed Core `v0.3.41-0.20260921020008-5fe990d2c3a1`, with
`GOWORK=off` and no replacement. Uncommitted Core files are not a dependency.
This is not the unpublished v0.3.41 tag and does not authorize a release.

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
