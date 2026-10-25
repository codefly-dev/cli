package runnable

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"

	"github.com/codefly-dev/core/agents/helpers/docker"
	"github.com/codefly-dev/core/agents/services"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	corerunnable "github.com/codefly-dev/core/runnable"
	"github.com/codefly-dev/core/runners/dockerrun"
)

// A runnable served locally is its generated harness, built into the Linux
// image its agent describes and run as one unit that cannot reach a network.
//
// The unit is two containers on a network of their own. The harness sits on
// that network alone: an internal network with no IPv4, so it has no route
// out, no name resolution beyond its one peer, and no way to reach the host.
// Its root is read-only, it runs as the unprivileged user its image names,
// every capability is dropped and privilege escalation is refused. The front
// is the one peer: a TCP relay that listens on the host's loopback, on the
// port the composition resolves for the runnable, and forwards to the harness.
// It carries bytes and nothing else; the harness still requires a Work Context
// on every call. Nothing the harness does reaches anything but its caller.
//
// This is the local placement of an authored tool. It is not the per-call
// gVisor invocation a deployment runs (module-runtime's KUBERNETES facility):
// one long-running harness serves every call, as a service does.

// relayImage is the front of a served runnable: a pinned BusyBox, whose nc
// relays one connection to the harness per caller connection. It is a Docker
// Official Image pinned by digest, so the bytes that sit between a caller and
// a harness are fixed.
const relayImage = "mirror.gcr.io/library/busybox:1.38.0@sha256:fd7dc98638c8e305f4dc34e979f1c0fdfdcaeb0fbf8fcff77ae834b6da3d7e6e"

// harnessAlias is the name the harness answers to on its unit's network. A
// unit's container names are long — a DNS label holds 63 characters — and the
// network holds only the harness and its relay, so one short alias is
// unambiguous.
const harnessAlias = "harness"

// harnessPort is where the harness listens inside its network. It is a
// container-private port, never published: the host reaches the relay.
const harnessPort = 8000

// unprivileged is the user the relay runs as; the harness runs as the user
// its image declares (the agent's recipe sets one).
const unprivileged = "65532:65532"

// harnessMemory and harnessProcesses bound what one harness may hold, so a
// document crafted to exhaust a parser exhausts its own container.
var (
	harnessMemory    int64 = 1 << 30
	harnessProcesses int64 = 256
)

// readyTimeout bounds how long a served harness may take to answer its first
// request. The image is built before the clock starts.
const readyTimeout = 60 * time.Second

// ImageRepository is the local repository a runnable's harness image is
// named in. The image is built for this host and never pushed.
func ImageRepository(r *resources.Runnable) string {
	return "codefly.local/runnables/" + r.Module() + "/" + r.Name
}

// Image builds the Linux image of r's generated harness: the agent prepares
// the release and describes its image as a DockerBuildPlan (Builder.Build);
// the plan is verified against the files on disk and built for this host's
// architecture with core's builder. output must be a new absolute directory.
// The returned reference is the built tag.
func Image(ctx context.Context, workspace *resources.Workspace, r *resources.Runnable, output string, logs io.Writer) (string, error) {
	if !filepath.IsAbs(output) {
		return "", fmt.Errorf("image output must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o700); err != nil {
		return "", err
	}
	if err := os.Mkdir(output, 0o700); err != nil {
		return "", fmt.Errorf("image output must be new: %w", err)
	}
	recipeDir := filepath.Join(output, "image")
	recipe, err := describeImage(ctx, workspace, r, output, recipeDir, logs)
	if err != nil {
		return "", err
	}
	builder, err := docker.NewBuilder(docker.BuilderConfiguration{
		Root:        recipeDir,
		Dockerfile:  recipe.GetDockerfile(),
		Destination: resources.NewDockerImage(recipe.GetImage()),
		Platform:    "linux/" + runtime.GOARCH,
		Output:      logs,
	})
	if err != nil {
		return "", err
	}
	result, err := builder.Build(ctx)
	if err != nil {
		return "", fmt.Errorf("build the harness image: %w", err)
	}
	return result.Image, nil
}

// describeImage asks the agent for the harness image recipe and releases the
// agent before anything is built: the image build is the CLI's, and an agent
// held open across it outlives the work it was loaded for.
func describeImage(ctx context.Context, workspace *resources.Workspace, r *resources.Runnable, output, recipeDir string, logs io.Writer) (*builderv0.DockerBuildRecipe, error) {
	client, closeAgent, err := load(ctx, workspace, r, logs)
	if err != nil {
		return nil, err
	}
	defer closeAgent()
	inputs, err := client.RunnableBuildInputs(ctx, &builderv0.RunnableBuildInputsRequest{OutputDirectory: filepath.Join(output, "prepared")})
	if err != nil {
		return nil, fmt.Errorf("agent RunnableBuildInputs: %w", err)
	}
	if inputs.GetState().GetState() != builderv0.RunnableBuildInputsStatus_SUCCESS {
		return nil, fmt.Errorf("agent did not prepare build inputs: %s", inputs.GetState().GetMessage())
	}
	repository := ImageRepository(r)
	built, err := client.Build(ctx, &builderv0.BuildRequest{
		OutputDirectory: recipeDir,
		BuildContext: &builderv0.BuildContext{Kind: &builderv0.BuildContext_DockerBuildContext{
			DockerBuildContext: &builderv0.DockerBuildContext{DockerRepository: repository}}},
	})
	if err != nil {
		return nil, fmt.Errorf("agent Build: %w; the runnable agent must emit an image recipe (runnable-python 0.0.7 or later)", err)
	}
	return harnessRecipe(recipeDir, built.GetResult().GetDockerBuildPlan(), repository+":"+r.Version)
}

// harnessRecipe holds an agent's plan to what a served harness needs: the plan
// verifies against the files on disk, it is one image named in the runnable's
// repository at its version, its context is the assembled output (never the
// author's source tree), and it can be built for this host.
func harnessRecipe(dir string, plan *builderv0.DockerBuildPlan, image string) (*builderv0.DockerBuildRecipe, error) {
	if plan == nil {
		return nil, fmt.Errorf("the runnable agent answered Build without an image recipe")
	}
	if err := services.VerifyDockerBuildPlan(dir, plan); err != nil {
		return nil, fmt.Errorf("verify the harness image recipe: %w", err)
	}
	if len(plan.GetRecipes()) != 1 {
		return nil, fmt.Errorf("the harness image recipe describes %d images, and a harness is one", len(plan.GetRecipes()))
	}
	recipe := plan.GetRecipes()[0]
	if recipe.GetImage() != image {
		return nil, fmt.Errorf("the harness image recipe names %q, not %q", recipe.GetImage(), image)
	}
	if recipe.GetContextRoot() != builderv0.RecipeContextRoot_RECIPE_CONTEXT_ROOT_OUTPUT || recipe.GetContext() != "." {
		return nil, fmt.Errorf("the harness image recipe must build the context the agent assembled in its output directory")
	}
	if platform := "linux/" + runtime.GOARCH; !slices.Contains(recipe.GetPlatforms(), platform) {
		return nil, fmt.Errorf("the harness image recipe targets %v, not this host's %s", recipe.GetPlatforms(), platform)
	}
	return recipe, nil
}

// Unit is one served harness: its relay's loopback port and the containers
// and network that hold it.
type Unit struct {
	// Name is the prefix every resource of the unit is named with.
	Name string
	// HostPort is the loopback port the relay listens on.
	HostPort int
	docker   *client.Client
}

// UnitName names a served runnable's resources after its workspace, module,
// name and the run's naming scope, so a second serve of the same runnable in
// the same scope replaces the first rather than standing beside it.
func UnitName(workspace string, r *resources.Runnable, scope string) string {
	parts := []string{"codefly-runnable", workspace, r.Module(), r.Name}
	if scope != "" {
		parts = append(parts, scope)
	}
	var name strings.Builder
	for _, c := range strings.ToLower(strings.Join(parts, "-")) {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' {
			name.WriteRune(c)
			continue
		}
		name.WriteByte('-')
	}
	return name.String()
}

// harnessContainer is the harness: on its internal network only, nothing
// published, read-only root, no capability, no privilege escalation, and a
// private /tmp for what an interpreter writes there.
func harnessContainer(image, networkName string) (*container.Config, *container.HostConfig, *network.NetworkingConfig) {
	config := &container.Config{
		Image: image,
		Env:   []string{corerunnable.ListenAddressEnv + "=[::]:" + strconv.Itoa(harnessPort)},
	}
	host := &container.HostConfig{
		NetworkMode:    container.NetworkMode(networkName),
		ReadonlyRootfs: true,
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges"},
		Tmpfs:          map[string]string{"/tmp": "rw,noexec,nosuid,size=64m"},
		Resources:      container.Resources{Memory: harnessMemory, PidsLimit: &harnessProcesses},
	}
	return config, host, &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{networkName: {Aliases: []string{harnessAlias}}}}
}

// relayContainer is the front: it publishes one port on the host's loopback
// and relays each connection to the harness by its alias on the internal
// network it joins after creation.
func relayContainer(hostPort int) (*container.Config, *container.HostConfig) {
	port := nat.Port(strconv.Itoa(harnessPort) + "/tcp")
	config := &container.Config{
		Image:        relayImage,
		User:         unprivileged,
		Cmd:          []string{"nc", "-lk", "-p", strconv.Itoa(harnessPort), "-e", "nc", harnessAlias, strconv.Itoa(harnessPort)},
		ExposedPorts: nat.PortSet{port: {}},
	}
	host := &container.HostConfig{
		NetworkMode:    "bridge",
		PortBindings:   nat.PortMap{port: []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: strconv.Itoa(hostPort)}}},
		ReadonlyRootfs: true,
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges"},
	}
	return config, host
}

// Serve starts r's harness image as a unit with no network, its relay on the
// host's loopback at hostPort, and waits until the harness answers through it.
func Serve(ctx context.Context, name, image string, hostPort int, logs io.Writer) (*Unit, error) {
	cli, err := dockerrun.NewClient()
	if err != nil {
		return nil, fmt.Errorf("connect to Docker: %w", err)
	}
	unit := &Unit{Name: name, HostPort: hostPort, docker: cli}
	if err := unit.Stop(ctx); err != nil {
		return nil, fmt.Errorf("remove a previous unit %s: %w", name, err)
	}
	served := false
	defer func() {
		if !served {
			_ = unit.Stop(context.WithoutCancel(ctx))
		}
	}()
	noIPv4, ipv6 := false, true
	if _, err := cli.NetworkCreate(ctx, unit.network(), network.CreateOptions{
		Internal: true, EnableIPv4: &noIPv4, EnableIPv6: &ipv6, Labels: unit.labels(),
	}); err != nil {
		return nil, fmt.Errorf("create the internal network of %s: %w", name, err)
	}
	if err := pull(ctx, cli, relayImage, logs); err != nil {
		return nil, err
	}
	config, host, networking := harnessContainer(image, unit.network())
	config.Labels = unit.labels()
	if err := createContainer(ctx, cli, unit.harness(), config, host, networking); err != nil {
		return nil, err
	}
	relay, relayHost := relayContainer(hostPort)
	relay.Labels = unit.labels()
	if err := createContainer(ctx, cli, unit.relay(), relay, relayHost, nil); err != nil {
		return nil, err
	}
	if err := cli.NetworkConnect(ctx, unit.network(), unit.relay(), nil); err != nil {
		return nil, fmt.Errorf("join the relay to the harness network: %w", err)
	}
	for _, id := range []string{unit.harness(), unit.relay()} {
		if err := cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
			return nil, fmt.Errorf("start %s: %w", id, err)
		}
	}
	if err := unit.awaitAnswer(ctx); err != nil {
		return nil, fmt.Errorf("%w\nharness logs:\n%s", err, unit.Logs(ctx))
	}
	served = true
	return unit, nil
}

// Address is the HTTP base URL the unit answers on.
func (u *Unit) Address() string {
	return "http://127.0.0.1:" + strconv.Itoa(u.HostPort)
}

// Logs returns what the harness wrote, for a refusal that needs its reason.
func (u *Unit) Logs(ctx context.Context) string {
	reader, err := u.docker.ContainerLogs(ctx, u.harness(), container.LogsOptions{ShowStdout: true, ShowStderr: true, Tail: "50"})
	if err != nil {
		return err.Error()
	}
	defer reader.Close()
	raw, _ := io.ReadAll(io.LimitReader(reader, 64<<10))
	return string(raw)
}

// Exec runs a command inside the harness container and returns its combined
// output: the means by which a person checks, from inside, what the harness
// can reach.
func (u *Unit) Exec(ctx context.Context, command ...string) (string, int, error) {
	created, err := u.docker.ContainerExecCreate(ctx, u.harness(), container.ExecOptions{Cmd: command, AttachStdout: true, AttachStderr: true})
	if err != nil {
		return "", 0, err
	}
	attached, err := u.docker.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{})
	if err != nil {
		return "", 0, err
	}
	defer attached.Close()
	raw, _ := io.ReadAll(io.LimitReader(attached.Reader, 64<<10))
	inspected, err := u.docker.ContainerExecInspect(ctx, created.ID)
	if err != nil {
		return string(raw), 0, err
	}
	return string(raw), inspected.ExitCode, nil
}

// Stop removes the unit's containers and network; removing what is absent is
// not an error.
func (u *Unit) Stop(ctx context.Context) error {
	var errs []error
	for _, id := range []string{u.relay(), u.harness()} {
		if err := u.docker.ContainerRemove(ctx, id, container.RemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	if err := u.docker.NetworkRemove(ctx, u.network()); err != nil && !cerrdefs.IsNotFound(err) {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Close releases the Docker connection.
func (u *Unit) Close() error { return u.docker.Close() }

func (u *Unit) harness() string { return u.Name + "-harness" }
func (u *Unit) relay() string   { return u.Name + "-relay" }
func (u *Unit) network() string { return u.Name + "-net" }

func (u *Unit) labels() map[string]string {
	return map[string]string{"dev.codefly.runnable.unit": u.Name}
}

// awaitAnswer waits for the harness's own HTTP answer through the relay. A
// TCP connect is not evidence: the relay accepts before the harness listens.
func (u *Unit) awaitAnswer(ctx context.Context) error {
	deadline := time.Now().Add(readyTimeout)
	probe := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DialContext: (&net.Dialer{Timeout: time.Second}).DialContext}}
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.Address()+corerunnable.ServedInvokeProcedure, nil)
		if err != nil {
			return err
		}
		if response, err := probe.Do(request); err == nil {
			_ = response.Body.Close()
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the harness did not answer on %s within %s", u.Address(), readyTimeout)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func createContainer(ctx context.Context, cli *client.Client, name string, config *container.Config, host *container.HostConfig, networking *network.NetworkingConfig) error {
	if _, err := cli.ContainerCreate(ctx, config, host, networking, nil, name); err != nil {
		return fmt.Errorf("create %s: %w", name, err)
	}
	return nil
}

// pull fetches the pinned relay image when it is absent.
func pull(ctx context.Context, cli *client.Client, reference string, logs io.Writer) error {
	name, _, _ := strings.Cut(reference, "@")
	if slash, colon := strings.LastIndex(name, "/"), strings.LastIndex(name, ":"); colon > slash {
		name = name[:colon]
	}
	present, err := cli.ImageList(ctx, image.ListOptions{Filters: filters.NewArgs(filters.Arg("reference", name))})
	if err == nil {
		for i := range present {
			for _, digest := range present[i].RepoDigests {
				if _, pinned, found := strings.Cut(digest, "@"); found && strings.HasSuffix(reference, "@"+pinned) {
					return nil
				}
			}
		}
	}
	reader, err := cli.ImagePull(ctx, reference, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull the relay image %s: %w", reference, err)
	}
	defer reader.Close()
	_, err = io.Copy(logs, reader)
	return err
}
