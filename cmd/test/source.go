package test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/blang/semver"
	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/cli/pkg/sourceworkspace"
	"github.com/codefly-dev/core/agents/manager"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	codev0 "github.com/codefly-dev/core/generated/go/codefly/services/code/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	toolingv0 "github.com/codefly-dev/core/generated/go/codefly/services/tooling/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
)

var (
	sourceDir            string
	sourceRuntimeContext string
	sourceTarget         string
	sourceFilters        []string
	sourceSuite          string
	sourceTimeout        string
	sourceVerbose        bool
	sourceRace           bool
	sourceCoverage       bool
	sourceAgent          string
	sourceQualification  bool
)

// SourceCmd validates an arbitrary checkout through the same Runtime.Test RPC
// used by persistent Codefly services. The adapter creates resource metadata;
// the selected plugin owns discovery, dependency context, and native execution.
var SourceCmd = &cobra.Command{
	Use:   "source",
	Short: "Run plugin-owned tests against an arbitrary source checkout",
	Args:  cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		ctx, done := common.NewContext()
		defer done()
		ctx, stop := common.SignalContext(ctx)
		defer stop()
		cli.Init()
		defer services.ClearAgents()

		dir := sourceDir
		if dir == "" {
			var err error
			dir, err = os.Getwd()
			if err != nil {
				return fmt.Errorf("resolve source directory: %w", err)
			}
		}
		prepared, err := prepareSourceWorkspace(ctx, dir, sourceAgent)
		if err != nil {
			return err
		}
		defer prepared.Close()
		if sourceQualification {
			if sourceAgent == "" {
				return fmt.Errorf("source qualification requires an exact --agent")
			}
			if err := verifySourceCapabilityHandshake(ctx, prepared.Service.Agent); err != nil {
				return err
			}
		}

		request := &runtimev0.TestRequest{
			Target:   sourceTarget,
			Filters:  append([]string(nil), sourceFilters...),
			Suite:    sourceSuite,
			Timeout:  sourceTimeout,
			Verbose:  sourceVerbose,
			Race:     sourceRace,
			Coverage: sourceCoverage,
		}
		flow, testErr := initSourceTest(ctx, prepared, request)
		if testErr == nil {
			testErr = common.WithHeartbeat(ctx, "running plugin-owned source tests", func() error {
				return testService(ctx, flow)
			})
		}
		var response *runtimev0.TestResponse
		if flow != nil {
			response = flow.OriginTestResponse()
		}
		if response != nil {
			fmt.Println(orchestration.RenderTestReport(response))
		}
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		stopErr := stopService(shutdownCtx, flow)
		cancel()
		if testErr != nil || (response != nil && !orchestration.TestSucceeded(response)) {
			if testErr != nil {
				return errors.Join(fmt.Errorf("source tests failed: %w", testErr), stopErr)
			}
			return errors.Join(fmt.Errorf("source tests failed"), stopErr)
		}
		if stopErr != nil {
			return fmt.Errorf("source tests passed but cleanup failed: %w", stopErr)
		}
		cli.Header(1, "Source tests passed through %s", prepared.Service.Agent.Identifier())
		return nil
	},
}

func prepareSourceWorkspace(ctx context.Context, dir, agentSpec string) (*sourceworkspace.Prepared, error) {
	if agentSpec == "" {
		return sourceworkspace.Prepare(ctx, dir)
	}
	if !strings.Contains(agentSpec, ":") {
		return nil, fmt.Errorf("source agent must include an exact version")
	}
	agent, err := resources.ParseAgent(ctx, resources.ServiceAgent, agentSpec)
	if err != nil {
		return nil, fmt.Errorf("invalid source agent: %w", err)
	}
	if agent.Version == "latest" {
		return nil, fmt.Errorf("source agent must use an exact version, not latest")
	}
	if _, err := semver.Parse(strings.TrimPrefix(agent.Version, "v")); err != nil || strings.HasPrefix(agent.Version, "v") {
		return nil, fmt.Errorf("source agent version %q is not canonical semantic version", agent.Version)
	}
	return sourceworkspace.PrepareWithAgent(ctx, dir, agent)
}

func verifySourceCapabilityHandshake(ctx context.Context, agent *resources.Agent) error {
	connection, err := manager.Load(ctx, agent, manager.WithoutSandbox(), manager.WithoutPrincipal())
	if err != nil {
		return fmt.Errorf("load exact source agent %s: %w", agent.Identifier(), err)
	}
	defer connection.Close()
	return verifySourceCapabilityClients(ctx, agent, connection.GRPCConn())
}

func verifySourceCapabilityClients(ctx context.Context, agent *resources.Agent, connection grpc.ClientConnInterface) error {
	info, err := agentv0.NewAgentClient(connection).GetAgentInformation(ctx, &agentv0.AgentInformationRequest{})
	if err != nil {
		return fmt.Errorf("source agent handshake: %w", err)
	}
	runtimeAdvertised := false
	for _, capability := range info.GetCapabilities() {
		if capability.GetType() == agentv0.Capability_RUNTIME {
			runtimeAdvertised = true
			break
		}
	}
	if !runtimeAdvertised {
		return fmt.Errorf("source agent %s does not advertise Runtime capability", agent.Identifier())
	}

	codeClient := codev0.NewCodeClient(connection)
	if _, err := codeClient.Execute(ctx, &codev0.CodeRequest{
		Operation: &codev0.CodeRequest_GetProjectInfo{GetProjectInfo: &codev0.GetProjectInfoRequest{}},
	}); err != nil {
		return fmt.Errorf("source agent %s Code capability handshake: %w", agent.Identifier(), err)
	}
	toolingClient := toolingv0.NewToolingClient(connection)
	if _, err := toolingClient.GetProjectInfo(ctx, &toolingv0.GetProjectInfoRequest{}); err != nil {
		return fmt.Errorf("source agent %s Tooling capability handshake: %w", agent.Identifier(), err)
	}
	return nil
}

func initSourceTest(ctx context.Context, prepared *sourceworkspace.Prepared, request *runtimev0.TestRequest) (*orchestration.Flow, error) {
	if err := resources.ValidateRuntimeContext(sourceRuntimeContext); err != nil {
		return nil, fmt.Errorf("invalid runtime context: %w", err)
	}
	env, err := orchestration.SelectEnvironment(prepared.Workspace, orchestration.LocalEnvironmentName)
	if err != nil {
		return nil, err
	}
	flow, err := orchestration.NewFlow(ctx, prepared.Workspace, prepared.Module, prepared.Service, env, orchestration.TestMode)
	if err != nil {
		return nil, err
	}
	flow.WithOutputSink(cli.NewOutputSink())
	flow.WithRuntimeContext(sourceRuntimeContext)
	flow.WithTestRequest(request)
	if err := flow.InitManagers(ctx); err != nil {
		return flow, err
	}
	if err := flow.Load(ctx); err != nil {
		return flow, err
	}
	return flow, nil
}

func init() {
	SourceCmd.Flags().StringVar(&sourceDir, "dir", "", "Source checkout (default: current directory)")
	SourceCmd.Flags().StringVar(&sourceRuntimeContext, "runtime-context", "free", "Runtime context for validation")
	SourceCmd.Flags().StringVar(&sourceTarget, "target", "", "Package/directory scope")
	SourceCmd.Flags().StringSliceVarP(&sourceFilters, "filter", "k", nil, "Test-name filter (repeatable)")
	SourceCmd.Flags().StringVar(&sourceSuite, "suite", "", "Named test suite")
	SourceCmd.Flags().StringVar(&sourceTimeout, "timeout", "", "Test timeout")
	SourceCmd.Flags().BoolVarP(&sourceVerbose, "verbose", "v", false, "Verbose test output")
	SourceCmd.Flags().BoolVar(&sourceRace, "race", false, "Enable plugin-defined race checking")
	SourceCmd.Flags().BoolVar(&sourceCoverage, "coverage", false, "Enable plugin-defined coverage")
	SourceCmd.Flags().StringVar(&sourceAgent, "agent", "", "Use an exact publisher/name:version instead of the compatibility pin")
	SourceCmd.Flags().BoolVar(&sourceQualification, "qualification", false, "Assert the exact agent's Runtime, Code, and Tooling handshake")
}
