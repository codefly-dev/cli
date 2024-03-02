package deploy

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/codefly-dev/cli/pkg/architecture"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/wool"
)

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
		cmd := exec.Command("sh", "-c", fmt.Sprintf("kustomize build %s/deployments/kustomize/applications/%s/services/%s/overlays/local | kubectl apply -f -", project.Dir(), fromUnique.Application, fromUnique.Name))
		w.Info(fmt.Sprintf("Applying kustomize deployment for %s", dep.Unique))
		w.Debug(fmt.Sprintf("Command: %s", cmd.String()))
		err = cmd.Run()
		if err != nil {
			return w.Wrapf(err, "cannot apply kustomize deployment")
		}
	}
	return nil
}
