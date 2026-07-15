package platform

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/codefly-dev/cli/pkg/deployments"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/sdk"
	"github.com/codefly-dev/core/wool"
	"google.golang.org/protobuf/encoding/protojson"
)

type Deployment struct {
	Service    *resources.Service
	OutputPath string
}

type DeploymentManager struct {
	env         *resources.Environment
	workspace   *resources.Workspace
	deployments []Deployment
}

func NewDeploymentManager(ctx context.Context, workspace *resources.Workspace, env *resources.Environment) *DeploymentManager {
	return &DeploymentManager{workspace: workspace, env: env}
}

func (d *DeploymentManager) Handle(ctx context.Context, service *resources.Service, module *resources.Module, deploy *builderv0.DeploymentOutput) error {
	w := wool.Get(ctx).In("Builder")
	switch v := deploy.Kind.(type) {
	case *builderv0.DeploymentOutput_Kubernetes:
		if v.Kubernetes.Kind == builderv0.KubernetesDeploymentOutput_KUSTOMIZE {
			d.deployments = append(d.deployments, Deployment{
				Service:    service,
				OutputPath: deployments.KustomizeDir(ctx, d.workspace, module, service),
			})
		}
	default:
		return w.NewError("unsupported deployment kind %T", deploy.Kind)
	}

	return nil
}

var _ deployments.Manager = &DeploymentManager{}

func (d *DeploymentManager) Deploy(ctx context.Context, workspace *resources.Workspace) error {
	w := wool.Get(ctx).In("UpdateWorkspace")
	w.Info("deploying workspace", wool.Field("workspace", workspace))
	url := fmt.Sprintf("%s/deploy", GetClient().URL("platform/workspace"))
	c := http.Client{Timeout: 5 * time.Second}

	for _, deploy := range d.deployments {
		proto, err := sdk.SerializeDirectory(deploy.OutputPath, []string{".yaml"})
		if err != nil {
			return w.Wrapf(err, "cannot serialize workspace")
		}
		json, err := protojson.Marshal(proto)
		if err != nil {
			return w.Wrapf(err, "cannot serialize workspace")
		}
		payload := fmt.Sprintf(`{"directory": %s}`, string(json))
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(payload))
		if err != nil {
			return w.Wrapf(err, "cannot create update workspace http request")
		}
		req.Header.Add(wool.Header(wool.UserAuthIDKey), "primary")
		resp, err := c.Do(req)
		if err != nil {
			return w.Wrapf(err, "cannot update workspace")
		}
		status := resp.StatusCode
		_ = resp.Body.Close()
		if status != http.StatusOK {
			return w.NewError("unexpected status code: %s", resp.Status)
		}
	}
	return nil
}
