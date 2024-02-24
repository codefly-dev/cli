package generate

import (
	"context"
	"os"
	"os/signal"

	"github.com/codefly-dev/cli/pkg/generators"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/services/services"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/configurations/languages"
	"github.com/codefly-dev/core/shared"
	"github.com/spf13/cobra"
)

// OpenAPICmd represents the deploy command
var OpenAPICmd = &cobra.Command{
	Use:   "openAPI",
	Short: "generate openAPI client code",
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := common.NewContext()
		defer done()

		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, os.Kill)
		defer stop()

		cli.RegisterCleanup(services.ClearAgents)

		project := common.Project(ctx)
		input, err := configurations.ParseService(serviceInput)
		cli.ExitOnError(err, "Cannot parse service input")
		service, err := project.LoadService(ctx, input)
		cli.ExitOnError(err, "Cannot load service")

		destination, err = shared.SolvePath(destination)
		cli.ExitOnError(err, "Cannot solve path")
		language := languages.FromString(languageInput)
		cli.ExitIf(language == languages.NotSupported, "Language not supported")
		err = generateOpenAPI(ctx, project, service, language, destination)
		cli.ExitOnError(err, "Cannot generate openAPI client code")
		cli.Header(1, "Work done!")
		cli.Done()
	},
}

func generateOpenAPI(ctx context.Context, project *configurations.Project, service *configurations.Service, language languages.Language, destination string) error {
	return generators.OpenAPI(ctx, project, service, language, destination)
}

func init() {
	OpenAPICmd.Flags().StringVar(&serviceInput, "service", "", "service to generate openAPI client code for")
	OpenAPICmd.Flags().StringVar(&languageInput, "language", "go", "languageInput to generate openAPI client code in")
	OpenAPICmd.Flags().StringVar(&destination, "destination", "", "destination for the client")
}
