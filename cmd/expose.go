package cmd

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"strings"
	"time"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/services/services"
	"github.com/codefly-dev/core/configurations"
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
	provider, err := providers.New(ctx, project)
	if err != nil {
		return w.Wrap(err)
	}
	// Get the DNS
	// We gather public endpoints URL -- from provider info
	info, err := provider.GetProjectProviderInformation(ctx, "dns")
	if err != nil {
		return w.Wrapf(err, "cannot get DNS provider information")
	}
	for unique, url := range info.Data {
		ref, err := configurations.ParseServiceUnique(unique)
		if err != nil {
			return w.Wrapf(err, "cannot parse unique: %s", unique)
		}
		dir := path.Join(project.Dir(), ref.Application, ref.Name)
		w.Debug("exposing", wool.Field("dir", dir))
		service, err := configurations.LoadServiceFromDirUnsafe(ctx, dir)
		if err != nil {
			return w.Wrapf(err, "cannot load service from dir: %s", dir)
		}
		namespace := service.Namespace
		k8sSvc := fmt.Sprintf("svc/%s-%s", ref.Name, ref.Application)

		port := strings.Split(url, ":")[1]
		go func(service string, port string, namespace string) {
			for {
				w.Info(fmt.Sprintf("exposing %s on port %s", ref.Unique(), port), wool.Field("service", service), wool.Field("port", port), wool.Field("namespace", namespace))
				cmd := exec.CommandContext(ctx, "kubectl", "port-forward", "-n", namespace, k8sSvc, fmt.Sprintf("%s:8080", port))
				err := cmd.Run()
				if err != nil {
					log.Printf("Failed to forward service: %s, error: %v", service, err)
				}
				time.Sleep(time.Second * 5) // wait for 5 seconds before retrying
			}
		}(k8sSvc, port, namespace)

	}
	<-ctx.Done()
	return nil
}
