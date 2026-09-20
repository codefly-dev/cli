# Independent upgrade coverage

This is the implementation boundary for [CLI #753](https://github.com/codefly-dev/cli/issues/753)
and its agent-admission work in [CLI #752](https://github.com/codefly-dev/cli/pull/752),
coordinated with [Core #589](https://github.com/codefly-dev/core/pull/589).
It is not a claim that the complete product workflow is available.
Generation stays in #751 and checkout diagnostics in #733; neither is folded
into #752. No additional tracker, merge, release or fleet rebuild is implied.

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

These are agent-admission guarantees, not proof of service behavior, retained
data recovery, authorized production artifacts or approval of a composition.
An agent used to build an artifact is not necessarily present in production.

## What remains under #753

1. **Product selections.** Integrate Core's instance-scoped replacement model
   into the CLI. Preserve inherited module/base releases and record exact
   selected artifacts without editing dependency source or requiring ancestor
   tags. Existing service-agent updates do not meet this requirement.
2. **Local development.** Expose one or multiple external checkouts through the
   shared selection model, preserving release choices and checkout files.
   Bind results to actual content, report dirty state, invalidate affected
   results on edits/rebuilds, and restore releases explicitly. Existing local
   overlays alone are not qualified evidence for this workflow.
3. **Selective acquisition.** Consume Core's computed artifact requirements.
   Missing required artifacts must fail, never trigger recursive source
   checkout, submodules or a hidden aggregate source tree. Current pinned-module
   materialization is not this nested acquisition planner.
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

The CLI still consumes released Core v0.3.40, not the in-progress #589 worktree.
Core is being changed independently; its uncommitted files are not a CLI build
dependency. A committed, reviewed integration point is needed for its shared
selection/acquisition, authenticated usage, compatibility and deployment APIs.

`pkg/composition/pinned.go:LoadModuleTrust` currently accepts a flat signer map.
Core #589 requires package-scoped signer authority. The CLI loader, fixtures,
trust-coverage checks and documented YAML must migrate together; no global-key
fallback is acceptable. There is currently no CLI wiring for the new
authenticated `ConsumerPins`/consumer-authority requirements or shared
undetermined verdict presentation.

The separate dependency-update policy in `.github/dependabot.yml` and
`pkg/cliupdate/dependabot_contract_test.go` still reserves Core bumps for manual
adoption and describes the old fleet ordering. Reconcile that policy with the
conformance matrix and reviewed Core API migrations under #753; the targeted
release-runbook correction is not a claim that this policy audit is complete.

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

This inventory is an integration checklist, not a completed all-path admission
audit. None of these routes currently calls Core's new `AdmitDeployment` API.

Real solution-kind loading has an additional Core blocker: the loader dispatches
all verified-artifact kinds to the provider artifact verifier, whose manifest
parser only accepts `codefly:provider`. The real-process solution admission test
therefore qualifies the RPC boundary using a service-kind test executable, not
the complete solution installation path. Core #589 must provide correct
kind-owned artifact verification before actual solution qualification can pass.

Daemon monitoring still classifies concrete process names. Core's public
ownership API needs an authenticated read-only membership/owner result,
including leaderless groups, so CLI does not duplicate private registry or
reaper logic. PostgreSQL IPC cleanup also awaits its agent-owned migration;
removing the host call without recovery would lose behavior.

Published-agent qualification remains a release blocker. Core #589 records 17
official service artifacts rejected for missing lifecycle declarations,
including Go 0.0.52, Next.js 0.0.153 and Redis 0.0.89. Known owner work includes
service-go#81, service-python#86, service-nextjs#129, service-generic#59,
service-redis#62 and service-postgres#138. Missing declarations require truthful
owner adoption and authorized publication, not a blanket fleet rebuild.
The supported-set inventory and non-service qualification are not complete.

Authentic packaged contract/usage evidence and real consumer qualification also
depend on module-saas-starter#852 and obin-ai/platform-obin#25, as tracked in
#753. Fixtures and source-built agent tests do not replace that evidence.

Runtime module/solution onboarding without restarting the host is not delivered
by these changes. Configuration delivery, registration and actual host
acceptance require their own evidence.
