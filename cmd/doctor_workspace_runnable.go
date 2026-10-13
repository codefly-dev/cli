package cmd

import (
	"context"
	"fmt"

	"github.com/codefly-dev/cli/cmd/generate"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
)

// codeRunnableBindingDeclined reports a binding key the environment declines
// with an empty runnable-scope-selections list: `codefly generate
// runnable-bindings` prepares no binding for that operation and grants it no
// authority.
const codeRunnableBindingDeclined = "runnable_binding_declined"

// codeRunnableBindingDeclineInvalid reports a decline `codefly generate
// runnable-bindings` refuses: the key names no operation a module derives, or
// one that declares no required scope slot and so has nothing to decline.
const codeRunnableBindingDeclineInvalid = "runnable_binding_decline_invalid"

// codeRunnableOperationsUnreadable reports that the operations a declined
// binding would decline cannot be read — a module that does not load, or a
// derived-operation tree that does not parse — so no decline can be judged.
// `codefly generate runnable-bindings` fails on the same read.
const codeRunnableOperationsUnreadable = "runnable_operations_unreadable"

// checkRunnableBindingDeclines judges every binding the environment declines
// with the rule generation applies (generate.CheckRunnableBindingDeclines),
// so the doctor never calls a decline deliberate that generation refuses.
//
// A decline generation honours is a warning, never a failure; it is reported
// because the service that would install the operation is refused at its own
// point, for a key that is simply absent, and this is where the operator
// finds out why. A decline generation refuses fails, naming the refusal.
func checkRunnableBindingDeclines(ctx context.Context, ws *resources.Workspace, env *environments.Environment, report *workspaceReadinessReport) {
	declines, err := generate.CheckRunnableBindingDeclines(ctx, ws, env)
	if err != nil {
		report.add(codeRunnableOperationsUnreadable, "runnable binding declines", checkStatusFail,
			fmt.Sprintf("environment %s declines runnable bindings, but the operations they would decline cannot be read: %v", env.Name, err),
			"run `codefly generate contracts` and `codefly generate runnables` for the module named above")
		return
	}
	for _, decline := range declines {
		key := decline.Key
		if decline.Refusal != nil {
			report.add(codeRunnableBindingDeclineInvalid, "runnable binding "+key, checkStatusFail,
				decline.Refusal.Error(),
				fmt.Sprintf("remove runnable-scope-selections[%s], or correct the key to the binding of an operation with a required scope slot; `codefly generate runnable-bindings --env %s` refuses it as it stands", key, env.Name))
			continue
		}
		report.add(codeRunnableBindingDeclined, "runnable binding "+key, "warn",
			fmt.Sprintf("environment %s declines %s (runnable-scope-selections: []): no binding is prepared and no authority granted, so a service that installs it is refused where it reads %s/%s",
				env.Name, key, generate.RunnableBindingsGroup, key),
			fmt.Sprintf("to install it, answer its required scope slots with exact resources under runnable-scope-selections[%s] and run `codefly generate runnable-bindings --env %s`", key, env.Name))
	}
}
