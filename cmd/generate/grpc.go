package generate

import (
	"context"
	"fmt"

	"github.com/codefly-dev/cli/pkg/generators"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/languages"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/spf13/cobra"
)

// GRPCCmd represents the deploy command
var GRPCCmd = &cobra.Command{
	Use:   "gRPC",
	Short: "Generate a typed gRPC client for a service endpoint",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()

		ctx, stop := common.SignalContext(ctx)
		defer stop()

		defer services.ClearAgents()

		workspace, err := common.LoadWorkspace(ctx)
		if err != nil {
			return err
		}
		service, module, err := workspace.FindUniqueModuleServiceByName(ctx, serviceInput)
		if err != nil {
			return fmt.Errorf("cannot find service from input: %w", err)
		}

		destination, err = solveOutputDirectory(ctx, destination)
		if err != nil {
			return fmt.Errorf("cannot solve destination path: %w", err)
		}
		language := languages.FromString(languageInput)
		if language == languages.NotSupported {
			return fmt.Errorf("language %q is not supported", languageInput)
		}
		if err := generateGRPC(ctx, workspace, module, service, language, destination); err != nil {
			return fmt.Errorf("cannot generate gRPC client code: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		cli.Header(1, "Work done!")
		return nil
	},
}

func generateGRPC(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service, language languages.Language, destination string) error {
	return generators.GRPC(ctx, workspace, module, service, language, destination)
}

func init() {
	GRPCCmd.Flags().StringVar(&serviceInput, "service", "", "service to generate gRPC client code for")
	GRPCCmd.Flags().StringVar(&languageInput, "language", "go", "languageInput to generate gRPC client code in")
	GRPCCmd.Flags().StringVar(&destination, "destination", "", "destination for the client")
}
