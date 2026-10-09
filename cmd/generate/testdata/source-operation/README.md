# Source-operation contracts and served surfaces

Source: [codefly-dev/module-saas-starter PR #1052](https://github.com/codefly-dev/module-saas-starter/pull/1052),
commit `86ee51fd8b4d66aad9da2509483e14d3c7534764`.

`accounts.binpb.gz` decompresses byte-for-byte to
`module/contracts/api/accounts/connect/contract.binpb` (identical on authority).
Uncompressed SHA-256:
`925e72a5ba36b8e53362643e501dc7337b35c4a77277fc172a0ba72bf8c4ba8f`.
The gzip wrapper only reduces size: `gzip.compress(bytes, mtime=0)`, without
rewriting or recompiling the descriptor.

`InvokeSourceOperation.json` and `PruneSourceOperationReceipts.json` are the
exact committed `runnable-package.json` bytes under
`module/contracts/runnables/accounts/connect/<Method>/`. The test verifies their
package digests and compares their contracts to the prepared bindings.
Both methods declare slot `source`, actions `invoke` and `read`, and `lookup: true`.
The fixture selects illustrative exact ID `source-a`; it grants no live source
authority. The CLI resolves endpoint addresses.

`surfaces.json` is a **fixture projection**, not an artifact emitted by that
producer head. It uses the generic `spec.api-contract-surfaces` input added in
this PR. The source lists are:

- `authority`: the 31 literal entries of `moduleAuthorityProcedures` in
  `module/services/accounts/code/pkg/business/module_authority_endpoint.go`,
  grouped by protobuf service and sorted. The authority listener uses this same
  set through `IsModuleAuthorityProcedure` in `grpc_auth_interceptor.go`.
- `connect`: the 258 procedures in
  `module/services/accounts/generated/service-catalog.json`'s `services` array,
  with `full_name` mapped to the Core catalog's `fullName` field. Generated
  `connect_registration_catalog_gen.go` registers that complete catalog.

Neither Invoke/Lookup nor Prune/Lookup belongs to authority's set. The host's
`generated/gateway-routes.json` also routes those four procedures to Connect,
but is not a complete listener inventory because it omits internal methods.

The host must generate this inventory from its routing owners, set
`spec.api-contract-surfaces`, and rerun contract/runnable generation. Its existing
published catalog at this head incorrectly lists all procedures on authority.
The test qualifies the CLI export-input/derivation/preparation boundary against
these exact contracts and declared surfaces; it does not claim that the old host
catalog is corrected or that a live invocation was exercised. Never widen the
authority listener or hand-edit generated output to satisfy the test.
