package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/cmd/generate"
	"github.com/codefly-dev/cli/pkg/orchestration"
	runnableops "github.com/codefly-dev/cli/pkg/runnable"
	"github.com/codefly-dev/core/resources"
	corerunnable "github.com/codefly-dev/core/runnable"
	"github.com/spf13/cobra"
)

var (
	runRunnableEnv         string
	runRunnableNamingScope string
)

// RunnableCmd serves one authored runnable's generated harness locally, as a
// unit that cannot reach a network.
var RunnableCmd = &cobra.Command{
	Use:   "runnable <name>",
	Short: "Serve a Runnable's generated harness locally, with no network",
	Long: `Build the Runnable's harness into the Linux image its agent describes, and serve
it as one unit that cannot reach a network, until interrupted.

The harness runs on an internal network of its own with no IPv4 route, no name
resolution beyond its one peer and no way to the host; its root is read-only,
it runs unprivileged with every capability dropped, and its memory and process
count are bounded. Its one peer is a pinned relay listening on the host's
loopback at the address the composition resolves for the runnable — the same
address ` + "`codefly generate runnable-bindings`" + ` prepares calls to — so a caller that
installed the binding reaches it. The harness still requires a Work Context on
every call. A file field reaches it as staged bytes; it never sees a path or a
URL.

The Runnable must declare the generated-service facility. When it declares an
operation block, the command prints the binding key and the prepared value
generation writes for it, which carries its contract and its tool exposure.
The environment must be local: the harness has no cluster placement yet.

Examples:
  codefly run runnable pdf-extract-tables
  codefly run runnable workbench/pdf-extract-tables --naming-scope demo`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		ctx, stop := common.SignalContext(ctx)
		defer stop()
		return runRunnable(ctx, args[0], cmd.Flags().Changed("naming-scope"), cmd.OutOrStdout(), cmd.ErrOrStderr())
	},
}

func init() {
	RunnableCmd.Flags().StringVar(&runRunnableEnv, "env", "local", "local environment whose naming scope and runnable-call-endpoint the binding is prepared with")
	RunnableCmd.Flags().StringVar(&runRunnableNamingScope, "naming-scope", "", "`scope` the harness address resolves in, as codefly run reads it: omitted keeps the environment's, empty clears it")
}

func runRunnable(ctx context.Context, name string, explicitScope bool, out, logs io.Writer) error {
	workspace, err := common.LoadWorkspace(ctx)
	if err != nil {
		return err
	}
	r, err := workspace.FindRunnableByName(ctx, name)
	if err != nil {
		return err
	}
	if !slices.Contains(r.Execution.Facilities, resources.RunnableFacilityGenerated) {
		return fmt.Errorf("runnable %s/%s declares no %s facility, so no harness of its serves calls", r.Module(), r.Name, resources.RunnableFacilityGenerated)
	}
	env, err := orchestration.SelectEnvironment(workspace, runRunnableEnv)
	if err != nil {
		return err
	}
	if explicitScope {
		env.NamingScope = runRunnableNamingScope
	}
	if !env.Local() {
		return fmt.Errorf("environment %s is not local; a runnable's harness is served locally only", env.Name)
	}
	module, err := workspace.LoadModuleFromName(ctx, r.Module())
	if err != nil {
		return err
	}
	harness, err := common.ResolveRunnableHarness(ctx, workspace.Name, r.Module(), r.Name, env.NamingScope)
	if err != nil {
		return err
	}
	port, err := hostPort(harness.Address)
	if err != nil {
		return err
	}
	key, value, bindErr := generate.PrepareAuthoredBinding(ctx, workspace, env, module, r)
	if bindErr != nil && !errors.Is(bindErr, corerunnable.ErrNotAnOperation) {
		return bindErr
	}

	output, err := runnableops.DefaultOutput(workspace, r)
	if err != nil {
		return err
	}
	output = filepath.Join(output, "image")
	if err = runnableops.ResetOutput(workspace, output); err != nil {
		return err
	}
	image, err := runnableops.Image(ctx, workspace, r, output, logs)
	if err != nil {
		return err
	}
	unit, err := runnableops.Serve(ctx, runnableops.UnitName(workspace.Name, r, env.NamingScope), image, port, logs)
	if err != nil {
		return err
	}
	defer func() {
		_ = unit.Stop(context.WithoutCancel(ctx))
		_ = unit.Close()
	}()

	_, _ = fmt.Fprintf(out, "Serving runnable %s/%s@%s from %s\n", r.Module(), r.Name, r.Version, image)
	_, _ = fmt.Fprintf(out, "Harness:  %s%s (no network: internal IPv6-only network, read-only root, no capabilities)\n", unit.Address(), corerunnable.ServedInvokeProcedure)
	_, _ = fmt.Fprintf(out, "Unit:     %s\n", unit.Name)
	if bindErr == nil {
		_, _ = fmt.Fprintf(out, "Binding:  %s/%s, as `codefly generate runnable-bindings --env %s` writes it:\n%s=%s\n",
			generate.RunnableBindingsGroup, key, env.Name, key, value)
	} else {
		_, _ = fmt.Fprintf(out, "Binding:  none — the runnable declares no operation block, so no caller is bound to it\n")
	}
	_, _ = fmt.Fprintln(out, "Press Ctrl-C to stop.")
	<-ctx.Done()
	return nil
}

// hostPort reads the port of the resolved harness address.
func hostPort(address string) (int, error) {
	parsed, err := url.Parse(address)
	if err != nil {
		return 0, fmt.Errorf("harness address %q: %w", address, err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("harness address %q carries no port", address)
	}
	return port, nil
}
