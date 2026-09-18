package show

import (
	"context"
	"fmt"
	"strings"

	runnablespkg "github.com/codefly-dev/cli/pkg/runnables"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

// derivedRunnableReport is what a derived operation answers with. It shares
// Identity with the declared report so the fields both surfaces have keep one
// spelling, and adds the facts only a derived operation has: the method it
// adapts, the published messages it reuses, and the policy and authority
// installed beside its package.
//
// It deliberately has no handler, build inputs or dependency report: a derived
// operation is reached on a service the workspace already runs, so there is no
// artifact to build and nothing for this command to resolve.
type derivedRunnableReport struct {
	Workspace string `json:"workspace"`
	runnablespkg.Identity
	Digest        string                  `json:"digest"`
	Service       string                  `json:"service"`
	Endpoint      string                  `json:"endpoint"`
	Method        string                  `json:"method"`
	InputMessage  string                  `json:"input_message"`
	OutputMessage string                  `json:"output_message"`
	Input         []runnableFieldReport   `json:"input"`
	Output        []runnableFieldReport   `json:"output"`
	Operation     *runnablespkg.Operation `json:"operation"`
}

// findDerivedOperations returns every derived operation a name selects. A name
// declared at several versions selects several, exactly as a declared runnable
// does, so --version picks one.
func findDerivedOperations(ctx context.Context, workspace *resources.Workspace, name, version string) ([]runnablespkg.Derived, error) {
	moduleName, operationName := resources.SplitUnique(name)
	var modules []*resources.Module
	if moduleName != "" {
		module, err := workspace.LoadModuleFromName(ctx, moduleName)
		if err != nil {
			return nil, err
		}
		modules = []*resources.Module{module}
	} else {
		var err error
		if modules, err = workspace.LoadModules(ctx); err != nil {
			return nil, err
		}
	}

	var matches []runnablespkg.Derived
	for _, module := range modules {
		derived, err := runnablespkg.LoadDerivedOperations(module.Dir())
		if err != nil {
			return nil, fmt.Errorf("cannot load the derived runnables of module %s: %w", module.Name, err)
		}
		for i := range derived {
			operation := &derived[i]
			if operation.Entry.Name != operationName {
				continue
			}
			if version != "" && operation.Entry.Version != version {
				continue
			}
			matches = append(matches, *operation)
		}
	}
	return matches, nil
}

func buildDerivedReport(workspace *resources.Workspace, derived *runnablespkg.Derived) derivedRunnableReport {
	return derivedRunnableReport{
		Workspace:     workspace.Name,
		Identity:      derived.Identity(),
		Digest:        derived.Entry.Digest,
		Service:       derived.Entry.Service,
		Endpoint:      derived.Entry.Endpoint,
		Method:        derived.Entry.Method,
		InputMessage:  derived.Entry.InputMessage,
		OutputMessage: derived.Entry.OutputMessage,
		Input:         packageFieldReports(derived.Package.GetContract().GetInput().GetFields()),
		Output:        packageFieldReports(derived.Package.GetContract().GetOutput().GetFields()),
		Operation:     derived.Operation,
	}
}

func packageFieldReports(fields []*basev0.RunnableField) []runnableFieldReport {
	reports := make([]runnableFieldReport, 0, len(fields))
	for _, field := range fields {
		reports = append(reports, runnableFieldReport{
			Name:     field.GetName(),
			Type:     strings.ToLower(field.GetType().String()),
			Optional: field.GetOptional(),
			Nullable: field.GetNullable(),
			Fields:   packageFieldReports(field.GetFields()),
			Items:    packageItemReport(field.GetItems()),
		})
	}
	return reports
}

func packageItemReport(item *basev0.RunnableField) *runnableFieldReport {
	if item == nil {
		return nil
	}
	reports := packageFieldReports([]*basev0.RunnableField{item})
	return &reports[0]
}

func printDerivedReport(cmd *cobra.Command, report *derivedRunnableReport) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Runnable:   %s @ %s (derived)\n", report.Name, report.Version)
	fmt.Fprintf(out, "Workspace:  %s\n", report.Workspace)
	fmt.Fprintf(out, "Module:     %s\n", report.Module)
	fmt.Fprintf(out, "Agent:      %s\n", report.Agent)
	fmt.Fprintf(out, "Protocol:   %s\n", report.Protocol)
	fmt.Fprintf(out, "Source:     %s\n", report.Source)
	fmt.Fprintf(out, "Method:     %s\n", report.Method)
	fmt.Fprintf(out, "Messages:   %s -> %s\n", report.InputMessage, report.OutputMessage)
	fmt.Fprintf(out, "Digest:     %s\n", report.Digest)

	fmt.Fprintln(out, "Contract:")
	fmt.Fprintln(out, "  input:")
	printRunnableSchema(cmd, report.Input, "    ")
	fmt.Fprintln(out, "  output:")
	printRunnableSchema(cmd, report.Output, "    ")

	fmt.Fprintln(out, "Execution:")
	fmt.Fprintf(out, "  facilities:   %s\n", strings.Join(report.Execution.Facilities, ", "))
	fmt.Fprintf(out, "  timeout:      %s\n", report.Execution.Timeout)
	fmt.Fprintf(out, "  cancellation: %s\n", report.Execution.Cancellation)
	fmt.Fprintf(out, "  recovery:     %s\n", report.Execution.Recovery)
	fmt.Fprintf(out, "  payload:      %d bytes in / %d bytes out\n", report.Execution.MaxInputBytes, report.Execution.MaxOutputBytes)

	operation := report.Operation
	fmt.Fprintln(out, "Policy:")
	fmt.Fprintf(out, "  attempts:     %d, %s per attempt, %s in total, %s backoff\n",
		operation.MaxAttempts, operation.AttemptTimeout, operation.TotalTimeout, operation.Backoff)
	if len(operation.RetryableCodes) > 0 {
		fmt.Fprintf(out, "  retry on:     %s\n", strings.Join(operation.RetryableCodes, ", "))
	}
	fmt.Fprintf(out, "  audience:     %s\n", operation.Audience)
	fmt.Fprintf(out, "  invoke:       %s\n", renderScopes(operation.InvokeScopes))
	fmt.Fprintf(out, "  lookup:       %s\n", renderScopes(operation.LookupScopes))
	if operation.LookupMethod != "" {
		fmt.Fprintf(out, "  receipt:      %s\n", operation.LookupMethod)
	}
}

func renderScopes(scopes []runnablespkg.Scope) string {
	if len(scopes) == 0 {
		return "(none)"
	}
	rendered := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		entry := scope.ResourceKind + ":" + strings.Join(scope.Actions, "+")
		if len(scope.ResourceIDs) > 0 {
			entry += " on " + strings.Join(scope.ResourceIDs, ", ")
		}
		rendered = append(rendered, entry)
	}
	return strings.Join(rendered, ", ")
}
