package generate

import (
	"context"
	"errors"
	"fmt"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/environments"
	runnablev0 "github.com/codefly-dev/core/generated/go/codefly/runnable/v0"
	"github.com/codefly-dev/core/resources"
	corerunnable "github.com/codefly-dev/core/runnable"
)

// An authored runnable — a handler someone wrote, served by its generated
// harness — is bound exactly as a derived operation is: under
// <MODULE>__<NAME> in the runnable-bindings group, as a
// codefly.runnable-prepared/v3 value, so whatever installs derived operations
// (robin's runnable tools, for one) installs it unchanged. It is bound when
// its declaration carries an operation block (corerunnable.AuthoredOperation);
// its tool exposure, when declared, travels on the policy.
//
// Its address is the harness's local one (common.ResolveRunnableHarness),
// which `codefly run runnable` serves. A harness has no cluster placement
// yet — the per-call invocation Job is module-runtime's — so an authored
// operation is prepared for a local environment only, and a non-local one is
// refused by name rather than given an address nothing serves.

// authoredOperation is one packaged runnable a module offers as an operation.
type authoredOperation struct {
	module   *resources.Module
	runnable *resources.Runnable
	spec     *corerunnable.OperationSpec
}

func (o authoredOperation) key() string {
	return RunnableBindingKey(o.module.Name, o.runnable.Name)
}

// loadAuthoredOperations reads every runnable of module that declares an
// operation block. A runnable without one is not an operation; one whose block
// does not validate fails the read, naming it.
func loadAuthoredOperations(ctx context.Context, module *resources.Module) ([]authoredOperation, error) {
	runnables, err := module.LoadRunnables(ctx)
	if err != nil {
		return nil, err
	}
	var operations []authoredOperation
	for _, r := range runnables {
		spec, err := corerunnable.AuthoredOperation(r)
		if errors.Is(err, corerunnable.ErrNotAnOperation) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("runnable %s/%s: %w", module.Name, r.Name, err)
		}
		operations = append(operations, authoredOperation{module: module, runnable: r, spec: spec})
	}
	return operations, nil
}

// declinableAuthored is declinableOperation's rule for an authored operation:
// only one that declares a required scope slot can be declined.
func declinableAuthored(env *environments.Environment, operation authoredOperation) error {
	if len(operation.spec.RequiredScopeSlots) == 0 {
		return fmt.Errorf("environment %s runnable-scope-selections[%s] in workspace.codefly.yaml is [], which declines the binding, but runnable %s/%s declares no required scope slot: there is nothing to decline; remove the entry",
			env.Name, operation.key(), operation.module.Name, operation.runnable.Name)
	}
	return nil
}

// prepareAuthored prepares one authored operation's binding for the
// environment the resolver answers for: its harness's address (or the
// declared call endpoint in front of it), its contract, and its policy with
// the environment's scope selections resolved.
func prepareAuthored(ctx context.Context, operation authoredOperation, resolver *endpointResolver) (*runnablev0.PreparedBinding, error) {
	if !resolver.env.Local() {
		return nil, fmt.Errorf("runnable %s/%s is served by its generated harness, which is placed only in a local environment (`codefly run runnable`); environment %s is not local",
			operation.module.Name, operation.runnable.Name, resolver.env.Name)
	}
	harness, err := common.ResolveRunnableHarness(ctx, resolver.workspace.Name, operation.module.Name, operation.runnable.Name, resolver.env.NamingScope)
	if err != nil {
		return nil, err
	}
	address := harness.Address
	if resolver.through != "" {
		address = resolver.through
	}
	resolved, err := operation.spec.ResolveScopeSlots(resolver.env.RunnableScopeSelections[operation.key()])
	if err != nil {
		return nil, fmt.Errorf("environment %s runnable-scope-selections[%s] in workspace.codefly.yaml: %w", resolver.env.Name, operation.key(), err)
	}
	return corerunnable.PrepareGeneratedService(operation.runnable, operation.module.Name, address, resolved.Policy())
}

// PrepareAuthoredBinding is the binding `generate runnable-bindings` writes
// for one authored runnable in env, encoded as the configuration value: what
// `codefly run runnable` shows for the harness it serves.
func PrepareAuthoredBinding(ctx context.Context, workspace *resources.Workspace, env *environments.Environment, module *resources.Module, r *resources.Runnable) (string, string, error) {
	operations, err := loadAuthoredOperations(ctx, module)
	if err != nil {
		return "", "", err
	}
	for _, operation := range operations {
		if operation.runnable.Name != r.Name {
			continue
		}
		resolver, err := newEndpointResolver(ctx, workspace, env)
		if err != nil {
			return "", "", err
		}
		if env.RunnableCallEndpoint != "" {
			if err = resolver.callThrough(ctx, env.RunnableCallEndpoint); err != nil {
				return "", "", err
			}
		}
		prepared, err := prepareAuthored(ctx, operation, resolver)
		if err != nil {
			return "", "", err
		}
		value, err := corerunnable.EncodePrepared(prepared)
		if err != nil {
			return "", "", err
		}
		return operation.key(), string(value), nil
	}
	return "", "", fmt.Errorf("runnable %s/%s declares no operation block, so it is bound to no caller: %w", module.Name, r.Name, corerunnable.ErrNotAnOperation)
}
