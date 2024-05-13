package generate

import (
	"context"
	"os"
	"os/signal"

	"github.com/codefly-dev/cli/pkg/generators"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/services/services"
	"github.com/codefly-dev/core/languages"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/spf13/cobra"
)

// GRPCCmd represents the deploy command
var GRPCCmd = &cobra.Command{
	Use:   "gRPC",
	Short: "generate gRPC client code",
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := common.NewContext()
		defer done()

		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, os.Kill)
		defer stop()

		cli.RegisterCleanup(services.ClearAgents)

		workspace := common.RequireWorkspace(ctx)
		svc, err := workspace.FindUniqueServiceByName(ctx, serviceInput)
		cli.ExitOnError(err, "Cannot find service from input")

		destination, err = shared.SolvePath(destination)
		cli.ExitOnError(err, "Cannot solve path")
		language := languages.FromString(languageInput)
		cli.ExitIf(language == languages.NotSupported, "Language not supported")
		err = generateGRPC(ctx, svc, language, destination)
		cli.ExitOnError(err, "Cannot generate gRPC client code")
		cli.Header(1, "Work done!")
		cli.Done()
	},
}

func generateGRPC(ctx context.Context, service *resources.Service, language languages.Language, destination string) error {
	return generators.GRPC(ctx, service, language, destination)
}

func init() {
	GRPCCmd.Flags().StringVar(&serviceInput, "service", "", "service to generate gRPC client code for")
	GRPCCmd.Flags().StringVar(&languageInput, "language", "go", "languageInput to generate gRPC client code in")
	GRPCCmd.Flags().StringVar(&destination, "destination", "", "destination for the client")
}
