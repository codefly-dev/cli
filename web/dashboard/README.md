# Codefly local dashboard

A small single-page web UI, bundled inside the CLI, for inspecting what Codefly
is running locally: services, live and historical logs, the dependency graph,
injected configuration and network mappings, plus flow controls (stop/destroy).

It is served by the CLI at its local REST port (default
`http://localhost:10001`) from `codefly server` and automatically during
`codefly run service`.

## How it fits together

- **Backend:** the CLI exposes the `codefly.cli.v0.CLI` service over the
  [Connect](https://connectrpc.com) protocol (`pkg/web/go-grpc/connect.go`).
- **Client:** this app talks Connect directly over `fetch`
  (`src/api/connect.ts`) — a small, dependency-free client for the CLI's narrow,
  stable API. Typed method wrappers and message types live in `src/api/cli.ts`,
  mirroring the proto3 JSON shape of the messages in
  `core/proto/codefly/{cli,observability,base}/v0`.
- **Embedding:** the production build is written to `pkg/web/go-grpc/out/`,
  which the Go package embeds with `//go:embed out/*` and serves at `/`.

Because the client speaks Connect over plain `fetch`, there is no proto codegen
step. If the `codefly.cli.v0.CLI` contract changes, update the types in
`src/api/cli.ts` accordingly.

## Develop

```bash
cd web/dashboard
npm install
npm run dev            # Vite dev server on http://localhost:5173
```

`vite dev` proxies `/codefly.cli.v0.CLI/*` to a locally running CLI server
(default `http://localhost:10001`). Start one in another terminal with
`codefly server` (or `codefly run service`) from a workspace. Point the proxy at
a different server with `CODEFLY_CLI_SERVER=http://localhost:PORT npm run dev`.

## Build (regenerate the embedded artifact)

```bash
make dashboard         # from the repo root: npm ci && npm run build
```

This writes the static build into `pkg/web/go-grpc/out/`, replacing its
contents. **`pkg/web/go-grpc/out/` is a committed, generated artifact** — after
changing the dashboard source, run `make dashboard` and commit the regenerated
`out/` alongside your source changes so `go:embed` keeps serving the current UI.
