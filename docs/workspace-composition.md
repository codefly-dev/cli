# One platform workspace plus product solutions

The product declares its selected platform workspace release once. That workspace
owns the platform module inventory, pins, resolution policy and shared wiring.
The product owns its solutions and environment target:

```yaml
name: product
layout: modules
workspaces:
  - name: platform-core
    source: team/platform-core
    version: 1.0.0
solutions:
  - name: wiki
    source: team/solutions
    module: solutions/wiki
    version: 2.0.0
  - name: lastlogin-go
    source: team/solutions
    module: solutions/lastlogin-go
    version: 2.0.0
module-resolution:
  wiki: git
  lastlogin-go: git
```

`codefly deploy gitops render` and the existing materializing command boundaries
acquire exact workspace releases before resolving modules. Workspace releases
are Git tags (`1.0.0` selects `v1.0.0`), not signed module packages. `latest`,
branches and version ranges are refused for workspace selection. Individual
modules continue to use their owner's signed-package policy or explicit Git
resolution declaration. The example opts the solutions into Git resolution;
this is not signature verification.

Workspace acquisition uses the configured module cache under `workspaces/` and
does not rewrite product declarations. A missing upgraded release fails closed.
Read-only diagnostics use the cache and report missing releases without fetching.
Use `path` instead of `source`/`version` for explicit local development; a repository
subdirectory can be selected with `workspace` alongside the release reference.

Core expands the graph in memory and retains the authored references on save.
Inherited module collisions and cycles fail, and resolution/trust policy is read
from the module's declaring workspace. Product configuration groups override
inherited workspace groups; imported environments never replace the product's.
Hosted environments still come from `codefly environment import`, and workload
manifests still come from `codefly deploy gitops render`.
