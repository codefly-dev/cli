# Dashboard

The local dashboard is a Vite/React SPA in `web/dashboard`, built into
`pkg/web/go-grpc/out` with `make dashboard`, and embedded into the CLI binary
via `go:embed`. It is served at `/` and talks Connect to
`/codefly.cli.v0.CLI/` on the same port (`pkg/web/go-grpc/http.go`).

## Ports

The gRPC and REST/dashboard ports are derived deterministically from the
workspace name:

- `network.CLIServerPort(name)` — gRPC port (even).
- `network.CLIRestPort(name) = CLIServerPort(name) + 1` — HTTP/dashboard port.

`name` is the workspace name, optionally suffixed with `-<naming-scope>` when
`--naming-scope` is passed. `CODEFLY_CLI_SERVER_PORT` overrides the gRPC port
(the REST port follows at +1).

## Reaching it

- `codefly run service <svc> --cli-server [--open]` — live dashboard, backed
  by the run's own flow.
- `codefly server [--open]` — attaches to a `--cli-server` run already
  serving this workspace if one is up, otherwise serves an inventory-only
  dashboard with no live runtime state.

See [docs/commands.md](commands.md) for the full flag reference.

## Tabs

| Tab | RPC(s) |
|-----|--------|
| Services | `GetWorkspaceInventory` |
| Logs | `Logs` stream + `ActiveLogHistory` |
| Dependency Graph | `GetWorkspaceServiceDependencyGraph` |
| Config & Network | `GetRuntimeConfigurations`, `GetDependenciesConfigurations`, `GetDependenciesNetworkMappings` |

## Developing

```bash
cd web/dashboard && npm ci && npm run dev
```

Point it at a running `codefly run service <svc> --cli-server` — CORS is open
for loopback origins. When done, rebuild the embedded assets and commit them:

```bash
make dashboard
```

`pkg/web/go-grpc/out` is a committed, generated artifact; it must be
regenerated and committed whenever `web/dashboard` source changes.
