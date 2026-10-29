package environment

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var imagesCmd = &cobra.Command{
	Use:   "images <env> --file <policy.yaml|->",
	Short: "Set registry relocations and immutable image roles for an environment",
	Long: `Replace an environment's build-images policy using a YAML or JSON file.
Service types are exact publisher/agent names; services are exact module/service
names. A service selection wins over the type selection for the same role.
Registry relocations apply to remaining agent defaults, preserving their digest.
Every replacement requires a reference pinned by sha256 and an agent-supported
recipe variant. Unsupported agents or variants refuse before image execution.
Use an empty object ({}) to clear the policy; --dry-run prints the change.
This edits workspace declarations only. It does not build or deploy images.`,
	Example:      "  codefly environment images staging --file images.yaml --dry-run\n  codefly environment images staging --file images.yaml",
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		file, _ := cmd.Flags().GetString("file")
		data, err := readContract(cmd.InOrStdin(), file)
		if err != nil {
			return err
		}
		policy, err := parseImagePolicy(data)
		if err != nil {
			return err
		}
		contract, err := json.Marshal(map[string]any{"schema": environments.CoordinateContractSchema, "requires_capabilities": []string{"build-image-bindings"}, "environment": map[string]any{"name": args[0], "build-images": policy}})
		if err != nil {
			return err
		}
		dir, err := resources.FindUp[resources.Workspace](ctx)
		if err != nil || dir == nil {
			return fmt.Errorf("no %s found", resources.WorkspaceConfigurationName)
		}
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		return runImport(ctx, &importOptions{dir: *dir, envName: args[0], contractData: contract, dryRun: dryRun, now: time.Now().UTC(), stdout: cmd.OutOrStdout()})
	},
}

func parseImagePolicy(data []byte) (*environments.BuildImages, error) {
	var policy *environments.BuildImages
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&policy); err != nil {
		return nil, err
	}
	if policy == nil {
		return nil, fmt.Errorf("image policy must be an object; use {} to clear it")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("image policy must contain exactly one document")
	}
	return policy, policy.Validate()
}

func init() {
	imagesCmd.Flags().String("file", "", "YAML or JSON image policy file, or - for stdin")
	_ = imagesCmd.MarkFlagRequired("file")
	imagesCmd.Flags().Bool("dry-run", false, "Print the workspace diff without writing")
	Cmd.AddCommand(imagesCmd)
}
