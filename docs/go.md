# Go Standards

## Formatting

https://github.com/mvdan/gofumpt

## Linting

Run `make lint` to build and run golangci-lint v2.14.0 with the module's Go
toolchain. CI uses the same version and builds it from source too. Keep the
Makefile and `.github/workflows/go.yml` pins together: v2.13.1 cannot read Go
1.27.2's export data (version 5), even when compiled with Go 1.27.2.

`make hooks` (once per clone) points git at `.githooks`: `pre-commit` refuses a
commit whose staged Go files are not gofmt-clean, and `pre-push` runs the same
pinned golangci-lint over every package the push changes, plus the CLI reference
test when a command or `docs/generated/cli-reference.md` changed. These are the checks that
otherwise bounce a PR in the merge queue twenty minutes later; `--no-verify`
skips them.
