# CLI releases and updates

Codefly releases are immutable GitHub releases built by GoReleaser. Each
semantic tag publishes four archives:

- `codefly_<version>_darwin_amd64.tar.gz`
- `codefly_<version>_darwin_arm64.tar.gz`
- `codefly_<version>_linux_amd64.tar.gz`
- `codefly_<version>_linux_arm64.tar.gz`

The same release contains `checksums.txt`, its publisher signature
`checksums.txt.sig`, one Syft SBOM per archive, and GitHub artifact
attestations. The binary reports the tag-derived semantic version, commit, and
UTC build date with `codefly version --json`. Published macOS binaries are
Developer ID signed and notarized before they enter either release archives or
the Homebrew cask.

## Install

Homebrew owns its installed files:

```shell
brew install --cask codefly-dev/cli/codefly
```

For a direct installation, download the archive for the required release,
operating system, and architecture from
`https://github.com/codefly-dev/cli/releases/tag/v<version>`. Extract `codefly`
to a user-owned executable directory such as `~/.local/bin`.

The old `codefly-dev/cli-releases` repository and its moving binary names are
not part of the release trust path and receive no fallback updates. Existing
users of the legacy Homebrew formula should migrate explicitly:

```shell
brew uninstall codefly
brew install --cask codefly-dev/cli/codefly
```

## Check and update

Check without changing anything:

```shell
codefly self check-update
codefly self check-update --channel beta
codefly self check-update --json
```

Install the latest eligible stable release into a directly owned binary:

```shell
codefly self update
```

The command asks before applying the replacement. Automation must give consent
with `--yes`. Prereleases require the explicit `--channel beta` option.
Installing a channel-selected version older than the running binary also
requires `--allow-downgrade`.

The updater downloads into `~/.codefly/update/staging`, authenticates the
signed checksum manifest with Codefly's pinned publisher certificate, checks
the archive checksum and executable platform, then atomically replaces the
exact running executable. A failed verification leaves the installed binary
unchanged. A failed replacement restores the previous binary; if the new
binary was committed but cleanup failed, the command reports the retained
path.

Install ownership is conservative:

| Installation | Update behavior |
| --- | --- |
| Direct release binary | Verified staged replacement |
| Homebrew formula or cask | Print the Homebrew migration or upgrade command |
| Source checkout, source build, or symlink | Print the `codefly self build` workflow |
| Container or managed prefix | Print the deployment-owned upgrade path |
| Unknown | Refuse replacement and report the installer-owned path |

Managed launchers can set `CODEFLY_INSTALL_KIND=managed` or
`CODEFLY_INSTALL_KIND=container` to make ownership explicit. The override
cannot grant direct-update permission.

Eligible released builds perform a best-effort stable-channel check at most
once per 24 hours after ordinary commands start. State is caller-owned at
`~/.codefly/update/state.json`. The check has a short timeout, never changes
the command result, preserves the last successful release cache, and prints a
given release notice once. Explicit checks report network and rate-limit
errors; a rate-limited response may return the preserved cached status.

## Verify a release manually

Download the archive, `checksums.txt`, and `checksums.txt.sig` from the same
immutable release. The pinned certificate is
[`pkg/cliupdate/release-signing-cert.pem`](../pkg/cliupdate/release-signing-cert.pem).
Its SHA-256 certificate fingerprint is:

```text
90:8A:DE:62:61:EB:3A:48:13:37:F7:D4:1A:5D:80:FF:08:3A:01:4E:53:4E:AD:84:34:76:ED:7F:3D:D1:13:ED
```

Verify the signature and checksums:

```shell
openssl x509 -in release-signing-cert.pem -pubkey -noout > release-public-key.pem
openssl dgst -sha256 -verify release-public-key.pem \
  -signature checksums.txt.sig checksums.txt
shasum -a 256 -c checksums.txt
gh attestation verify codefly_<version>_<os>_<arch>.tar.gz \
  --repo codefly-dev/cli
```

`checksums.txt` covers every archive and SBOM, so download all listed files
when verifying the complete manifest. Tokens and redirected signed download
URLs are never included in CLI output.

## Roll back

Every published version remains available by its immutable semantic tag. To
roll back a direct installation, manually download and verify the older
version, then replace the binary using the same user-owned destination.
Homebrew users should use Homebrew's version-management workflow; containers
and managed installations should redeploy their previous image or package.
Source checkouts should select the desired commit and run
`codefly self build`.

CLI release tags cannot be moved or re-tagged. A bad release is corrected by
publishing a new semantic version.
