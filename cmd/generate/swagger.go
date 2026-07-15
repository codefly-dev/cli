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
	"github.com/codefly-dev/core/shared"
	"github.com/spf13/cobra"
)

// OpenAPICmd represents the deploy command
var OpenAPICmd = &cobra.Command{
	Use:   "openAPI",
	Short: "generate openAPI client code",
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

		destination, err = shared.SolvePath(destination)
		if err != nil {
			return fmt.Errorf("cannot solve destination path: %w", err)
		}
		language := languages.FromString(languageInput)
		if language == languages.NotSupported {
			return fmt.Errorf("language %q is not supported", languageInput)
		}
		if err := generateOpenAPI(ctx, workspace, module, service, language, destination); err != nil {
			return fmt.Errorf("cannot generate openAPI client code: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		cli.Header(1, "Work done!")
		return nil
	},
}

func generateOpenAPI(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service, language languages.Language, destination string) error {
	return generators.OpenAPI(ctx, workspace, module, service, language, destination)
}

func init() {
	OpenAPICmd.Flags().StringVar(&serviceInput, "service", "", "service to generate openAPI client code for")
	OpenAPICmd.Flags().StringVar(&languageInput, "language", "go", "languageInput to generate openAPI client code in")
	OpenAPICmd.Flags().StringVar(&destination, "destination", "", "destination for the client")
}
