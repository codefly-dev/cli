package cmd

import (
	"context"
	"os"
	"os/signal"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/services/services"
	"github.com/codefly-dev/core/configurations"
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
	//w := wool.Get(ctx).In("expose")
	//provider, err := providers.NewConfigurationInformation(ctx, project)
	//if err != nil {
	//	return w.Wrap(err)
	//}
	//// Get the DNS
	//// We gather public endpoints URL -- from provider info
	//info, err := provider.GetProjectProviderInformation(ctx, "dns")
	//if err != nil {
	//	return w.Wrapf(err, "cannot get DNS provider information")
	//}
	//for unique, u := range info.Data {
	//	if !strings.HasPrefix(u, "http") {
	//		continue
	//	}
	//	ref, err := configurations.ParseServiceUnique(unique)
	//	if err != nil {
	//		return w.Wrapf(err, "cannot parse unique: %s", unique)
	//	}
	//	namespace := fmt.Sprintf("%s-%s", project.Name, ref.Application)
	//	k8sSvc := fmt.Sprintf("svc/%s", ref.Name)
	//	w.Debug("k8s", wool.Field("namespace", namespace), wool.Field("service", k8sSvc))
	//	//// Check if this service exists in this namespace
	//	_, err = exec.CommandContext(ctx, "kubectl", "get", k8sSvc, "-n", namespace).Output()
	//	if err != nil {
	//		return w.Wrapf(err, "cannot get service: %s", k8sSvc)
	//	}
	//
	//	// Start a port forward
	//	target, err := url.Parse(u)
	//	if err != nil {
	//		return w.Wrapf(err, "cannot parse URL: %s", u)
	//	}
	//	port := target.Port()
	//	go func(service string, port string, namespace string) {
	//		w.Info(fmt.Sprintf("exposing %s at http://localhost:%s", ref.Unique(), port), wool.Field("service", service), wool.Field("port", port), wool.Field("namespace", namespace))
	//		cmd := exec.CommandContext(ctx, "kubectl", "port-forward", "-n", namespace, k8sSvc, fmt.Sprintf("%s:8080", port))
	//		err := cmd.Run()
	//		if err != nil {
	//			log.Printf("Failed to forward service: %s, %s, error: %v", service, cmd.Args, err)
	//		}
	//	}(k8sSvc, port, namespace)
	//
	//}
	//<-ctx.Done()
	return nil
}
