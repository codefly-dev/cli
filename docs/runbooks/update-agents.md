# Runbook: Rebuild the CLI and agents from local source

Build the CLI (and optionally every agent) from your local checkout and install over the running
binary. This is the fast local-dev loop that replaces the old install script.

## When to use

- You changed CLI source and want the change live in your `codefly` binary.
- You changed an agent's source and want the workspace's agents rebuilt to match.

## Commands

### Rebuild + install the CLI

```bash
codefly self build
```

Builds from source and installs over the currently running binary. Useful flags:

- `--dir ./cli` — build from a specific checkout instead of the detected one.
- `--output /usr/local/bin/codefly` — write the binary to a specific path.

### Rebuild the CLI *and* all agents

```bash
codefly self build --with-agents
```

After installing the CLI, this rebuilds **every canonical agent repository** in the Codefly
workspace. Flags:

- `--native-only` — build only host-platform agent binaries; skip the Linux/amd64 container
  cross-build (the local-dev fast path).
- `-j N` — parallelism (number of agents built concurrently).
- `--audit-agents` — run the `govulncheck` audit on each agent (slow; **off by default**, because
  `--with-agents` is meant as a quick "pick up local changes" loop).

Quarantined agent packages are skipped by `--with-agents` (and by `agent build --all`).

### Get / refresh the canonical checkouts

`--with-agents` builds the canonical repositories in the workspace. To create or update those
checkouts:

```bash
codefly self pull
```

For a fresh flat checkout from scratch (no `codefly` binary required):

```bash
scripts/bootstrap.sh --dir ~/codefly.dev
```

## Gotchas

- **Two-pass build.** When running `codefly self build --with-agents`, a first pass may be needed
  to get a working CLI before it can build the agents; if agents fail on the first run, re-run the
  command. (See the `self-build-agents-gotchas` note.)
- **Go source-packager version pin.** The Go source packager used during `--with-agents` is
  version-pinned; a toolchain bump can require updating it. If a Go bump breaks `--with-agents`,
  check that pin — and follow [bump-go-version.md](bump-go-version.md) to move all repos together.
- **Installing agents for end users** (not local dev) is a different path: `codefly install`,
  `codefly update`, and the `codefly agents` subcommands manage installed agents from releases,
  rather than rebuilding from source.

## Verify

```bash
codefly version           # confirms the new CLI is installed
codefly agents            # inspect installed/available agents
```

## Checklist

- [ ] `codefly self build` (or `--with-agents`) completed without error
- [ ] `codefly version` reflects the rebuild
- [ ] For agents: the right canonical checkouts exist (`codefly self pull` if not)
- [ ] Re-ran once if the first `--with-agents` pass failed before the CLI was in place
