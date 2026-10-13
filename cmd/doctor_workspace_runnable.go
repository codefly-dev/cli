package cmd

import (
	"fmt"

	"github.com/codefly-dev/cli/cmd/generate"
	"github.com/codefly-dev/cli/pkg/environments"
)

// codeRunnableBindingDeclined reports a binding key the environment declines
// with an empty runnable-scope-selections list: `codefly generate
// runnable-bindings` prepares no binding for that operation and grants it no
// authority.
const codeRunnableBindingDeclined = "runnable_binding_declined"

// checkRunnableBindingDeclines names every binding the environment declines.
// A decline is deliberate, so it is a warning and never a failure; it is
// reported because the service that would install the operation is refused at
// its own point, for a key that is simply absent, and this is where the
// operator finds out why.
func checkRunnableBindingDeclines(env *environments.Environment, report *workspaceReadinessReport) {
	for _, key := range env.DeclinedRunnableBindings() {
		report.add(codeRunnableBindingDeclined, "runnable binding "+key, "warn",
			fmt.Sprintf("environment %s declines %s (runnable-scope-selections: []): no binding is prepared and no authority granted, so a service that installs it is refused where it reads %s/%s",
				env.Name, key, generate.RunnableBindingsGroup, key),
			fmt.Sprintf("to install it, answer its required scope slots with exact resources under runnable-scope-selections[%s] and run `codefly generate runnable-bindings --env %s`", key, env.Name))
	}
}
