# Product-owned selections

PR #752 and issue #753 are the single CLI delivery unit. Core owns resolution,
compatibility and admission. The CLI owns invocation, local state and effects.
No agent names, source-family guesses or linked-Core comparisons are admission rules.

## Available commands

`codefly composition` emits structured JSON. Selection/admission commands require:

- `--workspace`: the workspace declaring package-scoped `module-trust`.
- `--product`: the directory containing Core's `module.codefly.yaml` descriptor.
- Exactly one of `--configuration` (a JSON object of effective configuration
  values) or `--render-requests` (the actual typed render payloads described below).
  Values may contain secrets and are not printed or persisted by these commands.
- `--identity-key`: a private file containing at least 32 raw bytes. Use the same
  protected key when comparing inputs across qualification environments. Core
  computes an HMAC identity, not a guessable public hash of secret values.

Commands:

- `inspect-local-target ENVIRONMENT [--expected-identity DIGEST]`: resolve the
  workspace's explicit local k3d environment and read its real API. This command
  needs only `--workspace` and the environment name, not a product/configuration
  identity. It requires an explicit valid namespace, authenticated HTTPS and a
  resolved cluster CA. The returned binding hashes the verified cluster routing/CA
  identity, the observed `kube-system` UID and the selected namespace's name/UID.
  No credentials or kubeconfig paths are returned; the verified flattened config
  is passed to the read-only API query through stdin, not a temporary file.
  Missing/deleting namespaces and mismatches against an independently retained
  digest fail. Inspection has a 30-second total deadline, or the caller's shorter
  deadline. Recreating a namespace changes the identity even at the same name;
  unrelated namespace annotations do not. This is a live observation, not an
  atomic snapshot across API calls, a target-wide fence, mutation authority,
  functional qualification or deployment. Kubernetes has no built-in cluster UID;
  the `kube-system` UID is an incarnation anchor, not independent server attestation.
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
- `record-admission INPUTS.json POLICY.json DESTINATION.json`: perform the same
  Core admission and persist its identity and exact record, including signed
  qualifications. The destination must be absolute, outside the product/local
  checkouts, with an existing canonical parent. Publication is exclusive and
  synced: concurrent writers cannot overwrite an existing record, and incomplete
  writes never appear at the destination. Runtime paths and configuration values
  are not stored. An interrupted caller must inspect any completed destination;
  it must not overwrite it on retry. Filesystem sync/cleanup errors are reported
  even if publication already occurred.
- `recheck-admission INPUTS.json POLICY.json RECORD.json --expected-identity ID`:
  authenticate the saved record against an identity retained independently by
  the caller, then re-admit the actual runtime/output files under current policy.
  Core includes admission time in its identity; this reproduces the original
  record at its original time and separately checks current qualification expiry.
  Missing evidence, changed bytes/bindings/configuration, revoked signers, record
  tampering and stricter unmet policy fail. The output distinguishes the retained
  identity from the newly evaluated identity; neither means anything is running.
  These commands persist/recheck admission evidence, not an authorized approval
  decision: they do not establish who may choose policy or grant mutation rights.
  Never derive `--expected-identity` from an untrusted record being checked.
- `configure-approval-authority CONFIG.json [--expected-digest DIGEST]`: trusted-local
  administration of this product's host-owned policy, approver identity/public key,
  audience and exact target bindings. Initial configuration cannot overwrite an
  existing authority; replacement requires its current digest.
- `inspect-approval-authority`: inspect the installed authority's normalized digest
  and host path, without exporting signing credentials.
- `approve-admission INPUTS.json RECORD.json DESTINATION.json --expected-identity ID
  --expected-authority DIGEST --signing-key FILE --expires RFC3339`: re-admit the independently reviewed record
  using installed host policy and actual files, then persist a signed approval.
  The private key must match the installed approver, and expiry cannot exceed
  qualification validity. No policy, verification key or audience is accepted from
  the candidate. Destination publication is exclusive, as for `record-admission`.
  Both the admission identity and authority digest must match the independently
  reviewed values; changing host policy between review and signing fails.
- `check-approval INPUTS.json APPROVAL.json`: verify the signed approval against
  current installed authority, then re-admit actual files and qualifications.
  Policy/key/audience/target changes invalidate the approval. Missing claims,
  foreign signatures, altered records and expired evidence fail. Literal expiry
  is enforced, including time spent waiting for locks or verifying files.
- `inspect-approval-use ID`: read historical host consumption evidence for the
  `useIdentity` returned by approval inspection. This is not a deployment or
  health observation. Missing evidence is an error, not proof of no effects.
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

### Host Approval Authority

Authority is stored outside the product, workspace and selected local checkouts,
under an existing host `CODEFLY_HOME/composition-authorities/` directory, keyed by
the canonical product root. The configuration contains `audience`, `approver`
(Core `policy.Principal`, identity only), `key` (base64 Ed25519 public key),
`policy` (Core `DeploymentPolicy`) and `bindings` (exact non-secret target identity
digests). Policy must explicitly require functional qualification and supply its
signers. Core additionally enforces every selected owner's declared requirements.
The normalized authority digest includes all requirements, signer key bytes,
approver/key/audience and target bindings. Requirement ordering is immaterial.
Changing policy is a separate explicit administration operation, never inferred
from an approval file or accepted as an approval-command flag. Authority files
and directories must be owned by the effective user or root and cannot be
group/world writable. Every ancestor is checked through directory handles:
non-sticky writable ancestors and foreign-owned storage are rejected, while
trusted-owned sticky temporary ancestors remain valid. Reads validate the opened
file's owner and permissions; atomic replacements stay relative to the validated
registry handle. Home aliases require trusted ownership and protected ancestry
too; aliases containing parent traversal require selecting the canonical home
directly. macOS mutation-granting ACLs are rejected even when mode bits
look protected; deny-delete and read-only ACLs remain valid. Unsupported ownership
or access verification fails closed. This boundary does not defend against root
or another process running as the same user. Symlinked authority records and
storage inside a checkout are rejected. Concurrent configuration replacements
serialize and compare the prior digest. Keep the host configuration and its
`CODEFLY_HOME` selection under operator control; changing the host profile is
administration, not a remotely supplied deployment input.

Approval uses Core's existing Ed25519 scoped-authorization primitive with action
`composition.admission.approve`, exact admission resource, configured audience,
normalized authority digest and target-binding digest. This is the CLI's explicit
implementation contract, not a previously mandated handbook token schema. It is
not a Gateway mutation permit or an execution-recorder `evidence/append` grant.
The signing key file must contain 64 raw Ed25519 bytes with owner-only permissions;
it is never copied to host configuration or printed. Approval administration and
signing are intentionally not added to MCP or remote Gateway surfaces.

`check-approval` verifies evidence; it does not consume `MaxUses`, reserve a rollout
attempt, establish live target identity or grant a deployment effect. There is no
idempotent-effect claim. Effect owners must still use
their mutation authorization, durable attempt fencing, isolated exact approved
bytes, fresh target verification and effect-time admission. All existing effect
guards remain. These commands do not execute functional/stateful qualification
tests; a valid approval depends on those authorities' authentic signed evidence.

The effect-owner API `SelectionSession.ReserveApproval` now re-admits actual
inputs under the independently reviewed installed authority and consumes one
verified token in `CODEFLY_HOME/composition-approval-uses/`. The identity is bound
to the verified signer and token ID, not token encoding or current policy, so
rotation cannot reset use. A complete file is synced before exclusive publication;
registry and home directory entries are synced too. Concurrent processes cannot
replace an existing use, including an empty, damaged or symlinked entry. An error
after publication retains the use and reports uncertainty; cancellation or losing
the process reply never refunds it. Records contain Core admission evidence, not
the bearer token or runtime file paths. Historical inspection survives expiry and
policy rotation without treating old evidence as current authority.

This is durable single-use consumption **within one intact trusted host profile**,
not cross-host coordination, a target-wide fence, isolated execution inputs,
idempotent effects or production rollout integration. Restoring/deleting host
state can restore replayability and is not a supported recovery procedure. There
is deliberately no reserve/reset/retry CLI command: deployment owners must first
integrate qualification, exact-byte application, mutation authorization, target
fencing and observed/rollback reconciliation. Existing deployment guards remain.

`SelectionSession.PrepareApprovedInputs` provides a separate, process-local
input-isolation primitive. It verifies the host-authorized approval and actual
source inputs, copies admitted runtime artifacts and declared render outputs
into a random private directory under `CODEFLY_HOME/composition-approved-inputs/`,
then re-admits those copies through Core. Equal authenticated runtime digests can
share a copy without losing their instance/name identities. Caller-owned receipts,
qualifications and approval records are defensively copied. Private-directory
permissions and macOS allow ACLs are checked before copying any bytes.
Runtime admission and runtime/render copying open inputs nonblocking on supported
Unix platforms, then validate the opened descriptor as a bounded regular file.
A FIFO input or a file replaced by a FIFO after admission cannot wait for a peer
while retaining the product lock. Render copying keeps root-relative containment;
failed snapshot construction removes its partial copies without consuming approval.

The returned `ApprovedInputs` opens only named approved inputs as read-only
handles. Its `Reserve` method freshly checks the private copies, current selection,
host authority and literal expiry before calling durable approval consumption.
Changing or removing original cache/staging paths does not change these copies.
`Close` removes only that snapshot and never refunds approval use; cleanup errors
must be handled, and cleanup can be retried. These copies are not synced for crash
recovery, and interrupted processes may leave private directories behind. There
is no automatic resume or deployment retry. Same-UID/root tampering is not an
isolation guarantee. No production effect caller uses this API yet: target
verification/fencing, qualification execution and exact-byte application remain
required, and all effect guards remain in place.

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

Published Core `v0.3.41` supplies execution binding, the acquired-executable loader and
checked registered-group shutdown. These are consumed by selected-executor
staging. Qualified multi-instance deployment remains **blocked**.
The consumed Core also fixes the verifier hang reproduced against `3264a63d712e`:
replacing a render output with a FIFO between its path stat and blocking open
could stall verification past cancellation. Core now opens nonblocking and
validates the descriptor, retaining receipt/hash checks and root containment.
The fix belongs to Core #589, separate from CLI runtime/copy opening; no local
dependency replacement or verification bypass is used. This closes the FIFO
opening defect, not qualification or deployment integration.
The release also retains projection symlinks during Linux activation so concurrent
readers do not lose the preceding inode. It does not automatically opt executors
into `artifact-execution/v1` or authorize CLI, fleet or module publication.
An explicit rejection stopgap guards local apply/image-import entry points,
Flow deployment, platform sends, GitOps render/publication and rollback when a
participating product declares nested selections. This is not completed positive
admission at every low-level transport. It must be replaced by selection-bound
execution and effect-time Core admission, not removed to make deployments pass.

Not delivered: running the effective combination's functional/stateful tests,
selection-bound builds, OCI acquisition, production deployment authorization integration,
actual observed/rollback identity comparison, authorized upstream submission,
or hot onboarding without host restart. HTTP/Git/process regressions establish
selection and evidence plumbing, not real database retained-data recovery.
The generic environment primitives and the deprecated cell wrapper remain under
Core review; these commands do not establish a replacement wrapper or a rename.
