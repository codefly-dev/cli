package show

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/codefly-dev/cli/cmd/common"
	clicomposition "github.com/codefly-dev/cli/pkg/composition"
	"github.com/spf13/cobra"
)

var showFixturesJSON bool

// FixturesCmd lists the fixtures the workspace's composed packages declare, with
// the principals each one seeds. A fixture is the contract a solution test
// resolves an identity against instead of hardcoding a seeded login, so this
// reports what `--fixture` can name and what authenticating as one gets you.
var FixturesCmd = &cobra.Command{
	Use:   "fixtures",
	Short: "Show the fixtures the workspace's composed packages declare",
	Long: `List every fixture the packages this workspace composes declare, with the
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
  codefly show fixtures --json`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, done := common.NewContext()
		defer done()

		// Deliberately the plain loader: materializing pinned modules writes the
		// workspace overlay and .gitignore, which an inspection command must not
		// do. Resolving a composed module below still materializes into the
		// content-addressed cache, which leaves the workspace untouched.
		workspace, err := common.LoadWorkspace(ctx)
		if err != nil {
			return fmt.Errorf("cannot load workspace: %w", err)
		}

		fixtures, problems := clicomposition.WorkspaceFixtures(ctx, workspace)

		report := fixturesReport{Workspace: workspace.Name, Fixtures: []fixtureReport{}, Problems: []string{}}
		for _, fixture := range fixtures {
			entry := fixtureReport{
				Name:        fixture.Name,
				Description: fixture.Description,
				Principals:  []fixturePrincipalReport{},
			}
			for _, principal := range fixture.Principals {
				entry.Principals = append(entry.Principals, fixturePrincipalReport{
					ID:    principal.ID,
					Email: principal.Email,
					Role:  principal.Role,
				})
			}
			report.Fixtures = append(report.Fixtures, entry)
		}
		for _, problem := range problems {
			report.Problems = append(report.Problems, problem.Error())
		}

		out := cmd.OutOrStdout()
		if showFixturesJSON {
			encoder := json.NewEncoder(out)
			encoder.SetIndent("", "  ")
			if err := encoder.Encode(report); err != nil {
				return err
			}
			return errors.Join(problems...)
		}

		if len(report.Fixtures) == 0 {
			fmt.Fprintf(out, "Workspace %q composes no package declaring a fixture.\n", report.Workspace)
		} else {
			fmt.Fprintf(out, "Fixtures declared by the packages workspace %q composes:\n\n", report.Workspace)
			for _, fixture := range report.Fixtures {
				fmt.Fprintf(out, "• %s", fixture.Name)
				if fixture.Description != "" {
					fmt.Fprintf(out, " — %s", fixture.Description)
				}
				fmt.Fprintln(out)
				if len(fixture.Principals) == 0 {
					fmt.Fprintln(out, "    (seeds no principal)")
					continue
				}
				for _, principal := range fixture.Principals {
					fmt.Fprintf(out, "    %-16s %-32s [%s]\n", principal.ID, principal.Email, principal.Role)
				}
			}
		}
		return errors.Join(problems...)
	},
}

// fixturePrincipalReport omits the principal's token: the manifest ships inside
// the package, so the token is a development seed credential rather than a
// secret, but printing it on an inspection command puts it in scrollback and CI
// logs for no one who needed it there.
type fixturePrincipalReport struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Role  string `json:"role"`
}

type fixtureReport struct {
	Name        string                   `json:"name"`
	Description string                   `json:"description,omitempty"`
	Principals  []fixturePrincipalReport `json:"principals"`
}

type fixturesReport struct {
	Workspace string          `json:"workspace"`
	Fixtures  []fixtureReport `json:"fixtures"`
	Problems  []string        `json:"problems"`
}

func init() {
	FixturesCmd.Flags().BoolVar(&showFixturesJSON, "json", false, "Emit machine-readable JSON")
}
