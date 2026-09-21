package composition

import (
	"errors"
	"os"

	selection "github.com/codefly-dev/cli/pkg/composition"
	"github.com/codefly-dev/core/agents/manager"
	"github.com/codefly-dev/core/runners/sandbox"
)

func newRenderSession(workspace, product, path, keyPath string) (*selection.SelectionSession, error) {
	requests, key, err := readRenderConfiguration(path, keyPath)
	if err != nil {
		return nil, err
	}
	identity, err := selection.RenderConfigurationIdentity(key, requests)
	if err != nil {
		return nil, err
	}
	return selection.NewSelectionSession(workspace, product, identity)
}

func readRenderConfiguration(path, keyPath string) ([]selection.RenderInput, []byte, error) {
	if path == "" || keyPath == "" {
		return nil, nil, errors.New("--render-requests and --identity-key are required for selected-executor staging")
	}
	var requests []selection.RenderInput
	if err := readJSON(path, &requests); err != nil {
		return nil, nil, errors.New("invalid render requests JSON; payload values withheld")
	}
	key, err := os.ReadFile(keyPath)
	return requests, key, err
}

type stageFlags struct {
	outputParent, sandbox          string
	allowNetwork, withoutPrincipal bool
}

const noSandbox = "none"

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
