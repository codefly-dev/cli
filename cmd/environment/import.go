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
const provenanceMarker = "imported from coordinate contract"

// legacyProvenanceMarker is the spelling stamped before codefly/coordinate/v1.
// It is not a substring of the current marker, so a re-import over a workspace
// written by an older CLI has to match it explicitly or it would leave the stale
// line beside the new one.
const legacyProvenanceMarker = "imported from cell contract"

var (
	importNamespace string
	importDryRun    bool
)

var importCmd = &cobra.Command{
	Use:   "import <env> --coordinate-contract <file|->",
	Short: "Import explicit environment declarations from a codefly/coordinate/v1 contract",
	Long: `Import a codefly/coordinate/v1 descriptor into workspace.codefly.yaml.
The producer supplies Codefly environment declarations with resolved endpoints,
secret references and delivery paths. The requested environment and namespace
must match the declaration; import never retargets a contract.

  codefly environment import production --coordinate-contract coordinate.json

Declared fields replace their named values. Omitted fields and unrelated map
entries are preserved, comments included. Read from stdin with
--coordinate-contract -.`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()

		contractPath, contractFlagName, err := contractFlag(cmd)
		if err != nil {
			return err
		}
		if contractPath == "" {
			return fmt.Errorf("--%s is required", contractFlagName)
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
			return PostImportValidate(ctx, opts.dir, opts.envName)
		}
		return nil
	},
}

// contractFlag returns the descriptor location from --coordinate-contract, or
// from the superseded --cell-contract spelling still accepted for one release.
// It also reports which spelling it read, so a caller rejecting an empty value
// names the flag the operator actually passed.
func contractFlag(cmd *cobra.Command) (path string, flag string, err error) {
	if cmd.Flags().Changed("cell-contract") {
		if cmd.Flags().Changed("coordinate-contract") {
			return "", "", fmt.Errorf("--cell-contract is the former spelling of --coordinate-contract; pass only one")
		}
		path, err = cmd.Flags().GetString("cell-contract")
		return path, "cell-contract", err
	}
	path, err = cmd.Flags().GetString("coordinate-contract")
	return path, "coordinate-contract", err
}

// labelled renders an optional identifier as a leading-space suffix. The
// coordinate is an optional provenance label, so a contract that omits it would
// otherwise leave a dangling subject in the message and in the comment this
// stamps into workspace.codefly.yaml.
func labelled(coordinate string) string {
	if coordinate == "" {
		return ""
	}
	return " " + coordinate
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

// runImport parses the contract, merges its owned fields into the named
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
	contract, err := resources.ParseCoordinateContract(opts.contractData)
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
		_, declaration, decodeErr := yamledit.Document(opts.contractData)
		if decodeErr != nil {
			return decodeErr
		}
		applyContractFields(envNode, yamledit.MapValue(declaration, "environment"))
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
	fmt.Fprintf(opts.stdout, "Imported coordinate contract%s into environment %q of %s.\n",
		labelled(contract.Coordinate), opts.envName, resources.WorkspaceConfigurationName)
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
		// environments: present but carrying no items — a null value, a flow-empty
		// `[]`, or `{}`. Replace the whole key line rather than inserting block
		// items after it: appending a block sequence under an inline value like
		// `environments: []` produces invalid YAML that no longer loads.
		key := environmentsKeyLine(root)
		block := append([]string{"environments:"}, rendered...)
		return spliceLines(lines, key, key+1, block), nil
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

// Merge only explicitly declared fields. Omitted keys retain operator-owned
// values; sequences and scalars replace their exact named field.
func applyContractFields(target, declaration *yaml.Node) {
	for i := 0; i+1 < len(declaration.Content); i += 2 {
		key, value := declaration.Content[i].Value, declaration.Content[i+1]
		current := yamledit.MapValue(target, key)
		if value.Kind == yaml.MappingNode && len(value.Content) > 0 && current != nil && current.Kind == yaml.MappingNode {
			applyContractFields(current, value)
			continue
		}
		yamledit.SetMapValue(target, key, value)
	}
}

// stampProvenance writes (or, on re-import, replaces) the provenance comment
// above the environment item, keeping any operator comment lines around it.
func stampProvenance(envNode *yaml.Node, contract *resources.CoordinateContract, opts *importOptions) {
	line := fmt.Sprintf("# %s%s on %s; re-run: codefly environment import %s --coordinate-contract …",
		provenanceMarker, labelled(contract.Coordinate), opts.now.Format(time.RFC3339), opts.envName)

	kept := make([]string, 0)
	for _, existing := range strings.Split(envNode.HeadComment, "\n") {
		if existing == "" ||
			strings.Contains(existing, provenanceMarker) ||
			strings.Contains(existing, legacyProvenanceMarker) {
			continue
		}
		kept = append(kept, existing)
	}
	envNode.HeadComment = strings.Join(append([]string{line}, kept...), "\n")
}

func init() {
	importCmd.Flags().String("coordinate-contract", "", "Path to a codefly/coordinate/v1 descriptor, or - for stdin")
	importCmd.Flags().String("cell-contract", "", "Former spelling of --coordinate-contract")
	_ = importCmd.Flags().MarkDeprecated("cell-contract", "use --coordinate-contract")
	importCmd.Flags().StringVar(&importNamespace, "namespace", "", "Kubernetes namespace to deploy into (default: existing namespace, else workspace name)")
	importCmd.Flags().BoolVar(&importDryRun, "dry-run", false, "Print the unified diff of workspace.codefly.yaml and write nothing")
}
