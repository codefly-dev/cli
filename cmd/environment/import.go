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
			// UTC so the provenance timestamp does not vary by the operator's
			// machine timezone and churn the diff on re-import across a team.
			now:    time.Now().UTC(),
			stdout: cmd.OutOrStdout(),
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
// environment of workspace.codefly.yaml, and either writes the file or, under
// --dry-run, prints its unified diff and writes nothing.
//
// The merge preserves the rest of the document byte-for-byte: it re-serializes
// only the one environment item being imported and splices it back into the
// original text, so other environments, top-level keys, blank lines, and
// comments outside that item are never reflowed. (A whole-document round-trip
// through yaml.v3 would strip blank lines and normalize indentation across the
// entire file — an unreviewable diff, and the exact failure that would hide a
// wrong egress CIDR in the noise.)
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

	_, root, err := yamledit.Document(original)
	if err != nil {
		return fmt.Errorf("cannot parse %s: %w", resources.WorkspaceConfigurationName, err)
	}

	workspaceName := ""
	if n := yamledit.MapValue(root, "name"); n != nil {
		workspaceName = n.Value
	}

	envs := yamledit.MapValue(root, "environments")
	envNode := findExistingEnvironment(envs, opts.envName)

	namespace := resolveNamespace(opts, envNode, workspaceName)
	env, err := contract.ToEnvironment(opts.envName, namespace)
	if err != nil {
		return err
	}

	var updated []byte
	if envNode == nil {
		// New environment: serialize ToEnvironment's result whole, so a field
		// core maps from the contract is carried through without this consumer
		// maintaining a parallel allowlist of the fields to copy.
		item, eerr := yamledit.Encode(env)
		if eerr != nil {
			return eerr
		}
		stampProvenance(item, contract, opts)
		updated, err = insertEnvironment(original, root, envs, item)
	} else {
		// Capture the item's original line span BEFORE editing: node edits replace
		// subtrees with freshly-encoded nodes that carry no source position, so
		// EndLine after the edit would under-count and strand the original's
		// trailing lines.
		endLine := yamledit.EndLine(envNode)
		if aerr := applyContractFields(envNode, env, opts.envName, opts.namespaceSet); aerr != nil {
			return aerr
		}
		stampProvenance(envNode, contract, opts)
		updated, err = spliceEnvironment(original, envNode, endLine)
	}
	if err != nil {
		return err
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

// findExistingEnvironment returns the mapping node of the named environment in
// the environments sequence, or nil when the sequence is absent or has no such
// item.
func findExistingEnvironment(envs *yaml.Node, name string) *yaml.Node {
	if envs == nil || envs.Kind != yaml.SequenceNode {
		return nil
	}
	for _, item := range envs.Content {
		if n := yamledit.MapValue(item, "name"); n != nil && n.Value == name {
			return item
		}
	}
	return nil
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

// spliceEnvironment re-renders only envNode and replaces the exact lines it
// occupied in the original text (endLine is that item's last source line,
// captured before the node was edited), leaving every other byte untouched.
// Foot comments in the item's subtree are cleared before rendering because they
// have no node of their own and are preserved by the surrounding text —
// re-emitting them would duplicate them.
func spliceEnvironment(original []byte, envNode *yaml.Node, endLine int) ([]byte, error) {
	clearFootComments(envNode)
	lines := strings.Split(string(original), "\n")
	itemLine := envNode.Line - 1
	start := headCommentStart(lines, itemLine)
	end := endLine // 1-based inclusive last content line, captured before edits
	rendered, err := renderItem(envNode, lineIndent(lines[itemLine]))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(lines)+len(rendered))
	out = append(out, lines[:start]...)
	out = append(out, rendered...)
	out = append(out, lines[end:]...)
	return []byte(strings.Join(out, "\n")), nil
}

// insertEnvironment adds a new environment item to the original text without
// re-rendering the rest of the document: after the last existing item, right
// after the environments: key when the sequence is empty, or as a fresh
// environments: section appended at EOF when none is declared.
func insertEnvironment(original []byte, root, envs, item *yaml.Node) ([]byte, error) {
	lines := strings.Split(string(original), "\n")

	if envs != nil && envs.Kind == yaml.SequenceNode && len(envs.Content) > 0 {
		indent := lineIndent(lines[envs.Content[0].Line-1])
		rendered, err := renderItem(item, indent)
		if err != nil {
			return nil, err
		}
		last := envs.Content[len(envs.Content)-1]
		ins := yamledit.EndLine(last) // 0-based index of the line after the last item
		return spliceLines(lines, ins, ins, rendered), nil
	}

	rendered, err := renderItem(item, 4)
	if err != nil {
		return nil, err
	}
	if envs != nil {
		// environments: present but empty — insert items right after its key.
		key := environmentsKeyLine(root)
		return spliceLines(lines, key+1, key+1, rendered), nil
	}
	// No environments: key — append a fresh section at end of file.
	ins := len(lines)
	if ins > 0 && lines[ins-1] == "" {
		ins--
	}
	block := append([]string{"environments:"}, rendered...)
	return spliceLines(lines, ins, ins, block), nil
}

// spliceLines returns lines with [start,end) replaced by repl.
func spliceLines(lines []string, start, end int, repl []string) []byte {
	out := make([]string, 0, len(lines)-(end-start)+len(repl))
	out = append(out, lines[:start]...)
	out = append(out, repl...)
	out = append(out, lines[end:]...)
	return []byte(strings.Join(out, "\n"))
}

// environmentsKeyLine returns the 0-based line index of the environments: key.
func environmentsKeyLine(root *yaml.Node) int {
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "environments" {
			return root.Content[i].Line - 1
		}
	}
	return len(root.Content) // unreachable: callers only reach here when the key exists
}

// renderItem serializes a single sequence item and left-pads every line to the
// sequence's indentation, reproducing the project's canonical block style.
func renderItem(item *yaml.Node, indent int) ([]string, error) {
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{item}}
	b, err := yamledit.Marshal(&yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{seq}})
	if err != nil {
		return nil, err
	}
	pad := strings.Repeat(" ", indent)
	raw := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	out := make([]string, len(raw))
	for i, l := range raw {
		if l == "" {
			out[i] = ""
			continue
		}
		out[i] = pad + l
	}
	return out, nil
}

// headCommentStart walks back over the contiguous comment lines directly above
// itemLine — exactly the lines yaml.v3 attaches as the item's HeadComment (a
// blank line breaks the attachment) — and returns the first line index of that
// block, so a splice replaces the old head comment together with the item.
func headCommentStart(lines []string, itemLine int) int {
	start := itemLine
	for start > 0 && strings.HasPrefix(strings.TrimSpace(lines[start-1]), "#") {
		start--
	}
	return start
}

// lineIndent counts the leading spaces of a line.
func lineIndent(line string) int {
	n := 0
	for n < len(line) && line[n] == ' ' {
		n++
	}
	return n
}

// clearFootComments recursively clears FootComment on a node subtree.
func clearFootComments(node *yaml.Node) {
	node.FootComment = ""
	for _, child := range node.Content {
		clearFootComments(child)
	}
}

// applyContractFields overwrites the environment's contract-owned fields from
// env (the resources.Environment ToEnvironment produced) via node edits, leaving
// every other field and comment untouched. writeNamespace controls whether
// namespace is rewritten — only when --namespace was given (a new environment
// takes the whole ToEnvironment result elsewhere).
func applyContractFields(envNode *yaml.Node, env *resources.Environment, envName string, writeNamespace bool) error {
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
	if len(env.ManagedServices) > 0 {
		if err := applyManagedServices(envNode, env.ManagedServices, envName); err != nil {
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

// applyManagedServices updates the environment's managed database in place.
// core's ToEnvironment always emits the single database under the key "store",
// but an operator may already declare it under the service name it replaces —
// and that name, not "store", is what the deploy path matches. So the update
// targets an existing entry regardless of its key (an exact "store" match
// first, then the sole existing entry) and only inserts "store" when none
// exists. Two or more existing entries with no exact match is ambiguous and
// refused, rather than silently leaving a stale entry — with its stale, possibly
// wrong, egress CIDR, the outage this contract exists to prevent — beside a new
// one.
func applyManagedServices(envNode *yaml.Node, services map[string]resources.EnvironmentManagedService, envName string) error {
	for contractKey, svc := range services {
		managed := yamledit.MapValue(envNode, "managed-services")
		if managed == nil {
			managed = yamledit.EnsureMap(envNode, "managed-services")
			if err := setEncoded(managed, contractKey, svc); err != nil {
				return err
			}
			continue
		}
		target := yamledit.MapValue(managed, contractKey)
		if target == nil {
			keys := yamledit.MapKeys(managed)
			switch len(keys) {
			case 0:
				if err := setEncoded(managed, contractKey, svc); err != nil {
					return err
				}
				continue
			case 1:
				target = yamledit.MapValue(managed, keys[0])
			default:
				return fmt.Errorf("environment %q declares %d managed services (%s) but the cell contract maps one; rename the database's entry to %q or remove the extras so import can update it unambiguously",
					envName, len(keys), strings.Join(keys, ", "), contractKey)
			}
		}
		yamledit.SetMapValue(target, "kind", yamledit.Scalar(svc.Kind))
		yamledit.SetMapValue(target, "external-name", yamledit.Scalar(svc.ExternalName))
		if err := setEncoded(target, "egress-cidrs", svc.EgressCIDRs); err != nil {
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
