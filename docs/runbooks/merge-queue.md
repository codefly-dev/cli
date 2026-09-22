# Merge queue on `main`

## Why

`main` protection sets `strict: true` ("require branches to be up to date before
merging") with `enforce_admins: true`. Every merge moves `main`, which puts every
other open PR into `BEHIND` — so N green PRs cost N sequential update-and-retest
cycles, and nobody can click through it. On 2026-09-22 that left six PRs with all
five required gates green and none of them mergeable.

A merge queue fixes this by testing a *batch* of PRs stacked on current `main` as
one speculative branch and merging the whole batch off one CI run.

## Required gates

All five required contexts — `bootstrap-audit`, `control-integration`,
`coverage`, `race`, `dashboard` — are the `matrix.gate` legs of the single
`Go CLI` workflow (`.github/workflows/go.yml`). `lint` is deliberately **not**
required: it runs `golangci-lint --only-new-issues`, which has no diff baseline
outside a pull request.

## Setup

1. **Workflow trigger first.** `.github/workflows/go.yml` must carry
   `merge_group:` alongside `pull_request:`. The queue builds each entry on a
   `gh-readonly-queue/main/...` branch and fires `merge_group`; without the
   trigger the five gates never report, and each entry is ejected when
   `check_response_timeout_minutes` expires. This is the most common way merge
   queue adoption fails.

   The trigger must already be on `main` before the queue is enabled —
   `merge_group` runs the workflow as it exists on the default branch.

2. **Create the ruleset.** Merge queue cannot be expressed in classic branch
   protection.

   ```bash
   gh api -X POST repos/codefly-dev/cli/rulesets --input .github/merge-queue-ruleset.json
   ```

3. **Drop `strict` on the classic rule.** Rulesets and classic branch protection
   are *additive, most-restrictive-wins*. Leaving `strict: true` in place keeps
   demanding up-to-date branches and the queue buys nothing — the speculative
   branches are what guarantee up-to-date-ness now.

   ```bash
   gh api -X PATCH repos/codefly-dev/cli/branches/main/protection/required_status_checks \
     -F strict=false
   ```

   Keep `enforce_admins: true`; it is orthogonal.

## Using it

`gh pr merge <n> --merge-queue`, or the "Merge when ready" button, which replaces
auto-merge once the queue is on. `grouping_strategy: ALLGREEN` means a failing PR
is ejected from the batch and the rest still land. `max_entries_to_merge: 5`
merges up to five PRs per CI run.

Dependabot PRs queue like any other; ones with genuinely failing gates are
ejected rather than blocking the batch.
