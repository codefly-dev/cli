# Wire shapes pinned by digest

The two wire contracts this repository implements until core owns them
(`codefly/module-contract/v1`, `codefly/cell/v1`), described field by field
from the Go types and pinned by the SHA-256 of each description. A reader in
another repository pins the same digest; `TestWireShapesArePinnedByDigest`
refuses a change to either shape until the description and this digest are
regenerated with `go test ./pkg/gitops -run WireShapes -update-wire`.

- `cell.v1.txt` — sha256 `da3d1b6c39c8cd37541660188471208aeef4cda10c0d114c9c0c41c85bde934b`
- `module-contract.v1.txt` — sha256 `28e1f879d64f4b52c80e4d2ac8840c4460208ea3d61cf60ee3b35c8f5160602e`
