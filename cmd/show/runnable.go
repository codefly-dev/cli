package show

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

var showRunnableJSON bool

// RunnableCmd reports what a runnable declares and whether the workspace can
// satisfy it. Loading is core's strict loader, so an invalid declaration is
// reported as the load error it is rather than a partially rendered resource.
var RunnableCmd = &cobra.Command{
	Use:   "runnable <name>",
	Short: "Show a runnable's contract, execution bounds and dependency resolution",
	Long: `Show one runnable's declaration: its immutable identity, the agent that
builds it, its typed contract, its execution bounds, and whether each declared
dependency resolves in this workspace.

The name is module/name, or a bare name when it is unambiguous across modules.

An unresolved dependency is reported, not fatal: the command exits 0 so it can
describe every dependency in one pass. Unattended callers gate on --json and
check each dependency's "resolved" field.

Examples:
  codefly show runnable word-count
  codefly show runnable backend/word-count
  codefly show runnable word-count --json`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return showRunnable(cmd, args[0])
	},
}

type runnableFieldReport struct {
	Name     string                `json:"name,omitempty"`
	Type     string                `json:"type"`
	Optional bool                  `json:"optional,omitempty"`
	Nullable bool                  `json:"nullable,omitempty"`
	Fields   []runnableFieldReport `json:"fields,omitempty"`
	Items    *runnableFieldReport  `json:"items,omitempty"`
}

type runnableDependencyReport struct {
	Module    string   `json:"module"`
	Service   string   `json:"service"`
	Kind      string   `json:"kind"`
	Endpoints []string `json:"endpoints,omitempty"`
	Resolved  bool     `json:"resolved"`
	Problem   string   `json:"problem,omitempty"`
}

type runnableReport struct {
	Workspace      string                     `json:"workspace"`
	Module         string                     `json:"module"`
	Name           string                     `json:"name"`
	Version        string                     `json:"version"`
	Description    string                     `json:"description,omitempty"`
	Agent          string                     `json:"agent"`
	Protocol       string                     `json:"protocol"`
	Handler        string                     `json:"handler"`
	BuildInputs    []string                   `json:"build_inputs,omitempty"`
	Input          []runnableFieldReport      `json:"input"`
	Output         []runnableFieldReport      `json:"output"`
	Facilities     []string                   `json:"facilities"`
	Timeout        string                     `json:"timeout"`
	Cancellation   string                     `json:"cancellation"`
	Recovery       string                     `json:"recovery"`
	Concurrency    uint32                     `json:"concurrency,omitempty"`
	MaxInputBytes  uint64                     `json:"max_input_bytes"`
	MaxOutputBytes uint64                     `json:"max_output_bytes"`
	Dependencies   []runnableDependencyReport `json:"dependencies,omitempty"`
	Configurations []string                   `json:"workspace_configurations,omitempty"`
}

func showRunnable(cmd *cobra.Command, name string) error {
	ctx, done := common.NewContext()
	defer done()

	workspace, err := common.LoadWorkspace(ctx)
	if err != nil {
		return fmt.Errorf("cannot load workspace: %w", err)
	}

	runnable, err := workspace.FindRunnableByName(ctx, name)
	if err != nil {
		return fmt.Errorf("cannot load runnable %s: %w", name, err)
	}

	report := buildRunnableReport(ctx, workspace, runnable)

	if showRunnableJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(report)
	}
	printRunnableReport(cmd, &report)
	return nil
}

func buildRunnableReport(ctx context.Context, workspace *resources.Workspace, runnable *resources.Runnable) runnableReport {
	identity := runnable.Identity()
	execution := runnable.Execution
	report := runnableReport{
		Workspace:      workspace.Name,
		Module:         identity.Module,
		Name:           identity.Name,
		Version:        identity.Version,
		Description:    runnable.Description,
		Agent:          runnable.Agent.Identifier(),
		Protocol:       runnable.Contract.Protocol,
		Handler:        runnable.Entrypoint.Handler,
		BuildInputs:    runnable.Entrypoint.Inputs,
		Input:          runnableFieldReports(runnable.Contract.Input.Fields),
		Output:         runnableFieldReports(runnable.Contract.Output.Fields),
		Timeout:        execution.Timeout,
		Cancellation:   string(execution.Cancellation),
		Recovery:       string(execution.Recovery),
		Concurrency:    execution.Concurrency,
		MaxInputBytes:  execution.MaxInputBytes(),
		MaxOutputBytes: execution.MaxOutputBytes(),
		Configurations: runnable.WorkspaceConfigurationDependencies,
	}
	for _, facility := range execution.Facilities {
		report.Facilities = append(report.Facilities, string(facility))
	}
	for _, dep := range runnable.ServiceDependencies {
		report.Dependencies = append(report.Dependencies, resolveRunnableDependency(ctx, workspace, identity.Module, dep))
	}
	return report
}

// resolveRunnableDependency reports whether the workspace declares the service
// a runnable consumes and the endpoints it selects. It answers what the
// workspace declares; reachability and credential resolution belong to
// whoever installs and launches a binding.
func resolveRunnableDependency(ctx context.Context, workspace *resources.Workspace, ownModule string, dep *resources.ServiceDependency) runnableDependencyReport {
	module := dep.Module
	if module == "" {
		module = ownModule
	}
	report := runnableDependencyReport{Module: module, Service: dep.Name, Kind: string(dep.Kind)}
	if report.Kind == "" {
		report.Kind = "legacy"
	}
	for _, endpoint := range dep.Endpoints {
		report.Endpoints = append(report.Endpoints, endpoint.Name)
	}

	service, err := workspace.LoadService(ctx, &resources.ServiceWithModule{Name: dep.Name, Module: module})
	if err != nil {
		// The service being absent is only one of the reasons this fails: a
		// corrupt module file, a malformed declaration and an unreadable one
		// reach here too, and naming the wrong cause sends the reader looking
		// for a service that is sitting right there.
		report.Problem = fmt.Sprintf("cannot load service %s/%s: %v", module, dep.Name, err)
		return report
	}

	declared := make(map[string]bool, len(service.Endpoints))
	for _, endpoint := range service.Endpoints {
		declared[endpoint.Name] = true
	}
	var missing []string
	for _, endpoint := range dep.Endpoints {
		if !declared[endpoint.Name] {
			missing = append(missing, endpoint.Name)
		}
	}
	if len(missing) > 0 {
		report.Problem = fmt.Sprintf("service %s/%s declares no endpoint %s", module, dep.Name, strings.Join(missing, ", "))
		return report
	}
	// A runtime edge always consumes a reachable endpoint: core's binding
	// validation requires at least one mapping for it whether or not the
	// selection names endpoints, and an empty selection resolves to every
	// endpoint the service exports. A service exporting none can never
	// satisfy one, so reporting it resolved promises a binding core refuses.
	if dep.Kind == resources.DependencyKindRuntime && len(dep.Endpoints) == 0 && len(service.Endpoints) == 0 {
		report.Problem = fmt.Sprintf("service %s/%s exports no endpoint: a runtime dependency selecting none resolves to all of them", module, dep.Name)
		return report
	}
	report.Resolved = true
	return report
}

func runnableFieldReports(fields []*resources.RunnableField) []runnableFieldReport {
	reports := make([]runnableFieldReport, 0, len(fields))
	for _, field := range fields {
		reports = append(reports, runnableFieldReport{
			Name:     field.Name,
			Type:     string(field.Type),
			Optional: field.Optional,
			Nullable: field.Nullable,
			Fields:   runnableFieldReports(field.Fields),
			Items:    runnableItemReport(field.Items),
		})
	}
	return reports
}

func runnableItemReport(item *resources.RunnableField) *runnableFieldReport {
	if item == nil {
		return nil
	}
	reports := runnableFieldReports([]*resources.RunnableField{item})
	return &reports[0]
}

func printRunnableReport(cmd *cobra.Command, report *runnableReport) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Runnable:   %s @ %s\n", report.Name, report.Version)
	if report.Description != "" {
		fmt.Fprintf(out, "  %s\n", report.Description)
	}
	fmt.Fprintf(out, "Workspace:  %s\n", report.Workspace)
	fmt.Fprintf(out, "Module:     %s\n", report.Module)
	fmt.Fprintf(out, "Agent:      %s\n", report.Agent)
	fmt.Fprintf(out, "Protocol:   %s\n", report.Protocol)
	fmt.Fprintf(out, "Handler:    %s\n", report.Handler)
	if len(report.BuildInputs) > 0 {
		fmt.Fprintf(out, "Inputs:     %s\n", strings.Join(report.BuildInputs, ", "))
	}

	fmt.Fprintln(out, "Contract:")
	fmt.Fprintln(out, "  input:")
	printRunnableSchema(cmd, report.Input, "    ")
	fmt.Fprintln(out, "  output:")
	printRunnableSchema(cmd, report.Output, "    ")

	fmt.Fprintln(out, "Execution:")
	fmt.Fprintf(out, "  facilities:   %s\n", strings.Join(report.Facilities, ", "))
	fmt.Fprintf(out, "  timeout:      %s\n", report.Timeout)
	fmt.Fprintf(out, "  cancellation: %s\n", report.Cancellation)
	fmt.Fprintf(out, "  recovery:     %s\n", report.Recovery)
	fmt.Fprintf(out, "  payload:      %d bytes in / %d bytes out\n", report.MaxInputBytes, report.MaxOutputBytes)

	if len(report.Dependencies) > 0 {
		fmt.Fprintln(out, "Dependencies:")
		for _, dep := range report.Dependencies {
			status := "resolved"
			if !dep.Resolved {
				status = "UNRESOLVED: " + dep.Problem
			}
			endpoints := "all endpoints"
			if len(dep.Endpoints) > 0 {
				endpoints = strings.Join(dep.Endpoints, ", ")
			}
			fmt.Fprintf(out, "  %s/%s [%s] %s — %s\n", dep.Module, dep.Service, dep.Kind, endpoints, status)
		}
	}
	if len(report.Configurations) > 0 {
		fmt.Fprintf(out, "Configurations: %s\n", strings.Join(report.Configurations, ", "))
	}
}

// printRunnableSchema renders one side of the contract. An explicitly empty
// object schema is a valid contract, so it is named rather than left blank.
func printRunnableSchema(cmd *cobra.Command, fields []runnableFieldReport, indent string) {
	if len(fields) == 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "%s(empty)\n", indent)
		return
	}
	printRunnableFields(cmd, fields, indent)
}

func printRunnableFields(cmd *cobra.Command, fields []runnableFieldReport, indent string) {
	for _, field := range fields {
		printRunnableField(cmd, field.Name, field, indent)
	}
}

// printRunnableField renders one node of a schema. An array's element is a
// field in its own right and may itself be an array or an object, so it
// recurses through Items as well as Fields; printing only the element's type
// truncates array<array<T>> to array<array>.
func printRunnableField(cmd *cobra.Command, label string, field runnableFieldReport, indent string) {
	modifiers := ""
	switch {
	case field.Optional && field.Nullable:
		modifiers = " (optional, nullable)"
	case field.Optional:
		modifiers = " (optional)"
	case field.Nullable:
		modifiers = " (nullable)"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s%s: %s%s\n", indent, label, field.Type, modifiers)
	printRunnableFields(cmd, field.Fields, indent+"  ")
	if field.Items != nil {
		printRunnableField(cmd, "items", *field.Items, indent+"  ")
	}
}

func init() {
	RunnableCmd.Flags().BoolVar(&showRunnableJSON, "json", false, "Emit machine-readable JSON")
}
