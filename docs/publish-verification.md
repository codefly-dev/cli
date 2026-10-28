# Publication integrity and installer access

For executable agent releases, `codefly publish` verifies the archives after
publication whether the manifest assigns publication to `cli` or `workflow`.
Each required loader archive must appear in the release's producer checksum
list. Its SHA-256 must agree with GitHub's upload digest, and the downloaded
bytes must match the recorded size and digest. A missing archive, draft release,
missing checksum evidence, or mismatched bytes makes publication verification
fail. A published tag is not rolled back when verification fails.

When the CLI owns the uploads, it stages a deterministic `checksums.txt`
covering every archive and SBOM. Before uploading any file, it compares every
existing asset with the staged bytes. A retry can fill missing assets or reuse
identical assets, but cannot replace an existing archive, SBOM or checksum list.
It uploads the checksum list last, after the files it describes. A CLI-staged
SBOM is required and verified by its declared filename. Workflow-owned SBOMs
named `<archive>.sbom.json` are verified when present; a workflow need not publish
one.

After integrity verification, publication checks that each exact loader URL is
reachable using Core's installer credential rule: `GITHUB_TOKEN`, otherwise
`GH_TOKEN`, otherwise anonymous access. Publishing may use a `gh auth login`
credential; installer verification deliberately does not. A public release can
pass without an environment token. A private release needs a credential that
the installer can actually use.

These checks establish published archive integrity and installer access. They
do not start the agent or establish its runtime capabilities. Core owns download
verification and installed-cache admission; the release's agent CI owns runtime
qualification.
