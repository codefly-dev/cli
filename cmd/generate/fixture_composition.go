package generate

import (
	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/fixturefragments"
	"github.com/spf13/cobra"
)

var fixtureCompositionEnv string

// FixtureCompositionCmd writes the local authority's fixture-composition
// document for one environment, as `codefly run` does before the authority
// boots.
var FixtureCompositionCmd = &cobra.Command{
	Use:   "fixture-composition",
	Short: "Merge every composed module's fixture-composition fragment into the workspace's document",
	Long: `Merge the fixture-composition fragment every module of the workspace ships
(configurations/<profile>/` + fixturefragments.Group + `.yaml in the module's own directory)
into the workspace's configurations/<profile>/` + fixturefragments.Group + `.yaml, the document
the local authority reads.

Each module states, once, what the local authority's standard fixture roles may
do at its kinds; the composition's own module states only what is its own (a
collection its seed writes). Every grant of a role on one kind, every
installation and every stand-in module has exactly one source: two fragments
stating the same one are refused, naming both. The rules are the authority's
(codefly-dev/authority libraries/fixture-composition).

` + "`codefly run`" + ` writes the same document before the local authority boots, so a
developer never runs this first. The written file says it is generated in its
first line; a hand-written document is never overwritten, and this command
refuses it by name.

Examples:
  codefly generate fixture-composition
  codefly generate fixture-composition --env local`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, done := common.NewContext()
		defer done()
		// Fragments ship inside the composed modules, so a pinned module is
		// materialized first, as a run does.
		workspace, err := common.LoadWorkspaceWithPinnedModules(ctx)
		if err != nil {
			return err
		}
		env, err := environments.Select(workspace, fixtureCompositionEnv)
		if err != nil {
			return err
		}
		prepared, err := fixturefragments.Prepare(ctx, workspace, env)
		if err != nil {
			return err
		}
		changed, err := fixturefragments.Write(prepared)
		if err != nil {
			return err
		}
		verb := "is current"
		if changed {
			verb = "written"
		}
		cli.Header(1, "%s %s, from %d fragment(s)", prepared.Target, verb, len(prepared.Sources))
		for _, source := range prepared.Sources {
			cli.Info("  %s", source)
		}
		return nil
	},
}

func init() {
	FixtureCompositionCmd.Flags().StringVar(&fixtureCompositionEnv, "env", "local", "environment whose configuration profile the document is written for")
}
