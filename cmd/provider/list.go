package provider

import (
	"context"

	hostprovider "github.com/codefly-dev/cli/pkg/provider"
	"github.com/spf13/cobra"
)

var listSchema bool

var listCmd = &cobra.Command{
	Use:          "list [BINDING]",
	Short:        "List provider bindings for an environment, or print the binding schema",
	SilenceUsage: true,
	Args:         cobra.MaximumNArgs(1),
	RunE: run(func(ctx context.Context, args []string) error {
		if listSchema {
			return emitSchema()
		}

		s, err := loadSession(ctx, "list", envFlag)
		if err != nil {
			return err
		}
		if s.failed {
			return s.result.Emit(jsonFlag)
		}

		if len(args) == 1 {
			binding, ok := s.binding(args[0])
			if !ok {
				return s.result.Emit(jsonFlag)
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
	return s.result.Emit(jsonFlag)
}

func emitSchema() error {
	result := hostprovider.NewResult("list", envFlag)
	result.Schema = &hostprovider.SchemaDoc{
		Schema:           hostprovider.BindingsSchemaV0,
		Modes:            []string{string(hostprovider.ModeObserve), string(hostprovider.ModeManaged), string(hostprovider.ModeDisabled)},
		DeletionPolicies: []string{string(hostprovider.DeletionRetain), string(hostprovider.DeletionDeleteOwned)},
		OutputContracts:  registry.Contracts(),
	}
	return result.Emit(jsonFlag)
}

func init() {
	listCmd.Flags().StringVar(&envFlag, "env", "local", "Environment whose bindings to list")
	listCmd.Flags().BoolVar(&jsonFlag, "json", false, "Print a machine-readable report to stdout")
	listCmd.Flags().BoolVar(&listSchema, "schema", false, "Print the binding schema and declared output contracts instead of listing")
}
