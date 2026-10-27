# Generated client versions in a module release

`codefly publish` includes committed generated clients whose `sources` name a
package released by the module workspace. It finds tracked
`library.codefly.yaml` files throughout the repository, including `clients/`;
ignored caches and dependencies are not release inputs.

Before changing versions, publication checks that each selected source names the
owner's current package version and contract digest. It verifies copied bytes
and catalog metadata against the committed producer, including protobuf service
subsets through the same narrowing code used by generation. Stale clients must
be regenerated and committed
before releasing. Every selected artifact must be tracked and remain inside the
repository; symlinks are refused.

The release commit updates the selected source versions and copied catalogs. A
single-source client also receives the release version in its library manifest
and TypeScript/Python package manifests. Python stamping and publication both
read the TOML `project.version` field; tool versions, comments and unrelated
formatting remain unchanged. An aggregate keeps its own library and
language package versions; unrelated source entries stay unchanged. Contract
bytes and generation provenance are preserved. Go clients obtain their version
from the release tag.

These changes use the existing release transaction. If regeneration or a
release gate fails before commit, all client version changes are restored with
the module manifests and generated contracts.
