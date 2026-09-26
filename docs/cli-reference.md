# codefly command reference

Generated from the command tree by
`go test ./cmd -run TestCLIReferenceIsCurrent -update-reference`. Do not edit by hand:
a command's text lives on the command, so `--help` and this page always agree.

This page is the complete list. [commands.md](commands.md) is the narrative guide —
what a verb is for, and the chains it takes part in.

## `codefly`

Build, run, test, and deploy services in a Codefly workspace

```
Codefly turns service development operations into consistent, agent-backed workflows.

Use it to create workspace resources, run and test services locally, build
container images, and deploy services to configured environments.
```

```
codefly [flags]
```

Flags:

```
  -d, --debug                Enable debug mode
      --focus                Enable focus log mode
      --local-agents         Resolve agent versions from ~/.codefly/agents/ only (skip GitHub). Equivalent to setting CODEFLY_AGENT_SOURCE=local. Use when working on local agent builds or offline.
      --plugin-path string   Override the codefly home directory (default: ~/.codefly). Plugins, containers and logs resolve from <plugin-path>/agents, <plugin-path>/containers, <plugin-path>/logs. Equivalent to setting CODEFLY_HOME.
      --timestamps           Prefix log output with a wall-clock timestamp (HH:MM:SS); use --timestamps=false to hide it (default true)
      --trace                Enable trace mode
      --track string         Tracker of actions -- advanced usage
```

Subcommands:

- [`codefly add`](#codefly-add)
- [`codefly agent`](#codefly-agent)
- [`codefly audit`](#codefly-audit)
- [`codefly build`](#codefly-build)
- [`codefly ci`](#codefly-ci)
- [`codefly clear`](#codefly-clear)
- [`codefly companion`](#codefly-companion)
- [`codefly compile`](#codefly-compile)
- [`codefly completion`](#codefly-completion)
- [`codefly composition`](#codefly-composition)
- [`codefly config`](#codefly-config)
- [`codefly daemon`](#codefly-daemon)
- [`codefly delete`](#codefly-delete)
- [`codefly deploy`](#codefly-deploy)
- [`codefly doctor`](#codefly-doctor)
- [`codefly endpoint`](#codefly-endpoint)
- [`codefly environment`](#codefly-environment)
- [`codefly explain`](#codefly-explain)
- [`codefly expose`](#codefly-expose)
- [`codefly fix`](#codefly-fix)
- [`codefly generate`](#codefly-generate)
- [`codefly get`](#codefly-get)
- [`codefly import`](#codefly-import)
- [`codefly init`](#codefly-init)
- [`codefly install`](#codefly-install)
- [`codefly lint`](#codefly-lint)
- [`codefly list`](#codefly-list)
- [`codefly login`](#codefly-login)
- [`codefly logs`](#codefly-logs)
- [`codefly mcp`](#codefly-mcp)
- [`codefly open`](#codefly-open)
- [`codefly override`](#codefly-override)
- [`codefly package`](#codefly-package)
- [`codefly provider`](#codefly-provider)
- [`codefly ps`](#codefly-ps)
- [`codefly publish`](#codefly-publish)
- [`codefly replay`](#codefly-replay)
- [`codefly run`](#codefly-run)
- [`codefly sbom`](#codefly-sbom)
- [`codefly self`](#codefly-self)
- [`codefly server`](#codefly-server)
- [`codefly service`](#codefly-service)
- [`codefly show`](#codefly-show)
- [`codefly status`](#codefly-status)
- [`codefly stop`](#codefly-stop)
- [`codefly sync`](#codefly-sync)
- [`codefly terminal`](#codefly-terminal)
- [`codefly test`](#codefly-test)
- [`codefly update`](#codefly-update)
- [`codefly upgrade`](#codefly-upgrade)
- [`codefly version`](#codefly-version)

## `codefly add`

Create modules, services, dependencies, and other workspace resources

```
codefly add <subcommand>
```

Subcommands:

- [`codefly add application`](#codefly-add-application)
- [`codefly add application-dependency`](#codefly-add-application-dependency)
- [`codefly add dependency`](#codefly-add-dependency)
- [`codefly add job`](#codefly-add-job)
- [`codefly add library`](#codefly-add-library)
- [`codefly add library-dependency`](#codefly-add-library-dependency)
- [`codefly add module`](#codefly-add-module)
- [`codefly add runnable`](#codefly-add-runnable)
- [`codefly add service`](#codefly-add-service)

## `codefly add application`

Create an agent-backed application in a module

```
codefly add application [flags]
```

Flags:

```
      --agent string   Agent to build/run the application
      --default        Use default options
      --override       Override existing application
```

## `codefly add application-dependency`

Link an application to another application or service

```
codefly add application-dependency [flags]
```

## `codefly add dependency`

Link a service to another service it requires

```
codefly add dependency [flags]
```

## `codefly add job`

Create a scheduled or one-shot job in a module

```
Add a job (scheduled or one-shot task) to a module.

Jobs are ephemeral execution units for tasks like:
- Database migrations
- Data processing / ETL
- Scheduled tasks (cleanup, reports)
- Deployment tasks

Examples:
  # Create a one-shot job
  codefly add job db-migration --module=backend

  # Create a scheduled job with cron expression
  codefly add job cleanup --module=backend --type=scheduled --schedule="0 0 * * *"

  # Create a job with specific timeout
  codefly add job data-import --module=backend --timeout=1h
```

```
codefly add job [name]
```

Flags:

```
      --agent string      Job agent (e.g., go-job)
      --module string     Module name
      --schedule string   Cron schedule for scheduled jobs
      --timeout string    Job timeout (e.g., 5m, 1h) (default "30m")
      --type string       Execution type (one-shot, scheduled, triggered) (default "one-shot")
```

## `codefly add library`

Create an internal library for sharing code across services

```
Add an internal shared library to the workspace.

Libraries are internal shared code that can be used by multiple services.
They support versioning through git tags and can be linked as git submodules.

Examples:
  # Create a new local library with Go support
  codefly add library shared-models --languages=go

  # Create a library with multiple language support
  codefly add library utils --languages=go,python

  # Add an external library as a git submodule
  codefly add library shared-models --git=git@github.com:myorg/shared-models.git
```

```
codefly add library [name]
```

Flags:

```
      --branch string       Git branch for submodule (default "main")
      --git string          Git remote URL to add as submodule
  -l, --languages strings   Languages to support (e.g., go,python,typescript)
```

## `codefly add library-dependency`

Link an internal library to a service with optional local setup

```
Add an internal library as a dependency to a service.

This will:
1. Add the library to the service's library-dependencies in service.codefly.yaml
2. Optionally set up local development (go.mod replace, pip -e, npm link)

Examples:
  # Add library dependency with version constraint
  codefly add library-dependency shared-models --service=api --module=backend --version="^1.0.0"

  # Add with automatic local development setup
  codefly add library-dependency shared-models --service=api --module=backend --setup-local

  # Specify which language exports to use
  codefly add library-dependency shared-models --service=api --module=backend --languages=go
```

```
codefly add library-dependency [library-name]
```

Flags:

```
      --languages strings   Language exports to use
      --module string       Module name
      --service string      Service name
      --setup-local         Setup local development immediately
      --version string      Version constraint (e.g., ^1.0.0)
```

## `codefly add module`

Create a module to group related services and jobs

```
codefly add module [flags]
```

Flags:

```
      --agent string      Module template agent (e.g. user-management, rag)
  -i, --interactive       interactive mode
      --source string     Compose an out-of-repo module at a local path; the path lands in codefly.local.yaml, not committed config
      --version string    Committed version constraint recorded with a composed module's identity (default "latest")
      --worktree string   Compose an out-of-repo module by <owner/repo>@<ref>; the resolver finds your local worktree (location in codefly.local.yaml)
      --yes               Skip confirmation prompts (non-interactive/MCP mode)
```

## `codefly add runnable`

Create a typed Runnable through its language agent

```
Create a Runnable declaration and scaffold its handler through the selected
agent over gRPC. Both the pinned agent and handler path are explicit. Edit the
generated declaration's input/output contract and handler before building.

Creation is unattended. It fails if the name already exists or the agent lacks
the Runnable Builder lifecycle. It does not install or invoke work.
```

```
codefly add runnable <name>
```

Flags:

```
      --agent string     Pinned Runnable agent (publisher/name:version)
      --handler string   Handler path relative to the Runnable directory
      --json             Emit the created resource as JSON
      --module string    Owning module (defaults to the current module)
```

## `codefly add service`

Create an agent-backed service in a module

```
codefly add service [flags]
```

Flags:

```
      --agent string   Instance agent to get started
      --default        Use default options
      --override       Override existing service
```

## `codefly agent`

Develop, inspect, install, and validate Codefly service agents

```
codefly agent <subcommand>
```

Subcommands:

- [`codefly agent build`](#codefly-agent-build)
- [`codefly agent ci`](#codefly-agent-ci)
- [`codefly agent deps`](#codefly-agent-deps)
- [`codefly agent generate`](#codefly-agent-generate)
- [`codefly agent info`](#codefly-agent-info)
- [`codefly agent install`](#codefly-agent-install)
- [`codefly agent list`](#codefly-agent-list)
- [`codefly agent promote-source`](#codefly-agent-promote-source)
- [`codefly agent release`](#codefly-agent-release)
- [`codefly agent verify-platform`](#codefly-agent-verify-platform)
- [`codefly agent versions`](#codefly-agent-versions)

## `codefly agent build`

Compile an agent from source and install it for local use

```
Build compiles the agent in the current (or specified) directory and
installs the binary to ~/.codefly/agents/ so it can be loaded by the
Gateway daemon.

The directory must contain an agent.codefly.yaml with publisher, kind,
name, and version fields.

When run inside the codefly.dev monorepo, local replace directives for
wool and core are added automatically so go mod tidy succeeds without
requiring published module versions.

Use --all from the agents/services/ directory (or any parent containing
agent directories) to build every agent that has an agent.codefly.yaml.

Examples:
  cd agents/services/go-generic && codefly agent build
  codefly agent build --dir ./agents/services/go-generic
  cd agents/services && codefly agent build --all
```

```
codefly agent build [flags]
```

Flags:

```
      --all            Build all agents found in the current directory tree
      --dir string     Agent source directory (default: current directory)
      --fail-on-vuln   Fail the build if any HIGH/CRITICAL vulnerability is found
  -j, --jobs int       Max agents to build in parallel with --all (default: number of CPUs)
      --native-only    Build only the host-platform binary; skip the Linux/amd64 container cross-build (local dev fast path)
      --skip-audit     Explicitly skip the post-build vulnerability audit
```

## `codefly agent ci`

Validate an agent against source, release, and generated-service CI gates

```
codefly agent ci [flags]
```

Flags:

```
      --dir string         Agent source directory (default: current directory)
      --fail-on-vuln       Fail on actionable HIGH or CRITICAL agent vulnerabilities (default true)
      --format string      Final agent CI report format: text or json (default "text")
      --native-only        Build only the host agent binary; skip the Linux/amd64 release artifact
      --output string      Agent CI report and artifact directory (relative to the agent repository) (default ".codefly/agent-ci")
      --skip-audit         Explicitly skip the agent-binary vulnerability audit
      --skip-conformance   Explicitly skip fresh generated-service conformance
```

## `codefly agent deps`

Switch an agent between a local core checkout and a published version

```
Manage how an agent resolves its codefly-core dependency.

By default (or with --link) it wires a local go.work so the agent builds against
the monorepo's core SOURCE — local builds always work, with no network pull
(internal-only sync). CI ignores go.work (it's gitignored) and builds against the
published core in go.mod.

  --link            wire go.work -> local core (default; internal, no pull)
  --unlink          remove the local go.work (revert to published deps)
  --pin <version>   pin every lock the agent owns (go.mod, nested base fixtures,
                    and their factory templates) to a published core version +
                    tidy + verify the standalone build and that every owned
                    module passes 'go mod tidy -diff' (the ONLY mode that
                    pulls); "latest" allowed
  --dependency <module>@<version>
                    with --pin, also update an explicitly selected dependency
                    wherever it is already required (repeatable)
  --all             apply to every agent under the directory tree
  --dir <path>      target agent directory (default: current directory)

Examples:
  codefly agent deps                       # link this agent to local core
  codefly agent deps --all                 # link every agent in the tree
  codefly agent deps --pin latest          # pin to latest published core
  codefly agent deps --unlink --all        # drop local go.work everywhere
```

```
codefly agent deps [flags]
```

Flags:

```
      --all                      Apply to every agent found in the directory tree
      --dependency stringArray   With --pin, also pin an existing module@version across owned locks (repeatable)
      --dir string               Agent source directory (default: current directory)
      --link                     Wire go.work -> local core for dev builds (default action)
      --pin string               Pin go.mod to a published core version (e.g. latest, v0.1.164) + tidy + verify
      --unlink                   Remove the local go.work (revert to published deps)
```

## `codefly agent generate`

Generate a Codefly service template from an existing source directory

```
codefly agent generate [flags]
```

Flags:

```
      --service string   NewDir to the code to turn into library
```

## `codefly agent info`

Inspect metadata reported by an installed agent

```
codefly agent info <subcommand>
```

Subcommands:

- [`codefly agent info runnable`](#codefly-agent-info-runnable)
- [`codefly agent info service`](#codefly-agent-info-service)

## `codefly agent info runnable`

Load a runnable agent and print its reported capabilities

```
Load a runnable language agent over gRPC and print what it reports about itself.

Examples:
  codefly agent info runnable --agent=python:0.0.1
```

```
codefly agent info runnable [flags]
```

Flags:

```
      --agent string   Runnable agent to inspect (name:version)
```

## `codefly agent info service`

Load a service agent and print its reported capabilities

```
codefly agent info service [flags]
```

Flags:

```
      --agent string   Instance agentInput to get started
```

## `codefly agent install`

Download a released agent into the local Codefly cache

```
Download a released agent binary into the local Codefly cache.

--kind selects which agent kind to install; a runnable language agent is
installed by its language name, exactly like a service agent.

Examples:
  codefly agent install go-grpc:0.1.4
  codefly agent install python:0.0.1 --kind=runnable
```

```
codefly agent install <publisher/name[:version]>
```

Flags:

```
      --kind string      Agent kind to install (service, runnable, …) (default "service")
      --version string   Version to install (defaults to the version in the identifier or latest)
```

## `codefly agent list`

List every agent pinned in the workspace and its resolvability

```
codefly agent list [flags]
```

Flags:

```
      --json   Emit the workspace agent inventory as JSON
```

## `codefly agent promote-source`

Report the replacement for static source-agent promotion

```
codefly agent promote-source [flags]
```

## `codefly agent release`

Bump, PR, tag-on-merge, and verify a service agent's release

```
release turns the manual per-repo release cascade into one verb:

  1. gate locally BEFORE tagging: pin core (--pin) then run agent CI
  2. bump the manifest to the next version above the authoritative remote tag
  3. open a PR from a release branch — a human merges it, per branch policy
  4. tag the merge commit, then verify the release published a downloadable
     asset — failing loudly if the tag shipped no artifact

It is safe to re-run: an open PR is waited on, a merged PR is tagged, and a
release already tagged through this flow whose asset has not published yet is
re-verified rather than superseded by a higher version. Pass --no-wait to open
the PR and stop (tag + verify on a later re-run).

Requires the gh CLI to be authenticated (or GITHUB_TOKEN / GH_TOKEN set).

Examples:
  codefly agent release                     # patch bump, wait for merge, verify
  codefly agent release --pin latest        # pin core, then release
  codefly agent release --bump minor
  codefly agent release --no-wait           # open the PR only
```

```
codefly agent release [flags]
```

Flags:

```
      --bump string       Version bump from the latest tag: patch | minor | major (default "patch")
      --dir string        Agent source directory (default: current directory)
      --no-wait           Open the PR and stop; re-run after merge to tag and verify
      --pin string        Pin core to this published version before the CI gate (e.g. latest, v0.3.11)
      --platform string   Additional os_arch the release must ship (e.g. linux_arm64); linux_amd64 is always required
```

## `codefly agent verify-platform`

Verify selected agents ship a release asset for a platform

```
codefly agent verify-platform <os_arch> <publisher/name>...
```

## `codefly agent versions`

List an agent's versions and whether each is resolvable

```
codefly agent versions <publisher/name>
```

Flags:

```
      --json   Emit the version inventory as JSON
```

## `codefly audit`

Find vulnerable and outdated dependencies in services or workspaces

```
codefly audit <subcommand>
```

Subcommands:

- [`codefly audit go`](#codefly-audit-go)
- [`codefly audit service`](#codefly-audit-service)
- [`codefly audit workspace`](#codefly-audit-workspace)

## `codefly audit go`

Scan a Go module for vulnerabilities and stale dependencies

```
Audit the Go module at --dir (default: current directory) with govulncheck.

Findings are partitioned against the nearest .govulncheck.yaml (walking up
from --dir):
  - suppressed  — id listed in .govulncheck.yaml (reviewed, tracked)
  - actionable  — an upstream fix is available (BLOCKS with --fail-on-vuln)
  - unpatched   — no upstream fix yet (reported, never blocks)

Suppression entries whose 'reviewed:' date is older than --stale-days fail
the command so the rationale gets re-verified.

This replaces scripts/govulncheck.sh; CI calls it after building the CLI.
```

```
codefly audit go [flags]
```

Flags:

```
      --dir string       Go module directory to audit (default: current directory)
      --fail-on-vuln     Exit non-zero on an actionable finding or stale suppression (default true)
      --stale-days int   Fail when a suppression's reviewed date is older than this many days (0 disables) (default 45)
```

## `codefly audit service`

Scan one service for vulnerable and outdated dependencies

```
Run the service agent's Builder.Audit RPC. Reports CVEs from the
language's canonical scanner (govulncheck, npm audit, uv/pip-audit,
OSV Scanner, or Trivy) plus available dependency releases. Read-only — never
modifies code.
```

```
codefly audit service [name]
```

Flags:

```
      --fail-on-vuln   Exit non-zero if any HIGH/CRITICAL finding is present
      --include-dev    Include development/test-only dependencies
      --json           Emit raw JSON instead of a table
      --outdated       Also report available dependency releases (default true)
```

## `codefly audit workspace`

Scan every workspace service for vulnerable and outdated dependencies

```
codefly audit workspace [flags]
```

Flags:

```
      --fail-on-vuln   Exit non-zero if any HIGH/CRITICAL finding is present
      --include-dev    Include development/test-only dependencies
      --json           Emit raw JSON instead of a table
      --outdated       Also report outdated patch+minor releases (default true)
```

## `codefly build`

Build service/module images or a native Runnable package

```
codefly build <subcommand>
```

Subcommands:

- [`codefly build module`](#codefly-build-module)
- [`codefly build runnable`](#codefly-build-runnable)
- [`codefly build service`](#codefly-build-service)

## `codefly build module`

Build container images for every service in a module

```
codefly build module [name]
```

Flags:

```
      --cache-backend string     Cache transport backend (registry) (default "registry")
      --cache-from stringArray   Trusted registry cache repository to import (repeatable, without tag)
      --cache-mode string        Exported layers: max (default, includes dependencies) or min
      --cache-scope string       Stable workspace and trust-domain cache scope; service, recipe and platform are added automatically
      --cache-to stringArray     Registry cache repository to publish (repeatable; omit for read-only builds)
      --env string               Environment to build for (looks up registry/cluster from workspace.codefly.yaml) (default "local")
      --org string               Image registry override (wins over env's registry.url)
      --push                     Push the images to the registry
      --stand-alone              Begin services as standalone, i.e. without their dependencies
```

## `codefly build runnable`

Build and verify a native Runnable package through its agent

```
Generate and prepare the loaded Runnable through its agent, package a native
archive, and verify its release descriptor and actual artifact digest. The output
directory must be new; the default build directory is CLI-owned and is replaced
on each build. The name is module/name or an unambiguous bare name.

The agent owns language tooling and launch information. This command does not
install a binding, invoke a task or build an image. Build-time service prerequisites
and internal library preparation are not yet supported.
```

```
codefly build runnable <name>
```

Flags:

```
      --json            Emit the verified Runnable package descriptor as JSON
      --output string   New build directory (default: .codefly/build/runnables/module/name/version, replaced on each build)
```

## `codefly build service`

Build a service container image for a target environment

```
codefly build service [flags]
```

Flags:

```
      --builder string           docker buildx builder to run the build on (e.g. a native amd64 buildkit) to avoid local QEMU emulation
      --cache-backend string     Cache transport backend (registry) (default "registry")
      --cache-from stringArray   Trusted registry cache repository to import (repeatable, without tag)
      --cache-mode string        Exported layers: max (default, includes dependencies) or min
      --cache-scope string       Stable workspace and trust-domain cache scope; service, recipe and platform are added automatically
      --cache-to stringArray     Registry cache repository to publish (repeatable; omit for read-only builds)
      --env string               Environment to build for (looks up registry/cluster from workspace.codefly.yaml) (default "local")
      --org string               Image registry override (e.g. ghcr.io/myorg). Wins over the env's registry.url.
      --push                     Push the image to the repository
      --stand-alone              Begin service as standalone, i.e. without its dependencies
```

## `codefly ci`

Plan and run CI stages for services affected by a change

```
codefly ci <subcommand>
```

Subcommands:

- [`codefly ci build`](#codefly-ci-build)
- [`codefly ci compile`](#codefly-ci-compile)
- [`codefly ci deploy`](#codefly-ci-deploy)
- [`codefly ci lint`](#codefly-ci-lint)
- [`codefly ci plan`](#codefly-ci-plan)
- [`codefly ci push`](#codefly-ci-push)
- [`codefly ci run`](#codefly-ci-run)
- [`codefly ci test`](#codefly-ci-test)

## `codefly ci build`

Build affected services as a CI stage

```
codefly ci build [flags]
```

Flags:

```
      --all                      Select every service explicitly
      --base string              Base Git revision for affected-service discovery
      --cache-backend string     Cache transport backend (registry) (default "registry")
      --cache-from stringArray   Trusted registry cache repository to import (repeatable, without tag)
      --cache-mode string        Exported layers: max (default, includes dependencies) or min
      --cache-scope string       Stable workspace and trust-domain cache scope; service, recipe and platform are added automatically
      --cache-to stringArray     Registry cache repository to publish (repeatable; omit for read-only builds)
      --changed-file strings     Changed path supplied by the CI provider (repeatable; bypasses Git discovery)
      --fail-fast                Stop native tests and service scheduling after the first failure (default true)
      --format string            Final CI report format: text or json (default "text")
      --head string              Head Git revision (defaults to HEAD when --base is set)
      --image-sbom               Require digest-bound image SBOM evidence for every image the build produces
      --init-only                Initialize service only, i.e. without running it
      --jobs int                 Maximum parallel service tasks (0: auto, capped at 4)
      --load-only                LoadRequired service only, i.e. without running it
      --output string            Directory for Codefly CI reports and artifacts (relative to the workspace root) (default ".codefly/ci")
      --plan string              Replay a saved CI plan, validating independent selection bounds and local contents
      --runtime-context string   Runtime context for the flow (default "free")
      --scope string             Runtime scope (for testing encapsulation)
      --silent strings           Silent mode
```

## `codefly ci compile`

Compile or type-check affected services through their agents

```
codefly ci compile [flags]
```

Flags:

```
      --all                      Select every service explicitly
      --base string              Base Git revision for affected-service discovery
      --changed-file strings     Changed path supplied by the CI provider (repeatable; bypasses Git discovery)
      --fail-fast                Stop native tests and service scheduling after the first failure (default true)
      --format string            Final CI report format: text or json (default "text")
      --head string              Head Git revision (defaults to HEAD when --base is set)
      --jobs int                 Maximum parallel service tasks (0: auto, capped at 4)
      --output string            Directory for Codefly CI reports and artifacts (relative to the workspace root) (default ".codefly/ci")
      --plan string              Replay a saved CI plan, validating independent selection bounds and local contents
      --runtime-context string   Runtime context for validation (default "free")
      --silent strings           Silent services
```

## `codefly ci deploy`

Deploy workspace services as a CI stage

```
codefly ci deploy [flags]
```

Flags:

```
      --dry-run       Dry run the deployment
      --env string    Environment to deploy the service (default "local")
      --stand-alone   Begin service as standalone, i.e. without its dependencies
```

## `codefly ci lint`

Lint affected services through their agents

```
codefly ci lint [flags]
```

Flags:

```
      --all                      Select every service explicitly
      --base string              Base Git revision for affected-service discovery
      --changed-file strings     Changed path supplied by the CI provider (repeatable; bypasses Git discovery)
      --fail-fast                Stop native tests and service scheduling after the first failure (default true)
      --format string            Final CI report format: text or json (default "text")
      --head string              Head Git revision (defaults to HEAD when --base is set)
      --jobs int                 Maximum parallel service tasks (0: auto, capped at 4)
      --output string            Directory for Codefly CI reports and artifacts (relative to the workspace root) (default ".codefly/ci")
      --plan string              Replay a saved CI plan, validating independent selection bounds and local contents
      --runtime-context string   Runtime context for validation (default "free")
      --silent strings           Silent services
```

## `codefly ci plan`

List directly changed services and their transitive dependents

```
codefly ci plan [flags]
```

Flags:

```
      --all                       Select every service explicitly
      --allow-service-overrides   Plan against the machine-local per-service overrides in codefly.local.yaml instead of refusing
      --base string               Base Git revision for change discovery
      --changed-file strings      Changed path supplied by the CI provider (repeatable; bypasses Git discovery)
      --format string             Output format: text or json (default "text")
      --head string               Head Git revision (defaults to HEAD when --base is set)
      --phase strings             Phases bound by a replay plan (default: full CI gate)
      --replay                    Include validated execution plans and candidate content identity (use --format json; save outside the repository)
      --runtime-context string    Runtime context bound by a replay plan (default "free")
      --suite strings             Named test suites bound by a replay plan
```

## `codefly ci push`

Publish workspace state to the Codefly platform in CI

```
codefly ci push [flags]
```

Flags:

```
      --init-only                Initialize service only, i.e. without running it
      --load-only                LoadRequired service only, i.e. without running it
      --runtime-context string   Runtime context for the flow (default "free")
      --scope string             Runtime scope (for testing encapsulation)
      --silent strings           Silent mode
```

## `codefly ci run`

Run the complete Codefly-native CI gate for affected services

```
codefly ci run [flags]
```

Flags:

```
      --all                               Select every service explicitly
      --allow-service-overrides           Run against the machine-local per-service overrides in codefly.local.yaml instead of refusing
      --audit-include-dev                 Include development/test-only dependencies in audit evidence
      --audit-outdated                    Include outdated dependencies in audit evidence (default true)
      --base string                       Base Git revision for affected-service discovery
      --cache-backend string              Cache transport backend (registry) (default "registry")
      --cache-from stringArray            Trusted registry cache repository to import (repeatable, without tag)
      --cache-mode string                 Exported layers: max (default, includes dependencies) or min
      --cache-scope string                Stable workspace and trust-domain cache scope; service, recipe and platform are added automatically
      --cache-to stringArray              Registry cache repository to publish (repeatable; omit for read-only builds)
      --changed-file strings              Changed path supplied by the CI provider (repeatable; bypasses Git discovery)
      --disposable                        Give each validation/test flow a fresh resource scope and destroy its owned runtime resources afterward (for disposable fixtures only)
      --fail-fast                         Stop native tests and service scheduling after the first failure (default true)
      --fail-on-vuln                      Fail audit on HIGH or CRITICAL vulnerabilities (default true)
      --format string                     Final CI report format: text or json (default "text")
      --head string                       Head Git revision (defaults to HEAD when --base is set)
      --image-sbom                        Require digest-bound image SBOM evidence for every image the build phase produces
      --jobs int                          Maximum parallel service tasks (0: auto, capped at 4)
      --output string                     Directory for Codefly CI reports and artifacts (relative to the workspace root) (default ".codefly/ci")
      --override-port strings             Pin an endpoint to a host port (endpoint=port, e.g. app/subject/rest=45001; repeatable)
      --phase strings                     CI phase to run (repeatable or comma-separated; default: full Codefly gate)
      --plan string                       Replay a saved CI plan, validating independent selection bounds and local contents
      --reuse-audit-max-age duration      Maximum age of a reusable dependency-audit result; zero always re-runs audits because advisory data changes independently of source
      --reuse-environment string          Identity of the execution environment (for example the runner image digest; default $CODEFLY_CI_REUSE_ENVIRONMENT)
      --reuse-max-age duration            Maximum age of a reusable result (default 168h0m0s)
      --reuse-reference string            Reference this run publishes results under (default $CODEFLY_CI_REFERENCE)
      --reuse-results                     Reuse verified successful task results from a trusted reference instead of re-executing identical work
      --reuse-run string                  Identity of this run, recorded as the provenance of published results (default $CODEFLY_CI_RUN)
      --reuse-store string                Directory holding verified CI result records and artifacts (default $CODEFLY_CI_RESULT_STORE)
      --reuse-trusted-reference strings   Reference whose successful results may be reused (repeatable; required with --reuse-results)
      --runtime-context string            Runtime context for validation and tests (default "free")
      --sbom-include-dev                  Include development/test dependencies in CI SBOMs (default true)
      --silent strings                    Silent services
      --suite strings                     Named test suite to run during the test phase (repeatable; default: each agent's advertised default)
      --temporary-ports                   Allocate OS-probed ephemeral ports so this CI run's port space cannot collide with another run on the host
```

## `codefly ci test`

Test affected services and emit CI reports

```
codefly ci test [flags]
```

Flags:

```
      --all                      Select every service explicitly
      --base string              Base Git revision for affected-service discovery
      --changed-file strings     Changed path supplied by the CI provider (repeatable; bypasses Git discovery)
      --disposable               Give each validation/test flow a fresh resource scope and destroy its owned runtime resources afterward (for disposable fixtures only)
      --fail-fast                Stop native tests and service scheduling after the first failure (default true)
      --format string            Final CI report format: text or json (default "text")
      --head string              Head Git revision (defaults to HEAD when --base is set)
      --init-only                Initialize service only, i.e. without running it
      --jobs int                 Maximum parallel service tasks (0: auto, capped at 4)
      --load-only                LoadRequired service only, i.e. without running it
      --output string            Directory for Codefly CI reports and artifacts (relative to the workspace root) (default ".codefly/ci")
      --plan string              Replay a saved CI plan, validating independent selection bounds and local contents
      --runtime-context string   Runtime context for the flow (default "free")
      --scope string             Runtime scope (for testing encapsulation)
      --silent strings           Silent services
      --suite strings            Named test suite to run (repeatable; default: each agent's advertised default)
```

## `codefly clear`

Remove Codefly processes, containers, and stale local runtime state

```
Clear codefly state.

Without arguments, removes ALL codefly processes and ALL codefly-owned
docker containers. Pass one or more substring filters to scope the
container removal — only containers whose name contains at least one
filter are removed. The process kill is always wholesale because
codefly processes aren't per-service.

Examples:
  codefly clear                     # full reset
  codefly clear neo4j               # only neo4j container (keeps postgres)
  codefly clear neo4j postgres      # both
  codefly clear --keep-processes neo4j  # only the container, leave running codefly alone
  codefly clear --dry-run           # list what would be removed without doing it
```

```
codefly clear [name-filter...]
```

Flags:

```
      --dry-run           List what would be removed without removing anything
      --keep-containers   Don't remove docker containers (only kill processes)
      --keep-processes    Don't kill running codefly processes (only remove containers)
```

## `codefly companion`

Build and publish companion images used by Codefly toolchains

```
codefly companion <subcommand>
```

Subcommands:

- [`codefly companion build`](#codefly-companion-build)
- [`codefly companion list`](#codefly-companion-list)
- [`codefly companion publish`](#codefly-companion-publish)
- [`codefly companion push`](#codefly-companion-push)
- [`codefly companion verify`](#codefly-companion-verify)

## `codefly companion build`

Build one or all companion images from the core repository

```
Build builds a companion image from its directory.

With a name argument, builds just that companion. With --all, builds
every companion under <core>/companions/, in the order core's build
specs declare — every base image before what builds on it.

A companion is a directory under core/companions/ with an
info.codefly.yaml (declaring version) and either a Dockerfile, a
flake.nix, or both. When flake.nix is present AND nix is installed,
the flake build is preferred (reproducible, layered cache).

The image tag is derived from <name> and the version in info.codefly.yaml;
see Companion.Tag.

Examples:
  codefly companion build proto
  codefly companion build --all
  codefly companion build go --push                    # build then push
  codefly companion build --all --core-dir ./core      # explicit anchor
```

```
codefly companion build [name]
```

Flags:

```
      --all               Build every companion under <core>/companions/
      --core-dir string   Path to the core directory (default: walk up from cwd looking for companions/)
      --force-docker      Skip the flake.nix path even when present + nix is installed
      --platform string   Target platform(s) for Docker builds (e.g. linux/amd64 or linux/amd64,linux/arm64). Multiple platforms require --push.
      --pull              Always pull a newer base image (docker build --pull) — picks up upstream patch releases (e.g. golang:1.26-alpine → latest 1.26.x)
      --push              Push each image to the registry after a successful build
```

## `codefly companion list`

List companion image definitions in the core repository

```
codefly companion list [flags]
```

Flags:

```
      --core-dir string   Path to the core directory (default: walk up from cwd)
```

## `codefly companion publish`

Build and push companion images at their pinned versions

```
Publish builds each companion and pushes it to the registry under the
tag pinned in its info.codefly.yaml.

With a name argument, publishes just that companion. With --all, publishes
every image companion under <core>/companions/, in dependency order
(codefly base image first, then language runtimes, then dev tooling).

This is build + push in one step, scoped to the tags core embeds. Use it
from release/tag CI so an embedded tag is never missing from the registry.

Examples:
  codefly companion publish proto
  codefly companion publish --all
  codefly companion publish --all --core-dir ./core
```

```
codefly companion publish [name]
```

Flags:

```
      --all                         Publish every companion under <core>/companions/
      --core-dir string             Path to the core directory (default: walk up from cwd looking for companions/)
      --force                       Republish a companion whose tag is already in the registry, overwriting it
      --force-docker                Skip the flake.nix path even when present + nix is installed
      --platform string             Target platform(s) for Docker builds (e.g. linux/amd64,linux/arm64). Multiple platforms publish one manifest with buildx.
      --published-manifest string   Write JSON describing the companions this run actually built and pushed to this path
      --pull                        Always pull a newer base image (docker build --pull) before building
```

## `codefly companion push`

Push a previously built companion image to its registry

```
Push uses the version in <core>/companions/<name>/info.codefly.yaml
to compute the tag, then runs "docker push <tag>".

The image must already exist locally. Build it first with
"codefly companion build <name>".
```

```
codefly companion push <name>
```

Flags:

```
      --core-dir string   Path to the core directory (default: walk up from cwd)
```

## `codefly companion verify`

Verify companion images defined under core/companions exist in the registry

```
Verify resolves the tag each image companion pins in its info.codefly.yaml
and checks the manifest exists in the registry via "docker manifest
inspect". It verifies exactly the set "companion publish" produces.

With no argument it verifies every image companion under
<core>/companions/; pass a name to verify just one. It exits non-zero when
any tag is missing, listing the missing tags — wire it into CI so a bump
that references an unpublished tag fails fast instead of at runtime.

The check runs with no stored docker credentials, so it sees exactly what
an anonymous puller (agents, CI, other developers) sees — a package an
operator can see only because they're logged in locally would otherwise
pass verify and still 401 for everyone else.

Examples:
  codefly companion verify
  codefly companion verify proto
  codefly companion verify --core-dir ./core
```

```
codefly companion verify [name]
```

Flags:

```
      --all               Verify every companion under <core>/companions/ (default when no name is given)
      --core-dir string   Path to the core directory (default: walk up from cwd looking for companions/)
```

## `codefly compile`

Run plugin-owned compilation or type checking for a service

```
codefly compile <subcommand>
```

Subcommands:

- [`codefly compile service`](#codefly-compile-service)

## `codefly compile service`

Compile or type-check a service through its plugin

```
codefly compile service [module/]service
```

Flags:

```
      --runtime-context string   Runtime context for validation (default "free")
```

## `codefly completion`

Generate or install shell completion scripts for Codefly

```
Generate the completion script for the given shell to stdout, or
write it to that shell's conventional location with --install.

  codefly completion zsh             # print to stdout
  codefly completion zsh --install   # install for the current user

--install replaces scripts/build/add_code_completion.sh.
```

```
codefly completion [bash|zsh|fish|powershell]
```

Flags:

```
      --install   Write the completion script to the shell's conventional location instead of stdout
```

## `codefly composition`

Inspect and select product-owned component releases

```
codefly composition <subcommand>
```

Flags:

```
      --build-requests string    JSON array of typed, instance-scoped build RPC payloads; retained for render and admission
      --configuration string     JSON file containing the effective configuration values
      --identity-key string      Private file containing at least 32 raw bytes for configuration identity
      --product string           Directory containing module.codefly.yaml (default ".")
      --render-requests string   JSON array of typed, instance-scoped render RPC payloads
      --workspace string         Workspace containing package-scoped module-trust (default ".")
```

Subcommands:

- [`codefly composition acquire`](#codefly-composition-acquire)
- [`codefly composition approve-admission`](#codefly-composition-approve-admission)
- [`codefly composition check`](#codefly-composition-check)
- [`codefly composition check-approval`](#codefly-composition-check-approval)
- [`codefly composition check-deployment`](#codefly-composition-check-deployment)
- [`codefly composition check-inputs`](#codefly-composition-check-inputs)
- [`codefly composition configure-approval-authority`](#codefly-composition-configure-approval-authority)
- [`codefly composition develop`](#codefly-composition-develop)
- [`codefly composition init`](#codefly-composition-init)
- [`codefly composition init-release`](#codefly-composition-init-release)
- [`codefly composition inspect`](#codefly-composition-inspect)
- [`codefly composition inspect-approval-authority`](#codefly-composition-inspect-approval-authority)
- [`codefly composition inspect-approval-use`](#codefly-composition-inspect-approval-use)
- [`codefly composition inspect-local-target`](#codefly-composition-inspect-local-target)
- [`codefly composition prepare-render`](#codefly-composition-prepare-render)
- [`codefly composition propose-removal`](#codefly-composition-propose-removal)
- [`codefly composition publish-build`](#codefly-composition-publish-build)
- [`codefly composition recheck-admission`](#codefly-composition-recheck-admission)
- [`codefly composition record-admission`](#codefly-composition-record-admission)
- [`codefly composition restore`](#codefly-composition-restore)
- [`codefly composition select`](#codefly-composition-select)
- [`codefly composition select-release`](#codefly-composition-select-release)
- [`codefly composition stage-build`](#codefly-composition-stage-build)
- [`codefly composition stage-render`](#codefly-composition-stage-render)
- [`codefly composition upstream`](#codefly-composition-upstream)

## `codefly composition acquire`

Acquire only the artifacts required by Core's effective selection

```
codefly composition acquire [flags]
```

## `codefly composition approve-admission`

Sign exact qualified admission using installed host authority; never deploy

```
Rechecks the independently reviewed admission and actual files under installed host policy before signing with the configured approval key. Approval expires no later than qualification evidence. It does not run tests, consume authorization, apply or publish workloads.
```

```
codefly composition approve-admission INPUTS.json RECORD.json DESTINATION.json
```

Flags:

```
      --expected-authority string   Independently reviewed and retained host authority digest
      --expected-identity string    Independently reviewed and retained admission identity
      --expires string              Explicit RFC3339 approval expiry, bounded by qualification validity
      --signing-key string          Private file containing the configured approver's 64 raw Ed25519 key bytes
```

## `codefly composition check`

Evaluate authenticated actual-consumer compatibility with Core

```
codefly composition check TARGET ARTIFACT USAGE.json AUTHORITY.json
```

## `codefly composition check-approval`

Verify approval, installed authority and fresh inputs without consuming or deploying

```
codefly composition check-approval INPUTS.json APPROVAL.json
```

## `codefly composition check-deployment`

Check exact runtime files and signed qualifications with Core admission

```
codefly composition check-deployment INPUTS.json POLICY.json
```

## `codefly composition check-inputs`

Authenticate runtime and staged output files for qualification

```
codefly composition check-inputs INPUTS.json
```

## `codefly composition configure-approval-authority`

Install host-owned approval policy, identity, key and target bindings

```
Trusted-local administration only. Installs public authority configuration outside the workspace under CODEFLY_HOME. Replacing existing policy, key, audience or target bindings requires its current digest.
```

```
codefly composition configure-approval-authority CONFIG.json
```

Flags:

```
      --expected-digest string   Current authority digest required for an explicit replacement
```

## `codefly composition develop`

Use independent local module checkouts by instance target

```
codefly composition develop CHECKOUTS.json
```

## `codefly composition init`

Authenticate and record an initial release selection

```
codefly composition init INPUTS.json
```

## `codefly composition init-release`

Resolve and authenticate the base release without entering a digest

```
codefly composition init-release VERSION
```

## `codefly composition inspect`

Inspect inherited releases, replacements, requirements and local content

```
codefly composition inspect [flags]
```

## `codefly composition inspect-approval-authority`

Inspect the installed host approval authority identity

```
codefly composition inspect-approval-authority [flags]
```

## `codefly composition inspect-approval-use`

Inspect a retained host approval-use record without implying deployment

```
Read historical single-use consumption evidence from protected host storage. A record does not prove deployment or health; an absent record does not prove that no effect occurred. No reset, retry or deployment authorization is provided.
```

```
codefly composition inspect-approval-use ID
```

## `codefly composition inspect-local-target`

Read the live local Kubernetes target identity without mutation

```
Read an explicitly declared local k3d environment's cluster and namespace identities. An optional independently retained identity is checked against the live target. This does not qualify inputs, reserve approval, fence effects or deploy anything.
```

```
codefly composition inspect-local-target ENVIRONMENT
```

Flags:

```
      --expected-identity string   Independently retained target binding digest; mismatch refuses the check
```

## `codefly composition prepare-render`

Prepare Core-bound render requests without invoking executors

```
codefly composition prepare-render INPUTS.json
```

## `codefly composition propose-removal`

Ask Core to prove equivalence before proposing override removal

```
codefly composition propose-removal TARGET RELEASE.json
```

## `codefly composition publish-build`

Publish exact staged outputs by digest with owner-authorized derived evidence

```
codefly composition publish-build BUILD.json INPUTS.json SIGNERS.json
```

Flags:

```
      --expected-build string       Invocation evidence digest retained independently when stage-build completed
      --expected-selection string   Independently inspected effective selection identity
      --output string               Absolute new publication record outside the product and checkouts
      --repository string           Explicit OCI registry/repository for bytes and a digest-named retention reference; no module release
```

## `codefly composition recheck-admission`

Recheck retained admission against current policy and actual files

```
codefly composition recheck-admission INPUTS.json POLICY.json RECORD.json
```

Flags:

```
      --expected-identity string   Admission identity retained independently from the supplied record
```

## `codefly composition record-admission`

Persist Core admission without replacing records or authorizing deployment

```
codefly composition record-admission INPUTS.json POLICY.json DESTINATION.json
```

## `codefly composition restore`

Restore release selections without modifying local checkout files

```
codefly composition restore TARGET...
```

## `codefly composition select`

Authenticate and persist a nested component replacement

```
codefly composition select REPLACEMENT.json
```

## `codefly composition select-release`

Resolve and authenticate an instance replacement without entering a digest

```
codefly composition select-release TARGET VERSION
```

Flags:

```
      --reason string   Product owner's reason for the replacement
```

## `codefly composition stage-build`

Invoke selected source builds into verified staging without publication

```
codefly composition stage-build [flags]
```

Flags:

```
      --allow-network          Allow executor network access in the sandbox
      --output-parent string   Existing canonical absolute staging parent outside product and local checkouts
      --sandbox string         Executor sandbox: required or none (explicit unrestricted execution) (default "required")
      --without-principal      Explicit local execution without an authenticated principal; never deployment authorization
```

## `codefly composition stage-render`

Invoke exact selected executors into verified staging without deployment effects

```
codefly composition stage-render INPUTS.json
```

Flags:

```
      --allow-network          Allow executor network access in the sandbox
      --output-parent string   Existing canonical absolute staging parent outside product and local checkouts
      --sandbox string         Executor sandbox: required or none (explicit unrestricted execution) (default "required")
      --without-principal      Explicit local execution without an authenticated principal; never deployment authorization
```

## `codefly composition upstream`

Prepare owner-scoped adoption facts without submitting a request

```
codefly composition upstream [flags]
```

## `codefly config`

Provision and inspect service and workspace configuration values

```
Config writes and inspects the configuration files Codefly reads at run and
deploy time: <scope>/configurations/<profile>/<name>[.secret].env.

Configuration is Codefly's, so provisioning it is a Codefly operation — not a
shell script in your repository. Values are written but never printed.

Secrets are refused when git would track the destination file, are stored 0600
inside a 0700 directory, and are never overwritten without --force so re-running
setup cannot rotate a credential another service already holds.
```

```
codefly config <subcommand>
```

Subcommands:

- [`codefly config check`](#codefly-config-check)
- [`codefly config generate`](#codefly-config-generate)
- [`codefly config list`](#codefly-config-list)
- [`codefly config set`](#codefly-config-set)

## `codefly config check`

Verify that configuration keys are present without printing them

```
Check reports whether a configuration holds a non-empty value for each key.

It exits non-zero and names the missing keys when any is absent, which makes it
the readiness gate a launcher can run before starting a service. It never prints
a value.

Examples:
  codefly config check internal-auth CODEFLY_INTERNAL_TOKEN CODEFLY_GATEWAY_TOKEN
  codefly config check runtime EXECUTION_SCHEDULER_TOKEN --service mind/mind
```

```
codefly config check <name> [KEY...]
```

Flags:

```
      --env string               Environment whose configuration profile is used. Default: the workspace's local environment
      --plaintext                Check a non-secret configuration (<name>.env)
      --service module/service   Target a service's configuration (module/service, or a unique service name). Default: the workspace configuration
```

## `codefly config generate`

Provision configuration values with generated secrets

```
Generate stores cryptographically random values for the named keys.

This is the credential-provisioning path that replaces hand-written setup
scripts. Keys that already hold a value are left alone unless --force is passed,
so running it again never rotates a credential another service is holding.

Generated configurations are always secrets: 0600, in a 0700 directory, refused
when git would track the file. The value is never printed.

Examples:
  codefly config generate internal-auth CODEFLY_INTERNAL_TOKEN CODEFLY_GATEWAY_TOKEN
  codefly config generate mutation_permit ED25519_SEED_BASE64 --format base64 --service coordination/work-coordinator
```

```
codefly config generate <name> KEY [KEY...]
```

Flags:

```
      --bytes int                Number of random bytes to generate (default 32)
      --env string               Environment whose configuration profile is used. Default: the workspace's local environment
      --force                    Regenerate keys that already have a value (rotates the credential)
      --format string            Encoding of the generated value: hex or base64 (default "hex")
      --service module/service   Target a service's configuration (module/service, or a unique service name). Default: the workspace configuration
```

## `codefly config list`

List configuration names and their keys

```
List shows every configuration in the selected scope and profile with the key
names it holds. Values are never printed.

Examples:
  codefly config list
  codefly config list --service mind/mind
  codefly config list --env aws
```

```
codefly config list [flags]
```

Flags:

```
      --env string               Environment whose configuration profile is used. Default: the workspace's local environment
      --plaintext                List non-secret configurations (<name>.env)
      --service module/service   Target a service's configuration (module/service, or a unique service name). Default: the workspace configuration
```

## `codefly config set`

Write configuration values from explicit input

```
Set stores KEY=VALUE pairs in a configuration Codefly reads.

Existing keys are preserved unless --force is passed, so re-running a setup
sequence is idempotent. The value is taken from the argument; nothing is echoed.

Examples:
  codefly config set internal-auth CODEFLY_INTERNAL_TOKEN=$TOKEN
  codefly config set runtime EXECUTION_SCHEDULER_TOKEN=$TOKEN --service mind/mind
  codefly config set edge EDGE_URL=https://localhost:8080 --plaintext
```

```
codefly config set <name> KEY=VALUE [KEY=VALUE...]
```

Flags:

```
      --env string               Environment whose configuration profile is used. Default: the workspace's local environment
      --force                    Replace keys that already have a value
      --plaintext                Write a non-secret configuration (<name>.env) instead of <name>.secret.env
      --service module/service   Target a service's configuration (module/service, or a unique service name). Default: the workspace configuration
```

## `codefly daemon`

Run and inspect Codefly services in the background

```
codefly daemon <subcommand>
```

Subcommands:

- [`codefly daemon gateway`](#codefly-daemon-gateway)
- [`codefly daemon logs`](#codefly-daemon-logs)
- [`codefly daemon monitor`](#codefly-daemon-monitor)
- [`codefly daemon restart`](#codefly-daemon-restart)
- [`codefly daemon start`](#codefly-daemon-start)
- [`codefly daemon status`](#codefly-daemon-status)
- [`codefly daemon stop`](#codefly-daemon-stop)

## `codefly daemon gateway`

Serve Codefly workspace operations to Mind over gRPC

```
Starts the Mind Gateway gRPC server in the foreground. Typically invoked by 'daemon start --gateway' or by Mind automatically.
```

```
codefly daemon gateway [flags]
```

Flags:

```
      --dir string                          Working directory containing mind.yaml (default ".")
      --execution-authority-issuer string   Exact Work Context issuer
      --execution-authority-jwks string     HTTPS JWKS URL for Work Context verification
      --execution-exporter stringArray      Installed execution-exporter agent specification (repeatable)
      --execution-state-dir string          Owner-only execution key and receipt state directory (defaults per workspace)
      --governed-execution                  Enable signed Work Context admission and durable receipts for supported ApplyEdit/Test effects
      --port int                            gRPC listen port (default 50051)
```

## `codefly daemon logs`

Read or follow output from the background daemon

```
codefly daemon logs [flags]
```

Flags:

```
  -f, --follow     Follow log output
  -n, --tail int   Show last N lines
```

## `codefly daemon monitor`

Watch Codefly processes for CPU and memory pressure

```
Checks all codefly-related processes (agents, server, Neo4j) for:
- High CPU usage (>200% for 2 consecutive checks → auto-kill agents)
- High memory usage (>512MB → warning)
- Orphaned agent processes (>3 → warning)

Use -w/--watch for continuous monitoring.
Use --kill-orphans to clean up orphaned agent processes.
```

```
codefly daemon monitor [flags]
```

Flags:

```
      --kill-orphans   Kill orphaned agent processes
  -w, --watch          Run continuously (every 30s)
```

## `codefly daemon restart`

Restart the background daemon with its previous arguments

```
codefly daemon restart [flags]
```

## `codefly daemon start`

Run workspace services in a detached background daemon

```
Starts a detached codefly daemon that runs your services.

Any flags after "--" are forwarded to the underlying "run service" command.

Examples:
  codefly daemon start
  codefly daemon start -- --runtime-context nix
  codefly daemon start -- -d --service-path ./my-svc
```

```
codefly daemon start [-- flags passed to 'run service']
```

Flags:

```
      --dir string                          Working directory for gateway (requires --gateway) (default ".")
      --execution-authority-issuer string   Exact Work Context issuer
      --execution-authority-jwks string     HTTPS JWKS URL for Work Context verification
      --execution-exporter stringArray      Installed execution-exporter agent specification (repeatable)
      --execution-state-dir string          Owner-only execution key and receipt state directory (defaults per workspace)
      --gateway                             Start the Mind Gateway gRPC server instead of running services
      --governed-execution                  Enable signed Work Context admission and durable receipts for supported ApplyEdit/Test effects
      --port int                            gRPC port for gateway (requires --gateway) (default 50051)
```

## `codefly daemon status`

Report whether the Codefly daemon is running

```
codefly daemon status [flags]
```

## `codefly daemon stop`

Stop the active background daemon and its services

```
codefly daemon stop [flags]
```

## `codefly delete`

Remove modules or services from the current workspace

```
codefly delete <subcommand>
```

Subcommands:

- [`codefly delete module`](#codefly-delete-module)
- [`codefly delete service`](#codefly-delete-service)

## `codefly delete module`

Remove a module and its reference from the workspace

```
codefly delete module [flags]
```

## `codefly delete service`

Remove a service and clean up its dependency references

```
codefly delete service [flags]
```

## `codefly deploy`

Deploy a service or module to a configured environment

```
codefly deploy <subcommand>
```

Subcommands:

- [`codefly deploy dev`](#codefly-deploy-dev)
- [`codefly deploy gitops`](#codefly-deploy-gitops)
- [`codefly deploy init`](#codefly-deploy-init)
- [`codefly deploy module`](#codefly-deploy-module)
- [`codefly deploy secrets`](#codefly-deploy-secrets)
- [`codefly deploy service`](#codefly-deploy-service)
- [`codefly deploy solution`](#codefly-deploy-solution)

## `codefly deploy dev`

DEV ESCAPE HATCH: push one service's local code into an already-rendered GitOps environment

```
Build and push ONE service from local code — --path, else its machine-local
service override — through the same build path `deploy gitops render` uses, then
re-pin only that service's image digest in the environment's rendered tree.

The environment then runs code no release describes. The render inventory
records it under "dev", `codefly doctor workspace` warns while it is active, and
the next full `codefly deploy gitops render <module> --env <env>` clears it.
```

```
codefly deploy dev <module>/<service>
```

Flags:

```
      --app-project string                                         AppProject the environment was rendered for (checked against the render)
      --commit dev: <module>/<service> from <path>@<sha>[-dirty]   Commit the change as dev: <module>/<service> from <path>@<sha>[-dirty]
      --env string                                                 Environment whose rendered tree to patch (required)
      --path string                                                Service source directory to deploy (default: the machine-local service override)
      --push                                                       Push the commit (requires --commit)
```

## `codefly deploy gitops`

Render, publish, observe, and recover reviewed GitOps promotions

```
codefly deploy gitops <subcommand>
```

Subcommands:

- [`codefly deploy gitops observe`](#codefly-deploy-gitops-observe)
- [`codefly deploy gitops plan`](#codefly-deploy-gitops-plan)
- [`codefly deploy gitops publish`](#codefly-deploy-gitops-publish)
- [`codefly deploy gitops remote`](#codefly-deploy-gitops-remote)
- [`codefly deploy gitops render`](#codefly-deploy-gitops-render)
- [`codefly deploy gitops rollback`](#codefly-deploy-gitops-rollback)
- [`codefly deploy gitops snapshot`](#codefly-deploy-gitops-snapshot)

## `codefly deploy gitops observe`

Verify Argo CD reconciled the reviewed Git revision and store evidence

```
codefly deploy gitops observe [module]
```

Flags:

```
      --app-project string    Selected Argo CD AppProject
      --application strings   Argo CD application to observe (repeatable)
      --env string            Environment to promote (default "local")
      --local                 Observe a disposable local GitOps qualification
      --revision string       Expected immutable service snapshot revision
      --timeout duration      Maximum time to wait for Synced and Healthy (default 10m0s)
```

## `codefly deploy gitops plan`

Inspect the exact GitOps publication diff

```
codefly deploy gitops plan [module]
```

Flags:

```
      --allow-unresolved-contracts                                                        Downgrade a consumed contract whose exposing module is not yet deployed to a warning, for bootstrap ordering
      --env string                                                                        Environment to promote (default "local")
      --local                                                                             Use a disposable local file Git remote for k3d qualification
      --promotion-branch string                                                           Promotion branch (deterministic default when empty)
      --skip-workspace-readiness codefly doctor workspace --env <env> --module <module>   Proceed even when codefly doctor workspace --env <env> --module <module> says the workspace is not ready (for an operator mid-repair; the skip is announced in the output)
```

## `codefly deploy gitops publish`

Create a signed promotion commit and open or update its pull request

```
codefly deploy gitops publish [module]
```

Flags:

```
      --allow-unresolved-contracts                                                        Downgrade a consumed contract whose exposing module is not yet deployed to a warning, for bootstrap ordering
      --body string                                                                       Promotion pull request body
      --env string                                                                        Environment to promote (default "local")
      --local                                                                             Use a disposable local file Git remote for k3d qualification
      --message string                                                                    Signed commit message
      --promotion-branch string                                                           Promotion branch (deterministic default when empty)
      --skip-workspace-readiness codefly doctor workspace --env <env> --module <module>   Proceed even when codefly doctor workspace --env <env> --module <module> says the workspace is not ready (for an operator mid-repair; the skip is announced in the output)
      --title string                                                                      Promotion pull request title
  -y, --yes                                                                               Publish the inspected plan without an interactive confirmation
```

## `codefly deploy gitops remote`

Own the environment-scoped local read-only fetch remote Argo fetches from

```
codefly deploy gitops remote <subcommand>
```

Subcommands:

- [`codefly deploy gitops remote down`](#codefly-deploy-gitops-remote-down)
- [`codefly deploy gitops remote plan`](#codefly-deploy-gitops-remote-plan)
- [`codefly deploy gitops remote status`](#codefly-deploy-gitops-remote-status)
- [`codefly deploy gitops remote up`](#codefly-deploy-gitops-remote-up)

## `codefly deploy gitops remote down`

Tear down the fetch remote after re-validating ownership, preserving repository data

```
codefly deploy gitops remote down [flags]
```

Flags:

```
      --env string   Environment whose fetch remote to manage (default "local")
  -y, --yes          Tear down without an interactive confirmation
```

## `codefly deploy gitops remote plan`

Inspect the fetch remote that would serve the reviewed revision

```
codefly deploy gitops remote plan [module]
```

Flags:

```
      --env string   Environment whose fetch remote to manage (default "local")
```

## `codefly deploy gitops remote status`

Validate the fetch remote against its exact ownership and network identity

```
codefly deploy gitops remote status [flags]
```

Flags:

```
      --env string   Environment whose fetch remote to manage (default "local")
```

## `codefly deploy gitops remote up`

Create or refresh the read-only fetch remote for the reviewed revision

```
codefly deploy gitops remote up [module]
```

Flags:

```
      --env string   Environment whose fetch remote to manage (default "local")
```

## `codefly deploy gitops render`

Render and validate a module-owned manifest tree

```
codefly deploy gitops render [module]
```

Flags:

```
      --app-project string                                                                AppProject contract for cluster-scoped resources
      --env string                                                                        Environment to promote (default "local")
      --skip-workspace-readiness codefly doctor workspace --env <env> --module <module>   Proceed even when codefly doctor workspace --env <env> --module <module> says the workspace is not ready (for an operator mid-repair; the skip is announced in the output)
      --validate-cluster                                                                  Also dry-run each service's manifests server-side against the environment's declared cluster.context (off: a render needs no cluster)
```

## `codefly deploy gitops rollback`

Re-promote a prior reviewed Git tree through a new pull request

```
codefly deploy gitops rollback [module]
```

Flags:

```
      --allow-unresolved-contracts   Downgrade a consumed contract whose exposing module is not yet deployed to a warning, for bootstrap ordering
      --body string                  Promotion pull request body
      --env string                   Environment to promote (default "local")
      --local                        Use a disposable local file Git remote for k3d qualification
      --message string               Signed commit message
      --promotion-branch string      Promotion branch (deterministic default when empty)
      --title string                 Promotion pull request title
      --to-revision string           Previously reviewed Git revision to re-promote
  -y, --yes                          Publish the inspected plan without an interactive confirmation
```

## `codefly deploy gitops snapshot`

Render and validate the immutable service snapshot consumed by module generators

```
codefly deploy gitops snapshot [module]
```

Flags:

```
      --app-project string                                                                AppProject contract for cluster-scoped resources
      --env string                                                                        Environment to promote (default "local")
      --skip-workspace-readiness codefly doctor workspace --env <env> --module <module>   Proceed even when codefly doctor workspace --env <env> --module <module> says the workspace is not ready (for an operator mid-repair; the skip is announced in the output)
```

## `codefly deploy init`

Initialize version-controlled deployment manifests for the workspace

```
codefly deploy init [flags]
```

## `codefly deploy module`

Deploy every module service and apply its Kustomize configuration

```
codefly deploy module [name]
```

Flags:

```
      --app-project string      AppProject contract used to validate cluster-scoped rendered resources
      --dry-run                 Render the deployment without applying it
      --env string              Environment to deploy the module (default "local")
      --render-only             Render kustomize manifests to disk without applying. Used for gitops flows where ArgoCD/Flux syncs from the rendered tree.
      --wait-for string         Completion stage the deployment must establish before it is reported as successful (applied, bootstrapped, healthy) (default "applied")
      --wait-timeout duration   Budget for observing the deployment when --wait-for goes beyond applied (default 10m0s)
```

## `codefly deploy secrets`

Plan and write the secret-store values a rendered environment's ExternalSecrets read

```
Reads every ExternalSecret `deploy gitops render` projected for --env under
deployments/modules, and resolves each remote property to a source: kept (already
stored), derived (federation credentials and the registrar's digests of them),
propagated (a configuration value another remote key already holds), generated
(declared random by the environment's service-secrets.generate), or required
(supplied by the operator, and named). The store is the backend behind the
SecretStore the render names, read from the environment's cluster.

Values are never printed: the plan names keys and sources only.

--dry-run prints the plan and writes nothing. --metadata-only additionally never
reads a stored value: it knows which remote keys exist, not what they hold.

--module limits the plan to the remote keys the named modules' services read,
plus their federation counterpart — the registrar's digest properties encoding a
credential those keys hold — and names that counterpart in the plan. Every other
remote key is still read, so a scoped key keeps agreeing with what the store
already holds, but nothing outside the scope is planned or written.
```

```
codefly deploy secrets [flags]
```

Flags:

```
      --allow-missing    Write what can be resolved even when some properties must still be supplied
      --dry-run          Print the plan and write nothing
      --env string       Environment whose rendered ExternalSecrets to seed (default "local")
      --metadata-only    With --dry-run: never read a stored value, only which remote keys exist
      --module strings   Plan only these modules' remote keys (comma-separated or repeated), plus the registrar digests of their credentials
  -y, --yes              Write without an interactive confirmation
```

## `codefly deploy service`

Deploy a service to the selected workspace environment

```
codefly deploy service [flags]
```

Flags:

```
      --app-project string      AppProject contract used to validate cluster-scoped rendered resources
      --dry-run                 Render the deployment without applying it
      --env string              Environment to deploy the service (default "local")
      --render-only             Render kustomize manifests to disk without applying. Used for gitops flows where ArgoCD/Flux syncs from the rendered tree.
      --stand-alone             Begin service as standalone, i.e. without its dependencies
      --wait-for string         Completion stage the deployment must establish before it is reported as successful (applied, bootstrapped, healthy) (default "applied")
      --wait-timeout duration   Budget for observing the deployment when --wait-for goes beyond applied (default 10m0s)
```

## `codefly deploy solution`

Package and render a solution into the gitops tree

```
codefly deploy solution [name]
```

Flags:

```
      --agent string            codefly:solution executor identity (publisher:name:version)
      --app-project string      AppProject contract used to validate cluster-scoped rendered resources
      --env string              Environment to deploy the solution (default "local")
      --reference string        Target OCI reference to push the packaged solution to
      --source string           Solution source directory to package as an OCI artifact
      --values stringToString   Values passed to the solution executor's render (key=value) (default [])
```

## `codefly doctor`

Diagnose local prerequisites and workspace configuration problems

```
Run environment health checks and print actionable fixes.

Verifies the things that quietly break a run — Docker reachability, the codefly
home + installed agents, free disk, process limits (macOS), and stray agent
processes — and tells you how to fix each one.
```

```
codefly doctor [flags]
```

Subcommands:

- [`codefly doctor workspace`](#codefly-doctor-workspace)

## `codefly doctor workspace`

Validate workspace paths, configuration, and agent readiness without changes

```
Validate, from the current directory, that the workspace is ready for
`codefly run` / `codefly test` — without starting anything.

The check discovers the workspace, resolves the selected environment
(default: local), validates declared secret backends and their executables,
verifies that required workspace and service configurations exist for the
environment, and resolves secret provider references (op://…) in memory,
discarding the values immediately. It is strictly read-only: it never creates
directories or files, never starts agents, containers, or services, and never
prints secret values or raw references.

Designed for fresh git worktrees, where ignored *.secret.env files are absent
and the failure would otherwise only surface mid-orchestration.

Exit codes:
  0  the workspace is ready (warnings allowed)
  1  at least one check failed, or the command itself failed

With --json, a versioned report is printed to stdout:
  {schema_version, workspace, workspace_dir, environment, environment_declared,
   module?, service?, status: ready|not_ready, checks: [{code, name, status,
   message, remediation?}]}

--module and --service narrow the scope to one unit's declared configuration
dependencies, so a sibling unit's missing configuration does not fail the
check. --service is the narrower of the two and wins when both are given.

Stable diagnostic codes: workspace_not_found, workspace_invalid,
environment_not_found, module_not_found, service_not_found,
module_reference_unresolved,
module_trust_missing, module_checkout_version_drift,
service_override_active, service_override_unresolved,
service_override_contract_drift, agent_override_active,
agent_override_invalid, gitops_dev_deployment_active,
agent_dev_build,
configuration_directory_missing, configuration_missing,
configuration_invalid, configuration_duplicate, provider_not_configured,
provider_executable_missing, provider_authentication_required,
provider_resolution_failed, plaintext_not_allowed, reference_scheme_unknown,
timeout. External provider
binding checks add external_provider.* codes (bindings_unreadable,
bindings_schema_unknown, and the per-binding validation codes).
```

```
codefly doctor workspace [flags]
```

Flags:

```
      --env string         Environment to validate against (default "local")
      --json               Print a machine-readable report to stdout
      --module string      Restrict validation to one module's services and their declared configuration dependencies
      --service string     Restrict validation to one service's declared configuration dependencies
      --timeout duration   Overall bound; secret resolution is cancelled when it expires (default 30s)
```

## `codefly endpoint`

Resolve one service endpoint to a script-friendly host and port

```
Endpoint resolves ONE endpoint of a service to its concrete host:port and
prints just that address to stdout (nothing else). Exits non-zero if no
single endpoint matches.

A running flow is authoritative for the concrete address; otherwise the
address is computed deterministically so it still resolves before the service
starts. Use --type to pick the API when a service has several endpoints, or
--endpoint to pick by name; if exactly one endpoint matches, neither is
required. Pass --require-up to fail unless the endpoint's declared readiness
predicate passes.

Examples:
  codefly endpoint mind --type grpc           # -> localhost:6690
  codefly endpoint mind --type http           # -> http://localhost:6691
  PORT=$(codefly endpoint mind --type grpc)
  codefly endpoint mind --type grpc --require-up
```

```
codefly endpoint [service]
```

Flags:

```
      --endpoint string       Endpoint name to resolve (when a service has several of the same API)
      --naming-scope string   Naming scope the service runs under (advanced; empty for the normal case)
      --require-up            Fail unless the endpoint's declared readiness predicate passes
      --type string           API type to resolve (grpc, rest, tcp, http, connect, mcp)
```

## `codefly environment`

Declare and inspect deploy environments

```
codefly environment <subcommand>
```

Subcommands:

- [`codefly environment import`](#codefly-environment-import)
- [`codefly environment show`](#codefly-environment-show)

## `codefly environment import`

Import explicit environment declarations from a codefly/coordinate/v1 contract

```
Import a codefly/coordinate/v1 descriptor into workspace.codefly.yaml.
The producer supplies Codefly environment declarations with resolved endpoints,
secret references and delivery paths. The requested environment and namespace
must match the declaration; import never retargets a contract.

  codefly environment import production --coordinate-contract coordinate.json

Declared fields replace their named values. Omitted fields and unrelated map
entries are preserved, comments included. Read from stdin with
--coordinate-contract -.
```

```
codefly environment import <env> --coordinate-contract <file|->
```

Flags:

```
      --coordinate-contract string   Path to a codefly/coordinate/v1 descriptor, or - for stdin
      --dry-run                      Print the unified diff of workspace.codefly.yaml and write nothing
      --namespace string             Assert the namespace declared by the producer
```

## `codefly environment show`

Print the resolved environment as YAML (or --json)

```
codefly environment show <env>
```

Flags:

```
      --json   Print the environment as JSON instead of YAML
```

## `codefly explain`

Show static help with an optional workspace-aware AI explanation

```
Show the complete static help for a command and, when a help provider is installed,
append a contextual AI explanation.

The provider receives command help and a bounded inventory of workspace,
module, service, job, and environment names. It never receives source files.
If the provider is unavailable or not configured, the static help still succeeds.
```

```
codefly explain [command...]
```

## `codefly expose`

Expose workspace services for local Kubernetes development

```
codefly expose <subcommand>
```

Subcommands:

- [`codefly expose service`](#codefly-expose-service)

## `codefly expose service`

Render edge routing manifests for a service's public endpoints

```
Service renders the Kubernetes routing manifests that publish a service's
public endpoints at the shared gateway. Hostnames come from the environment's
ingress intent (or --host); the in-cluster backend and port are resolved
deterministically, so nothing is guessed.

The default backend emits Gateway API GRPCRoute/HTTPRoute (implemented by Istio
when the gateway's class is istio); --routing istio emits the legacy
VirtualService envelope instead. Pass --prefix to scope gRPC routes to a proto
package.

Manifests print to stdout by default, or write to --output as one file per
service and environment so install/uninstall carries routing automatically.

Examples:
  codefly expose service accounts --host api.acme.dev --prefix acme.accounts.v1
  codefly expose service accounts --routing istio --output deployment/routes
```

```
codefly expose service [service]
```

Flags:

```
      --env string                 Environment whose namespace and ingress hosts to render for (default "local")
      --gateway string             Shared gateway the routes attach to (default "codefly-gateway")
      --gateway-namespace string   Namespace of the shared gateway (defaults to the service namespace)
      --host stringArray           Override the ingress hostnames (repeatable)
      --mtls                       Emit an Istio STRICT-mTLS PeerAuthentication (selector app=<service>); enable only when your workloads carry that label
      --output string              Write manifests to this directory instead of stdout
      --prefix string              gRPC proto package to scope gRPC routes to (matched as <prefix>.*)
      --routing string             Routing backend (gateway-api, istio) (default "gateway-api")
```

## `codefly fix`

Apply safe, plugin-owned repairs to service source code

```
codefly fix <subcommand>
```

Subcommands:

- [`codefly fix service`](#codefly-fix-service)
- [`codefly fix source`](#codefly-fix-source)

## `codefly fix service`

Repair a service's source with its language plugin

```
codefly fix service [module/]service
```

Flags:

```
      --aggressive     Enable explicitly aggressive language fixes
      --check          Preview without writing and fail if changes are needed
      --dry-run        Preview changes and evidence without writing
  -f, --file strings   Source-root-relative file to repair (repeatable)
      --json           Emit machine-readable JSON evidence
      --mode string    Fix mode: safe or aggressive (default "safe")
  -p, --path strings   Directory scope to repair recursively (repeatable)
```

## `codefly fix source`

Repair an arbitrary source checkout with its detected plugin

```
codefly fix source [flags]
```

Flags:

```
      --aggressive     Enable explicitly aggressive language fixes
      --check          Preview without writing and fail if changes are needed
      --dir string     Source checkout (default: current directory)
      --dry-run        Preview changes and evidence without writing
  -f, --file strings   Source-root-relative file to repair (repeatable)
      --json           Emit machine-readable JSON evidence
      --mode string    Fix mode: safe or aggressive (default "safe")
  -p, --path strings   Directory scope to repair recursively (repeatable)
```

## `codefly generate`

Generate API clients or protobuf bindings from service contracts

```
codefly generate <subcommand>
```

Subcommands:

- [`codefly generate client`](#codefly-generate-client)
- [`codefly generate contracts`](#codefly-generate-contracts)
- [`codefly generate proto`](#codefly-generate-proto)
- [`codefly generate runnables`](#codefly-generate-runnables)
- [`codefly generate tenant-overlays`](#codefly-generate-tenant-overlays)

## `codefly generate client`

Generate a codefly library (bindings + facade) from an API contract

```
Generate a codefly library — generated bindings plus a generated facade —
into libraries/<name>/<language>/, with a library.codefly.yaml recording the
contract it was generated from.

The contract can come from the local module (--from module/service[/endpoint]),
from a composed module package (--from package:<id>@<version>), or from a
contracts directory on disk (--from contracts:<dir>).
```

```
codefly generate client [flags]
```

Flags:

```
      --endpoint string      select an endpoint when --from package:/contracts: resolves to more than one: service/endpoint (a local --from names its endpoint as its own third segment instead)
      --force                overwrite an existing library with a different contract digest
      --from string          contract source: module/service[/endpoint], package:<id>@<version>, or contracts:<dir>
      --go-module string     Go module path for the generated go/ library (default: github.com/codefly-dev/<name>-go)
      --language strings     languages to generate: go,typescript,python
      --module-name string   facade entry-point name (default: derived from the contract)
      --name string          library name (default: <module>-<service>-client)
      --no-facade            generate bindings only, no facade
      --npm-scope string     npm package name for the generated typescript/ library (default: @codefly-dev/<name>)
      --output string        output directory (default: <workspace>/libraries/<name>)
      --services strings     restrict the facade to these protobuf services (protobuf contracts only)
```

## `codefly generate contracts`

Export a module's interface endpoints as API contracts into its package

```
Export the API contract of every endpoint a module's interface exposes into
contracts/api and write the catalog (contracts/api/catalog.codefly.json), so the
module package carries the contract.

gRPC and connect endpoints get a serialized FileDescriptorSet (contract.binpb)
plus a copy of the service's proto sources; REST endpoints get their OpenAPI
document (openapi.json). HTTP, TCP, and MCP endpoints have no machine-readable
contract and are skipped, as is a connect endpoint whose service has no proto.

Run it before module-package build; the package carries the result. --check
is the CI drift gate: it regenerates into a temporary directory and compares
against what's on disk, without writing anything.
```

```
codefly generate contracts [module]
```

Flags:

```
      --check           do not write; exit 1 if the on-disk catalog differs from what would be generated
      --format string   output format: text|json (default "text")
      --output string   output directory for generated contracts (default: <module dir>/contracts/api)
```

## `codefly generate proto`

Generate Go and Python bindings from local protobuf files

```
Generate code from local proto files without pushing to buf.build first.

Runs buf inside the versioned proto companion image, using the buf.gen.yaml in
the --proto directory, or an explicit --template relative to --output.
--local selects --output/buf.gen.local.yaml, not execution on the host.
Go, gRPC, Connect, gateway, OpenAPI and TypeScript
outputs, then goimports over every Go output the template declares. Nothing
runs on the host but Docker, and the plugins and the formatter are the image's,
pinned by its tag, so two machines regenerate the same bytes.

Examples:
  codefly generate proto --proto ../proto --output ./generated
  codefly generate proto --proto ../proto --output ../code --path saas/v1/service.proto
```

```
codefly generate proto [flags]
```

Flags:

```
      --local             select --output/buf.gen.local.yaml; plugins still run inside the pinned companion
      --output string     path to output directory with buf.gen.yaml (required)
      --path strings      limit generation to a proto-relative path (repeatable)
      --proto string      path to proto source directory (required)
      --template string   generation template, relative to --output or absolute; runs in its own directory
```

## `codefly generate runnables`

Derive SERVICE-facility Runnable packages from the methods a module's contracts mark

```
Derive a Runnable package for every gRPC method a module's published contracts
mark with the codefly.runnable.v0.operation option.

A unary, idempotent method an owner service already publishes becomes a
Runnable by derivation, not by authoring: the method option says which methods
are operations and under what execution policy, and the message descriptors say
what the contract is. Nothing is written twice.

The input is what `codefly generate contracts` already wrote — the serialized
FileDescriptorSet of each gRPC/connect endpoint. Run that first; no flag names a
method, because the option is the only selector.

For each marked method this writes, under contracts/runnables:

  <service>/<endpoint>/<Method>/runnable-package.json   the canonical package
  <service>/<endpoint>/<Method>/operation.json          its execution policy and authority
  index.json                                            one row per operation

The tree is fully owned: a method that no longer carries the option loses its
directory. A module with no marked method writes an empty index, not an error.

A streaming method, a payload outside the bounded schema profile, or an option
core refuses is named and skipped, and the command exits non-zero after
reporting every one of them.

--check is the CI drift gate: it regenerates into a temporary directory and
prints a unified diff of what differs, without writing anything.

Examples:
  codefly generate runnables
  codefly generate runnables documents
  codefly generate runnables --check
```

```
codefly generate runnables [module]
```

Flags:

```
      --check           do not write; exit 1 with a unified diff if the on-disk tree differs from what would be generated
      --output string   output directory for derived runnables (default: <module dir>/contracts/runnables)
```

## `codefly generate tenant-overlays`

Generate overlays/<tenant>-<cloud>/ from a tenant model

```
Expand a tenant model into per-tenant Kustomize overlays.

Instead of hand-authoring one overlay per tenant × cloud, declare the matrix
once in a tenant model file. Each generated overlays/<tenant>-<cloud>/ layers
the shared base and patches only what varies between tenants: the
VirtualService host and the External Secrets store reference.

The base directory named by the model must exist and contain a VirtualService.

Example tenant model:

  schema-version: codefly.dev/tenant-model/v1
  base: base
  tenants:
    - name: acme
      cloud: aws
      host: acme.example.com
      secret-store: acme-aws
    - name: acme
      cloud: gcp
      host: acme.gcp.example.com
      secret-store: acme-gcp

Example:
  codefly generate tenant-overlays --model deployment/kustomize/tenants.codefly.yaml
```

```
codefly generate tenant-overlays [flags]
```

Flags:

```
      --model string   path to the tenant model file (required)
      --root string    deployment tree root containing the base (default: tenant model directory)
```

## `codefly get`

Inspect configured service endpoints and their live reachability

```
codefly get <subcommand>
```

Subcommands:

- [`codefly get endpoints`](#codefly-get-endpoints)

## `codefly get endpoints`

Report addresses and health for a service's declared endpoints

```
Endpoints lists the endpoints declared by a service — name, API type,
visibility — together with the localhost address each binds to and whether
it is currently reachable.

A running scoped flow is authoritative for addresses. Without one, addresses
fall back to the same deterministic hash used for offline planning. The STATUS
column reflects a live TCP probe: "up" means something is listening now.

Examples:
  codefly get endpoints                 # active service
	codefly get endpoints mind            # by name
	codefly get endpoints mind --type grpc
```

```
codefly get endpoints [service]
```

Flags:

```
      --naming-scope string   Naming scope the service runs under (advanced; empty for the normal case)
      --type string           Filter by API type (grpc, rest, tcp, http, connect, mcp)
```

## `codefly import`

Import existing source code as Codefly workspace resources

```
codefly import <subcommand>
```

## `codefly init`

Create and configure a new Codefly workspace

```
codefly init <subcommand>
```

Subcommands:

- [`codefly init workspace`](#codefly-init-workspace)

## `codefly init workspace`

Create a Codefly workspace in a new directory

```
codefly init workspace [flags]
```

Flags:

```
      --default         use default values for all prompts
  -i, --interactive     interactive mode
      --layout string   workspace layout: flat or modules
```

## `codefly install`

Install reusable components into the current workspace

```
codefly install <subcommand>
```

Subcommands:

- [`codefly install library`](#codefly-install-library)

## `codefly install library`

Install a published library export into the current workspace

```
Resolve the highest published version of a library export satisfying a
semantic version constraint, and print its durable install handle.

Examples:
  codefly install library authkit@^1.0.0 --language go
  codefly install library authkit@^1.0.0 --language go --destination ./services/api
```

```
codefly install library <name>@<constraint>
```

Flags:

```
      --destination string   Directory to run the native install command in (default: only print the resolved coordinates)
      --language string      Language export to install (go, python, typescript)
```

## `codefly lint`

Run plugin-owned lint checks for a service

```
codefly lint <subcommand>
```

Subcommands:

- [`codefly lint service`](#codefly-lint-service)

## `codefly lint service`

Run the service plugin's configured lint checks

```
codefly lint service [module/]service
```

Flags:

```
      --runtime-context string   Runtime context for validation (default "free")
```

## `codefly list`

List jobs, runnables and other resources in the current workspace

```
codefly list <subcommand>
```

Subcommands:

- [`codefly list jobs`](#codefly-list-jobs)
- [`codefly list libraries`](#codefly-list-libraries)
- [`codefly list runnables`](#codefly-list-runnables)

## `codefly list jobs`

List jobs across the workspace or within one module

```
List all jobs in the workspace or a specific module.

Examples:
  # List all jobs
  codefly list jobs

  # List jobs in a specific module
	codefly list jobs --module=backend
```

```
codefly list jobs [flags]
```

Flags:

```
      --module string   Module to list jobs from
```

## `codefly list libraries`

List internal libraries available to workspace services

```
List the libraries under the workspace's libraries/ directory.

--remote additionally queries each library's configured store (network) for
its published versions.

Examples:
  codefly list libraries
  codefly list libraries --remote
  codefly list libraries --json
```

```
codefly list libraries [flags]
```

Flags:

```
      --json     Emit machine-readable JSON
      --remote   Include each library's published versions from its configured store (network)
```

## `codefly list runnables`

List runnables across the workspace or within one module

```
List the runnables (typed finite operations) declared in the workspace.

A runnable is identified by module/name @ version: a version is an immutable
release and several versions of one name coexist.

Examples:
  codefly list runnables
  codefly list runnables --module=backend
  codefly list runnables --json
```

```
codefly list runnables [flags]
```

Flags:

```
      --json            Emit machine-readable JSON
      --module string   Module to list runnables from
```

## `codefly login`

Authenticate this workspace with the Codefly platform

```
codefly login [flags]
```

## `codefly logs`

Read or follow logs from the current Codefly session

```
Show codefly's detailed session logs (~/.codefly/logs/<date>.log).

By default prints the last 100 lines of today's log, pretty-printed. This is the
log the failure hint points at, so the usual flow after a command fails is:

  codefly logs            # what just happened
  codefly logs -f         # follow live (e.g. in a second terminal during a run)
  codefly logs -n 500     # more history
  codefly logs --raw      # raw JSON lines (for jq / grep)
  codefly logs --path     # just print the file path (for scripts)
```

```
codefly logs [flags]
```

Flags:

```
      --file string   Read a specific log file instead of today's
  -f, --follow        Follow the log live
      --path          Print only the log file path
      --raw           Print raw JSON lines (for jq/grep)
  -n, --tail int      Show the last N lines (0 = all) (default 100)
```

## `codefly mcp`

Expose Codefly workspace tools to AI clients over MCP

```
Model Context Protocol (MCP) server for codefly.

This allows AI assistants like Claude to interact with your codefly workspace,
query services, and perform development operations.

Usage with Claude Desktop:
  Add to your Claude Desktop config file:
    macOS:   ~/Library/Application Support/Claude/claude_desktop_config.json
    Windows: %APPDATA%\Claude\claude_desktop_config.json
  {
    "mcpServers": {
      "codefly": {
        "command": "codefly",
        "args": ["mcp", "serve"]
      }
    }
  }
```

```
codefly mcp <subcommand>
```

Subcommands:

- [`codefly mcp serve`](#codefly-mcp-serve)
- [`codefly mcp tools`](#codefly-mcp-tools)

## `codefly mcp serve`

Run the Codefly MCP server over standard input and output

```
Start the MCP server in stdio mode for integration with AI assistants.

The server communicates via JSON-RPC 2.0 over stdin/stdout, following the
Model Context Protocol specification.
```

```
codefly mcp serve [flags]
```

## `codefly mcp tools`

List the tools exposed by the Codefly MCP server

```
codefly mcp tools [flags]
```

## `codefly open`

Open a workspace, module, or service in your configured editor

```
codefly open <subcommand>
```

Subcommands:

- [`codefly open module`](#codefly-open-module)
- [`codefly open service`](#codefly-open-service)
- [`codefly open workspace`](#codefly-open-workspace)

## `codefly open module`

Open the active module in your configured editor

```
codefly open module [flags]
```

Flags:

```
      --editor string   your editor: 'code' for vscode, 'goland' for goland (default "code")
```

## `codefly open service`

Open the active service in your configured editor

```
codefly open service [flags]
```

Flags:

```
      --editor string   your editor: 'code' for vscode, 'goland' for goland (default "code")
```

## `codefly open workspace`

Open the current workspace in your configured editor

```
codefly open workspace [flags]
```

Flags:

```
      --editor string   your editor: 'code' for vscode, 'goland' for goland (default "code")
```

## `codefly override`

Point part of a composed module somewhere else on this machine

```
codefly override <subcommand>
```

Subcommands:

- [`codefly override service`](#codefly-override-service)

## `codefly override service`

Run one service of a composed module from a checkout, a worktree, or another version

```
Override where a single service of a composed module comes from on this machine.

The override is written to codefly.local.yaml, which is gitignored and never
committed: the workspace's committed configuration keeps naming the module as a
whole, and only this machine runs the service from somewhere else. The rest of
the module is unaffected.

Exactly one of --path, --worktree, or --version selects the source:

  --path      a directory holding the service, for editing it in place
  --worktree  <owner/repo>@<ref>, matched against your local checkouts; the
              service is taken from that checkout's copy of the module
  --version   the module package at that version, pulled under the workspace's
              module-trust policy; the service is taken from it

The overriding directory must be the same service the module composed: same
name, same agent name, and at least the endpoints the module declares. Its agent
version may differ — running a service at a different agent version is a reason
to override it.

This works on every workspace layout. On a flat (single-module) workspace the
module name is the workspace's own name, and `codefly run service --service-path`
remains the per-run spelling for the service you are launching.
```

```
codefly override service <module>/<service>
```

Flags:

```
      --clear             remove the override and go back to the module's own copy
      --path string       directory holding the service
      --version string    module package version to take the service from
      --worktree string   <owner/repo>@<ref> of a local checkout to take the service from
```

## `codefly package`

Create portable service artifacts with a Codefly plugin

```
codefly package <subcommand>
```

Subcommands:

- [`codefly package service`](#codefly-package-service)

## `codefly package service`

Package one service's source resource with its plugin

```
codefly package service [module/]service
```

Flags:

```
      --format string            Output format: text or json (default "text")
      --name string              Portable artifact base name (defaults to resource name)
      --output-dir string        Directory for portable package artifacts (default ".codefly/packages")
      --publisher string         Release subject publisher
      --sbom                     Emit release-bound CycloneDX evidence (default true)
      --subject-name string      Release subject name
      --subject-version string   Release subject version
      --target strings           Target os/architecture (repeatable; empty uses the host)
```

## `codefly provider`

Manage external provider bindings for an environment

```
Manage external provider bindings declared in provider-bindings.codefly.yaml.

A binding names an exact provider agent, a management mode (observe, managed, or
disabled), the public inputs and secret references it collects, and the output
configuration contract it projects into. Bindings are environment-scoped: select
one with --env.

Every command prints a deterministic report with --json and never emits a secret
value or a raw provider body. Exit codes are stable:

  0  success, no diff, or apply complete
  1  invalid configuration, incompatible provider, or unclassified failure
  2  a valid plan diff is present
  3  policy denied the operation
  4  the operation requires approval
  5  a partial outcome (some effects landed)
  6  an uncertain outcome that must not be blindly retried
  7  a stale plan, state, or endpoint
```

```
codefly provider <subcommand>
```

Subcommands:

- [`codefly provider apply`](#codefly-provider-apply)
- [`codefly provider destroy`](#codefly-provider-destroy)
- [`codefly provider disconnect`](#codefly-provider-disconnect)
- [`codefly provider doctor`](#codefly-provider-doctor)
- [`codefly provider import`](#codefly-provider-import)
- [`codefly provider list`](#codefly-provider-list)
- [`codefly provider plan`](#codefly-provider-plan)
- [`codefly provider setup`](#codefly-provider-setup)

## `codefly provider apply`

Execute a previously calculated plan

```
codefly provider apply [flags]
```

Flags:

```
      --json                               Print a machine-readable report to stdout
      --plan codefly provider plan --out   Path to a plan produced by codefly provider plan --out
```

## `codefly provider destroy`

Delete the owned or adopted remote resources of a binding

```
Delete a binding's remote resources.

Only resources the binding owns or has explicitly adopted are deleted, and only
when the binding declares deletion-policy: delete-owned. The default retain
policy never deletes remote resources.
```

```
codefly provider destroy BINDING
```

Flags:

```
      --env string   Environment the binding belongs to (default "local")
      --json         Print a machine-readable report to stdout
```

## `codefly provider disconnect`

Stop managing a binding without deleting its remote resources

```
codefly provider disconnect BINDING
```

Flags:

```
      --env string   Environment the binding belongs to (default "local")
      --json         Print a machine-readable report to stdout
```

## `codefly provider doctor`

Diagnose provider bindings for an environment

```
Diagnose provider bindings.

This command performs the offline half of provider diagnostics: it validates
each binding's identity, mode, secrets hygiene, output contract, and endpoint
references. The remote, read-only half — authentication, account, observation,
resource health, and drift — requires the host coordinator and is reported as
unavailable until that layer is wired.

For the bounded, no-agent workspace checks, use `codefly doctor workspace`.
```

```
codefly provider doctor [BINDING]
```

Flags:

```
      --env string   Environment whose bindings to diagnose (default "local")
      --json         Print a machine-readable report to stdout
```

## `codefly provider import`

Adopt an existing remote resource into a binding by exact identity

```
Adopt an existing remote resource into a binding.

Adoption is exact and explicit: it requires the resource TYPE and its exact
REMOTE_ID. URL, name, domain, or email similarity never adopts a resource.
```

```
codefly provider import BINDING TYPE REMOTE_ID
```

Flags:

```
      --env string   Environment the binding belongs to (default "local")
      --json         Print a machine-readable report to stdout
```

## `codefly provider list`

List provider bindings for an environment, or print the binding schema

```
codefly provider list [BINDING]
```

Flags:

```
      --env string   Environment whose bindings to list (default "local")
      --json         Print a machine-readable report to stdout
      --schema       Print the binding schema and declared output contracts instead of listing
```

## `codefly provider plan`

Calculate a deterministic plan for a binding

```
codefly provider plan BINDING
```

Flags:

```
      --env string      Environment the binding belongs to (default "local")
      --json            Print a machine-readable report to stdout
      --out string      Write the calculated plan to this path
      --refresh-only    Refresh observation without calculating a plan
      --validate-only   Validate desired input without observing remote state
```

## `codefly provider setup`

Run the full setup lifecycle for a binding

```
codefly provider setup BINDING
```

Flags:

```
      --dry-run      Validate and plan without applying any effect
      --env string   Environment the binding belongs to (default "local")
      --json         Print a machine-readable report to stdout
```

## `codefly ps`

List dev servers running in codefly workspaces on this machine

```
List frontend dev servers (next dev / npm run dev / vite) running inside a
codefly workspace, machine-wide. STATUS is one of: orphaned (codefly's, escaped
its supervisor — reaped by 'codefly clear'), tracked (codefly's, still
supervised), or external (not codefly's — shown for visibility, never reaped).
```

```
codefly ps [flags]
```

Flags:

```
      --json   Print the dev servers as JSON
```

## `codefly publish`

Version, tag, and push a release for the current Codefly repository

```
publish bumps the manifest version and lands it on main through a
release pull request, then tags the commit main ends up carrying and
pushes that tag. One command for every codefly-dev repo — modules
(core, cli, sdk-go) and agents (every services/* and modules/*) all
use the same flow.

Routing the bump through a pull request is what lets main require its
checks of every account, admins included: a release commit pushed
straight to main carries no check results and a protected main would
have to exempt someone to accept it. Tags are outside branch
protection, so the tag push stays direct.

The mode is auto-detected from cwd:
  agent.codefly.yaml         → agent
  version/info.codefly.yaml  → core module
  pkg/cli/info.yaml          → cli
  info.codefly.yaml          → standalone module (sdk-go, etc.)

Pre-flight gates (any failure aborts cleanly with no side effects):
  - working tree clean
  - on main branch
  - in sync with origin/main
  - tag doesn't already exist (local or remote)

Push is NEVER --force for main or the tag. If pre-flight fails the
operator must resolve the divergence by hand — refusing to overwrite
in-flight work is the whole point.

If the release pull request merges but the tag push then fails, the
bump is on main without a release. Re-run publish: it recognizes the
untagged release commit and finishes it rather than bumping again.

For service-agent repos (agent.codefly.yaml) publish also, in order:
  - runs release-grade agent CI against the bumped version and aborts
    the publish untouched if it fails or a required loader platform
    (darwin/arm64, linux/amd64) is missing
  - creates the GitHub release for the new tag
  - uploads the loader-compatible archives + SBOMs
  - verifies every archive resolves through the install URL resolver
Requires the gh CLI to be authenticated.

Module-agent repos run source/build/audit CI and publish the immutable Git tag
their module package is built from; they do not publish service-loader assets.

Running from CI (--ci, on by default when CI=true): the runner has no git
identity and no signing key, so the release commit and tag are made as the
GitHub Actions bot with signing off (main still receives GitHub's signed squash
commit), and a release whose CI never registers — the signature of a token
that cannot trigger workflows — fails after a grace period instead of waiting
out the whole budget. A workflow-owned agent release (release.owner: workflow)
no longer needs a darwin/arm64 host: its own workflow builds every platform.

--remote releases from GitHub instead of from here: it dispatches the
repository's release workflow (.github/workflows/publish.yml by default, the
caller of codefly-dev/cli's reusable publish-agent.yml) on main and returns;
the laptop can close. --wait follows the run to its conclusion.

Examples:
  codefly publish              # patch bump
  codefly publish minor
  codefly publish major
  codefly publish beta         # next beta, or advance beta.N
  codefly publish --dry-run    # show what would happen, change nothing
  codefly publish --remote     # run the release on GitHub, return at once
  codefly publish minor --remote --wait
```

```
codefly publish [patch|minor|major|beta]
```

Flags:

```
      --ci                run as a CI job: bot git identity, no signing, fail fast when CI never registers (default: on when CI=true)
      --dir string        manifest directory (default: cwd)
      --dry-run           show what would happen without modifying anything
      --remote            dispatch the repository's release workflow on GitHub instead of releasing from this machine
      --wait              with --remote, follow the dispatched release to its conclusion
      --workflow string   with --remote, the release workflow file in .github/workflows (default "publish.yml")
```

Subcommands:

- [`codefly publish all`](#codefly-publish-all)
- [`codefly publish clients`](#codefly-publish-clients)
- [`codefly publish dev`](#codefly-publish-dev)
- [`codefly publish library`](#codefly-publish-library)
- [`codefly publish re-tag`](#codefly-publish-re-tag)

## `codefly publish all`

Release every Codefly repository in dependency order

```
all discovers every git repo under the workspace carrying a codefly
manifest (agent.codefly.yaml | version/info.codefly.yaml | pkg/cli/info.yaml
| info.codefly.yaml) and runs the same flow as plain `codefly publish`
on each.

Order is core → cli → standalone modules → agents, so a dependency is
released before the consumers that pin it.

Safety — the whole run is atomic at the pre-flight boundary:
  1. EVERY repo is validated first (clean tree, on main, in sync with
     origin, target tag free; service agents also: host can build every loader
     platform and gh is available). If ANY repo fails, the run aborts
     before a single tag is pushed.
  2. Only once all repos pass does it publish, sequentially, stopping at
     the first real failure (and reporting what already shipped).

Release-grade agent CI is the publish gate itself, so it runs per-agent
during step 2 — a CI failure there aborts the run with earlier repos
already shipped, exactly like any other execute-phase failure.

As with plain publish, each bump lands on its repo's main through a
release pull request and the tag is cut from the merged commit; no
push is ever --force.

Examples:
  codefly publish all              # patch-bump every repo
  codefly publish all minor
  codefly publish all --dry-run    # print the full plan, change nothing
  codefly publish all --root DIR   # workspace root (default: nearest go.work, else cwd)
  codefly publish all --remote     # each release runs on GitHub, one after another

With --remote nothing is built or pushed from this machine: every repository
must carry the release workflow (checked for all of them before the first
dispatch), and each dispatched release must conclude successfully before the
next repository's is dispatched, so a consumer is never released against a
dependency that did not ship.
```

```
codefly publish all [patch|minor|major]
```

Flags:

```
      --ci codefly publish --help   run as a CI job (default: on when CI=true); see codefly publish --help
      --dry-run                     print the full plan without modifying anything
      --remote                      release each repository on GitHub, in dependency order: dispatch its release workflow and wait for it before the next
      --root string                 workspace root to scan (default: nearest go.work ancestor, else cwd)
      --workflow string             with --remote, the release workflow file every repository carries (default "publish.yml")
```

## `codefly publish clients`

Generate and publish a client library for every API contract a module exports

```
Publish a client library for each endpoint in the module's interface: block
that exports an API contract (codefly generate contracts), in every language
the contract kind supports — go, typescript and python for protobuf, go and
typescript for OpenAPI.

Each endpoint becomes one codefly library named <module>-<service>-<endpoint>-client,
generated exactly as `codefly generate client` would (bindings plus facade) and
published at the module package version through the stores configured under
the workspace's libraries.publish block. Versions are immutable.

An endpoint restricts or opts out of client publishing in clients.codefly.yaml:

  schema: codefly/module-clients-config/v1
  endpoints:
    - service: accounts
      endpoint: connect
      languages: [go, typescript]                # default: every supported language
      services: [AuditService, WebhookService]   # facade subset; protobuf only
      publish: false                             # opt out

contracts/clients.codefly.json records what was published (library, language,
import path, digest) for the current package version. A contract whose digest
moved without a package version bump is refused: bump module.package.codefly.yaml
first. Re-running after a partial failure publishes only what is still missing.

Examples:
  codefly publish clients saas-starter --dry-run     # plan + identities, no toolchain, no network
  codefly publish clients saas-starter --check       # CI gate: every client of this version is recorded as published
  codefly publish clients saas-starter --language go
  codefly publish clients saas-starter --output ./libraries   # keep the generated libraries
```

```
codefly publish clients [module]
```

Flags:

```
      --check                       Exit 1 unless contracts/clients.codefly.json records every client of the current package version; publishes nothing
      --create-missing-repository   Create a go/python client's GitHub repository when it does not exist yet (private unless --public-repository)
      --dry-run                     Show what would be generated and published without touching a toolchain or a store
      --language strings            Restrict publishing to these languages (default: every language each endpoint declares or supports)
      --output string               Directory to generate the libraries into (default: a temporary directory removed afterwards)
      --public-repository           Create repositories public instead of private; a client's bindings carry every message in the contract, so this discloses the whole surface
```

## `codefly publish dev`

Publish an agent build for iteration, without a release

```
dev builds the agent in the current repository exactly as `codefly publish`
builds it (release-grade agent CI, the same loader archives and SBOMs) and
publishes it under a dev version derived from the commit:

  <current-version>-dev.<12-char-sha>     e.g. 0.1.47-dev.abc123def456

It runs on any branch and bumps nothing: agent.codefly.yaml keeps its version,
no release pull request is opened, and main is never touched. The build is
published where release assets go, as a GitHub PRERELEASE that is never marked
Latest, under the tag v<dev-version> pointing at HEAD. Semver ranks a prerelease
below its release, so a dev build can never collide with or outrank one, and
`version: latest` never resolves to it.

A dirty working tree is refused unless --allow-dirty is given; the build then
carries changes the tagged commit does not, and publishing different bytes
again under the same commit is refused (published assets are immutable) —
commit to get a new dev version. An agent whose releases are published by a
workflow (release.owner: workflow) is built by that workflow from the tag, so
--allow-dirty is refused for it, and its GoReleaser configuration must publish
prerelease tags as prereleases (release.prerelease: auto).

Dev builds are for iteration only. `codefly publish patch` remains the release path.
```

```
codefly publish dev [flags]
```

Flags:

```
      --allow-dirty   publish from a working tree with uncommitted changes
      --dir string    agent directory (default: cwd)
      --dry-run       print the dev version without building or publishing
```

## `codefly publish library`

Publish a workspace library's language exports to their configured stores

```
Publish a workspace library (codefly add library) to the durable stores
configured under the workspace's libraries.publish block — a GitHub repository
tagged at the version for go/python, an npm-compatible registry for
typescript. Published versions are immutable: an identical retry adopts the
existing version, while different bytes require a version bump.

Configure workspace.codefly.yaml:

  libraries:
    publish:
      go: {owner: codefly-dev}
      typescript: {registry: https://npm.pkg.github.com, scope: "@codefly-dev"}
      python: {owner: codefly-dev}

Examples:
  codefly publish library authkit --dry-run
  codefly publish library authkit --version 1.2.0
  codefly publish library authkit --language go,python
```

```
codefly publish library <name>
```

Flags:

```
      --create-missing-repository   Create a go/python export's GitHub repository when it does not exist yet (private unless --public-repository)
      --dry-run                     Show what would be published without publishing anything
      --language strings            Languages to publish (defaults to all of the library's declared languages)
      --public-repository           Create repositories public instead of private; a client's bindings carry every message in the contract, so this discloses the whole surface
      --version string              Version to publish (defaults to the library's version)
```

## `codefly publish re-tag`

Move the current manifest tag to HEAD without rewriting main

```
re-tag deletes the manifest's current tag locally + on origin and
recreates it at HEAD. Used when CI needs a fresh tag-push trigger
without bumping the version.

The TAG is force-pushed (that's the whole point); main is never
touched. CLI tags are immutable and cannot use this recovery path.
Pre-flight checks are the same as publish minus the "tag doesn't
exist" gate (which would invert the meaning here).

If the tag doesn't exist, re-tag refuses and points at plain
publish — first-time releases use the bump path.
```

```
codefly publish re-tag [flags]
```

Flags:

```
      --ci           run as a CI job: bot git identity, no signing (default: on when CI=true)
      --dir string   manifest directory (default: cwd)
      --dry-run      show what would happen without modifying anything
```

## `codefly replay`

Re-run operations recorded in a Codefly action track

```
codefly replay [flags]
```

Flags:

```
      --dir string     Replay codefly tracks
      --track string   Replay codefly tracks
```

## `codefly run`

Start a service or job in its local workspace context

```
codefly run <subcommand>
```

Subcommands:

- [`codefly run command`](#codefly-run-command)
- [`codefly run job`](#codefly-run-job)
- [`codefly run service`](#codefly-run-service)
- [`codefly run solution`](#codefly-run-solution)

## `codefly run command`

Run a command with the current service's dependencies

```
Run an explicit one-shot command inside the current service's managed
dependency context.

Codefly starts the service's declared dependencies, waits for readiness,
injects typed endpoints and configurations, executes argv directly, and tears
the owned dependency flow down when the command exits. Use -- before the
program so every following flag belongs to the child command.

Examples:
	codefly run command -- ./scripts/verify
  codefly run command -- ./bin/maintenance --once
```

```
codefly run command -- <program> [args...]
```

Flags:

```
      --dependency-timeout duration   Maximum time to wait for dependencies to become ready (default 2m0s)
      --exclude-dependency strings    Exclude optional dependency services (repeatable)
      --fixture string                Fixture override for the dependency flow
      --naming-scope string           Runtime naming scope for dependency isolation
      --profile string                Named workspace run profile
      --silent strings                Silence dependency services in CLI output
```

## `codefly run job`

Run a one-shot or scheduled job from its module configuration

```
Run a job (scheduled or one-shot task).

Jobs execute to completion and then exit. They can depend on services
which will be started before the job runs.

Examples:
  # Run a job
  codefly run job db-migration --module=backend

  # Run a job with its service dependencies started
  codefly run job db-migration --module=backend --with-services
```

```
codefly run job [name]
```

Flags:

```
      --module string   Module name
      --with-services   Start service dependencies before running job
```

## `codefly run service`

Start one or more services locally with their dependency graph

```
Start services locally with their dependency graph.

Naming several services runs them as roots of a single graph: the services they
share are resolved once and started once, and each root is wired to them as it
would be on its own. This is what a product composition needs — several
solutions against one host — since a graph per solution collides on ports and
on container-recovery scope.

Examples:
  codefly run service app/backend
  codefly run service lastlogin-go/backend lastlogin-python/backend wiki/backend
```

```
codefly run service [service...]
```

Flags:

```
      --cli-server                     Start CLI server
      --env string                     Workspace environment to run (default "local")
      --exclude-dependency strings     Exclude optional dependency services from the run (repeatable, e.g. infra/temporal)
      --exclude-root                   Exclude root service
      --fixture string                 Fixture override (defaults to the selected Codefly environment)
      --headless                       Run without TUI (auto-enabled when no TTY, e.g. MCP, CI, pipes)
      --init-only                      Initialize service only, i.e. without running it
      --load-only                      LoadRequired service only, i.e. without running it
      --naming-scope string            Runtime naming scope: fold a scope into port derivation for a disjoint port set (parallel runs / test encapsulation)
      --open                           Open the dashboard in the default browser (requires --cli-server)
      --output-env string              Write one service's full SDK/runtime environment to an owner-only file
      --output-env-service string      Service whose runtime environment to export (module/service; defaults to the root service)
      --profile string                 Named workspace run profile
      --remote strings                 Remote services
      --runtime-context string         Runtime context for the flow (native/container/nix/free; free picks the first advertised backend) (default "free")
      --service-path string            Path to the service
      --set strings                    Per-service runtime env override (repeatable), e.g. --set warden:CODEFLY__FIXTURE=dogfood
      --silent strings                 Silence services in CLI output
      --stand-alone                    Begin service as standalone, i.e. without its dependencies
      --start-docker                   Auto-start a local Docker engine (OrbStack/Docker Desktop/colima/…) if a service needs Docker and it isn't running; --start-docker=false to disable (default true)
      --temporary-ports show network   Run this flow as a disposable invocation: OS-probed ephemeral ports plus a generated naming scope isolating its agents, containers and runtime state (advanced; not previewable via show network). The Codefly SDK sets it for test-owned dependency stacks. Passing --naming-scope wins, and passing it empty asks for no scope at all
```

## `codefly run solution`

Start a solution locally: boot its service-entry with the full dependency graph

```
codefly run solution [flags]
```

Flags:

```
      --env string                     Workspace environment to run (default "local")
      --exclude-dependency strings     Exclude optional dependency services from the run (repeatable, e.g. infra/temporal)
      --fixture string                 Fixture override (defaults to the selected Codefly environment)
      --headless                       Run without TUI (auto-enabled when no TTY, e.g. MCP, CI, pipes)
      --naming-scope string            Runtime naming scope: fold a scope into port derivation for a disjoint port set (parallel runs / test encapsulation)
      --profile string                 Named workspace run profile
      --set strings                    Per-service runtime env override (repeatable), e.g. --set warden:CODEFLY__FIXTURE=dogfood
      --silent strings                 Silence services in CLI output
      --temporary-ports show network   Run this flow as a disposable invocation: OS-probed ephemeral ports plus a generated naming scope isolating its agents, containers and runtime state (advanced; not previewable via show network). The Codefly SDK sets it for test-owned dependency stacks. Passing --naming-scope wins, and passing it empty asks for no scope at all
```

## `codefly sbom`

Generate CycloneDX software bills of materials for services

```
codefly sbom <subcommand>
```

Subcommands:

- [`codefly sbom service`](#codefly-sbom-service)
- [`codefly sbom workspace`](#codefly-sbom-workspace)

## `codefly sbom service`

Generate a CycloneDX SBOM for one service through its agent

```
codefly sbom service [name]
```

Flags:

```
      --include-dev     Include development/test dependencies
  -o, --output string   Output file, or - for stdout (default "-")
```

## `codefly sbom workspace`

Generate CycloneDX SBOMs for every service in the workspace

```
codefly sbom workspace [flags]
```

Flags:

```
      --include-dev         Include development/test dependencies
      --output-dir string   Directory for per-service CycloneDX files (default ".codefly/sbom")
```

## `codefly self`

Maintain a local Codefly CLI checkout and installation

```
codefly self <subcommand>
```

Subcommands:

- [`codefly self build`](#codefly-self-build)
- [`codefly self check-update`](#codefly-self-check-update)
- [`codefly self pull`](#codefly-self-pull)
- [`codefly self update`](#codefly-self-update)

## `codefly self build`

Build the CLI from source and install the resulting binary

```
Build compiles the codefly CLI from source and installs it over the
binary currently on your PATH (the one you just invoked).

The CLI source directory is auto-detected by walking up from the current
directory: it looks for the cli module (the directory whose go.mod is
github.com/codefly-dev/cli), or a monorepo root containing cli/. Inside the
codefly.dev monorepo the build picks up local core/ and wool/ changes
automatically via go.work.

With --with-agents, after the CLI is installed it also rebuilds every canonical
agent repository in the Codefly workspace.
Agent repositories are discovered by their top-level agent.codefly.yaml.
Only canonical checkouts whose directory name matches their origin repository
are built; duplicate and task-specific checkouts are reported and skipped.
This is the ONE command to pick up local changes to both the CLI and the
agents in a single step. The per-agent govulncheck audit is skipped during
the bulk build (it adds minutes); pass --audit-agents to run it.

By default each agent is built twice: the native host binary and a
Linux/amd64 static binary for Docker-mode container runs. On a mac running
everything natively the Linux binary is never executed, so --native-only
skips that cross-build and rebuilds only the host binaries — halving the
agent build work for a local edit→rebuild→run loop.

Agents build in parallel, defaulting to one job per CPU. Use -j/--jobs to
cap that concurrency (mirrors `codefly agent build --all -j`).

Examples:
  codefly self build
  codefly self build --with-agents
  codefly self build --with-agents --native-only
  codefly self build --with-agents -j 4
  codefly self build --with-agents --audit-agents
  codefly self build --dir ./cli
  codefly self build --output /usr/local/bin/codefly
```

```
codefly self build [flags]
```

Flags:

```
      --arch string     Cross-compile for this GOARCH (e.g. amd64); implies cross-compile
      --audit-agents    Run the govulncheck audit on each agent during --with-agents (slow; off by default)
      --dir string      CLI source directory (default: auto-detect from current directory)
  -j, --jobs int        With --with-agents: max agents to build in parallel (default: number of CPUs)
      --native-only     With --with-agents: build only host-platform agent binaries; skip the Linux/amd64 container cross-build (local dev fast path)
      --os string       Cross-compile for this GOOS (e.g. linux); produces a static binary instead of installing
      --output string   Install path (default: the running codefly binary; for --os/--arch: bin/<os>/codefly)
      --with-agents     After rebuilding the CLI, also rebuild every canonical agent repository in the Codefly workspace
```

## `codefly self check-update`

Check the authenticated Codefly release feed for an update

```
codefly self check-update [flags]
```

Flags:

```
      --channel string   Release channel: stable or beta (default "stable")
      --json             Print a machine-readable update status
```

## `codefly self pull`

Fast-forward the CLI, Core, LLM, and plugins to their latest main branches

```
Pull updates exactly the repositories that define the local Codefly
runtime: cli/, core/, llm/, and canonical plugin repositories.

Plugins are discovered by a top-level agent.codefly.yaml, the same boundary
used by `codefly self build --with-agents`. A checkout is canonical
when its directory name matches its origin repository name; duplicate worktrees
and task-specific clones are not touched. SDKs, templates, examples, websites,
release repositories, and other workspace siblings are outside this command's
scope.

It never overrides existing code:
  - Repositories on another branch or detached HEAD are skipped.
  - Local commits are preserved (it MERGES origin/<branch>, it never resets).
  - Uncommitted changes are stashed before the merge and restored after.
  - If a merge or stash restore would conflict, that repo is left exactly as
    it was and reported, so you can resolve it manually.

Examples:
  codefly self pull
  codefly self pull --branch develop
  codefly self pull --remote upstream
  codefly self pull --dir ~/Development/deus/codefly.dev
```

```
codefly self pull [flags]
```

Flags:

```
      --branch string   Branch to pull from (default "main")
      --dir string      Monorepo root or any directory inside it (default: auto-detect from current directory)
      --remote string   Remote to pull from (default "origin")
```

## `codefly self update`

Install a verified Codefly release when this binary is directly owned

```
codefly self update [flags]
```

Flags:

```
      --allow-downgrade   Allow the selected channel release to be older than the running version
      --channel string    Release channel: stable or beta (default "stable")
      --yes               Install without an interactive confirmation
```

## `codefly server`

Serve the local dashboard for the current workspace (attaches to a running codefly when one is up)

```
Serve the local dashboard for the current workspace.

If a "codefly run service --cli-server" is already serving this workspace's
dashboard, attach to it: print its URL and exit without starting a second
server.

Otherwise, start an inventory-only dashboard: the Services, Logs and Config
tabs show declared inventory but no live runtime state, since no run is
attached.
```

```
codefly server [flags]
```

Flags:

```
      --open   Open the dashboard in the default browser
```

## `codefly service`

Manage durable per-user services with the native OS supervisor

```
Manage foreground service processes with launchd LaunchAgents on macOS
or systemd user units on Linux. Service definitions are versioned, contain
only non-sensitive configuration, and remain authoritative across CLI runs.
```

```
codefly service <subcommand>
```

Flags:

```
      --json   Emit the typed result as JSON
```

Subcommands:

- [`codefly service install`](#codefly-service-install)
- [`codefly service restart`](#codefly-service-restart)
- [`codefly service start`](#codefly-service-start)
- [`codefly service status`](#codefly-service-status)
- [`codefly service stop`](#codefly-service-stop)
- [`codefly service uninstall`](#codefly-service-uninstall)

## `codefly service install`

Install or atomically update a native service definition

```
codefly service install LABEL
```

Flags:

```
      --executable string          Absolute foreground executable path
      --health-http string         HTTP(S) readiness URL
      --health-interval duration   Readiness retry interval (default 250ms)
      --health-tcp string          TCP readiness address as host:port
      --health-timeout duration    Readiness wait timeout (default 15s)
      --log-mode string            Log routing: native or files (platform default when omitted)
      --public-arg stringArray     Explicitly public executable argument (repeatable)
      --public-env stringArray     Explicitly public NAME=VALUE environment variable (repeatable)
      --restart string             Restart policy: on-failure or never (default "on-failure")
      --restart-delay duration     Delay before a crash restart (default 5s)
      --start-at-login             Enable the service for future user logins (default true)
      --stderr-log string          Absolute stderr log file for file routing
      --stdout-log string          Absolute stdout log file for file routing
      --version string             Materialized service contract version
      --working-directory string   Absolute service working directory
```

## `codefly service restart`

Restart an installed service

```
codefly service restart LABEL
```

## `codefly service start`

Start an installed service

```
codefly service start LABEL
```

## `codefly service status`

Show native process and product health state

```
codefly service status LABEL
```

## `codefly service stop`

Stop a service without triggering crash restart

```
codefly service stop LABEL
```

## `codefly service uninstall`

Remove supervisor configuration while preserving product data

```
codefly service uninstall LABEL
```

Flags:

```
      --version string   Require the installed contract to have this version
```

## `codefly show`

Inspect workspace dependency and network configuration

```
codefly show <subcommand>
```

Subcommands:

- [`codefly show dependencies`](#codefly-show-dependencies)
- [`codefly show fixtures`](#codefly-show-fixtures)
- [`codefly show network`](#codefly-show-network)
- [`codefly show runnable`](#codefly-show-runnable)

## `codefly show dependencies`

Show a service's dependency graph and startup order

```
codefly show dependencies [service]
```

## `codefly show fixtures`

Show the fixtures the workspace's composed packages declare

```
List every fixture the packages this workspace composes declare, with the
principals each seeds.

A fixture names the state a composed host boots with under CODEFLY__FIXTURE, so
these are exactly the names "codefly run solution --fixture" accepts. Each
principal is reported by id, email and role; role is the lookup key a test
resolves an identity by. Seed tokens are not printed.

A package that cannot be read, and a name two packages both declare, are
reported as problems — after the listing, so the fixtures that did resolve are
still named. The command exits non-zero when it reports one.

Examples:
  codefly show fixtures
  codefly show fixtures --json
```

```
codefly show fixtures [flags]
```

Flags:

```
      --json   Emit machine-readable JSON
```

## `codefly show network`

Show service bindings and dependency endpoint addresses

```
codefly show network [flags]
```

Flags:

```
      --naming-scope run --naming-scope   naming scope used to derive deterministic ports (matches run --naming-scope)
```

## `codefly show runnable`

Show a runnable's contract, execution bounds and dependency resolution

```
Show one runnable's declaration: its immutable identity, the agent that
builds it, its typed contract, its execution bounds, and whether each declared
dependency resolves in this workspace.

The name is module/name, or a bare name when it is unambiguous across modules.
A name declared at several versions is ambiguous too; --version selects one.

An operation a module derives from a marked gRPC method — what "codefly
generate runnables" writes — is shown here too, with the method it adapts and
the execution policy and authority installed beside its package.

An unresolved dependency is reported, not fatal: the command exits 0 so it can
describe every dependency in one pass. Unattended callers gate on --json and
check each dependency's "resolved" field.

Examples:
  codefly show runnable word-count
  codefly show runnable backend/word-count
  codefly show runnable word-count --version=0.2.0
  codefly show runnable word-count --json
```

```
codefly show runnable <name>
```

Flags:

```
      --json             Emit machine-readable JSON
      --version string   Select one release when a name is declared at several versions
```

## `codefly status`

Check system status (releases, agents, health)

```
codefly status <subcommand>
```

Subcommands:

- [`codefly status release`](#codefly-status-release)

## `codefly status release`

Check release status of all agents and core

```
Show release status including:
  - Version comparison between agents and latest core
  - Recent release attempts (successes and failures)
  - Agent health (security audits, tests, manifests)
  - Version deltas
```

```
codefly status release [flags]
```

Flags:

```
      --create-issues   auto-create GitHub issues for out-of-date agents
```

## `codefly stop`

Stop Codefly processes while preserving stateful containers for reuse

```
codefly stop [name-filter...]
```

## `codefly sync`

Reconcile services, libraries, or composed modules with their dependencies

```
codefly sync <subcommand>
```

Subcommands:

- [`codefly sync library-dependencies`](#codefly-sync-library-dependencies)
- [`codefly sync service`](#codefly-sync-service)
- [`codefly sync solution-sdk`](#codefly-sync-solution-sdk)

## `codefly sync library-dependencies`

Prepare local development links for a service's internal libraries

```
Configure local development for library dependencies of a service.

This will:
- For Go: Add replace directives to go.mod pointing to local library paths
- For Python: Install libraries in editable mode (pip install -e)
- For TypeScript/Node: Set up npm link

Examples:
  # Setup local dev for all library dependencies of a service
  codefly sync library-dependencies --service=api --module=backend

  # Cleanup local development setup (for production builds)
  codefly sync library-dependencies --service=api --module=backend --cleanup
```

```
codefly sync library-dependencies [flags]
```

Flags:

```
      --cleanup          Remove local development setup (for production)
      --module string    Module name
      --service string   Service name
```

## `codefly sync service`

Regenerate a service's dependency-derived configuration through its agent

```
codefly sync service [flags]
```

Flags:

```
      --init-only     Initialize service only, i.e. without syncning it
      --stand-alone   Begin service as standalone, i.e. without its dependencies
```

## `codefly sync solution-sdk`

Aggregate one client library per solution from api.consumes

```
Reads api.consumes in solution.codefly.yaml and produces one aggregated
client library per solution, carrying only the declared services, generated
from the contracts of the module packages the workspace composes. Also keeps
the runtime service-dependencies in step with the declaration.
```

```
codefly sync solution-sdk [flags]
```

Flags:

```
      --apply-dependencies   add missing service-dependencies entries for every bound consume entry
      --check                do not write; exit 1 if the library or service-dependencies are out of date
      --language strings     languages to generate: go,typescript,python
```

## `codefly terminal`

Open an interactive shell scoped to a workspace resource

```
Opens a terminal session scoped to a module/service directory.
The session runs inside the codefly daemon and persists across disconnections.

Examples:
  codefly terminal
  codefly terminal --module app --service api
  codefly terminal --shell /bin/zsh
```

```
codefly terminal [flags]
```

Flags:

```
      --module string    Module name (optional)
      --server string    codefly gRPC server address (default: derived from workspace)
      --service string   Service name (optional)
      --shell string     Shell override (default: $SHELL)
```

## `codefly test`

Run plugin-owned tests for a service, a solution, or a source checkout

```
codefly test <subcommand>
```

Subcommands:

- [`codefly test service`](#codefly-test-service)
- [`codefly test solution`](#codefly-test-solution)
- [`codefly test source`](#codefly-test-source)

## `codefly test service`

Run a service's configured tests through its agent

```
Test a service via the agent's Test RPC.

All flags are forwarded to the agent, which maps them to its native test runner:
  Go (go test):       --filter → -run "(p1|p2)", --race, --timeout, --coverage
  JS (vitest):        --filter → --testNamePattern, --suite=e2e → npm run test:e2e
  JS (playwright):    --filter → --grep
  Python (pytest):    --filter → -k "p1 or p2"

Anything after '--' is passed verbatim to the underlying runner as extra args.

Examples:
  codefly test service                           # run all tests
  codefly test service --filter TestAuth         # run tests matching TestAuth
  codefly test service --filter Auth --filter API   # OR: TestAuth or TestAPI
  codefly test service --suite e2e               # run e2e suite (Playwright, etc.)
  codefly test service --target ./pkg/business   # scope to a package/dir
  codefly test service -- --shard 1/2            # pass --shard 1/2 to runner
```

```
codefly test service [flags]
```

Flags:

```
      --coverage                       Run with coverage instrumentation
      --env string                     Workspace environment to test in (default "local")
      --exclude-dependency strings     Exclude optional dependency services from the test (repeatable, e.g. infra/temporal)
  -k, --filter strings                 Name regex pattern (repeatable; OR-combined). -k mirrors pytest
      --fixture string                 Fixture override (defaults to the selected Codefly environment)
      --headless                       Run without TUI (auto-enabled when no TTY)
      --init-only                      Initialize service only, i.e. without running it
      --load-only                      LoadRequired service only, i.e. without running it
      --naming-scope string            Runtime naming scope: fold a scope into port derivation for a disjoint port set (parallel runs / test encapsulation)
      --output-env string              Write one service's full SDK/runtime environment to an owner-only file
      --profile string                 Named workspace run profile
      --race                           Run with race detector (Go)
      --runtime-context string         Runtime context for the flow (default "free")
      --suite string                   Named suite: unit (default), integration, e2e, smoke
      --target string                  Package/directory scope (Go: ./pkg/foo, Python: tests/unit)
      --temporary-ports show network   Run this flow as a disposable invocation: OS-probed ephemeral ports plus a generated naming scope isolating its agents, containers and runtime state (advanced; not previewable via show network). The Codefly SDK sets it for test-owned dependency stacks. Passing --naming-scope wins, and passing it empty asks for no scope at all (default true)
      --timeout string                 Per-test timeout, e.g. 30s
  -v, --verbose                        Verbose runner output
```

## `codefly test solution`

Test a solution: its service-entry's tests, plus the composition tests its composed modules contribute

```
codefly test solution [flags]
```

Flags:

```
      --coverage                       Run with coverage instrumentation
      --env string                     Workspace environment to test in (default "local")
      --exclude-dependency strings     Exclude optional dependency services from the test (repeatable, e.g. infra/temporal)
  -k, --filter strings                 Name regex pattern (repeatable; OR-combined). -k mirrors pytest
      --fixture string                 Fixture override (defaults to the selected Codefly environment)
      --headless                       Run without TUI (auto-enabled when no TTY)
      --init-only                      Initialize service only, i.e. without running it
      --load-only                      LoadRequired service only, i.e. without running it
      --naming-scope string            Runtime naming scope: fold a scope into port derivation for a disjoint port set (parallel runs / test encapsulation)
      --output-env string              Write one service's full SDK/runtime environment to an owner-only file
      --profile string                 Named workspace run profile
      --race                           Run with race detector (Go)
      --runtime-context string         Runtime context for the flow (default "free")
      --suite string                   Named suite: unit (default), integration, e2e, smoke
      --target string                  Package/directory scope (Go: ./pkg/foo, Python: tests/unit)
      --temporary-ports show network   Run this flow as a disposable invocation: OS-probed ephemeral ports plus a generated naming scope isolating its agents, containers and runtime state (advanced; not previewable via show network). The Codefly SDK sets it for test-owned dependency stacks. Passing --naming-scope wins, and passing it empty asks for no scope at all (default true)
      --timeout string                 Per-test timeout, e.g. 30s
  -v, --verbose                        Verbose runner output
```

## `codefly test source`

Run plugin-owned tests against an arbitrary source checkout

```
codefly test source [flags]
```

Flags:

```
      --agent string             Select publisher/name[:version] instead of discovering installed agents
      --coverage                 Enable plugin-defined coverage
      --dir string               Source checkout (default: current directory)
  -k, --filter strings           Test-name filter (repeatable)
      --qualification            Assert the selected agent's Runtime, Code, and Tooling handshake
      --race                     Enable plugin-defined race checking
      --runtime-context string   Runtime context for validation (default "free")
      --suite string             Named test suite
      --target string            Package/directory scope
      --timeout string           Test timeout
  -v, --verbose                  Verbose test output
```

## `codefly update`

Refresh service agents or repository dependencies

```
codefly update <subcommand>
```

Subcommands:

- [`codefly update deps`](#codefly-update-deps)
- [`codefly update workspace`](#codefly-update-workspace)

## `codefly update deps`

Update Go dependencies and optionally refresh companion images

```
Update dependencies across the monorepo.

For each Go module under --dir (default: current directory), update deps:
  - finds outdated EXTERNAL deps (govulncheck/go list -u)
  - go get <pkg>@<latest-safe> for each (first-party codefly-dev modules
    are left to go.work / 'codefly agents deps --pin')
  - go mod tidy
  - re-audit with govulncheck (unless --audit=false)

With --companions, also rebuilds every companion image with 'docker build
--pull' so floating base tags (golang:1.26-alpine, …) pick up upstream
patch releases — clearing base-image CVEs like the Go stdlib advisories.

Examples:
  codefly update deps                 # update Go deps under cwd + audit
  codefly update deps --dir .         # same, explicit
  codefly update deps --companions    # also rebuild companion images (--pull)
  codefly update deps --audit=false   # skip the post-update audit
```

```
codefly update deps [flags]
```

Flags:

```
      --audit            Run the govulncheck audit after updating (default true)
      --companions       Also rebuild companion Docker images with fresh base layers (docker build --pull)
      --dir string       Root to update (default: current directory; recurses into every go.mod)
      --go string        Pin the Go toolchain to this version across every go.mod + go.work (e.g. 1.26.4) — clears stdlib CVEs in standalone/CI builds
      --stale-days int   Audit: fail when a suppression's reviewed date is older than this many days (0 disables) (default 45)
```

## `codefly update workspace`

Update every workspace service to its latest compatible agent

```
codefly update workspace [flags]
```

## `codefly upgrade`

Apply semver-safe dependency upgrades to services or workspaces

```
codefly upgrade <subcommand>
```

Subcommands:

- [`codefly upgrade security`](#codefly-upgrade-security)
- [`codefly upgrade service`](#codefly-upgrade-service)
- [`codefly upgrade workspace`](#codefly-upgrade-workspace)

## `codefly upgrade security`

Upgrade dependencies needed to remediate actionable Go vulnerabilities

```
Security runs the same govulncheck audit as `codefly agent build` and then
APPLIES the fixes it finds actionable:

  module vulnerabilities → go get <module>@<fixedVersion>
  stdlib  vulnerabilities → bump go.mod toolchain to the fixed Go version

followed by go mod tidy and a re-audit. Unpatched-upstream findings (no fix
available) are reported but never fail the command.

By default it operates on the Go module in the current directory. Use --all to
sweep every Go module in the codefly.dev monorepo (cli, core, and every
service/toolbox plugin). Use --dry-run to see the plan without changing
anything.

Examples:
  codefly upgrade security                 # fix the current module
  codefly upgrade security --all           # fix every module in the monorepo
  codefly upgrade security --all --dry-run # preview the monorepo-wide plan
```

```
codefly upgrade security [flags]
```

Flags:

```
      --all          Sweep every Go module in the codefly.dev monorepo
      --dir string   Module directory (default: current directory)
      --dry-run      Show the upgrade plan without applying it
```

## `codefly upgrade service`

Apply semver-safe dependency upgrades to one service

```
Run the service agent's Builder.Upgrade RPC. Defaults to
patch+minor (semver-safe) bumps. Pass --major to allow breaking
upgrades. Pass --dry-run to preview without writing the lockfile.
```

```
codefly upgrade service [name]
```

Flags:

```
      --dry-run        Preview changes without writing the lockfile
      --json           Emit raw JSON instead of a table
      --major          Allow major-version bumps (breaking)
      --only strings   Restrict upgrades to these packages (comma-separated)
```

## `codefly upgrade workspace`

Apply semver-safe dependency upgrades to every workspace service

```
codefly upgrade workspace [flags]
```

## `codefly version`

Print the installed Codefly CLI version

```
codefly version [flags]
```

Flags:

```
      --json   Print version, commit, and build date as JSON
```
