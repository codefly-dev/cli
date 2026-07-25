package control

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"

	"github.com/codefly-dev/cli/pkg/orchestration"
)

// This file finishes the remaining Introspector/Lifecycle checks:
//   - Lint / Compile: flow-driven static validation (like Test), plugin-backed
//     under the hood via the LintMode/CompileMode playbook.
//   - RunChecks: a plain shell command in the service dir (direct, no plugin) —
//     matching the MCP run_checks tool.
//   - Addresses: resolve a running service's reachable endpoints from the flow.

// runCheckFlow drives a static-validation flow (Lint/Compile). Pass/fail is the
// Start error — the playbook stops after the origin's terminal action and there
// is no per-run report getter, so success == Start returned nil.
//
// Caveat: because Start is the only signal, an infrastructure failure at the
// Start phase (no runtime backend, agent crash, Docker down) is reported as
// Passed:false with the error text in Output — indistinguishable from a genuine
// lint/compile failure. Callers that need to tell them apart cannot today.
func (p *planeImpl) runCheckFlow(ctx context.Context, mode orchestration.Mode, req CheckRequest) (CheckResult, error) {
	flow, err := p.buildFlow(ctx, mode, req.Service, orchestration.LocalEnvironmentName, func(f *orchestration.Flow) {
		// Static validation wants source + toolchain, not live dependencies.
		f.WithStandAlone(true)
		if req.RuntimeContext != "" {
			f.WithRuntimeContext(req.RuntimeContext)
		}
	})
	if err != nil {
		return CheckResult{}, err
	}
	defer stopFlow(flow)
	if err := flow.Start(ctx); err != nil {
		// A failing check is a result, not a call error.
		return CheckResult{Passed: false, Output: err.Error()}, nil
	}
	return CheckResult{Passed: true}, nil
}

func (p *planeImpl) Lint(ctx context.Context, req CheckRequest) (CheckResult, error) {
	return p.runCheckFlow(ctx, orchestration.LintMode, req)
}

func (p *planeImpl) Compile(ctx context.Context, req CheckRequest) (CheckResult, error) {
	return p.runCheckFlow(ctx, orchestration.CompileMode, req)
}

// RunChecks runs a command in the service directory (default `go test ./...`,
// matching the MCP tool). The command is argv split on whitespace and executed
// directly — NOT interpreted by a shell, so pipes, &&, quoting, and globs do not
// work. req.RuntimeContext is ignored (only Lint/Compile use it). A non-zero
// exit is a failing check, not a call error.
func (p *planeImpl) RunChecks(ctx context.Context, req CheckRequest) (CheckResult, error) {
	_, _, service, err := p.loadTarget(ctx, req.Service)
	if err != nil {
		return CheckResult{}, err
	}
	command := req.Command
	if command == "" {
		command = "go test ./..."
	}
	parts := strings.Fields(command)
	if len(parts) == 0 {
		return CheckResult{}, fmt.Errorf("empty command")
	}
	cmd := exec.CommandContext(ctx, parts[0], parts[1:]...)
	cmd.Dir = service.Dir()
	out, err := cmd.CombinedOutput()
	output := string(out)
	// A start failure (binary not found, ctx cancelled) yields no output — keep
	// the error text so a failing check is never blank.
	if err != nil && output == "" {
		output = err.Error()
	}
	return CheckResult{Passed: err == nil, Output: output}, nil
}

// Addresses resolves a running service's reachable endpoints. Addresses only
// exist while a flow is running (they come from its network mappings), so this
// errors when nothing is running and skips endpoints that aren't reachable.
func (p *planeImpl) Addresses(ctx context.Context, serviceName string) ([]Endpoint, error) {
	if p.host == nil || p.host.Flows() == nil {
		return nil, fmt.Errorf("no running flow; addresses are only available while a service is running")
	}
	_, managed := p.host.Flows().Active()
	flow, _ := managed.(*orchestration.Flow)
	if flow == nil {
		return nil, fmt.Errorf("no running flow; addresses are only available while a service is running")
	}
	_, module, service, err := p.loadTarget(ctx, serviceName)
	if err != nil {
		return nil, err
	}
	var endpoints []Endpoint
	for _, ep := range service.Endpoints {
		addr, err := flow.GetAddressForEndpoint(ctx, module.Name, service.Name, ep.Name)
		if err != nil {
			// No public mapping yet (or not resolvable) — skip this endpoint.
			continue
		}
		endpoint := Endpoint{Service: service.Name, Name: ep.Name, Type: ep.API}
		if host, portStr, splitErr := net.SplitHostPort(addr); splitErr == nil {
			endpoint.Host = host
			endpoint.Port, _ = strconv.Atoi(portStr)
		} else {
			endpoint.Host = addr
		}
		endpoints = append(endpoints, endpoint)
	}
	return endpoints, nil
}
