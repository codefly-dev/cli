package cmd

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	providers "github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/standards"
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

		workspace := common.Workspace(ctx)
		err := expose(ctx, workspace)
		cli.ExitOnError(err, "Cannot expose service")
	},
}

type KubernetesService struct {
	Namespace string
	Name      string
	*resources.ServiceIdentity
}

func expose(ctx context.Context, workspace *resources.Workspace) error {
	w := wool.Get(ctx).In("expose")
	var environ *resources.Environment
	if env == "local" {
		environ = resources.LocalEnvironment()
	} else {
		environ = &resources.Environment{Name: env}
	}
	// Get the running network manager
	configurationManager, err := providers.NewManager(ctx, workspace)
	if err != nil {
		return w.Wrap(err)
	}
	localReader, err := providers.NewConfigurationLocalReader(ctx, workspace)
	if err != nil {
		return w.Wrap(err)
	}
	configurationManager.WithLoader(localReader)
	err = configurationManager.Load(ctx, resources.LocalEnvironment())

	networkManager, err := network.NewDeployManager(ctx, configurationManager)
	if err != nil {
		return w.Wrap(err)
	}
	// Loop over svcs
	svcs, err := workspace.LoadServices(ctx)
	if err != nil {
		return w.Wrap(err)
	}

	var k8sServices []*KubernetesService
	for _, service := range svcs {
		id, err := service.Identity()
		if err != nil {
			return w.Wrap(err)
		}
		cli.RegisterLoggingResource(id.Unique())
		k8sService, err := GetKubernetesService(ctx, environ, workspace, id, networkManager)
		if err != nil {
			return w.Wrap(err)
		}
		k8sServices = append(k8sServices, k8sService)
		for _, endpoint := range service.Endpoints {
			if endpoint.Visibility == resources.VisibilityPublic {
				err = exposeService(ctx, environ, workspace, id, endpoint, k8sService)
				if err != nil {
					return w.Wrap(err)
				}
			}
		}
	}
	fetchAllPodsLogs(ctx, k8sServices)
	<-ctx.Done()
	return nil
}

func fetchAllPodsLogs(ctx context.Context, k8sServices []*KubernetesService) {
	var wg sync.WaitGroup

	for _, k8sService := range k8sServices {
		wg.Add(1)
		go func(svc *KubernetesService) {
			defer wg.Done()
			err := fetchNamespacePodLogs(ctx, svc)
			if err != nil {
				log.Printf("Error fetching logs for namespace %s: %v", svc.Namespace, err)
			}
		}(k8sService)
	}

	wg.Wait()
}

func fetchNamespacePodLogs(ctx context.Context, svc *KubernetesService) error {
	// Get all pods in the namespace
	podsCmd := exec.CommandContext(ctx, "kubectl", "get", "pods", "-n", svc.Namespace, "-o", "jsonpath={.items[*].metadata.name}")
	podsOut, err := podsCmd.Output()
	if err != nil {
		return err
	}

	pods := strings.Fields(string(podsOut))

	var wg sync.WaitGroup
	for _, pod := range pods {
		if !strings.Contains(pod, svc.ServiceIdentity.Name) {
			continue
		}
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			_ = fetchPodLogs(ctx, svc, p)
		}(pod)
	}

	wg.Wait()
	return nil
}

func fetchPodLogs(ctx context.Context, service *KubernetesService, pod string) error {
	w := wool.Get(ctx).In("fetchPodLogs")
	identifier := &wool.Identifier{Unique: service.Unique(), Kind: "SERVICE"}

	logsCmd := exec.CommandContext(ctx, "kubectl", "logs", "-n", service.Namespace, "-f", pod)
	stdout, err := logsCmd.StdoutPipe()
	if err != nil {
		return w.Wrapf(err, "error creating StdoutPipe for pod %s in namespace %s", pod, service.Namespace)
	}

	err = logsCmd.Start()
	if err != nil {
		return w.Wrapf(err, "error starting logs command for pod %s in namespace %s", pod, service.Namespace)
	}

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		text := scanner.Text()
		if strings.Contains(text, "failed to try resolving symlinks in path") {
			continue // Ignore this specific error message
		}
		cli.GetLogger().ProcessWithSource(identifier, &wool.Log{Message: text, Level: wool.FORWARD})
	}

	if err := scanner.Err(); err != nil {
		return w.Wrapf(err, "error scanning logs for pod %s in namespace %s", pod, service.Namespace)
	}

	err = logsCmd.Wait()
	if err != nil {
		return w.Wrapf(err, "error waiting for logs command for pod %s in namespace %s", pod, service.Namespace)
	}
	return nil
}

func GetKubernetesService(ctx context.Context, env *resources.Environment, workspace *resources.Workspace, service *resources.ServiceIdentity, networkManager network.Manager) (*KubernetesService, error) {
	w := wool.Get(ctx).In("getKubernetesService")
	namespace, err := networkManager.GetNamespace(ctx, env, workspace, service)
	if err != nil {
		return nil, w.Wrap(err)
	}
	k8sSvc := fmt.Sprintf("svc/%s", service.Name)
	w.Debug("k8s", wool.Field("namespace", namespace), wool.Field("service", k8sSvc))
	return &KubernetesService{Namespace: namespace, Name: k8sSvc, ServiceIdentity: service}, nil
}

func exposeService(ctx context.Context, env *resources.Environment, workspace *resources.Workspace, service *resources.ServiceIdentity, endpoint *resources.Endpoint, k8sSvc *KubernetesService) error {
	w := wool.Get(ctx).In("exposeService")
	// Check if this service exists in this namespace
	_, err := exec.CommandContext(ctx, "kubectl", "get", k8sSvc.Name, "-n", k8sSvc.Namespace).Output()
	if err != nil {
		//w.Warn(fmt.Sprintf("cannot get service: %s", k8sSvc.Name))
		return nil
	}
	hostPort := network.ToNamedPort(ctx, workspace.Name, service.Module, service.Name, endpoint.Name, endpoint.API)
	targetPort := standards.Port(endpoint.API)

	go func() {
		w.Info(fmt.Sprintf("exposing %s at http://localhost:%d", service.Unique(), hostPort), wool.Field("service", service.Unique()), wool.Field("port", targetPort), wool.Field("namespace", k8sSvc.Namespace))
		cmd := exec.CommandContext(ctx, "kubectl", "port-forward", "-n", k8sSvc.Namespace, k8sSvc.Name, fmt.Sprintf("%d:%d", hostPort, targetPort))
		out, err := cmd.CombinedOutput()
		if err != nil {
			log.Printf("Failed to forward service: %s, %s, error: %v, out: %s", service.Unique(), cmd.Args, err, out)
		}
	}()

	return nil
}

var env string

func init() {
	ExposeCmd.Flags().StringVar(&env, "env", "local", "Environment to deploy the service")
}
