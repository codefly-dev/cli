package runners

import (
	"context"
	"fmt"
	"time"

	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/agents/network"

	"github.com/codefly-dev/core/shared"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
)

type DockerRunner struct {
	cli           *client.Client
	ctx           context.Context
	Containers    []*ContainerInstance
	AgentLogger   *agents.AgentLogger
	ServiceLogger *agents.ServiceLogger
}

type ContainerInstance struct {
	ID    string
	Name  string
	Image string
	Host  string
	Port  int
}

// NewDockerRunner creates a new docker runner
func NewDockerRunner(ctx context.Context, serviceLogger *agents.ServiceLogger, agentLogger *agents.AgentLogger) (*DockerRunner, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, agentLogger.Wrapf(err, "cannot create docker client")
	}
	return &DockerRunner{
		ctx:           ctx,
		cli:           cli,
		AgentLogger:   agentLogger,
		ServiceLogger: serviceLogger,
	}, nil
}

type VolumeMount struct {
	Source string
	Target string
}

type Option struct {
	Cmd     []string
	Volumes []VolumeMount
	Envs    []string
}

type DockerOption func(option *Option)

func WithCmd(cmd ...string) DockerOption {
	return func(opt *Option) {
		opt.Cmd = append(opt.Cmd, cmd...)
	}
}

func WithVolume(source, target string) DockerOption {
	return func(opt *Option) {
		opt.Volumes = append(opt.Volumes, VolumeMount{Source: source, Target: target})
	}
}

func WithEnvironmentVariable(key, value string) DockerOption {
	return func(opt *Option) {
		opt.Envs = append(opt.Envs, fmt.Sprintf("%s=%s", key, value))
	}
}

type DockerImage struct {
	Image string
}

type CreateDockerInput struct {
	DockerImage
	ApplicationEndpointInstance *network.ApplicationEndpointInstance
}

func (r *DockerRunner) CreateContainer(input CreateDockerInput, opts ...DockerOption) error {
	options := &Option{}
	for _, opt := range opts {
		opt(options)
	}
	name := input.ApplicationEndpointInstance.Name()
	r.AgentLogger.Debugf("PortBinding %s", input.ApplicationEndpointInstance.ApplicationEndpoint.PortBinding)

	good, err := r.ContainerReady(name)
	if err != nil {
		return r.AgentLogger.Wrapf(err, "cannot check if container is ready")
	}
	if good {
		return nil
	}

	portMapping := nat.PortMap{
		nat.Port(input.ApplicationEndpointInstance.ApplicationEndpoint.PortBinding): []nat.PortBinding{
			{
				HostPort: input.ApplicationEndpointInstance.StringPort(),
			},
		},
	}
	r.AgentLogger.Debugf("port mapping: %v", portMapping)

	cfg := &container.Config{
		Image: input.Image,
		ExposedPorts: nat.PortSet{
			nat.Port(input.ApplicationEndpointInstance.ApplicationEndpoint.PortBinding): struct{}{},
		},
	}
	// err = r.EnsureImage(input.Image)

	var mounts []mount.Mount
	for _, volume := range options.Volumes {
		// Mounting volumes (in this case, for nginx context)
		mounts = append(mounts, mount.Mount{
			Type:   mount.TypeBind,
			Source: volume.Source, // replace with the actual path to your nginx.conf on your host
			Target: volume.Target,
		})
	}

	if options.Cmd != nil {
		cfg.Cmd = options.Cmd
	}
	cfg.Env = options.Envs

	t := time.Now()
	resp, err := r.cli.ContainerCreate(r.ctx,
		cfg,
		&container.HostConfig{
			AutoRemove:   true,
			PortBindings: portMapping,
			Mounts:       mounts,
		}, nil, nil, name)
	r.AgentLogger.Debugf("creating <%s> from <%s> took: %v", name, input.Image, time.Since(t))
	if err != nil {
		return r.AgentLogger.Wrapf(err, "cannot create container")
	}
	if err != nil {
		return r.AgentLogger.Wrapf(err, "cannot create container")
	}
	instance := ContainerInstance{
		Name: name,
		ID:   resp.ID,
	}
	r.Containers = append(r.Containers, &instance)
	return nil
}

func (r *DockerRunner) cleanContainers(name string) error {
	containers, err := r.cli.ContainerList(r.ctx, types.ContainerListOptions{All: true})
	if err != nil {
		return r.AgentLogger.Wrapf(err, "cannot list containers")
	}
	var id string

	match := fmt.Sprintf("/%s", name)
	for _, c := range containers {
		for _, cn := range c.Names {
			if cn == match { // Docker prefixes names with a "/"
				id = c.ID
				goto found
			}
		}
	}
found:
	if id == "" {
		return nil
	}
	inspectedContainer, err := r.cli.ContainerInspect(r.ctx, id)
	if err != nil {
		return r.AgentLogger.Wrapf(err, "cannot inspect container")
	}

	if inspectedContainer.State.Status == "exited" || inspectedContainer.State.Status == "dead" {
		// Restart the container if it's stopped TODO: port strategy
		//if err := r.cli.ContainerRestart(r.ctx, id, nil); err != nil {
		//	return r.AgentLogger.Wrapf(err, "cannot restart container")
		//}
		//r.AgentLogger.Info("container restarted")
		if err := r.cli.ContainerRemove(context.Background(), id, types.ContainerRemoveOptions{}); err != nil {
			r.AgentLogger.Warn("Error remove container %s: %v", name, err)
			return r.AgentLogger.Wrapf(err, "cannot remove container")
		}
	} else {
		r.AgentLogger.Info("container is running")
		// Stop the container
		if err := r.cli.ContainerStop(context.Background(), id, container.StopOptions{}); err != nil {
			r.AgentLogger.Warn("Error stopping container %s: %v", name, err)
			return r.AgentLogger.Wrapf(err, "cannot stop container")
		}

		// Remove the container
		if err := r.cli.ContainerRemove(context.Background(), id, types.ContainerRemoveOptions{}); err != nil {
			return r.AgentLogger.Wrapf(err, "cannot remove stopped container")
		}
	}
	//// Stop the container
	//if err := r.cli.ContainerStop(context.Background(), id, container.StopOptions{}); err != nil {
	//	r.AgentLogger.Warn("Error stopping container %s: %v", name, err)
	//	return r.AgentLogger.Wrapf(err, "cannot stop container")
	//}

	//// Remove the container
	//if err := r.cli.ContainerRemove(context.Background(), id, types.ContainerRemoveOptions{}); err != nil {
	//	return r.AgentLogger.Wrapf(err, "cannot remove stopped container")
	//}
	//status, errs := r.cli.ContainerWait(context.Background(), id, container.WaitConditionRemoved)
	//select {
	//case err := <-errs:
	//	if err != nil {
	//		r.AgentLogger.Debugf("cannot wait for container to be removed: %v", err)
	//		return nil
	//	}
	//case s := <-status:
	//	if s.StatusCode == 0 {
	//		r.AgentLogger.Debugf("container %s removed successfully", name)
	//		return nil
	//	}
	//}
	//return r.AgentLogger.Errorf("not sure why I am here")
	return nil
}

func (r *DockerRunner) StartContainer(c *ContainerInstance) error {
	if err := r.cli.ContainerStart(r.ctx, c.ID, types.ContainerStartOptions{}); err != nil {
		return err
	}
	return nil
}

func (r *DockerRunner) Start() error {
	for _, c := range r.Containers {
		err := r.StartContainer(c)
		if err != nil {
			return r.AgentLogger.Wrapf(err, "cannot start container")
		}
		// After the container starts, get its logs
		out, err := r.cli.ContainerLogs(r.ctx, c.ID,
			types.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Follow: true, Timestamps: false, Details: false})
		if err != nil {
			return r.AgentLogger.Wrapf(err, "cannot get container logs")
		}

		go func(name string) {
			ForwardLogs(out, r.ServiceLogger)
		}(c.Name)
	}
	return nil
}

func (r *DockerRunner) Stop() error {
	logger := shared.NewLogger("DockerRunner.Stop")
	logger.Debugf("cleaning up everything by default")
	for _, c := range r.Containers {
		logger.Debugf("cleaning %v", c.Name)
		err := r.cleanContainers(c.Name)
		if err != nil {
			r.AgentLogger.Warn("cannot clean container %s: %v", c.Name, err)
		}
	}
	return nil
}

func (r *DockerRunner) EnsureImage(imageName string) error {
	// Check if image is available locally
	ctx := context.Background()
	images, err := r.cli.ImageList(ctx, types.ImageListOptions{})
	if err != nil {
		panic(err)
	}

	imageExists := false
	for _, image := range images {
		for _, tag := range image.RepoTags {
			if tag == imageName {
				imageExists = true
				break
			}
		}
	}

	// If image is not available locally, pull it
	if !imageExists {
		fmt.Println("Image not found locally. Pulling...")
		_, err := r.cli.ImagePull(ctx, imageName, types.ImagePullOptions{})
		if err != nil {
			panic(err)
		}
		fmt.Println("Image pulled successfully.")
	}
	return nil
}

func (r *DockerRunner) IP(instance *network.ApplicationEndpointInstance) (string, error) {
	// Get IP Address
	r.AgentLogger.TODO("DEAL WITH NETWORK DOCKER")
	return "172.17.0.2", nil
	//r.AgentLogger.Debugf("Instance container %v", instance)
	//// Get container JSON object
	//container, err := r.cli.ContainerInspect(context.Background(), instance.Name())
	//if err != nil {
	//	return "", r.AgentLogger.Wrapf(err, "cannot inspect container")
	//}
	//ipAddress := container.NetworkSettings.IPAddress
	//// If using the default bridge network, you might want to get the IP from the Networks map
	//if ipAddress != "" {
	//	return ipAddress, nil
	//}
	//for _, network := range container.NetworkSettings.Networks {
	//	return network.IPAddress, nil
	//}
	//return "", r.AgentLogger.Errorf("cannot get ip address")
}

func (r *DockerRunner) ContainerReady(name string) (bool, error) {
	containers, err := r.cli.ContainerList(r.ctx, types.ContainerListOptions{All: true})
	if err != nil {
		return false, r.AgentLogger.Wrapf(err, "cannot list containers")
	}
	var id string

	match := fmt.Sprintf("/%s", name)
	for _, c := range containers {
		for _, cn := range c.Names {
			if cn == match { // Docker prefixes names with a "/"
				id = c.ID
				goto found
			}
		}
	}
found:
	if id == "" {
		return false, nil
	}
	inspectedContainer, err := r.cli.ContainerInspect(r.ctx, id)
	if err != nil {
		return false, r.AgentLogger.Wrapf(err, "cannot inspect container")
	}

	if inspectedContainer.State.Status == "running" {
		return true, nil
	}
	return false, nil
}
