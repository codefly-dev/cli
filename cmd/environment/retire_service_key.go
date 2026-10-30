package environment

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/codefly-dev/cli/pkg/cli/yamledit"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"
)

// retirableBlocks are the environment blocks whose `services` map may be
// re-keyed by an explicit retirement.
//
// `service-secrets` is deliberately absent and is refused by name. A bare key
// there is not merely a different spelling: RemoteRef changes what is owned and
// what is a credential, so rewriting the key produces a workspace that
// validation then rejects as an unknown service, with the mapping falling back
// to a remote key the consumers never name. Retiring a secrets key needs its
// own answer, not this one.
var retirableBlocks = map[string]bool{"service-config": true, "service-identity": true, "service-mesh": true}

// retirement is one operator-stated replacement: in this block, this bare key
// is the old spelling of this module-qualified service.
type retirement struct {
	block       string
	bare        string
	destination string
	raw         string
}

// parseRetirements reads the repeatable --retire-service-key directives.
//
// The CLI does not infer that a bare key was ever the contract's. Name suffixes
// and equal values are not evidence of that — a bare key can name a genuinely
// different service whose values happen to match — so the operator states both
// halves and the CLI does exactly what it was told, or refuses.
func parseRetirements(values []string) ([]retirement, error) {
	parsed := make([]retirement, 0, len(values))
	seen := map[string]string{}
	for _, raw := range values {
		block, rest, ok := strings.Cut(strings.TrimSpace(raw), ":")
		if !ok {
			return nil, fmt.Errorf("--retire-service-key %q is malformed: expected <block>:<bare-key>=<module>/<service>", raw)
		}
		bare, destination, ok := strings.Cut(rest, "=")
		if !ok {
			return nil, fmt.Errorf("--retire-service-key %q is malformed: expected <block>:<bare-key>=<module>/<service>", raw)
		}
		block, bare, destination = strings.TrimSpace(block), strings.TrimSpace(bare), strings.TrimSpace(destination)
		switch {
		case bare == "" || destination == "":
			return nil, fmt.Errorf("--retire-service-key %q is malformed: expected <block>:<bare-key>=<module>/<service>", raw)
		case strings.Contains(bare, "/"):
			return nil, fmt.Errorf("--retire-service-key %q retires %q, which is already module-qualified; only a bare key is retired", raw, bare)
		case !strings.Contains(destination, "/"):
			return nil, fmt.Errorf("--retire-service-key %q names destination %q, which is not module-qualified; write it as <module>/<service>", raw, destination)
		case block == "service-secrets":
			return nil, fmt.Errorf("--retire-service-key %q: service-secrets keys are not retired this way, because a remote reference is not merely a spelling of the service name", raw)
		case !retirableBlocks[block]:
			return nil, fmt.Errorf("--retire-service-key %q names block %q; supported blocks are %s", raw, block, strings.Join(sortedRetirableBlocks(), ", "))
		}
		key := block + ":" + bare
		if previous, repeated := seen[key]; repeated {
			if previous == destination {
				return nil, fmt.Errorf("--retire-service-key names %s %q twice", block, bare)
			}
			return nil, fmt.Errorf("--retire-service-key names %s %q twice, for %q and %q; one key has one destination", block, bare, previous, destination)
		}
		seen[key] = destination
		parsed = append(parsed, retirement{block: block, bare: bare, destination: destination, raw: raw})
	}
	return parsed, nil
}

func sortedRetirableBlocks() []string {
	blocks := make([]string, 0, len(retirableBlocks))
	for block := range retirableBlocks {
		blocks = append(blocks, block)
	}
	sort.Strings(blocks)
	return blocks
}

// applyRetirements removes each stated bare key from the selected environment,
// after checking every directive against the merged declaration and the
// workspace graph. Every refusal happens before the caller writes anything, so
// a rejected import leaves the file as it was.
//
// It removes the WHOLE named entry, not the part that overlaps its
// replacement. EnvironmentServiceConfig.ValuesFor copies the bare entry's
// values and then overlays the qualified entry's, so a bare entry can
// contribute keys the destination does not declare; those keys go with it. The
// returned notes say so, per retirement, naming the keys that will be lost, so
// the operator sees it before the write and in --dry-run.
//
// It changes only the environment being imported. Another environment that
// carries the same bare key keeps it: whether that one is also stale is its
// owner's call, not a consequence of this one.
//
// The directive is one-shot by design. It requires the bare key to still be
// present, so once a run has written the file the same command refuses — a
// retry after a post-import readiness error must omit the flag.
func applyRetirements(ctx context.Context, target, declaration *yaml.Node, workspace *resources.Workspace, retirements []retirement) ([]string, error) {
	if len(retirements) == 0 {
		return nil, nil
	}
	// Membership is proved against the real service graph: every module loaded
	// and its service references read. A name index built from module
	// references alone can report a qualified name the workspace does not
	// actually compose, and a destination that does not exist is exactly what
	// must be refused here.
	composed, err := workspace.LoadServiceWithModules(inspectionContext(ctx))
	if err != nil {
		return nil, err
	}
	notes := make([]string, 0, len(retirements))
	for _, r := range retirements {
		if !composes(composed, r.destination) {
			return nil, fmt.Errorf("--retire-service-key %q names destination %q, which the workspace composes no service for", r.raw, r.destination)
		}
		currentBlock := yamledit.MapValue(target, r.block)
		currentServices := yamledit.MapValue(currentBlock, "services")
		retiring := yamledit.MapValue(currentServices, r.bare)
		if currentServices == nil || currentServices.Kind != yaml.MappingNode || retiring == nil {
			return nil, fmt.Errorf("--retire-service-key %q: environment %s declares no %s key %q, so there is nothing to retire "+
				"(the directive is one-shot: omit it when re-running after a successful write)",
				r.raw, environmentLabel(target), r.block, r.bare)
		}
		// The contract still naming the bare key means it is current, not
		// retired: withdrawing it would delete a key the very next import
		// writes back.
		declaredServices := yamledit.MapValue(yamledit.MapValue(declaration, r.block), "services")
		// The destination has to be what the INCOMING contract declares under
		// this block. Retirement is authorized against a replacement that just
		// arrived; pointing it at a key the contract does not name would make
		// it a plain deletion wearing a migration's clothes.
		if yamledit.MapValue(declaredServices, r.destination) == nil {
			return nil, fmt.Errorf("--retire-service-key %q: the incoming contract declares no %s key %q, so there is no replacement to retire into",
				r.raw, r.block, r.destination)
		}
		if yamledit.MapValue(declaredServices, r.bare) != nil {
			return nil, fmt.Errorf("--retire-service-key %q: the incoming contract still declares %s %q, so it is not a retired spelling",
				r.raw, r.block, r.bare)
		}
		note := fmt.Sprintf("Retiring %s %q into %q: the whole entry is removed.", r.block, r.bare, r.destination)
		if lost := valueKeysLost(retiring, yamledit.MapValue(currentServices, r.destination)); len(lost) > 0 {
			note += fmt.Sprintf(" %q does not declare %s, so %s no longer set for it.",
				r.destination, strings.Join(quoteAll(lost), ", "), plural(len(lost), "that key is", "those keys are"))
		}
		notes = append(notes, note)
		removeMapKey(currentServices, r.bare)
	}
	return notes, nil
}

// valueKeysLost names the `values` keys the retiring entry carries that its
// destination does not, sorted. ValuesFor overlays bare then qualified, so
// these are the keys the service stops seeing once the bare entry is gone.
func valueKeysLost(retiring, destination *yaml.Node) []string {
	kept := map[string]bool{}
	for _, key := range yamledit.MapKeys(yamledit.MapValue(destination, "values")) {
		kept[key] = true
	}
	var lost []string
	for _, key := range yamledit.MapKeys(yamledit.MapValue(retiring, "values")) {
		if !kept[key] {
			lost = append(lost, key)
		}
	}
	sort.Strings(lost)
	return lost
}

func quoteAll(values []string) []string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, strconv.Quote(value))
	}
	return quoted
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// resetInvocationFlags returns this command's flags to their unset state after
// a run.
//
// The command is a package-level var and cobra keeps each parsed flag's value
// and its Changed bit on that object's FlagSet, so a process that runs the
// command twice carries the first run's flags into the second. Separate
// processes are unaffected, which is why only the two-Execute path shows it.
//
// It is not specific to one flag. `--retire-service-key` inherited its
// directives and then failed the one-shot check; `--cell-contract` stayed
// Changed and made a later `--coordinate-contract` run fail "pass only one";
// `--namespace` would leak an assertion the second run never made, since
// Changed is what distinguishes asserting a namespace from omitting it. Every
// flag here is invocation-scoped, so every one is cleared.
func resetInvocationFlags(cmd *cobra.Command) {
	cmd.Flags().VisitAll(func(flag *pflag.Flag) {
		if slice, ok := flag.Value.(pflag.SliceValue); ok {
			_ = slice.Replace(nil)
		} else {
			_ = flag.Value.Set(flag.DefValue)
		}
		flag.Changed = false
	})
}

// inspectionContext is the context a retirement directive is checked under.// inspectionContext is the context a retirement directive is checked under. It
// adds nothing to the host's own; it takes two things away.
//
// Migration is refused, so a legacy flat module is not decoded, merged, saved
// or removed on the way to answering whether a destination exists. Core's own
// doc is explicit that this bounds workspace files and no more: "hosts remain
// responsible for the effects of any resolver they attach to the context."
//
// So the resolver is replaced too. The host installs one that resolves a
// pinned `workspaces:` reference by cloning it into the CLI-managed module
// cache; a dry-run that acquired a dependency to decide it would refuse is not
// the read-only check this operation promises. Answering with a refusal costs
// the operator one explicit materialization they would have run anyway.
//
// Sources already prepared are unaffected, because they never reach a
// resolver: a module with a local path override, or one already materialized,
// resolves from disk. Only an unmaterialized pinned reference asks — and that
// is exactly the acquisition being declined.
func inspectionContext(ctx context.Context) context.Context {
	return resources.WithWorkspaceResolver(
		resources.WithWorkspaceMigrationRefusal(ctx),
		func(_ context.Context, ref *resources.WorkspaceReference) (string, error) {
			name := "a workspace reference"
			if ref != nil {
				name = strconv.Quote(ref.Name)
			}
			return "", fmt.Errorf("--retire-service-key checks the destination against the composition, and %s is not materialized here; "+
				"this check acquires nothing, so materialize it first and import again", name)
		},
	)
}

// composes reports whether the workspace actually composes the qualified// composes reports whether the workspace actually composes the qualified
// <module>/<service> destination.
func composes(services []*resources.ServiceWithModule, destination string) bool {
	module, service, qualified := strings.Cut(destination, "/")
	if !qualified {
		return false
	}
	for _, found := range services {
		if found != nil && found.Module == module && found.Name == service {
			return true
		}
	}
	return false
}

func removeMapKey(mapping *yaml.Node, key string) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
			return
		}
	}
}

func environmentLabel(envNode *yaml.Node) string {
	if name := yamledit.MapValue(envNode, "name"); name != nil {
		return strconv.Quote(name.Value)
	}
	return "the selected environment"
}
