package composition

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	selection "github.com/codefly-dev/cli/pkg/composition"
	core "github.com/codefly-dev/core/composition"
	updatev0 "github.com/codefly-dev/core/generated/go/codefly/update/v0"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
)

// NewCommand keeps command state per invocation, including tests and MCP callers.
func NewCommand() *cobra.Command {
	var workspace, product, configuration, identityKey, renderRequests, buildRequests string
	command := &cobra.Command{Use: "composition", Short: "Inspect and select product-owned component releases"}
	command.PersistentFlags().StringVar(&workspace, "workspace", ".", "Workspace containing package-scoped module-trust")
	command.PersistentFlags().StringVar(&product, "product", ".", "Directory containing module.codefly.yaml")
	command.PersistentFlags().StringVar(&configuration, "configuration", "", "JSON file containing the effective configuration values")
	command.PersistentFlags().StringVar(&identityKey, "identity-key", "", "Private file containing at least 32 raw bytes for configuration identity")
	command.PersistentFlags().StringVar(&renderRequests, "render-requests", "", "JSON array of typed, instance-scoped render RPC payloads")
	command.PersistentFlags().StringVar(&buildRequests, "build-requests", "", "JSON array of typed, instance-scoped build RPC payloads; retained for render and admission")
	command.MarkFlagsMutuallyExclusive("configuration", "render-requests")
	command.MarkFlagsMutuallyExclusive("configuration", "build-requests")
	command.AddCommand(newTargetCommand(&workspace))
	session := func() (*selection.SelectionSession, error) {
		if buildRequests != "" && renderRequests == "" {
			return nil, errors.New("--build-requests requires --render-requests to bind the complete execution configuration")
		}
		if renderRequests != "" {
			return newRenderSession(workspace, product, renderRequests, identityKey, buildRequests)
		}
		if configuration == "" || identityKey == "" {
			return nil, errors.New("--configuration and --identity-key are required; configuration identity cannot be inferred")
		}
		var values map[string]string
		if err := readJSON(configuration, &values); err != nil {
			return nil, err
		}
		if values == nil {
			return nil, errors.New("configuration must be an object, not null")
		}
		key, err := readCommandFile(identityKey)
		if err != nil {
			return nil, err
		}
		identity, err := core.ConfigurationIdentity(key, values)
		if err != nil {
			return nil, err
		}
		return selection.NewSelectionSession(workspace, product, identity)
	}
	add := func(use, short string, args cobra.PositionalArgs, run func(*cobra.Command, *selection.SelectionSession, []string) (any, error)) *cobra.Command {
		child := &cobra.Command{Use: use, Short: short, Args: args, RunE: func(cmd *cobra.Command, args []string) error {
			current, err := session()
			if err != nil {
				return err
			}
			value, err := run(cmd, current, args)
			if err != nil {
				return err
			}
			encoder := json.NewEncoder(cmd.OutOrStdout())
			encoder.SetIndent("", "  ")
			if err := encoder.Encode(value); err != nil {
				return err
			}
			if compatibility, ok := value.(*selection.CompatibilityInspection); ok {
				var result updatev0.UpdateResult
				if err := protojson.Unmarshal(compatibility.Result, &result); err != nil {
					return err
				}
				if result.Verdict != updatev0.Verdict_VERDICT_SAFE && result.Verdict != updatev0.Verdict_VERDICT_NEW_CAPABILITY {
					return compatibilityRefused{}
				}
			}
			return nil
		}}
		command.AddCommand(child)
		return child
	}
	add("init INPUTS.json", "Authenticate and record an initial release selection", cobra.ExactArgs(1), func(cmd *cobra.Command, current *selection.SelectionSession, args []string) (any, error) {
		var inputs selection.SelectionInputs
		if err := readJSON(args[0], &inputs); err != nil {
			return nil, err
		}
		return current.Initialize(cmd.Context(), &inputs)
	})
	add("init-release VERSION", "Resolve and authenticate the base release without entering a digest", cobra.ExactArgs(1), func(cmd *cobra.Command, current *selection.SelectionSession, args []string) (any, error) {
		return current.InitializeVersion(cmd.Context(), args[0])
	})
	add("inspect", "Inspect inherited releases, replacements, requirements and local content", cobra.NoArgs, func(cmd *cobra.Command, current *selection.SelectionSession, _ []string) (any, error) {
		return current.Inspect(cmd.Context())
	})
	add("select REPLACEMENT.json", "Authenticate and persist a nested component replacement", cobra.ExactArgs(1), func(cmd *cobra.Command, current *selection.SelectionSession, args []string) (any, error) {
		var replacement core.Replacement
		if err := readJSON(args[0], &replacement); err != nil {
			return nil, err
		}
		return current.Select(cmd.Context(), &replacement)
	})
	var rationale string
	selectVersion := add("select-release TARGET VERSION", "Resolve and authenticate an instance replacement without entering a digest", cobra.ExactArgs(2), func(cmd *cobra.Command, current *selection.SelectionSession, args []string) (any, error) {
		return current.SelectVersion(cmd.Context(), args[0], args[1], rationale)
	})
	selectVersion.Flags().StringVar(&rationale, "reason", "", "Product owner's reason for the replacement")
	add("develop CHECKOUTS.json", "Use independent local module checkouts by instance target", cobra.ExactArgs(1), func(cmd *cobra.Command, current *selection.SelectionSession, args []string) (any, error) {
		var changes map[string]string
		if err := readJSON(args[0], &changes); err != nil {
			return nil, err
		}
		if len(changes) == 0 {
			return nil, errors.New("at least one checkout change is required")
		}
		return current.Develop(cmd.Context(), changes)
	})
	add("restore TARGET...", "Restore release selections without modifying local checkout files", cobra.MinimumNArgs(1), func(cmd *cobra.Command, current *selection.SelectionSession, args []string) (any, error) {
		changes := make(map[string]string)
		for _, target := range args {
			changes[target] = ""
		}
		return current.Develop(cmd.Context(), changes)
	})
	add("acquire", "Acquire only the artifacts required by Core's effective selection", cobra.NoArgs, func(cmd *cobra.Command, current *selection.SelectionSession, _ []string) (any, error) {
		return current.Acquire(cmd.Context(), nil)
	})
	add("check-deployment INPUTS.json POLICY.json", "Check exact runtime files and signed qualifications with Core admission", cobra.ExactArgs(2), func(cmd *cobra.Command, current *selection.SelectionSession, args []string) (any, error) {
		var inputs selection.DeploymentFiles
		var policy core.DeploymentPolicy
		if err := readJSON(args[0], &inputs); err != nil {
			return nil, err
		}
		if err := readJSON(args[1], &policy); err != nil {
			return nil, err
		}
		return current.Admit(cmd.Context(), &inputs, policy, time.Now())
	})
	add("record-admission INPUTS.json POLICY.json DESTINATION.json", "Persist Core admission without replacing records or authorizing deployment", cobra.ExactArgs(3), recordAdmission)
	var expectedAdmission string
	recheck := add("recheck-admission INPUTS.json POLICY.json RECORD.json", "Recheck retained admission against current policy and actual files", cobra.ExactArgs(3), func(cmd *cobra.Command, current *selection.SelectionSession, args []string) (any, error) {
		return recheckAdmission(cmd, current, args, expectedAdmission)
	})
	recheck.Flags().StringVar(&expectedAdmission, "expected-identity", "", "Admission identity retained independently from the supplied record")
	_ = recheck.MarkFlagRequired("expected-identity")
	addApprovalCommands(add)
	add("prepare-render INPUTS.json", "Prepare Core-bound render requests without invoking executors", cobra.ExactArgs(1), func(cmd *cobra.Command, current *selection.SelectionSession, args []string) (any, error) {
		var inputs selection.DeploymentFiles
		if err := readJSON(args[0], &inputs); err != nil {
			return nil, err
		}
		return current.PrepareRender(cmd.Context(), &inputs)
	})
	var stage stageFlags
	stageCommand := add("stage-render INPUTS.json", "Invoke exact selected executors into verified staging without deployment effects", cobra.ExactArgs(1), func(cmd *cobra.Command, current *selection.SelectionSession, args []string) (any, error) {
		return runStageRender(cmd, current, args[0], renderRequests, buildRequests, identityKey, stage)
	})
	addStageFlags(stageCommand, &stage)
	var buildStage stageFlags
	buildCommand := add("stage-build", "Invoke selected source builds into verified staging without publication", cobra.NoArgs, func(cmd *cobra.Command, current *selection.SelectionSession, _ []string) (any, error) {
		return runStageBuild(cmd, current, renderRequests, buildRequests, identityKey, buildStage)
	})
	addStageFlags(buildCommand, &buildStage)
	var publication buildPublicationFlags
	publishCommand := add("publish-build BUILD.json INPUTS.json SIGNERS.json", "Publish exact staged outputs by digest with owner-authorized derived evidence", cobra.ExactArgs(3), func(cmd *cobra.Command, current *selection.SelectionSession, args []string) (any, error) {
		return publication.run(cmd, current, workspace, args)
	})
	publication.register(publishCommand)
	add("check-inputs INPUTS.json", "Authenticate runtime and staged output files for qualification", cobra.ExactArgs(1), func(cmd *cobra.Command, current *selection.SelectionSession, args []string) (any, error) {
		var inputs selection.DeploymentFiles
		if err := readJSON(args[0], &inputs); err != nil {
			return nil, err
		}
		return current.CheckInputs(cmd.Context(), &inputs)
	})
	add("check TARGET ARTIFACT USAGE.json AUTHORITY.json", "Evaluate authenticated actual-consumer compatibility with Core", cobra.ExactArgs(4), func(cmd *cobra.Command, current *selection.SelectionSession, args []string) (any, error) {
		var usage core.SignedConsumerUsage
		var authority core.ConsumerUsageAuthority
		if err := readJSON(args[2], &usage); err != nil {
			return nil, err
		}
		if err := readJSON(args[3], &authority); err != nil {
			return nil, err
		}
		return current.CheckCompatibility(cmd.Context(), args[0], args[1], usage, authority, time.Now(), nil)
	})
	add("upstream", "Prepare owner-scoped adoption facts without submitting a request", cobra.NoArgs, func(cmd *cobra.Command, current *selection.SelectionSession, _ []string) (any, error) {
		return current.Upstream(cmd.Context())
	})
	add("propose-removal TARGET RELEASE.json", "Ask Core to prove equivalence before proposing override removal", cobra.ExactArgs(2), func(cmd *cobra.Command, current *selection.SelectionSession, args []string) (any, error) {
		var candidate core.ReleaseSelection
		if err := readJSON(args[1], &candidate); err != nil {
			return nil, err
		}
		return current.ProposeRemoval(cmd.Context(), candidate, args[0])
	})
	return command
}

type compatibilityRefused struct{}

func (compatibilityRefused) Error() string {
	return "compatibility does not admit the selected replacement"
}
func (compatibilityRefused) MachineReadable() bool { return true }
func (compatibilityRefused) CommandExitCode() int  { return 2 }

func readJSON(path string, value any) error {
	data, err := readCommandFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("unexpected trailing JSON value")
	}
	return nil
}

const maxCommandInputBytes = 16 << 20

func readCommandFile(path string) (data []byte, resultErr error) {
	file, err := selection.OpenInputFile(context.Background(), path)
	if err != nil {
		return nil, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, file.Close())
		if resultErr != nil {
			clear(data)
			data = nil
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > maxCommandInputBytes {
		return nil, errors.New("composition command input exceeds the 16 MiB size limit")
	}
	data, err = io.ReadAll(io.LimitReader(file, maxCommandInputBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxCommandInputBytes {
		return nil, errors.New("composition command input exceeds the 16 MiB size limit")
	}
	return data, nil
}
