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
// That is not a hypothetical. Of six module tags cut in one release round, two
// carried dev agent pins. A composition downstream of them could then not
// complete its own release, because its procedure rests on "a module released
// after the agent fix pins the released agent itself" — and those modules did
// not. The pin cost one line at the pull request that wrote it and two further
// module release rounds afterwards. Every one of those pins carried a comment
// saying to move it before tagging; a comment is not a gate.
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
//   - go.mod: requires under an owner the repository itself publishes under (see
//     [firstPartyPrefixes]).
//
// Nothing here is specific to any product, workspace or organisation. The gate
// is told what to judge by the repository in front of it: the owners it treats as
// first-party come from that repository's own go.mod module paths, and the
// agent-overrides exception is recognised by the shape of the declaration rather
// than by who wrote it.
//
// # Carriers, and why they are not all refused alike
//
// [CarrierConfig] is refused outright: a prerelease there is the defect above.
//
// [CarrierAgentOverride] is the one sanctioned carrier. A dev loop publishes an
// agent build with `codefly publish dev` and pins it in agent-overrides, which is
// what `codefly update workspace --agent-override` exists to write; that loop also
// has to reach a shared environment, which a gitignored machine-local overlay
// cannot do. So a prerelease is allowed there on the default branch when the
// entry carries a label — a comment naming the issue it stands in for — and
// refused under [Options.Release], which is the scope a tag is cut in. That turns
// "drop every dev override before tagging", which a release procedure can ask for
// but not enforce, into a gate.
//
// [CarrierGoModule] is reported and not refused unless [Options.GoModules] asks
// for it. First-party Go pseudo-versions are pervasive in trees that are
// otherwise clean: of four module releases verified to have correct agent pins,
// three carried them — 27, 9 and 3 of them — and many arrive as `// indirect`
// entries no change in the repository can move. Refusing them by default would
// fail three of this package's own clean fixtures and every comparable repository
// on the day the gate landed, which is how a gate gets switched off. They are
// surfaced so a repository can tighten at its own pace.
package prerelease
