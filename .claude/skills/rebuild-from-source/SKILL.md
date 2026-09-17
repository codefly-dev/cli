---
name: rebuild-from-source
description: Rebuild the codefly CLI, and optionally every canonical agent, from local checkouts and install over the running binary via "codefly self build". Use when a CLI or agent source change must be live in the codefly binary before testing, when agents in a workspace are stale relative to local source, or when "self build --with-agents" fails. Not for installing released agents for end users.
---

# Rebuild the CLI and agents from local source

**Flags, checkout setup, and the verification checklist:**
[docs/runbooks/update-agents.md](../../../docs/runbooks/update-agents.md).

## What must not be skipped

- **`codefly self build` installs over the running binary.** After a CLI source change, nothing
  you test reflects that change until you run it. `codefly version` confirms the swap.
- **`--with-agents` may need two passes.** The first pass can be what produces a CLI capable of
  building the agents; if agents fail on the first run, re-run before investigating.
- **`--native-only` is the local-dev fast path** — it skips the Linux/amd64 container
  cross-build. Reach for `-j N` on parallelism, and leave `--audit-agents` off unless you want
  the slow `govulncheck` pass.
- **Agents build from the canonical checkouts**, not from wherever you happen to be.
  `codefly self pull` creates or refreshes them; `scripts/bootstrap.sh --dir ~/codefly.dev` does
  a flat checkout from scratch with no `codefly` binary needed.

## Distinct from installing agents

`codefly install` / `update` / `agents` manage agents from *releases*. This skill is the
source loop. Don't mix them when diagnosing a stale agent — check which path produced the binary
in `~/.codefly/` first.
