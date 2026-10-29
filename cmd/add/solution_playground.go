package add

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
)

// The playground: the composition that runs this solution LOCALLY, on the
// development authority, the way every solution in the fleet is run.
//
// WHY IT IS SCAFFOLDED. A solution's page is served on ONE origin — the
// authority's edge — which signs the person in and routes /v1/<name> to the
// backend with their Work Context. Nothing about that is discoverable from
// the solution's own files, and getting it wrong does not look like an error:
// open the shell's own port instead and the page says "This page is not
// behind its sign-in edge", open the authority's API port and every path
// 404s. Both were hit, repeatedly, reconstructing this by hand.
//
// The one thing that is genuinely not derivable is that THE EDGE IS NAMED
// EXPLICITLY. It is not a dependency of the backend and nothing elects it as
// ingress, so neither `run solution` nor `run workspace` starts it — the
// fleet's own Makefiles list it first in the services they run, and this
// writes the same line.
//
// LOCAL ONLY. In a cell the host's gateway is the origin and signs the person
// in; the development authority stands in for it here. Nothing in this
// directory describes a deployment.
const playgroundDir = "playground"

// authorityModule is the development authority the playground composes.
//
// It is pinned by COMMIT, because the repository publishes no module release
// — its only tags are its Go libraries' — and every solution in the fleet
// pins it the same way. The commit is resolved from the default branch at
// scaffold time rather than written into this binary, where it would be
// stale by the next release; --authority-version overrides it.
const authorityModule = "codefly-dev/authority"

// applySolutionPlayground writes the playground beside the solution.
//
// It never fails the scaffold: the solution is the deliverable and is already
// written and validated, so a playground that cannot be pinned is reported
// and skipped rather than discarding a correct solution.
func applySolutionPlayground(ctx context.Context, module *resources.Module, authorityVersion string) {
	pin := authorityVersion
	if pin == "" {
		resolved, err := defaultBranchCommit(ctx, authorityModule)
		if err != nil {
			cli.Warning("the playground was not written: cannot resolve %s's default branch (%v). Re-run with --authority-version <commit>.", authorityModule, err)
			return
		}
		pin = resolved
	}

	root := filepath.Join(module.Dir(), playgroundDir)
	files := map[string]string{
		"workspace.codefly.yaml":             playgroundWorkspace(module.Name, pin),
		"configurations/local/authority.env": playgroundAuthorityGroup(),
		"configurations/local/edge.env":      playgroundEdgeGroup(module.Name),
		"README.md":                          playgroundReadme(module.Name),
	}
	// The solution's own fragment of the authority's fixture-composition
	// document. It belongs beside the SOLUTION, not in the playground: the
	// composing workspace merges every composed module's fragment, and this
	// is the solution's.
	if err := shared.WriteFileAtomic(ctx,
		filepath.Join(module.Dir(), "configurations", "local", "fixture-composition.yaml"),
		[]byte(fixtureComposition(module.Name)), 0o644); err != nil {
		cli.Warning("the fixture composition was not written: %v", err)
		return
	}
	for name, content := range files {
		if err := shared.WriteFileAtomic(ctx, filepath.Join(root, filepath.FromSlash(name)), []byte(content), 0o644); err != nil {
			cli.Warning("the playground's %s was not written: %v", name, err)
			return
		}
	}
	if err := shared.WriteFileAtomic(ctx, filepath.Join(module.Dir(), "Makefile"), []byte(playgroundMakefile(module.Name)), 0o644); err != nil {
		cli.Warning("the playground's Makefile was not written: %v", err)
		return
	}
	cli.Info("wrote playground/ — `make playground` runs this solution on the development authority")
}

// defaultBranchCommit resolves a GitHub repository's default-branch head.
//
// A module pin must be a commit the default branch holds. A PR head looks
// identical in the manifest and dies on squash-merge, when the commit it
// names stops existing on any branch — so the pin is read from HEAD, which
// is the default branch by definition.
func defaultBranchCommit(ctx context.Context, repository string) (string, error) {
	//nolint:gosec // G204: a fixed git subcommand over a constant repository
	// path; repository is this file's own authorityModule constant.
	command := exec.CommandContext(ctx, "git", "ls-remote", "https://github.com/"+repository, "HEAD")
	output, err := command.Output()
	if err != nil {
		return "", err
	}
	commit, _, found := strings.Cut(strings.TrimSpace(string(output)), "\t")
	if !found || len(commit) != 40 {
		return "", fmt.Errorf("git ls-remote answered %q", strings.TrimSpace(string(output)))
	}
	return commit, nil
}

func playgroundWorkspace(name, authorityPin string) string {
	return fmt.Sprintf(`# This solution, run on the DEVELOPMENT AUTHORITY: the composition a person
# opens locally, and the only one this directory describes. In a cell the
# host's gateway is the origin and issues the Work Contexts; here the
# authority's edge stands in for it.
#
# It composes the authority and nothing else, because this solution consumes
# no module yet. Add each module it calls here as you add it to
# solution.codefly.yaml's api.consumes, AND COPY THAT MODULE'S CONFIGURATION
# GROUP WHOLE: a group is the set its consumers declare, not the modules you
# call, and trimming one gets "module contract slot cannot be resolved",
# "slot resolves ambiguously", or "it is not a module this workspace
# composes".
name: %s-playground
layout: modules

module-resolution:
    authority: git

modules:
    - name: %s
      path: ..
    # Pinned by COMMIT: the authority publishes no module release, and the
    # commit must be one the DEFAULT BRANCH holds. A PR head reads the same
    # here and dies on squash-merge, when the commit it names is on no branch.
    - name: authority
      source: %s
      module: modules/authority
      version: %q

environments:
    - name: local
      description: %s on the development authority
      fixture: local
      host:
          coordinate: codefly/local/authority
          component: authority
          domain: local
          audience: https://authority.local.test
          trust_domain: authority.local
          delivery: authority/local/grpc
      service-secrets:
          generate:
              # The hop signs each admission it issues with its own seed, and
              # registers the public half with the authority. Generated per
              # run; never committed.
              - scope: service
                configuration: hop-signing
                services: [authority/hop]
                keys: [SEED]
`, name, name, authorityModule, authorityPin, titleFromName(name))
}

func playgroundAuthorityGroup() string {
	return `# The authority interface: this solution's backend asks it who the viewer is
# and is issued every Work Context it sends.
endpoint=${endpoint:authority/local/grpc}
# The organisation this solution's own work runs in: one of the fixture's.
organization=acme
# The interface's HTTP face, for a module with no generated client of its own.
rest-endpoint=${endpoint:authority/local/rest}
`
}

func playgroundEdgeGroup(name string) string {
	return fmt.Sprintf(`# The edge: the browser's ONE origin. It signs the person in from the
# authority's fixture, routes /v1/%s to this solution's backend with that
# person's Work Context, and serves the solution's page on every other path.
#
# Opening the page at the shell's own port instead is the mistake this file
# exists to prevent: the shell asks the edge for /auth/session on its own
# origin, finds nothing, and says so.
MODULES=%s
%s_REST=${endpoint:%s/backend/http}
FRONTEND=${endpoint:%s/web/http}
`, name, name, strings.ToUpper(strings.ReplaceAll(name, "-", "_")), name, name)
}

// playgroundMakefile writes the run line, whose one non-obvious part is that
// authority/edge is NAMED. Nothing starts it otherwise.
func playgroundMakefile(name string) string {
	return fmt.Sprintf(`.PHONY: playground

# %s on the development authority, as a solution runs in a cell with no saas
# host: the authority's edge is the one origin the browser opens and signs a
# person in from the authority's fixture, and this solution's backend is
# issued every Work Context it sends.
#
# THE EDGE IS LISTED FIRST AND THAT IS LOAD-BEARING. It is not a dependency of
# the backend and nothing elects it as ingress, so neither `+"`run solution`"+`
# nor `+"`run workspace`"+` starts it: name it, or the page is served on no
# origin that can sign anyone in.
PLAYGROUND_SERVICES = authority/edge %s/web %s/backend

playground:
	cd %s && codefly run service $(PLAYGROUND_SERVICES) --env local
`, titleFromName(name), name, name, playgroundDir)
}

func playgroundReadme(name string) string {
	return fmt.Sprintf(`# %s — the playground

	make playground

Then open the address the EDGE prints:

	edge listening on :<port>, routing 1 module(s)

## What runs, and why each one

| | |
|---|---|
| `+"`authority/edge`"+` | the browser's one origin: signs the person in, serves the page, routes `+"`/v1/%s`"+` to the backend with their Work Context |
| `+"`authority/local`"+` | the authority itself, which issues every Work Context and checks each call against the route's published policy |
| `+"`authority/store`"+` | the authority's own store, where approvals persist |
| `+"`%s/backend`"+` | this solution's entry service |
| `+"`%s/web`"+` | the shell that serves the page, mounting the same `+"`./Page`"+` a host federates |

## The two addresses that are not it

- **the shell's own port.** The page asks the edge for `+"`/auth/session`"+` on
  the origin it is served from; on the shell's port there is no edge, and the
  page says "This page is not behind its sign-in edge".
- **the authority's REST port.** That is the authority's API, not the edge.
  Every browser path 404s there.

## Local only

In a cell the host's gateway is the origin and issues the Work Contexts. The
development authority stands in for it here, and nothing in this directory
describes a deployment.
`, titleFromName(name), name, name, name)
}

// fixtureComposition is this solution's fragment of the local authority's
// fixture-composition document: who, in the development fixture, has this
// solution INSTALLED.
//
// Without it the solution runs and the person signs in, and the authority
// still refuses every capability — "audience %q is not installed in this
// organization, so there is no installation to seal the capability to" — so
// the page renders its not-installed branch and nothing else. The fixture
// itself names only who exists and the authority's own vocabulary; which
// solutions are installed is the composing workspace's to say, merged from
// each composed module's fragment.
//
// alex is the acme admin of the authority's shipped fixture, so the
// installation is owned by a person who exists. Change the organization, the
// owner and the role as the solution grows roles of its own.
func fixtureComposition(name string) string {
	return fmt.Sprintf(`# %s's fragment of the local authority's fixture-composition document
# (codefly-dev/authority libraries/fixture-composition, schema v1): what is
# this solution's own in the fixture of a run that composes it.
#
# `+"`codefly run`"+` merges this with every composed module's fragment into the
# composing workspace's configurations/local/fixture-composition.yaml, the
# document the authority reads. A grant a module states in its own fragment is
# never restated here: the merge refuses it, naming both.
schema: codefly.dev/authority/fixture-composition/v1
installations:
  # WITHOUT THIS the authority refuses every capability at this solution —
  # "audience %q is not installed in this organization" — and the page can
  # only render its not-installed branch.
  #
  # TEAMS ARE WHAT ADMIT ANYONE BUT THE OWNER. The role below is the
  # owner's, and the owner is alex, the acme admin of the authority's
  # shipped fixture; with no teams every other person signs in and is
  # refused by name —
  # "<person> holds no role on %s" — which is a correct refusal and a
  # useless playground. finance and engineering are the fixture's acme teams,
  # so maya, rémi, alex and dev can all open it; no-roles, who is in no team,
  # still cannot, and that is the fixture showing a real refusal.
  - {organization: acme, target: %s, role: viewer, owner: alex, teams: [finance, engineering]}
`, titleFromName(name), name, name, name)
}
