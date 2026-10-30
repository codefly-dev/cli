package delivery

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/delivery/signing"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
)

var prepareOptions struct {
	overlay, out, env string
	developer         bool
}

// PrepareCmd writes the paired documents `codefly delivery apply` sends, from
// the carriers a render already settled and the declarations the modules
// themselves hold.
//
// NOT exposed over MCP: it signs with release custody and writes a document
// set a cell then delivers, which is a publishing step rather than something
// to drive conversationally.
var PrepareCmd = &cobra.Command{
	Use: "prepare", Short: "Prepare the paired delivery documents from a rendered delivery overlay",
	Long: `Write one prepared paired document per module manifest in a rendered delivery
overlay, for 'codefly delivery apply' to send.

Each document carries BOTH signed halves: the Core carrier the overlay already
holds — settled at its generation by the publish that wrote it — and the
authority declaration signed against that carrier's exact bytes, read from the
modules themselves: method policies, scope ceilings, roles, owner-awareness,
the installation selector and its person-context edges. Core's own manifest has
no roles field, so this is the only way a module's roles reach a host.

Nothing is re-rendered and no generation is re-settled: a manifest the overlay
does not carry is not invented here, and a manifest whose carrier is unsigned
is refused by name rather than prepared Core-only — a document that quietly
dropped the authority half would reach the host looking like a success and
compose a module with no roles.

A withdrawal declares nothing and keeps its Core-only identity.

The documents carry no credential. 'codefly delivery apply' reads it from the
projection at execution.

Signing uses release custody (VAULT_ADDR + VAULT_TOKEN +
CODEFLY_RELEASE_TRANSIT_KEY, or CODEFLY_RELEASE_KMS_KEY). Pass --developer to
sign with this machine's developer key instead, which only a local
qualification host admits.`,
	Example: `  codefly delivery prepare --overlay deployments/environments/blue/modules/wiki/delivery --out ./prepared --env blue
  codefly delivery prepare --overlay ./delivery --out ./prepared --env local --developer`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, cancel := common.NewContext()
		defer cancel()
		if prepareOptions.overlay == "" {
			return errors.New("--overlay is the rendered delivery overlay holding the module manifest carriers")
		}
		if prepareOptions.out == "" {
			return errors.New("--out is the directory the prepared documents are written to")
		}
		if prepareOptions.env == "" {
			return errors.New("--env names the environment whose host the declarations are read for")
		}
		workspace, err := common.LoadWorkspace(ctx)
		if err != nil {
			return err
		}
		env, err := environments.Select(workspace, prepareOptions.env)
		if err != nil {
			return err
		}
		signer, err := prepareSigner()
		if err != nil {
			return err
		}
		written, err := gitops.PreparePairedDocuments(ctx, prepareOptions.overlay, prepareOptions.out,
			gitops.WorkspaceAuthoritySources(workspace, env), signer)
		if err != nil {
			return err
		}
		for _, name := range written {
			if _, err = fmt.Fprintf(cmd.OutOrStdout(), "prepared %s\n", name); err != nil {
				return err
			}
		}
		return nil
	},
}

// prepareSigner is release custody, or — asked for explicitly — this machine's
// developer key, which only a local qualification host admits. It is never
// chosen implicitly: a document signed by a developer key and delivered to a
// real host is refused there, far from the cause.
func prepareSigner() (signing.Signer, error) {
	if prepareOptions.developer {
		return loadKey()
	}
	signer := signing.FromEnvironment(nil)
	if _, err := signer.Sign([]byte("probe")); err != nil {
		return nil, fmt.Errorf("%w; pass --developer to sign with this machine's developer key for a local qualification host", err)
	}
	return signer, nil
}

func init() {
	PrepareCmd.Flags().StringVar(&prepareOptions.overlay, "overlay", "", "Rendered delivery overlay holding the module manifest carriers")
	PrepareCmd.Flags().StringVar(&prepareOptions.out, "out", "", "Directory the prepared documents are written to")
	PrepareCmd.Flags().StringVar(&prepareOptions.env, "env", "", "Environment whose host the declarations are read for")
	PrepareCmd.Flags().BoolVar(&prepareOptions.developer, "developer", false, "Sign with this machine's developer key, for a local qualification host")
}
