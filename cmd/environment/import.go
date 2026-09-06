package environment

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli/yamledit"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/pmezard/go-difflib/difflib"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// provenanceMarker is the stable substring identifying the import provenance
// comment. A re-import replaces the line carrying it instead of stacking a
// second one.
const provenanceMarker = "imported from cell contract"

var (
	importNamespace string
	importDryRun    bool
)

var importCmd = &cobra.Command{
	Use:   "import <env> --cell-contract <file|->",
	Short: "Point an environment at a cell by consuming its codefly/cell/v1 contract",
	Long: `Import a cell descriptor (codefly/cell/v1) into an environment in
workspace.codefly.yaml, so cell facts — cluster, registry, managed-database
egress CIDRs, secret store, DNS, gitops path — are sourced from the contract
instead of hand-typed.

The descriptor is produced on the platform side, e.g. by infra-base's
` + "`obinctl cell-contract <coordinate>`" + `. Read it from a file or from stdin
with ` + "`-`" + `:

  obinctl cell-contract hosted-eastus2 | codefly environment import azure --cell-contract -

Only the fields the contract owns are replaced; operator-owned fields
(description, ingress, resource-quota, per-service secret mappings, …) are
preserved, comments included.`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()

		contractPath, err := cmd.Flags().GetString("cell-contract")
		if err != nil {
			return err
		}
		if contractPath == "" {
			return fmt.Errorf("--cell-contract is required")
		}
		data, err := readContract(cmd.InOrStdin(), contractPath)
		if err != nil {
			return err
		}

		dir, err := resources.FindUp[resources.Workspace](ctx)
		if err != nil || dir == nil {
			return fmt.Errorf("no %s found from the current directory upward", resources.WorkspaceConfigurationName)
		}

		opts := importOptions{
			dir:          *dir,
			envName:      args[0],
			contractData: data,
			namespace:    importNamespace,
			namespaceSet: cmd.Flags().Changed("namespace"),
			dryRun:       importDryRun,
			now:          time.Now(),
			stdout:       cmd.OutOrStdout(),
		}
		if err := runImport(ctx, &opts); err != nil {
			return err
		}
		if importDryRun {
			return nil
		}
		if PostImportValidate != nil {
			PostImportValidate(ctx, opts.dir, opts.envName)
		}
		return nil
	},
}

// readContract returns the descriptor bytes from path, or from stdin when path
// is "-".
func readContract(stdin io.Reader, path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(stdin)
	}
	return os.ReadFile(path)
}

type importOptions struct {
	dir          string
	envName      string
	contractData []byte
	namespace    string
	namespaceSet bool
	dryRun       bool
	now          time.Time
	stdout       io.Writer
}

// runImport parses the cell contract, merges its owned fields into the named
// environment of workspace.codefly.yaml (preserving every operator-owned field
// and comment), and either writes the file or, under --dry-run, prints its
// unified diff and writes nothing.
func runImport(ctx context.Context, opts *importOptions) error {
	contract, err := resources.ParseCellContract(opts.contractData)
	if err != nil {
		return err
	}

	file := filepath.Join(opts.dir, resources.WorkspaceConfigurationName)
	original, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", resources.WorkspaceConfigurationName, err)
	}

	doc, root, err := yamledit.Document(original)
	if err != nil {
		return fmt.Errorf("cannot parse %s: %w", resources.WorkspaceConfigurationName, err)
	}

	workspaceName := ""
	if n := yamledit.MapValue(root, "name"); n != nil {
		workspaceName = n.Value
	}

	envNode, envs := findEnvironmentNode(root, opts.envName)
	isNew := envNode == nil

	namespace := resolveNamespace(opts, envNode, workspaceName)
	env, err := contract.ToEnvironment(opts.envName, namespace)
	if err != nil {
		return err
	}

	if isNew {
		envNode = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		yamledit.SetMapValue(envNode, "name", yamledit.Scalar(opts.envName))
		envs.Content = append(envs.Content, envNode)
	}

	if aerr := applyContractFields(envNode, env, opts.namespaceSet || isNew); aerr != nil {
		return aerr
	}
	stampProvenance(envNode, contract, opts)

	updated, err := yamledit.Marshal(doc)
	if err != nil {
		return fmt.Errorf("cannot render %s: %w", resources.WorkspaceConfigurationName, err)
	}

	if opts.dryRun {
		diff, derr := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
			A:        difflib.SplitLines(string(original)),
			B:        difflib.SplitLines(string(updated)),
			FromFile: "a/" + resources.WorkspaceConfigurationName,
			ToFile:   "b/" + resources.WorkspaceConfigurationName,
			Context:  3,
		})
		if derr != nil {
			return derr
		}
		fmt.Fprint(opts.stdout, diff)
		return nil
	}

	if err := shared.WriteFileAtomic(ctx, file, updated, 0o600); err != nil {
		return fmt.Errorf("cannot write %s: %w", resources.WorkspaceConfigurationName, err)
	}
	fmt.Fprintf(opts.stdout, "Imported cell contract %s (%s) into environment %q of %s.\n",
		contract.Cell, contract.Coordinate, opts.envName, resources.WorkspaceConfigurationName)
	return nil
}

// findEnvironmentNode returns the mapping node of the named environment (or nil
// when absent) and the environments sequence node, creating an empty sequence
// on root when none is declared yet.
func findEnvironmentNode(root *yaml.Node, name string) (*yaml.Node, *yaml.Node) {
	envs := yamledit.MapValue(root, "environments")
	if envs == nil || envs.Kind != yaml.SequenceNode {
		envs = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		yamledit.SetMapValue(root, "environments", envs)
		return nil, envs
	}
	for _, item := range envs.Content {
		if n := yamledit.MapValue(item, "name"); n != nil && n.Value == name {
			return item, envs
		}
	}
	return nil, envs
}

// resolveNamespace picks the deploy namespace: the --namespace flag, else the
// existing environment's namespace, else the workspace name.
func resolveNamespace(opts *importOptions, envNode *yaml.Node, workspaceName string) string {
	if opts.namespaceSet {
		return opts.namespace
	}
	if envNode != nil {
		if n := yamledit.MapValue(envNode, "namespace"); n != nil && n.Value != "" {
			return n.Value
		}
	}
	return workspaceName
}

// applyContractFields overwrites the environment's contract-owned fields from
// env (the resources.Environment ToEnvironment produced), leaving every other
// field and comment untouched. writeNamespace controls whether namespace is
// (re)written — always for a new environment, otherwise only when --namespace
// was given.
func applyContractFields(envNode *yaml.Node, env *resources.Environment, writeNamespace bool) error {
	if writeNamespace {
		yamledit.SetMapValue(envNode, "namespace", yamledit.Scalar(env.Namespace))
	}
	if env.Cluster != nil {
		if err := setEncoded(envNode, "cluster", env.Cluster); err != nil {
			return err
		}
	}
	if env.Registry != nil {
		if err := setEncoded(envNode, "registry", env.Registry); err != nil {
			return err
		}
	}
	if env.Gitops != nil {
		gitops := yamledit.EnsureMap(envNode, "gitops")
		yamledit.SetMapValue(gitops, "repo-url", yamledit.Scalar(env.Gitops.RepoURL))
		yamledit.SetMapValue(gitops, "path", yamledit.Scalar(env.Gitops.Path))
		// branch is operator-owned: keep an existing one, seed the default only
		// when the block is created here.
		if yamledit.MapValue(gitops, "branch") == nil {
			yamledit.SetMapValue(gitops, "branch", yamledit.Scalar(env.Gitops.Branch))
		}
	}
	for name, svc := range env.ManagedServices {
		managed := yamledit.EnsureMap(envNode, "managed-services")
		store := yamledit.MapValue(managed, name)
		if store == nil {
			if err := setEncoded(managed, name, svc); err != nil {
				return err
			}
			continue
		}
		// Existing entry: the contract owns kind, external-name and egress-cidrs;
		// secret-references stay as the operator wrote them.
		yamledit.SetMapValue(store, "kind", yamledit.Scalar(svc.Kind))
		yamledit.SetMapValue(store, "external-name", yamledit.Scalar(svc.ExternalName))
		if err := setEncoded(store, "egress-cidrs", svc.EgressCIDRs); err != nil {
			return err
		}
	}
	if env.ServiceSecrets != nil {
		serviceSecrets := yamledit.EnsureMap(envNode, "service-secrets")
		if err := setEncoded(serviceSecrets, "secret-store", env.ServiceSecrets.SecretStore); err != nil {
			return err
		}
	}
	if env.Dns != nil {
		if err := setEncoded(envNode, "dns", env.Dns); err != nil {
			return err
		}
	}
	return nil
}

func setEncoded(node *yaml.Node, key string, value any) error {
	encoded, err := yamledit.Encode(value)
	if err != nil {
		return err
	}
	yamledit.SetMapValue(node, key, encoded)
	return nil
}

// stampProvenance writes (or, on re-import, replaces) the provenance comment
// above the environment item, keeping any operator comment lines around it.
func stampProvenance(envNode *yaml.Node, contract *resources.CellContract, opts *importOptions) {
	line := fmt.Sprintf("# %s %s (%s) on %s; re-run: codefly environment import %s --cell-contract …",
		provenanceMarker, contract.Cell, contract.Coordinate, opts.now.Format(time.RFC3339), opts.envName)

	kept := make([]string, 0)
	for _, existing := range strings.Split(envNode.HeadComment, "\n") {
		if existing == "" || strings.Contains(existing, provenanceMarker) {
			continue
		}
		kept = append(kept, existing)
	}
	envNode.HeadComment = strings.Join(append([]string{line}, kept...), "\n")
}

func init() {
	importCmd.Flags().String("cell-contract", "", "Path to a codefly/cell/v1 descriptor, or - for stdin")
	importCmd.Flags().StringVar(&importNamespace, "namespace", "", "Kubernetes namespace to deploy into (default: existing namespace, else workspace name)")
	importCmd.Flags().BoolVar(&importDryRun, "dry-run", false, "Print the unified diff of workspace.codefly.yaml and write nothing")
}
