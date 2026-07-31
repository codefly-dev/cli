package provider

import (
	"context"

	hostprovider "github.com/codefly-dev/cli/pkg/provider"
	"github.com/spf13/cobra"
)

var doctorCmd = &cobra.Command{
	Use:          "doctor [BINDING]",
	Short:        "Diagnose provider bindings for an environment",
	SilenceUsage: true,
	Args:         cobra.MaximumNArgs(1),
	Long: `Diagnose provider bindings.

This command performs the offline half of provider diagnostics: it validates
each binding's identity, mode, secrets hygiene, output contract, and endpoint
references. The remote, read-only half — authentication, account, observation,
resource health, and drift — requires the host coordinator and is reported as
unavailable until that layer is wired.

For the bounded, no-agent workspace checks, use ` + "`codefly doctor workspace`" + `.`,
	RunE: run(func(ctx context.Context, args []string) error {
		s, err := loadSession(ctx, "doctor", envFlag)
		if err != nil {
			return err
		}
		if s.failed {
			return s.result.Emit(jsonFlag)
		}

		var bindings []*hostprovider.Binding
		if len(args) == 1 {
			binding, ok := s.binding(args[0])
			if !ok {
				return s.result.Emit(jsonFlag)
			}
			bindings = []*hostprovider.Binding{binding}
		} else {
			bindings = s.document.ForEnvironment(s.env.Name)
		}

		allValid := true
		for _, binding := range bindings {
			valid := s.validate(binding)
			s.result.Bindings = append(s.result.Bindings, binding.View(valid))
			allValid = allValid && valid
		}

		// Offline validation passed; the remote diagnostics cannot run yet.
		if allValid {
			s.result.Fail(hostprovider.CodeCoordinatorUnavailable, "offline checks passed; remote diagnostics require the host coordinator, not available in this build")
		}
		return s.result.Emit(jsonFlag)
	}),
}

func init() {
	doctorCmd.Flags().StringVar(&envFlag, "env", "local", "Environment whose bindings to diagnose")
	doctorCmd.Flags().BoolVar(&jsonFlag, "json", false, "Print a machine-readable report to stdout")
}
