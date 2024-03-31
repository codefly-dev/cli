package cmd

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/services/services"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/configurations/standards"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/providers"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// ExposeCmd represents the expose command
var ExposeCmd = &cobra.Command{
	Use:   "expose",
	Short: "Expose a service",
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := common.NewContext()
		defer done()

		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, os.Kill)
		defer stop()

		defer services.ClearAgents()

		project := common.Project(ctx)
		err := expose(ctx, project)
		cli.ExitOnError(err, "Cannot expose service")
	},
}

func expose(ctx context.Context, project *configurations.Project) error {
	w := wool.Get(ctx).In("expose")
	// Get the running network manager
	configurationManager, err := providers.NewManager(ctx, project)
	if err != nil {
		return w.Wrap(err)
	}
	localReader, err := providers.NewConfigurationLocalReader(ctx, project)
	if err != nil {
		return w.Wrap(err)
	}
	configurationManager.WithLoader(localReader)
	err = configurationManager.Load(ctx, configurations.Local())

	networkManager, err := network.NewDeployManager(ctx, configurationManager)
	if err != nil {
		return w.Wrap(err)
	}
	// Loop over svcs
	svcs, err := project.LoadServices(ctx)
	if err != nil {
		return w.Wrap(err)
	}
	for _, service := range svcs {
		for _, endpoint := range service.Endpoints {
			if endpoint.Visibility == configurations.VisibilityPublic {
				err = exposeService(ctx, service, endpoint, networkManager)
				if err != nil {
					return w.Wrap(err)
				}
			}
		}
	}
	<-ctx.Done()
	return nil
}

func exposeService(ctx context.Context, service *configurations.Service, endpoint *configurations.Endpoint, networkManager network.Manager) error {
	w := wool.Get(ctx).In("exposeService")
	namespace, err := networkManager.GetNamespace(ctx, service, configurations.Local())
	if err != nil {
		return w.Wrap(err)
	}
	k8sSvc := fmt.Sprintf("svc/%s", service.Name)
	w.Debug("k8s", wool.Field("namespace", namespace), wool.Field("service", k8sSvc))
	// Check if this service exists in this namespace
	_, err = exec.CommandContext(ctx, "kubectl", "get", k8sSvc, "-n", namespace).Output()
	if err != nil {
		w.Warn(fmt.Sprintf("cannot get service: %s", k8sSvc))
		return nil
	}
	hostPort := network.ToNamedPort(ctx, service.Application, service.Name, endpoint.Name, endpoint.API)
	targetPort := standards.Port(endpoint.API)

	go func() {
		w.Info(fmt.Sprintf("exposing %s at http://localhost:%d", service.Unique(), hostPort), wool.Field("service", service), wool.Field("port", targetPort), wool.Field("namespace", namespace))
		cmd := exec.CommandContext(ctx, "kubectl", "port-forward", "-n", namespace, k8sSvc, fmt.Sprintf("%d:%d", hostPort, targetPort))
		out, err := cmd.CombinedOutput()
		if err != nil {
			log.Printf("Failed to forward service: %s, %s, error: %v, out: %s", service.Unique(), cmd.Args, err, out)
		}
	}()
	return nil
}
