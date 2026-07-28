package common

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/standards"
)

// NormalizeAPIType lowercases/trims an API-type filter and validates it
// against the supported set. An empty filter is valid (means "any").
func NormalizeAPIType(apiType string) (string, error) {
	apiType = strings.ToLower(strings.TrimSpace(apiType))
	if apiType == "" {
		return "", nil
	}
	if slices.Contains(standards.APIS(), apiType) {
		return apiType, nil
	}
	return "", fmt.Errorf("unknown endpoint type %q (valid: %s)", apiType, strings.Join(standards.APIS(), ", "))
}

// FilterEndpoints returns the endpoints matching the given API type (when
// non-empty) and name (when non-empty). Matching is case-insensitive.
func FilterEndpoints(endpoints []*resources.Endpoint, apiType, name string) []*resources.Endpoint {
	out := make([]*resources.Endpoint, 0, len(endpoints))
	for _, ep := range endpoints {
		if apiType != "" && !strings.EqualFold(ep.API, apiType) {
			continue
		}
		if name != "" && !strings.EqualFold(ep.Name, name) {
			continue
		}
		out = append(out, ep)
	}
	return out
}

// ResolvedEndpoint is the deterministic native resolution of one endpoint.
type ResolvedEndpoint struct {
	// Address is the localhost address the endpoint binds to (e.g.
	// "localhost:6690" or "http://localhost:6691"). Empty for external
	// endpoints, which are not port-hashed.
	Address string
	// HostPort is the bare host:port form, suitable for a TCP liveness probe.
	HostPort string
	// External is true for external-visibility endpoints, whose address comes
	// from a runtime DNS record rather than the deterministic port hash, so it
	// cannot be computed offline here.
	External bool
	// Unsupported is true when the endpoint's API is not one codefly hashes a
	// port for. The runtime drops such endpoints (no mapping, nothing bound),
	// so there is no address to compute — we must not fabricate one.
	Unsupported bool
}

// ResolveNative computes an endpoint's deterministic native address WITHOUT a
// running flow, by reusing the exact port-hash + address formatting the
// runtime uses (network.NativeFor). namingScope must match the scope the
// service was/will be run with (empty for the normal interactive case).
func ResolveNative(ctx context.Context, workspace, module, service, namingScope string, ep *resources.Endpoint) (ResolvedEndpoint, error) {
	if ep.Visibility == resources.VisibilityExternal {
		return ResolvedEndpoint{External: true}, nil
	}
	// NativeFor only needs the endpoint name and API (for port hashing and
	// http/rest address formatting); module/service are passed explicitly.
	// Build a minimal proto endpoint rather than ep.Proto(), whose full
	// validation requires Module/Service fields that aren't populated on the
	// statically-loaded service.Endpoints.
	//
	// Mirror Proto()'s api inference EXACTLY: fold Name→api only when Name is
	// itself a supported API. If the resulting api is still unsupported, the
	// runtime would drop this endpoint entirely (no port, nothing bound), so
	// report it as unresolvable rather than hashing a port that never exists.
	api := ep.API
	if api == "" && standards.IsSupportedAPI(ep.Name) == nil {
		api = ep.Name
	}
	if standards.IsSupportedAPI(api) != nil {
		return ResolvedEndpoint{Unsupported: true}, nil
	}
	pe := &basev0.Endpoint{Name: ep.Name, Api: api, Visibility: ep.Visibility}
	inst := network.NativeFor(ctx, workspace, module, service, namingScope, pe)
	return ResolvedEndpoint{
		Address: inst.Address,
		// Hostname is the bare host (e.g. "localhost"); Host is already
		// "host:port", so the probe target must be built from Hostname+Port.
		HostPort: fmt.Sprintf("%s:%d", inst.Hostname, inst.Port),
	}, nil
}

// ResolvePreferredNative returns the concrete endpoint recorded by a live,
// scoped Codefly flow when its CLI server is available. Static deterministic
// resolution remains the offline fallback. This matters for container
// runtimes and persisted flow allocations, which may legitimately bind a
// different host port than a fresh hash would predict.
func ResolvePreferredNative(ctx context.Context, workspace, module, service, namingScope string, ep *resources.Endpoint) (ResolvedEndpoint, error) {
	resolved, err := ResolveNative(ctx, workspace, module, service, namingScope, ep)
	if err != nil || resolved.External || resolved.Unsupported {
		return resolved, err
	}
	client, closeClient := DialDaemon(workspace, namingScope)
	if client == nil {
		return resolved, nil
	}
	defer closeClient()
	address, err := ResolveAddress(ctx, client, module, service, ep.Name)
	if err != nil {
		return resolved, nil
	}
	running, err := resolvedEndpointFromAddress(address)
	if err != nil {
		return resolved, nil
	}
	return running, nil
}

func resolvedEndpointFromAddress(address string) (ResolvedEndpoint, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return ResolvedEndpoint{}, fmt.Errorf("empty runtime endpoint address")
	}
	hostPort := address
	if strings.Contains(address, "://") {
		parsed, err := url.Parse(address)
		if err != nil {
			return ResolvedEndpoint{}, fmt.Errorf("parse runtime endpoint address: %w", err)
		}
		hostPort = parsed.Host
	}
	if _, _, err := net.SplitHostPort(hostPort); err != nil {
		return ResolvedEndpoint{}, fmt.Errorf("runtime endpoint address has no concrete port: %q", address)
	}
	return ResolvedEndpoint{Address: address, HostPort: hostPort}, nil
}

// Reachable reports whether a TCP connection to hostPort succeeds within a
// short timeout — the honest signal of whether the endpoint is actually live,
// independent of any orchestration flow.
func Reachable(hostPort string) bool {
	if hostPort == "" {
		return false
	}
	conn, err := net.DialTimeout("tcp", hostPort, 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
