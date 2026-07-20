# `codefly doctor workspace` — provider-backed qualification record

Local qualification of the readiness check against the real 1Password CLI (no
fake provider, no canned resolver). Recorded per codefly-dev/cli#82.

## Environment

- Date: 2026-07-20
- CLI revision: `c86f83394105e5672e1ff2f056e3826a6b5c45c9` (+ the doctor-workspace change under review)
- Core: `github.com/codefly-dev/core v0.2.24`
- Provider: 1Password CLI `op` 2.35.0 at `/opt/homebrew/bin/op` (real binary; no account signed in on the qualification host)

## Fixture manifests (sha256)

```
72d538bb972e6cb787632bf889ae6f741777f890b1ba5cb9adbd1cdb290997d5  configurations/local/auth0.secret.env
68794ffc185ed878f9f05eba9c5ae00a6afac7c1f56d82256a39818b8f0c6df2  modules/backend/module.codefly.yaml
a62677306371bdb9961ecda77c789d6cd9e32a4dc829d50c5af20ab3dc60de03  modules/backend/services/api/service.codefly.yaml
6c5cef454dc7cb2ec809dfc709fce2b4073eb0c2559b081e0826fbfe3ce3d9cd  workspace.codefly.yaml
```

The workspace declares `environments: [{name: local, secrets: [{kind:
1password}]}]`; the `api` service declares
`workspace-configuration-dependencies: [auth0]`;
`configurations/local/auth0.secret.env` holds a single `op://…` reference
(reference redacted here by policy — the manifest hash above covers it).

## Run 1 — real provider, not authenticated

```
$ codefly doctor workspace --json
```

Result (redacted to the diagnostic): the real `op read` was invoked and failed;
the doctor classified it and exited `1`:

```json
{
  "code": "provider_authentication_required",
  "name": "secret references",
  "status": "fail",
  "message": "the op backend requires authentication (failed on key CLIENT_SECRET of configuration \"auth0\")",
  "remediation": "sign in to the provider (e.g. `op signin`) and re-run"
}
```

The raw `op://` reference, provider stderr, and all values are absent from both
JSON and human output. No file was created or modified in the fixture
(specifically, no `configurations/<env>` directory materialized).

## Run 2 — ready path

Same fixture with the reference replaced by a local plaintext value (the
sanctioned local escape hatch): all checks `ok`, `status: "ready"`, exit `0`,
and the summary reports the secret value count without echoing any value.

## Scope

This qualification exercises the real provider executable end to end through
the locked/unauthenticated path — the exact state a fresh worktree or CI host
is in. A signed-in success-path run (`op read` returning a value that the
doctor resolves in memory and discards) requires an interactive 1Password
sign-in and should be re-recorded here by a developer with a signed-in
account:

```
op signin && codefly doctor workspace --json
```
