# Wire shapes pinned by digest

The two wire contracts this repository implements until core owns them
(`codefly/module-contract/v1`, `codefly/cell/v1`), described field by field
from the Go types and pinned by the SHA-256 of each description. A reader in
another repository pins the same digest; `TestWireShapesArePinnedByDigest`
refuses a change to either shape until the description and this digest are
regenerated with `go test ./pkg/gitops -run WireShapes -update-wire`.

- `cell.v1.txt` — sha256 `da3d1b6c39c8cd37541660188471208aeef4cda10c0d114c9c0c41c85bde934b`
- `module-contract.v1.txt` — sha256 `28e1f879d64f4b52c80e4d2ac8840c4460208ea3d61cf60ee3b35c8f5160602e`

What the digest pins, and what it does not: the descriptions are read off the
Go types' YAML tags, so the digest is a **structural drift detector** for the
tagged fields — it pins the descriptions, not the behaviour. A custom decoder
is outside it: `Ceiling.Actions` and `Ceiling.Scopes` carry no tags because
`Ceiling.UnmarshalYAML` implements the scope-ceiling union itself (the object
spelling and the two sequence spellings, with their refusals), so a change to
the forms that decoder accepts, or to any refusal rule, leaves the digest
unchanged. Those rules are held by `pkg/modulecontract`'s tests here and by
nothing shared; the shared fixtures arrive with the move to core, which is
the item this stopgap stands in for.
