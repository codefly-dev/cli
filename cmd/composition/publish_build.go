package composition

import (
	"errors"
	"path/filepath"

	selection "github.com/codefly-dev/cli/pkg/composition"
	"github.com/codefly-dev/cli/pkg/control"
	"github.com/spf13/cobra"
)

type buildPublicationFlags struct {
	expected, build, repository, destination string
}

type buildSignerFile struct {
	Signer  string `json:"signer"`
	KeyFile string `json:"keyFile"`
}

func (flags *buildPublicationFlags) register(command *cobra.Command) {
	command.Flags().StringVar(&flags.expected, "expected-selection", "", "Independently inspected effective selection identity")
	command.Flags().StringVar(&flags.build, "expected-build", "", "Invocation evidence digest retained independently when stage-build completed")
	command.Flags().StringVar(&flags.repository, "repository", "", "Explicit OCI registry/repository for bytes and a digest-named retention reference; no module release")
	command.Flags().StringVar(&flags.destination, "output", "", "Absolute new publication record outside the product and checkouts")
	for _, name := range []string{"expected-selection", "expected-build", "repository", "output"} {
		_ = command.MarkFlagRequired(name)
	}
}

func (flags *buildPublicationFlags) run(cmd *cobra.Command, session *selection.SelectionSession, workspace string, args []string) (any, error) {
	var staged selection.StagedBuild
	var inputs selection.DeploymentFiles
	var keys map[string]buildSignerFile
	if err := readJSON(args[0], &staged); err != nil {
		return nil, err
	}
	if err := readJSON(args[1], &inputs); err != nil {
		return nil, err
	}
	if err := readJSON(args[2], &keys); err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, errors.New("package-scoped build signing keys are required")
	}
	options := selection.BuildPublicationOptions{ExpectedSelection: flags.expected, ExpectedBuild: flags.build, Repository: flags.repository, Destination: flags.destination, Signers: make(map[string]selection.BuildSigningKey)}
	defer func() {
		for _, signer := range options.Signers {
			clear(signer.Key)
		}
	}()
	for owner, source := range keys {
		key, err := readApprovalKey(source.KeyFile)
		if err != nil {
			return nil, err
		}
		options.Signers[owner] = selection.BuildSigningKey{Signer: source.Signer, Key: key}
	}
	root, err := filepath.Abs(workspace)
	if err != nil {
		return nil, err
	}
	plane := control.New()
	if err = plane.ConfigureMutationAuthority(cmd.Context(), control.AuthorityConfig{Mode: control.AuthorityPrepared}); err != nil {
		return nil, err
	}
	prepared, err := plane.PrepareMutation(cmd.Context(), control.Mutation{Kind: control.MutationCompositionBuildPublish,
		Payload: &selection.BuildPublicationMutation{Workspace: root, Product: session.Root, ConfigurationIdentity: session.ConfigurationIdentity, Staged: staged, Inputs: inputs, Options: options}})
	if err != nil {
		return nil, err
	}
	result, err := plane.ApplyPreparedMutation(cmd.Context(), prepared)
	if err != nil {
		return nil, err
	}
	if result.CompositionBuild == nil {
		return nil, errors.New("build publication returned no verified result")
	}
	return result.CompositionBuild, nil
}
