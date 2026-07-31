package provider

import (
	"context"

	hostprovider "github.com/codefly-dev/cli/pkg/provider"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:          "list [BINDING]",
	Short:        "List provider bindings for an environment, or print the binding schema",
	SilenceUsage: true,
	Args:         cobra.MaximumNArgs(1),
	RunE: run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
		jsonOut := jsonOf(cmd)
		if schema, _ := cmd.Flags().GetBool("schema"); schema {
			return emitSchema(envOf(cmd), jsonOut)
		}

		s, err := loadSession(ctx, "list", envOf(cmd), jsonOut)
		if err != nil {
			return err
		}
		if s.failed {
			return s.emit()
		}

		if len(args) == 1 {
			binding, ok := s.binding(args[0])
			if !ok {
				return s.emit()
			}
			return listBindings(s, []*hostprovider.Binding{binding})
		}
		return listBindings(s, s.document.ForEnvironment(s.env.Name))
	}),
}

func listBindings(s *session, bindings []*hostprovider.Binding) error {
	for _, binding := range bindings {
		valid := s.validate(binding)
		s.result.Bindings = append(s.result.Bindings, binding.View(valid))
	}
	return s.emit()
}

func emitSchema(envName string, jsonOut bool) error {
	result := hostprovider.NewResult("list", envName)
	result.Schema = &hostprovider.SchemaDoc{
		Schema:           hostprovider.BindingsSchemaV0,
		Modes:            []string{string(hostprovider.ModeObserve), string(hostprovider.ModeManaged), string(hostprovider.ModeDisabled)},
		DeletionPolicies: []string{string(hostprovider.DeletionRetain), string(hostprovider.DeletionDeleteOwned)},
		OutputContracts:  registry.Contracts(),
	}
	return result.Emit(jsonOut)
}

func init() {
	addEnvJSONFlags(listCmd, "Environment whose bindings to list")
	listCmd.Flags().Bool("schema", false, "Print the binding schema and declared output contracts instead of listing")
}
