package composition

import (
	"errors"

	selection "github.com/codefly-dev/cli/pkg/composition"
	"github.com/codefly-dev/core/agents/manager"
	"github.com/codefly-dev/core/runners/sandbox"
	"github.com/spf13/cobra"
)

func newRenderSession(workspace, product, path, keyPath, buildPath string) (*selection.SelectionSession, error) {
	requests, key, err := readRenderConfiguration(path, keyPath)
	if err != nil {
		return nil, err
	}
	builds, err := readBuildConfiguration(buildPath)
	if err != nil {
		return nil, err
	}
	identity, err := selection.ExecutionConfigurationIdentity(key, requests, builds)
	if err != nil {
		return nil, err
	}
	return selection.NewSelectionSession(workspace, product, identity)
}

func readBuildConfiguration(path string) ([]selection.BuildInput, error) {
	if path == "" {
		return nil, nil
	}
	var requests []selection.BuildInput
	if err := readJSON(path, &requests); err != nil || len(requests) == 0 {
		return nil, errors.New("invalid or empty build requests JSON; payload values withheld")
	}
	return requests, nil
}

func readStageOptions(renderPath, buildPath, keyPath string, flags stageFlags) (*selection.StageOptions, error) {
	if err := flags.validate(); err != nil {
		return nil, err
	}
	requests, key, err := readRenderConfiguration(renderPath, keyPath)
	if err != nil {
		return nil, err
	}
	builds, err := readBuildConfiguration(buildPath)
	if err != nil {
		return nil, err
	}
	return &selection.StageOptions{OutputParent: flags.outputParent, Requests: requests, BuildRequests: builds, IdentityKey: key, LoadOptions: flags.loadOptions}, nil
}

func runStageRender(cmd *cobra.Command, session *selection.SelectionSession, inputsPath, renderPath, buildPath, keyPath string, flags stageFlags) (any, error) {
	options, err := readStageOptions(renderPath, buildPath, keyPath, flags)
	if err != nil {
		return nil, err
	}
	var inputs selection.DeploymentFiles
	if err = readJSON(inputsPath, &inputs); err != nil {
		return nil, err
	}
	return session.StageRender(cmd.Context(), &inputs, options)
}

func runStageBuild(cmd *cobra.Command, session *selection.SelectionSession, renderPath, buildPath, keyPath string, flags stageFlags) (any, error) {
	options, err := readStageOptions(renderPath, buildPath, keyPath, flags)
	if err != nil {
		return nil, err
	}
	return session.StageBuild(cmd.Context(), options)
}

func readRenderConfiguration(path, keyPath string) ([]selection.RenderInput, []byte, error) {
	if path == "" || keyPath == "" {
		return nil, nil, errors.New("--render-requests and --identity-key are required for selected-executor staging")
	}
	var requests []selection.RenderInput
	if err := readJSON(path, &requests); err != nil {
		return nil, nil, errors.New("invalid render requests JSON; payload values withheld")
	}
	key, err := readCommandFile(keyPath)
	return requests, key, err
}

type stageFlags struct {
	outputParent, sandbox          string
	allowNetwork, withoutPrincipal bool
}

const noSandbox = "none"

func addStageFlags(command *cobra.Command, flags *stageFlags) {
	command.Flags().StringVar(&flags.outputParent, "output-parent", "", "Existing canonical absolute staging parent outside product and local checkouts")
	command.Flags().StringVar(&flags.sandbox, "sandbox", "required", "Executor sandbox: required or none (explicit unrestricted execution)")
	command.Flags().BoolVar(&flags.allowNetwork, "allow-network", false, "Allow executor network access in the sandbox")
	command.Flags().BoolVar(&flags.withoutPrincipal, "without-principal", false, "Explicit local execution without an authenticated principal; never deployment authorization")
}

func (flags stageFlags) validate() error {
	if flags.outputParent == "" {
		return errors.New("--output-parent is required")
	}
	if !flags.withoutPrincipal {
		return errors.New("local staging requires explicit --without-principal; it does not authorize deployment")
	}
	if flags.sandbox != "required" && flags.sandbox != noSandbox {
		return errors.New("--sandbox must be required or none")
	}
	if flags.sandbox == noSandbox && flags.allowNetwork {
		return errors.New("--allow-network applies only to a required sandbox")
	}
	return nil
}

func (flags stageFlags) loadOptions(directory string) ([]manager.LoadOption, error) {
	if err := flags.validate(); err != nil {
		return nil, err
	}
	opts := []manager.LoadOption{manager.WithoutPrincipal(), manager.WithUDS(), manager.WithWorkDir(directory)}
	if flags.sandbox == noSandbox {
		return append(opts, manager.WithoutSandbox()), nil
	}
	sb, err := sandbox.New()
	if err != nil {
		return nil, err
	}
	if sb.Backend() == sandbox.BackendNative {
		return nil, errors.New("required executor sandbox is unavailable on this platform")
	}
	network := sandbox.NetworkDeny
	if flags.allowNetwork {
		network = sandbox.NetworkOpen
	}
	sb.WithWritePaths(directory).WithNetwork(network)
	return append(opts, manager.WithSandbox(sb)), nil
}
