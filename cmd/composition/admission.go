package composition

import (
	"time"

	selection "github.com/codefly-dev/cli/pkg/composition"
	core "github.com/codefly-dev/core/composition"
	"github.com/spf13/cobra"
)

func recordAdmission(cmd *cobra.Command, session *selection.SelectionSession, args []string) (any, error) {
	var inputs selection.DeploymentFiles
	var policy core.DeploymentPolicy
	if err := readJSON(args[0], &inputs); err != nil {
		return nil, err
	}
	if err := readJSON(args[1], &policy); err != nil {
		return nil, err
	}
	return session.RecordAdmission(cmd.Context(), &inputs, policy, time.Now(), args[2])
}

func recheckAdmission(cmd *cobra.Command, session *selection.SelectionSession, args []string, expected string) (any, error) {
	var inputs selection.DeploymentFiles
	var policy core.DeploymentPolicy
	var recorded selection.AdmissionInspection
	if err := readJSON(args[0], &inputs); err != nil {
		return nil, err
	}
	if err := readJSON(args[1], &policy); err != nil {
		return nil, err
	}
	if err := readJSON(args[2], &recorded); err != nil {
		return nil, err
	}
	return session.RecheckAdmission(cmd.Context(), &inputs, policy, &recorded, expected, time.Now())
}
