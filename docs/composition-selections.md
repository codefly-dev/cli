# Product-owned selections

PR #752 and issue #753 are the single CLI delivery unit. Core owns resolution,
compatibility and admission. The CLI owns invocation, local state and effects.
No agent names, source-family guesses or linked-Core comparisons are admission rules.

## Available commands

`codefly composition` emits structured JSON. Every command requires:

- `--workspace`: the workspace declaring package-scoped `module-trust`.
- `--product`: the directory containing Core's `module.codefly.yaml` descriptor.
- `--configuration`: a JSON object of effective configuration values, including
  secrets. Values are not printed or persisted by these commands.
- `--identity-key`: a private file containing at least 32 raw bytes. Use the same
  protected key when comparing inputs across qualification environments. Core
  computes an HMAC identity, not a guessable public hash of secret values.

Commands:

- `init-release VERSION`: acquire signed metadata and authenticate an exact base
  release, then create `module.codefly.selection.json`. Never overwrite an existing
  release selection. No dependency sources are downloaded.
- `init INPUTS.json`: initialize from Core's `ReleaseSelection` under `root`, plus
  optional `sourceBuilds` targets and `artifacts` requests (`Target`, `Name`). This
  declares extra acquisitions, such as a contracts snapshot. Root digests supplied
  by a caller are still authenticated against owner-scoped signed metadata.
- `select-release TARGET VERSION --reason TEXT`: resolve the component identity
  from the participating instance, authenticate the candidate and persist its
  immutable replacement in the product descriptor. Preserve existing additional
  requirements and reject concurrent changes to the inspected selection.
- `select REPLACEMENT.json`: the same operation with Core's explicit replacement
  fields `target`, `release`, `requirements` and `rationale`.
- `inspect`: Core's complete effective record, inherited/selected releases,
  differences, provenance, acquisition/build requirements and local content
  identities. Git dirty state is supplementary; `null` means unavailable, not clean.
- `develop CHECKOUTS.json`: map one or more instance targets to independent
  absolute checkout paths. Persist only `.codefly/composition-local.json`.
- `restore TARGET...`: remove those local substitutions without touching checkout
  files, release selections or other local substitutions.
- `acquire`: fetch only Core's computed HTTPS artifact requirements. Check digests
  before atomically publishing cache entries and rehash every cache hit. No
  submodules, recursive checkout, archive fallback or implicit source collection.
  Missing/private/unsupported transports fail explicitly; OCI acquisition is not
  implemented here. Build requirements are reported, not executed.
- `check TARGET ARTIFACT USAGE.json AUTHORITY.json`: authenticate consumer usage
  with Core against the exact effective selection and instance, authenticate the
  inherited and selected owner contracts snapshots, then use Core's evaluator.
  ARTIFACT must be a requested `contracts` artifact carrying protobuf JSON for
  Core's `ContractSnapshot`. Signed usage uses Core's `SignedConsumerUsage` and
  `ConsumerUsageAuthority` JSON shapes (byte arrays are base64). SAFE and
  NEW_CAPABILITY return zero; BREAKING and UNDETERMINED emit the report and exit 2.
  Changed contracts without source-supported evidence remain UNDETERMINED.
- `check-inputs INPUTS.json`: authenticate actual runtime files and compute Core's
  selection/runtime/binding identities for qualification. Not functional evidence.
- `check-deployment INPUTS.json POLICY.json`: invoke Core's `AdmitDeployment` on
  actual runtime files, signed derived outputs, target bindings and qualifications.
  Policy uses Core's `DeploymentPolicy`. Missing owner-required functional/stateful
  evidence, expired or foreign qualifications, patched outputs and local checkouts
  are rejected. This is inspection under the supplied policy, not authority for a
  later executor to deploy different bytes or choose its own policy.
- `upstream`: prepare Core's owner-scoped adoption facts. No GitHub submission,
  merge or release occurs. Local-development facts remain labelled as such.
- `propose-removal TARGET RELEASE.json`: ask Core to prove full effective-input
  equivalence against a candidate base before proposing removal. Does not apply
  the proposal or discard the override.

`INPUTS.json` for deployment inspection contains `runtime` entries with `target`,
`name`, and `path`; `bindings` maps target names to non-secret identity digests;
`derived` and `qualifications` use Core's signed statement/signature types.
`module-trust.build-signers` is package-scoped, separately from release `signers`.

Commit the product descriptor and release selection file. Keep the identity key,
configuration files, `.codefly/composition-local.json`, artifact cache and
`.codefly-composition.lock` machine-local. Commands never edit a checkout, Git
index or dependency source. Cross-process locks serialize CLI mutations; atomic
writes preserve the preceding selection on failure. Changed local bytes change
Core's identity even when the checkout path or release label has not changed.

## Explicit Deployment Blocker

Core #589 confirmed there is no typed contract connecting resolved nested
instance/artifact selections to build/render operation inputs and acknowledged
output provenance. `ReleaseArtifact` supplies purpose/URI/digest; `BuildRequirement`
names source/service/agent/output, but neither supplies that binding. The existing
Solution render RPC takes a single artifact reference and executor-defined values.
The legacy composition projection explicitly rejects nested selections.

Until Core defines this contract and executors implement it, multi-instance
build/render/deployment is **blocked**, not equivalent to a legacy deployment.
An explicit rejection stopgap guards local apply/image-import entry points,
Flow deployment, platform sends, GitOps render/publication and rollback when a
participating product declares nested selections. This is not completed positive
admission at every low-level transport. It must be replaced by selection-bound
execution and effect-time Core admission, not removed to make deployments pass.

Not delivered: running the effective combination's functional/stateful tests,
selection-bound build/render, OCI acquisition, durable deployment approval,
actual observed/rollback identity comparison, authorized upstream submission,
or hot onboarding without host restart. HTTP/Git/process regressions establish
selection and evidence plumbing, not real database retained-data recovery.
The generic environment primitives and the deprecated cell wrapper remain under
Core review; these commands do not establish a replacement wrapper or a rename.
