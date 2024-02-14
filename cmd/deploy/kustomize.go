package deploy

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/architecture"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// KustomizeCmd represents the run command
var KustomizeCmd = &cobra.Command{
	Use:   "kustomize",
	Short: "Apply Kustomize deployment",

	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := common.NewContext()
		defer done()
		project := common.Project(ctx)
		service := common.Service(ctx)
		err := kustomize(ctx, project, service)
		cli.ExitOnError(err, "Cannot apply Kustomize deployment")
	},
}

func kustomize(ctx context.Context, project *configurations.Project, service *configurations.Service) error {
	w := wool.Get(ctx).In("kustomize")
	dependencies, err := architecture.NewServiceDependencies(ctx, project)
	if err != nil {
		return err
	}
	services, err := dependencies.OrderTo(ctx, service.Unique())
	if err != nil {
		return err
	}
	services = append(services, architecture.Service{Unique: service.Unique()})
	for _, dep := range services {
		// Execute the kustomize build command
		fromUnique, err := configurations.ParseServiceUnique(dep.Unique)
		if err != nil {
			return w.Wrapf(err, "cannot parse unique: %s", dep.Unique)
		}
		cmd := exec.Command("sh", "-c", fmt.Sprintf("kustomize build %s/_deployments/kustomize/%s/%s/overlays/local | kubectl apply -f -", project.Dir(), fromUnique.Application, fromUnique.Name))
		w.Info(fmt.Sprintf("Applying kustomize deployment for %s", dep.Unique))
		w.Debug(fmt.Sprintf("Command: %s", cmd.String()))
		err = cmd.Run()
		if err != nil {
			return w.Wrapf(err, "cannot apply kustomize deployment")
		}
	}
	return nil
}
