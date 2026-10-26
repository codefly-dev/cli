package orchestration

import (
	"context"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/codefly-dev/core/runners/dockerrun"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
)

// The loopback and wildcard addresses of each family, as Docker and the
// socket table spell them.
const (
	loopbackIPv4 = "127.0.0.1"
	loopbackIPv6 = "::1"
	wildcardIPv4 = "0.0.0.0"
	wildcardIPv6 = "::"
)

// listOwnedContainers lists the codefly containers carrying one exact recovery
// scope. It is a variable so a test can stand in for the Docker engine.
var listOwnedContainers = func(ctx context.Context, scopeID string) ([]container.Summary, error) {
	cli, err := dockerrun.NewClient()
	if err != nil {
		return nil, err
	}
	defer cli.Close()
	listCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return cli.ContainerList(listCtx, container.ListOptions{Filters: filters.NewArgs(
		filters.Arg("label", dockerrun.LabelCodeflyOwner+"=true"),
		filters.Arg("label", dockerrun.LabelCodeflyRecoveryScope+"="+scopeID),
	)})
}

// adoptedContainerAddresses answers which loopback addresses the first-Init
// port guard must not treat as a squatter: the ones published by the
// container this service's agent will adopt. A Docker store with
// keep-running leaves its container up so the next run reattaches to it by
// name, and that container still publishes the service's deterministic port —
// the one this Init proposes.
//
// The container is identified the way the agent's runner reuses it: the name
// every Core service agent gives it (dockerrun.ContainerName over the
// service's unique name in the run's workspace and naming scope, as
// Base.UniqueWithWorkspace spells it), owned by codefly,
// labelled with this flow's exact recovery scope and, when it carries one, its
// namespace (the rule dockerrun applies before reusing a container), running,
// and not an ephemeral one, which is never reused across sessions. An address
// is excused only for the loopback family the container publishes on: a
// 127.0.0.1 or 0.0.0.0 publication covers 127.0.0.1, a ::1 or :: one covers
// ::1, so a squatter on the other family is still refused. Anything this
// cannot establish — no recovery identity, no Docker engine — excuses nothing.
func (runner *Runner) adoptedContainerAddresses(ctx context.Context) func(address string) bool {
	scopeID, namespace, ok := strings.Cut(runner.containerRecoveryIdentity, ":")
	if !ok || scopeID == "" || runner.instance == nil || runner.instance.Identity == nil ||
		runner.world == nil || runner.world.Workspace == nil || runner.world.Env == nil {
		return func(string) bool { return false }
	}
	// The workspace and naming scope the agent's Load receives
	// (RuntimeInstance.Load sends the run's workspace name, not the identity's).
	identity, workspace := runner.instance.Identity, runner.world.Workspace.Name
	unique := identity.UniqueWithWorkspace(workspace)
	if scope := runner.world.Env.NamingScope; scope != "" {
		unique = identity.UniqueWithWorkspaceAndScope(workspace, scope)
	}
	name := dockerrun.ContainerName(unique)
	var containers []container.Summary
	listed := false
	return func(address string) bool {
		if !listed {
			listed = true
			containers, _ = listOwnedContainers(ctx, scopeID)
		}
		return slices.ContainsFunc(containers, func(c container.Summary) bool {
			return adoptableContainer(&c, name, scopeID, namespace) && publishesAddress(&c, address)
		})
	}
}

func adoptableContainer(c *container.Summary, name, scopeID, namespace string) bool {
	labels := c.Labels
	if labels[dockerrun.LabelCodeflyOwner] != "true" || labels[dockerrun.LabelCodeflyRecoveryScope] != scopeID ||
		labels[dockerrun.LabelCodeflyName] != name || labels[dockerrun.LabelCodeflyEphemeral] == "true" || c.State != "running" {
		return false
	}
	if found := labels[dockerrun.LabelCodeflyRecoveryNamespace]; found != "" && found != namespace {
		return false
	}
	return slices.Contains(c.Names, "/"+name)
}

// publishesAddress reports whether c publishes the loopback host:port address
// on that address's own family.
func publishesAddress(c *container.Summary, address string) bool {
	host, rawPort, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	port, err := strconv.ParseUint(rawPort, 10, 16)
	if err != nil {
		return false
	}
	var covering []string
	switch host {
	case loopbackIPv4:
		covering = []string{loopbackIPv4, wildcardIPv4}
	case loopbackIPv6:
		covering = []string{loopbackIPv6, wildcardIPv6}
	default:
		return false
	}
	return slices.ContainsFunc(c.Ports, func(p container.Port) bool {
		return uint64(p.PublicPort) == port && p.Type == "tcp" && slices.Contains(covering, p.IP)
	})
}
