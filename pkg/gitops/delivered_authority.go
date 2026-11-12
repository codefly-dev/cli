package gitops

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	authorityv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/authority/v1"
	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
	"github.com/codefly-dev/authority/libraries/authority-core/go/decide"
	"github.com/codefly-dev/authority/libraries/authority-core/go/delivered"
	"github.com/codefly-dev/authority/libraries/authority-core/go/domain"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/workingrender"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"
)

// DeliveredAuthority is what an environment's committed tree delivers to its
// host: each module as the authority composes it from its paired delivery
// document (delivered.Module, the host's own reading), and what the module
// envelope allows of each binding. Every entry names the file it was read
// from; one that could not be read keeps the reason and is never left out,
// so a reader cannot take a failed read for a module that asks nothing.
type DeliveredAuthority struct {
	Modules map[string]DeliveredModule // by principal
	// Envelope is, per principal and binding id, what the envelope allows.
	Envelope                     map[string]map[string]domain.Binding
	EnvelopeFile, EnvelopeUnread string
}

// DeliveredModule is one module of a DeliveredAuthority.
type DeliveredModule struct {
	Module       domain.Module
	File, Unread string
}

// ReadDeliveredAuthority reads environment's module manifests and envelope in
// trees, the environment's own module trees as EnvironmentModuleTrees lists
// them, a later tree winning. It reads files only: no cluster and no
// credential.
func ReadDeliveredAuthority(trees []string, environment string) (*DeliveredAuthority, error) {
	out := &DeliveredAuthority{Modules: map[string]DeliveredModule{}}
	var files []string
	for _, tree := range trees {
		found, _ := filepath.Glob(filepath.Join(tree, "*", "module-manifests", "overlays", environment, "*.yaml"))
		files = append(files, found...)
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		if filepath.Base(file) == "module-envelope.yaml" {
			out.EnvelopeFile, out.EnvelopeUnread = file, ""
			if out.Envelope, err = envelopeBindings(data); err != nil {
				out.EnvelopeUnread = err.Error()
			}
			continue
		}
		document, held, err := pairedDocumentFromConfigMap(filepath.Base(file), data)
		if err != nil {
			out.Modules[file] = DeliveredModule{File: file, Unread: err.Error()}
			continue
		}
		if !held || document.manifest.Removed {
			continue
		}
		m, err := deliveredModule(document)
		entry := DeliveredModule{Module: m, File: file}
		if err != nil {
			entry.Unread = err.Error()
		}
		out.Modules[m.Principal] = entry
	}
	if len(out.Modules) == 0 {
		return nil, fmt.Errorf("no paired delivery document is rendered for environment %q under %s, so nothing can be said about what that cell enforces; render it: codefly deploy gitops render --all --env %s", environment, strings.Join(trees, " or "), environment)
	}
	if out.EnvelopeFile == "" {
		out.EnvelopeUnread = fmt.Sprintf("environment %q carries no module-envelope.yaml: `codefly deploy gitops publish` of the host module writes it", environment)
	}
	return out, nil
}

// deliveredModule composes a module from its paired document exactly as a
// host does: core's manifest, completed by the authority's document.
func deliveredModule(document preparedDocument) (domain.Module, error) {
	if document.request.GetAuthority() == nil {
		return delivered.Module(document.manifest, nil)
	}
	var wire compositionv1.AuthorityManifestV1
	if err := proto.Unmarshal(document.request.GetAuthority().GetDocument(), &wire); err != nil {
		return delivered.FromCore(document.manifest), fmt.Errorf("the authority's document does not read: %w", err)
	}
	doc := &delivered.Document{Module: wire.GetModule(), Generation: wire.GetGeneration(), CoreManifestDigest: wire.GetCoreManifestDigest(),
		ScopeCeilings: coreScopes(wire.GetScopeCeilings()), OwnerAware: wire.GetOwnerAware(), InstallationRole: wire.GetInstallationRole(),
		SharedKinds: wire.GetSharedKinds()}
	for _, d := range wire.GetEdgeDependencies() {
		doc.EdgeDependencies = append(doc.EdgeDependencies, domain.EdgeDependency{Module: d.GetModule(), Presence: d.GetPresence()})
	}
	for _, m := range wire.GetMethods() {
		doc.Methods = append(doc.Methods, delivered.Method{Method: m.GetMethod(), Requires: coreScopes(m.GetRequires()), AnyCheckedCaller: m.GetAnyCheckedCaller()})
	}
	for _, r := range wire.GetRoles() {
		role := delivered.Role{Name: r.GetName(), Scopes: coreScopes(r.GetScopes()), InstallationOwned: coreScopes(r.GetInstallationOwnedScopes())}
		for _, through := range r.GetThrough() {
			if role.Through == nil {
				role.Through = map[string]delivered.Scopes{}
			}
			role.Through[through.GetBinding()] = append(role.Through[through.GetBinding()], coreScopes(through.GetScopes())...)
		}
		doc.Roles = append(doc.Roles, role)
	}
	return delivered.Module(document.manifest, doc)
}

// coreScopes reads interface scopes as core's, byte for byte.
func coreScopes(in []*authorityv1.WorkScopeV1) []*basev0.WorkScopeV1 {
	out := make([]*basev0.WorkScopeV1, 0, len(in))
	for _, sc := range in {
		raw, _ := proto.Marshal(sc)
		c := &basev0.WorkScopeV1{}
		_ = proto.Unmarshal(raw, c)
		out = append(out, c)
	}
	return out
}

// envelopeBindings reads a rendered module envelope as bindings. An unknown
// field in a scope is an error: a grant this cannot read is not read as none.
func envelopeBindings(data []byte) (map[string]map[string]domain.Binding, error) {
	var holder struct {
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal(data, &holder); err != nil {
		return nil, err
	}
	var envelope struct {
		Principals map[string]struct {
			OperationAudiences map[string]struct {
				Audience string            `json:"audience"`
				Invoke   []json.RawMessage `json:"invoke_scopes"`
				Headless []json.RawMessage `json:"headless_scopes"`
				Lookup   []json.RawMessage `json:"lookup_scopes"`
			} `json:"operation_audiences"`
		} `json:"principals"`
	}
	document, named := holder.Data["module-envelope.json"]
	if !named {
		return nil, fmt.Errorf("it carries no module-envelope.json")
	}
	if err := json.Unmarshal([]byte(document), &envelope); err != nil {
		return nil, err
	}
	scopes := func(in []json.RawMessage) (decide.Scopes, error) {
		var out decide.Scopes
		for _, raw := range in {
			sc := &basev0.WorkScopeV1{}
			if err := protojson.Unmarshal(raw, sc); err != nil {
				return nil, err
			}
			out = append(out, sc)
		}
		return out, nil
	}
	out := map[string]map[string]domain.Binding{}
	for principal, entry := range envelope.Principals {
		out[principal] = map[string]domain.Binding{}
		for id, allowed := range entry.OperationAudiences {
			b := domain.Binding{ID: id, Audience: allowed.Audience}
			var errs [3]error
			b.InvokeScopes, errs[0] = scopes(allowed.Invoke)
			b.HeadlessScopes, errs[1] = scopes(allowed.Headless)
			b.LookupScopes, errs[2] = scopes(allowed.Lookup)
			for _, err := range errs {
				if err != nil {
					return nil, fmt.Errorf("principals[%s].operation_audiences[%s]: %w", principal, id, err)
				}
			}
			out[principal][id] = b
		}
	}
	return out, nil
}

var (
	tableConstant = regexp.MustCompile(`(?m)^\s*(\w+)\s+Permission = "([^"]+)"$`)
	tableMethod   = regexp.MustCompile(`(?m)^\s*"([^"]+)":\s*\{AnyCheckedCaller: (true|false), Requires: \[\]Permission\{([^}]*)\}\},?$`)
)

// PermissionTable reads a generated permissions_gen.go (codefly generate
// permissions) back as the method policies it holds: what a service's own
// test hop and handlers read. A file this cannot read as that generator's
// output is an error, never an empty table.
func PermissionTable(path string) (map[string]domain.MethodPolicy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	constants := map[string]string{}
	for _, match := range tableConstant.FindAllStringSubmatch(string(data), -1) {
		constants[match[1]] = match[2]
	}
	out := map[string]domain.MethodPolicy{}
	for _, match := range tableMethod.FindAllStringSubmatch(string(data), -1) {
		open, _ := strconv.ParseBool(match[2]) // the pattern admits only true and false
		policy := domain.MethodPolicy{AnyCheckedCaller: open}
		for _, name := range strings.FieldsFunc(match[3], func(r rune) bool { return r == ',' || r == ' ' }) {
			kind, action, ok := strings.Cut(constants[name], ":")
			if !ok {
				return nil, fmt.Errorf("%s: %s requires %s, which the file does not define", path, match[1], name)
			}
			policy.Requires = append(policy.Requires, &basev0.WorkScopeV1{ResourceKind: kind, Actions: []string{action}})
		}
		out[match[1]] = policy
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s holds no method table this reads: it is not the output of codefly generate permissions", path)
	}
	return out, nil
}

// PublishedPolicies reads what each method the module at dir publishes
// requires (MethodPolicies), as the authority's own policy type.
func PublishedPolicies(dir string) (map[string]domain.MethodPolicy, error) {
	methods, declared, err := MethodPolicies(dir)
	if err != nil {
		return nil, err
	}
	if !declared {
		return nil, fmt.Errorf("%s publishes no method policies; run codefly generate contracts", dir)
	}
	out := make(map[string]domain.MethodPolicy, len(methods))
	for _, m := range methods {
		out[m.GetMethod()] = domain.MethodPolicy{Requires: coreScopes(m.GetRequires()), AnyCheckedCaller: m.GetAnyCheckedCaller()}
	}
	return out, nil
}

// OwnDeclaration reads, from the module contract at dir, the principal the
// module is delivered under and whether it declares that it checks owners.
func OwnDeclaration(dir string) (principal string, ownerAware bool, err error) {
	contract, err := ownContract(dir)
	if err != nil {
		return "", false, err
	}
	if contract == nil {
		return "", false, fmt.Errorf("%s publishes no module contract", dir)
	}
	return contract.Principal, contract.OwnerAware, nil
}

// EnvironmentModuleTrees lists the module trees in workspace that say what
// env delivers, in the order a reader lets the later win: the environment's
// working render (workingrender.Modules), what a publish would deliver, then
// its own delivered tree, `<gitops path>/modules`, as the workspace's checkout
// holds it. Another environment's tree is never among them: a tree under a
// path env does not deliver to is another environment's, whatever overlays it
// carries, and an environment delivering to a repository of its own has no
// delivered tree in this checkout.
func EnvironmentModuleTrees(workspace *resources.Workspace, env *environments.Environment) ([]string, error) {
	trees := make([]string, 1, 2)
	trees[0] = workingrender.Modules(workspace.Dir(), env.Name)
	defaults, err := environments.WorkspaceGitops(workspace)
	if err != nil {
		return nil, err
	}
	if env.Gitops != nil && env.Gitops.RepoURL != "" && defaults != nil && defaults.RepoURL != "" && env.Gitops.RepoURL != defaults.RepoURL {
		return trees, nil
	}
	gitopsPath, err := validateRelativePath(gitopsPathOf(env, defaults))
	if err != nil {
		return nil, fmt.Errorf("gitops.path: %w", err)
	}
	return append(trees, filepath.Join(workspace.Dir(), filepath.FromSlash(deliveryModulesPath(gitopsPath)))), nil
}
