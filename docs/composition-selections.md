# Product-owned selections

PR #752 and issue #753 are the single CLI delivery unit. Core owns resolution,
compatibility and admission. The CLI owns invocation, local state and effects.
No agent names, source-family guesses or linked-Core comparisons are admission rules.

## Available commands

`codefly composition` emits structured JSON. Every command requires:

- `--workspace`: the workspace declaring package-scoped `module-trust`.
- `--product`: the directory containing Core's `module.codefly.yaml` descriptor.
- Exactly one of `--configuration` (a JSON object of effective configuration
  values) or `--render-requests` (the actual typed render payloads described below).
  Values may contain secrets and are not printed or persisted by these commands.
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
- `prepare-render INPUTS.json`: ask Core to authenticate runtime streams once for
  all selected services and emit their bound protobuf-JSON render requests.
  Requires signed artifact-operation declarations and media types. Does not
  invoke an executor, provide configuration values or establish qualification.
- `stage-render INPUTS.json --output-parent ABSOLUTE_DIRECTORY --without-principal`:
  requires `--render-requests`; acquire exact selected executors, authenticate
  their live contracts through Core's loader, invoke Builder Deploy or Solution
  Render, require Core's checked registered-group shutdown, and verify receipts
  against actual files. Shutdown failure discards the receipt and prevents a
  completion record, even when rendering succeeded.
  This stages only; no apply, image import, publication or approval is performed.
  Returns the new private directory, its `inputs.json` and Core's deployment-input
  record. Prior qualifications/executions are rejected, never silently reused.
- `check-inputs INPUTS.json`: authenticate actual runtime files and staged render
  outputs, computing Core's selection/runtime/binding/execution identities for
  qualification. Without executions this checks runtime inputs only, never
  deployment readiness.
- `check-deployment INPUTS.json POLICY.json`: invoke Core's `AdmitDeployment` on
  actual runtime files, verified render outputs for every selected service,
  signed derived outputs, target bindings and qualifications.
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
`executions` contains one entry per selected service: `target`, `service`,
`directory` (absolute canonical path to an isolated staging directory), and
`receipt` (the executor's protobuf-JSON ArtifactExecutionReceipt object).
Directories must be disjoint. Every inspection prepares the current Core
requests, rejects extra/missing/duplicate/foreign receipts, and calls
`VerifyArtifactExecutionDirectory` to hash the actual output files. Undeclared,
escaped, symlinked and missing output files are rejected. The resulting sealed
evidence feeds Core admission; qualifications must sign `ExecutionIdentity`.
Changing output bytes invalidates qualification even with unchanged runtime
inputs. Receipt JSON is not proof of executor invocation or live capability:
the inspection commands inspect supplied evidence, not authenticate its RPC origin or
authorize deployment. Deployment owners must keep outputs isolated and reverify
them at the effect boundary.
`module-trust.build-signers` is package-scoped, separately from release `signers`.

Commit the product descriptor and release selection file. Keep the identity key,
configuration files, `.codefly/composition-local.json`, artifact cache and
`.codefly-composition.lock` machine-local. Commands never edit a checkout, Git
index or dependency source. Cross-process locks serialize CLI mutations; atomic
writes preserve the preceding selection on failure. Changed local bytes change
Core's identity even when the checkout path or release label has not changed.

## Selected-executor staging

Core `3264a63d712e` fixes the reproduced `5fe990d2c3a1` shutdown defect, where
`AgentConn.Close` could return while a same-group child continued writing.
`invokeRender` now requires successful `CloseAndWait` before accepting a receipt.
Cleanup uses a fresh ten-second context independent of RPC cancellation/deadline;
shutdown errors are joined with render errors, never logged and ignored.
Registry/private-directory cleanup failure also refuses completion. Tests use
real authenticated child writers, canceled RPCs and filesystem cleanup denial.
No raw-kill or parallel CLI lifecycle workaround is used.

This establishes shutdown of the authenticated registered group, not intentionally
detached processes or unrelated writers. It does not authorize deployment.
Persisted receipt JSON alone cannot establish invocation or shutdown provenance;
old completion records are not retroactively qualified by upgrading Core.
Keep output isolation, effect-time revalidation and deployment guards in place.

`--render-requests` is an array of `{target, service, protocol, request}` objects.
Use Core instance targets, for example `modules/left`, and the protocol from the
signed operation declaration. `request` is strict protobuf JSON of the existing
Builder `DeploymentRequest` or Solution `RenderRequest`, not an alternative
artifact-selection model. Every participating service needs exactly one request.
The CLI owns `execution`, staging destinations and verified Solution identity;
supplying those fields is rejected. Solution `artifactReference` is also rejected.
Core's HMAC configuration identity covers deterministic protobuf encodings of all
these payloads, keyed by instance/service/protocol. Use the same request file and
identity key for selection, staging and subsequent qualification inspection.
Changing environment, configuration, values or target changes this identity.

The staging parent must already exist at a canonical absolute path outside the
product and independent local checkouts. Each invocation creates its own private
directory; a complete `inputs.json` is written atomically only after every service
passes verification. Failure/cancellation removes only that invocation's staging
directory, reporting cleanup failures. A host crash can leave an incomplete directory without a completion
record; it is not approval, and existing completed batches are not overwritten.

`--sandbox=required` is the default, with UDS and network denied. Unsupported or
missing sandboxes fail, never fall back. `--allow-network` explicitly allows
sandboxed executor network access. `--sandbox=none` explicitly permits unrestricted
local execution. `--without-principal` is mandatory for this local command; it
does not grant production authorization. The programmatic boundary also requires
explicit Core sandbox/principal options. No credentials or policy are invented.

Supported acquired executors are raw native executable bytes declared as
`application/octet-stream`; archives, OCI manifests and other representations
are rejected, not unpacked or inferred. Core `manager.LoadArtifact` snapshots and
hashes the exact bytes, requires generic and operation-specific live declarations,
and restricts the connection to the prepared operation. Solution identity comes
from digest-only inspection and is carried unchanged into Render. No installed
agent coordinates, provider descriptors, downloads or install mutations occur.
Owner executors must truthfully implement `artifact-execution/v1`; linking newer
Core does not opt them in. Tests use independently built authentic subprocesses,
not published production executor qualification.

## Explicit Deployment Blocker

Core `3264a63d712e` supplies execution binding, the acquired-executable loader and
checked registered-group shutdown. These are consumed by selected-executor
staging. Qualified multi-instance deployment remains **blocked**.
An explicit rejection stopgap guards local apply/image-import entry points,
Flow deployment, platform sends, GitOps render/publication and rollback when a
participating product declares nested selections. This is not completed positive
admission at every low-level transport. It must be replaced by selection-bound
execution and effect-time Core admission, not removed to make deployments pass.

Not delivered: running the effective combination's functional/stateful tests,
selection-bound builds, OCI acquisition, durable deployment approval,
actual observed/rollback identity comparison, authorized upstream submission,
or hot onboarding without host restart. HTTP/Git/process regressions establish
selection and evidence plumbing, not real database retained-data recovery.
The generic environment primitives and the deprecated cell wrapper remain under
Core review; these commands do not establish a replacement wrapper or a rename.
