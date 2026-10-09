# InvokeSourceOperation descriptor

`accounts.binpb.gz` decompresses byte-for-byte to
`module/contracts/api/accounts/authority/contract.binpb` from
[codefly-dev/module-saas-starter PR #1052](https://github.com/codefly-dev/module-saas-starter/pull/1052),
commit `f4ce896d954101b164c3b511cbeea555f17302f7`.

SHA-256 of the uncompressed descriptor:
`68d440e5b983efde2c1eeabe8b89abfacad5858511ae90971e38f1d032390085`.

That head has no `contracts/runnables/` output yet. The regression derives it
from the published descriptor, whose `DatasourceService.InvokeSourceOperation`
declares slot `source`, required actions `invoke` and `read`, and `lookup: true`.
The fixture environment selects the illustrative exact ID `source-a`; it grants
no live source authority. Endpoint addresses are resolved by the CLI.

The gzip wrapper only reduces fixture size; no descriptor is rewritten or
recompiled. To refresh, take the published descriptor from a reviewed producer
commit, compress with `gzip.compress(bytes, mtime=0)`, and update this provenance
and the test's digest together.
