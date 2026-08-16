package generate

import (
	"fmt"
	"path/filepath"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/tenants"
	"github.com/codefly-dev/core/shared"
	"github.com/spf13/cobra"
)

var tenantModel string
var tenantRoot string

// TenantOverlaysCmd generates per-tenant Kustomize overlays from a tenant model.
var TenantOverlaysCmd = &cobra.Command{
	Use:   "tenant-overlays",
	Short: "Generate overlays/<tenant>-<cloud>/ from a tenant model",
	Long: `Expand a tenant model into per-tenant Kustomize overlays.

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
`,
	Args: cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		modelPath, err := shared.SolvePath(tenantModel)
		if err != nil {
			return fmt.Errorf("cannot solve tenant model path: %w", err)
		}
		root := tenantRoot
		if root == "" {
			root = filepath.Dir(modelPath)
		} else if root, err = shared.SolvePath(root); err != nil {
			return fmt.Errorf("cannot solve deployment tree root: %w", err)
		}
		model, err := tenants.LoadModel(modelPath)
		if err != nil {
			return err
		}
		written, err := tenants.Generate(root, model)
		if err != nil {
			return err
		}
		for _, overlay := range written {
			cli.Info("Generated overlays/%s", overlay)
		}
		cli.Header(1, "Generated %d tenant overlay(s)", len(written))
		return nil
	},
}

func init() {
	TenantOverlaysCmd.Flags().StringVar(&tenantModel, "model", "", "path to the tenant model file (required)")
	TenantOverlaysCmd.Flags().StringVar(&tenantRoot, "root", "", "deployment tree root containing the base (default: tenant model directory)")
	_ = TenantOverlaysCmd.MarkFlagRequired("model")
}
