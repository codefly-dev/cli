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
	RunE: run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
		s, err := loadSession(ctx, "doctor", envOf(cmd), jsonOf(cmd))
		if err != nil {
			return err
		}
		if s.failed {
			return s.emit()
		}
		s.diagnose(args)
		return s.emit()
	}),
}

// diagnose validates the selected bindings offline and applies the remote
// gating. When a specific binding is named but absent it fails the result with
// binding_not_found. With no bindings to diagnose the result stays a success:
// there is nothing to observe, so it is not a failure. When every selected
// binding is valid, the remote diagnostics still cannot run, which is the
// coordinator-unavailable outcome.
func (s *session) diagnose(args []string) {
	var bindings []*hostprovider.Binding
	if len(args) == 1 {
		binding, ok := s.binding(args[0])
		if !ok {
			return
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

	if allValid && len(bindings) > 0 {
		s.result.Fail(hostprovider.CodeCoordinatorUnavailable, "offline checks passed; remote diagnostics require the host coordinator, not available in this build")
	}
}

func init() {
	addEnvJSONFlags(doctorCmd, "Environment whose bindings to diagnose")
}
