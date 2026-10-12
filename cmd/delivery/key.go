package delivery

import (
	"fmt"

	"github.com/codefly-dev/cli/pkg/delivery/signing"
	"github.com/spf13/cobra"
)

var loadKey = signing.LoadOrCreateDeveloperKey

// KeyCmd reveals only the public identity needed to admit this machine locally.
var KeyCmd = &cobra.Command{
	Use: "key", Short: "Create or show this machine's developer delivery key",
	Long:    "Create or load the machine's developer signing key and print its key ID and public key PEM. Add the key ID under host.developers of a local k3d environment. The private key remains in local custody.",
	Example: "  codefly delivery key", Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		signer, err := loadKey()
		if err != nil {
			return err
		}
		public, err := signing.PublicKeyPEM(signer)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s\n%s", signer.KeyID(), public)
		return err
	},
}
