// Package prerelease is the merge gate that keeps a prerelease version off a
// repository's default branch, so a tag cut from it can never contain one.
//
// # The rule
//
// A prerelease version pin — a semver prerelease component (0.1.48-dev.e87db5e08865,
// -rc.1, -alpha) or a Go pseudo-version (v0.0.0-20260930123456-abcdef123456,
// which is the same defect spelled differently) — names a build that was never
// released. A module that pins one and is then tagged ships a release that
// depends on an unreleased artifact, and every consumer of that tag inherits the
// dependency without being told.
//
// That is not a hypothetical. Modules have been tagged carrying dev agent pins,
// and a composition that consumes them then cannot drop its own
// agent-overrides, because its release procedure rests on "a module released
// after the agent fix pins the released agent itself", and those modules do
// not. The pin costs one line at the PR that writes it and a module release
// round afterwards.
//
// # What is scanned
//
// Only files git tracks (see [Discover]). A version that is not committed cannot
// reach the default branch, which is also what makes the local dev loop free:
// codefly.local.yaml is gitignored, so it is invisible to this gate by
// construction rather than by a special case.
//
//   - every *.codefly.yaml: every `version:` key, at any depth — agent.version in
//     a service, modules[].version and workspaces[].version in a workspace, a
//     module's or library's own version. Walking for the key rather than for a
//     known schema means a new versioned field is covered the day it is added.
//   - workspace.codefly.yaml's top-level agent-overrides block, whose values are
//     versions under an agent identity rather than under a `version:` key.
//   - go.mod: first-party requires (see [DefaultFirstParty]).
//
// # Carriers, and why they are not all refused alike
//
// [CarrierConfig] is refused outright: a prerelease there is the defect above.
//
// [CarrierAgentOverride] is the one sanctioned carrier. A composition's dev
// loop publishes an agent with `codefly publish dev` and pins it in
// agent-overrides, and
// `codefly update workspace --agent-override` exists to write it; that loop also
// has to reach a shared environment, which a gitignored overlay cannot do. So a
// prerelease is allowed there on the default branch when the entry carries a
// label — a comment naming the issue it stands in for — and refused under
// [Options.Release], which is the scope a tag is cut in. That turns a release
// procedure's "drop every dev override before tagging" step into a gate.
//
// [CarrierGoModule] is reported and not refused unless [Options.GoModules] asks
// for it. First-party Go pseudo-versions are pervasive in trees that are
// otherwise clean — releases whose agent pins are correct carry them — and many
// arrive as `// indirect` entries no change in
// the repository can move. Refusing them by default would fail every repository
// in the fleet on the day the gate landed, which is how a gate gets switched
// off. They are surfaced so a repository can tighten at its own pace.
package prerelease
