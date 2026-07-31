package provider

import (
	"context"
	"errors"
	"fmt"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/orchestration"
	hostprovider "github.com/codefly-dev/cli/pkg/provider"
	"github.com/codefly-dev/core/provider/configuration"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

func errMutuallyExclusive(a, b string) error {
	return fmt.Errorf("%s and %s are mutually exclusive", a, b)
}

func errRequiredFlag(flag string) error {
	return fmt.Errorf("%s is required", flag)
}

func errImportIdentity() error {
	return errors.New("import requires an exact resource TYPE and REMOTE_ID")
}

// Shared flags across the group. Each subcommand wires the ones it accepts.
var (
	envFlag  string
	jsonFlag bool
)

// registry holds the declared configuration output contracts. It is stateless
// and safe to share across commands.
var registry = configuration.NewRegistry()

// session is the loaded workspace context a provider command operates on.
type session struct {
	ctx      context.Context
	env      *resources.Environment
	document *hostprovider.Document
	result   *hostprovider.Result
	failed   bool
}

// loadSession resolves the workspace, selects the environment, and loads the
// bindings document. A missing workspace or undeclared environment is a fatal
// usage error. A malformed or unknown-schema document is not fatal: it is
// recorded on the result, which the caller emits.
func loadSession(ctx context.Context, command, envName string) (*session, error) {
	ws, err := common.LoadWorkspace(ctx)
	if err != nil {
		return nil, err
	}
	env, err := orchestration.SelectEnvironment(ws, envName)
	if err != nil {
		return nil, err
	}

	s := &session{ctx: ctx, env: env, result: hostprovider.NewResult(command, env.Name)}

	doc, _, err := hostprovider.LoadDocument(ws.Dir())
	if err != nil {
		code := hostprovider.CodeBindingsUnreadable
		if errors.Is(err, hostprovider.ErrUnknownSchema) {
			code = hostprovider.CodeBindingsSchemaUnknown
		}
		s.result.Fail(code, "%v", err)
		s.failed = true
		return s, nil
	}
	s.document = doc
	return s, nil
}

// binding resolves a named binding within the session's environment, failing
// the result with binding_not_found when it is absent.
func (s *session) binding(name string) (*hostprovider.Binding, bool) {
	binding, ok := s.document.Binding(s.env.Name, name)
	if !ok {
		s.result.Fail(hostprovider.CodeBindingNotFound, "no binding %q declared for environment %q", name, s.env.Name)
		s.failed = true
	}
	return binding, ok
}

// validate runs offline validation of a binding, appending every finding to the
// session result and returning whether the binding is valid.
func (s *session) validate(binding *hostprovider.Binding) bool {
	diagnostics := hostprovider.ValidateBinding(s.ctx, binding, registry)
	s.result.Diagnostics = append(s.result.Diagnostics, diagnostics...)
	if len(diagnostics) > 0 {
		s.result.Status = hostprovider.StatusInvalid
		return false
	}
	return true
}

// gate records the terminal outcome for a coordinator-dependent operation. It
// validates the binding offline, appends its redacted view, and then fails
// closed: a disabled binding never starts an agent, and every live operation
// requires the host coordinator, which is not wired in this build.
func (s *session) gate(binding *hostprovider.Binding, command string) error {
	valid := s.validate(binding)
	s.result.Bindings = append(s.result.Bindings, binding.View(valid))
	switch {
	case !valid:
		// validate already classified the result as invalid.
	case binding.Mode == hostprovider.ModeDisabled:
		s.result.Fail(hostprovider.CodeInvalidMode, "binding %q is disabled; no agent is started for it", binding.Name)
	default:
		s.result.Fail(hostprovider.CodeCoordinatorUnavailable, "the host coordinator required to %s binding %q is not available in this build", command, binding.Name)
	}
	return s.result.Emit(jsonFlag)
}

// gatedCommand is the full flow for a coordinator-dependent command that
// operates on a single named binding: load, resolve, validate, and fail closed.
func gatedCommand(ctx context.Context, command, envName, bindingName string) error {
	s, err := loadSession(ctx, command, envName)
	if err != nil {
		return err
	}
	if s.failed {
		return s.result.Emit(jsonFlag)
	}
	binding, ok := s.binding(bindingName)
	if !ok {
		return s.result.Emit(jsonFlag)
	}
	return s.gate(binding, command)
}

// run is the standard RunE preamble: open a wool context, run the update check,
// and delegate to the command body.
func run(body func(ctx context.Context, args []string) error) func(*cobra.Command, []string) error {
	return func(_ *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		cli.Init()
		return body(ctx, args)
	}
}
