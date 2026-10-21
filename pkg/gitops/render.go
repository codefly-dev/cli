package gitops

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	coreservices "github.com/codefly-dev/core/agents/services"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
	"sigs.k8s.io/kustomize/kyaml/openapi"
	kyaml "sigs.k8s.io/kustomize/kyaml/yaml"
)

var (
	digestImagePattern = regexp.MustCompile(`^.+@sha256:[a-fA-F0-9]{64}$`)
	digestPattern      = regexp.MustCompile(`^sha256:[a-fA-F0-9]{64}$`)
	placeholderPattern = regexp.MustCompile(`(?i)(\$\{[^}]+\}|\{\{[^}]+\}\}|<<[^>]+>>|\bCHANGE_?ME\b|\bREPLACE_?ME\b)`)
)

const argoAPIGroup = "argoproj.io"
const overlaysDirectory = "overlays"
const rbacAPIGroup = "rbac.authorization.k8s.io"

type manifest struct {
	path  string
	group string
	kind  string
	value map[string]any
	// Destination namespace supplied by the applying Application when output omits it.
	namespace *string
}

type kustomization struct {
	path       string
	directory  string
	references []string
}

type projectContract struct {
	name             string
	present          bool
	declared         bool
	destinations     map[string]struct{}
	clusterResources map[string]struct{}
}

func RenderOwnedTree(ctx context.Context, opts *RenderOptions, generate func(context.Context, string) error) (RenderResult, error) {
	if opts.Destination == "" {
		return RenderResult{}, fmt.Errorf("render destination is required")
	}
	destination, err := filepath.Abs(opts.Destination)
	if err != nil {
		return RenderResult{}, fmt.Errorf("resolve render destination: %w", err)
	}
	parent := filepath.Dir(destination)
	if err = os.MkdirAll(parent, 0o755); err != nil {
		return RenderResult{}, fmt.Errorf("create render parent: %w", err)
	}
	parentRoot, err := os.OpenRoot(parent)
	if err != nil {
		return RenderResult{}, err
	}
	defer parentRoot.Close()
	stageName := ".codefly-render-" + uuid.NewString()
	if err = parentRoot.Mkdir(stageName, 0o755); err != nil {
		return RenderResult{}, err
	}
	defer func() { _ = parentRoot.RemoveAll(stageName) }()
	stage := filepath.Join(parent, stageName)
	owned := filepath.Join(stage, "tree")
	if err = parentRoot.Mkdir(filepath.Join(stageName, "tree"), 0o755); err != nil {
		return RenderResult{}, fmt.Errorf("create staged owned tree: %w", err)
	}
	tree, err := newRenderRoot(owned)
	if err != nil {
		return RenderResult{}, err
	}
	defer tree.Close()
	opts.renderRoot = tree
	defer func() { opts.renderRoot = nil }()
	owned = tree.path
	if err = generate(ctx, owned); err != nil {
		return RenderResult{}, fmt.Errorf("generate staged manifests: %w", err)
	}
	if err = tree.inspect(owned); err != nil {
		return RenderResult{}, err
	}
	// Before anything measures or validates the staged tree: a promotable render
	// deploys into a namespace provisioned outside it, so no service unit may
	// ship a Namespace claiming that namespace. See service_namespace.go.
	elided, err := elideProvisionedNamespaces(owned, opts)
	if err != nil {
		return RenderResult{}, err
	}
	// The workload credential is projected into each identity workload's
	// declared container, and the module manifest written, BEFORE validateTree
	// so the ConfigMap that carries it is held to the same promotable ruleset
	// as the rest of the tree, and before buildInventory so both are hashed
	// into the render digest like any other delivered file.
	if err = projectWorkloadIdentity(tree, owned, opts); err != nil {
		return RenderResult{}, err
	}
	if err = projectServiceMesh(tree, owned, opts); err != nil {
		return RenderResult{}, err
	}
	// Before the manifest and the validation read the tree: a Job or CronJob
	// an agent rendered without pod labels is given the two that name it,
	// so every pod-producing workload is one the cell file can select.
	if err = labelPodTemplates(ctx, owned); err != nil {
		return RenderResult{}, err
	}
	declared, err := renderModuleManifest(owned, destination, opts)
	if err != nil {
		return RenderResult{}, err
	}
	opts.ModuleManifestPath = deliveredManifestPath(declared)
	manifests, err := validateTree(owned, opts)
	if err != nil {
		return RenderResult{}, err
	}
	sizing := computeSizing(manifests)
	inventory, err := buildInventory(owned, opts)
	if err != nil {
		return RenderResult{}, err
	}
	// The unit graph and its Kubernetes output evidence are checked HERE, where
	// the inventory is produced and installed. Every other entry point that
	// reads an inventory validates it; this one wrote and installed its own
	// without doing so, so a unit graph that no loader would accept — including
	// output evidence that is not restricted — was still what the tree shipped
	// with. A producer that validates less than its readers is the fault, not
	// the readers.
	if err = validateInventoryUnits(&inventory); err != nil {
		return RenderResult{}, fmt.Errorf("validate rendered inventory: %w", err)
	}
	canonical, err := json.MarshalIndent(inventory, "", "  ")
	if err != nil {
		return RenderResult{}, fmt.Errorf("encode render inventory: %w", err)
	}
	canonical = append(canonical, '\n')
	// The inventory contains public manifest identities and must remain inspectable beside the rendered files.
	if err := tree.write(owned, InventoryFilename, canonical); err != nil {
		return RenderResult{}, fmt.Errorf("write render inventory: %w", err)
	}
	// A dev deployment is only ever cleared by a render; read what the tree
	// being replaced carried so the caller can say so. An unreadable previous
	// inventory is not this render's concern: the tree is replaced whole.
	var cleared []InventoryDevDeployment
	if previous, previousErr := LoadInventory(destination); previousErr == nil {
		cleared = previous.Dev
	}
	if err := tree.checkIdentity(); err != nil {
		return RenderResult{}, err
	}
	if err := replaceOwnedTree(parentRoot, filepath.Join(stageName, "tree"), filepath.Base(destination)); err != nil {
		return RenderResult{}, err
	}
	return RenderResult{
		Path: destination, Inventory: inventory, Sizing: sizing,
		ElidedNamespaces: elided, ClearedDev: cleared,
		ModuleManifest: declared, UndeclaredManifest: opts.UndeclaredManifest,
	}, nil
}

func LoadInventory(root string) (Inventory, error) {
	return loadInventory(root, "render")
}

func loadInventory(directory, label string) (Inventory, error) {
	data, err := readWithin(directory, InventoryFilename)
	if err != nil {
		return Inventory{}, fmt.Errorf("read %s inventory: %w", label, err)
	}
	return decodeInventory(data, label)
}

// decodeInventory validates and decodes an inventory's already-read bytes. It
// is the reusable half of loadInventory: admission resolves an exposing
// module's inventory from a git blob (git show <baseBranch>:<path>) rather
// than a working-tree file, since a long-lived promotion branch only ever
// refreshes the one module path it is republishing — every sibling module's
// working-tree copy can be stale relative to the base branch — so it needs
// the exact same canonical/schema validation without going through a path.
// ErrUnsupportedInventorySchema is an inventory written at another schema
// version: a tree rendered by another CLI, which this one cannot read and must
// not vouch for. A reader that walks sibling trees reports such a tree as one
// to render again rather than refusing to proceed, so a schema move converges
// module by module instead of deadlocking on every sibling at once.
var ErrUnsupportedInventorySchema = errors.New("unsupported render inventory schema")

func decodeInventory(data []byte, label string) (Inventory, error) {
	var inventory Inventory
	if err := json.Unmarshal(data, &inventory); err != nil {
		return Inventory{}, fmt.Errorf("decode %s inventory: %w", label, err)
	}
	if inventory.SchemaVersion != SchemaVersion {
		return Inventory{}, fmt.Errorf("%w: %s inventory schema %d (this CLI reads %d)", ErrUnsupportedInventorySchema, label, inventory.SchemaVersion, SchemaVersion)
	}
	canonical, err := json.MarshalIndent(inventory, "", "  ")
	if err != nil {
		return Inventory{}, fmt.Errorf("encode %s inventory: %w", label, err)
	}
	canonical = append(canonical, '\n')
	if !bytes.Equal(data, canonical) {
		return Inventory{}, fmt.Errorf("%s inventory is not canonical", label)
	}
	return inventory, nil
}

func ValidateRenderedTree(root, project string, promotable bool) error {
	tree, err := newRenderRoot(root)
	if err != nil {
		return err
	}
	defer tree.Close()
	root = tree.path
	data, err := tree.read(root, InventoryFilename)
	if err != nil {
		return err
	}
	inventory, err := decodeInventory(data, "render")
	if err != nil {
		return err
	}
	if project == "" {
		project = inventory.AppProject
	} else if inventory.AppProject != project {
		return fmt.Errorf("render inventory AppProject %q differs from selected AppProject %q", inventory.AppProject, project)
	}
	if err = validateInventoryUnits(&inventory); err != nil {
		return err
	}
	opts := &RenderOptions{
		Module: inventory.Module, Unit: inventory.Unit,
		renderRoot:         tree,
		ModuleManifestPath: inventory.ModuleManifestPath,
		OwnedPath:          inventory.OwnedPath, ModulePath: inventory.ModulePath, Units: inventory.Units,
		Environment: inventory.Environment, Namespace: inventory.Namespace,
		AppProject: project, Promotable: promotable, Package: inventory.Package, Pin: inventory.Pin,
		CheckUnitDirectories: inventory.Unit == "",
	}
	for idx := range inventory.Units {
		unit := &inventory.Units[idx]
		if unit.Path != "" {
			opts.UnitNames = append(opts.UnitNames, unit.Name)
		}
	}
	if _, err = validateTree(root, opts); err != nil {
		return err
	}
	actual, err := buildInventory(root, opts)
	if err != nil {
		return err
	}
	return validateInventory(&inventory, &actual, "render")
}

func ValidateServiceSnapshot(root string) error {
	tree, err := newRenderRoot(root)
	if err != nil {
		return err
	}
	defer tree.Close()
	root = tree.path
	data, err := tree.read(root, InventoryFilename)
	if err != nil {
		return err
	}
	inventory, err := decodeInventory(data, "render")
	if err != nil {
		return err
	}
	if err = validateInventoryUnits(&inventory); err != nil {
		return err
	}
	// The render record at the root, the inventory, is the snapshot's own.
	allowed := map[string]struct{}{InventoryFilename: {}, moduleBundleDir: {}}
	// The delivery overlays the inventory records are part of the snapshot:
	// the Applications that deliver them read the snapshot revision.
	if inventory.ModuleManifestPath != "" {
		allowed[inventory.ModuleManifestPath] = struct{}{}
	}
	rendered := renderedUnits(&inventory)
	for idx := range rendered {
		unit := &rendered[idx]
		directory, ok := unitDirectory(unit.Kind)
		if !ok {
			return fmt.Errorf("service snapshot unit %s has unknown kind %q", unit.Name, unit.Kind)
		}
		allowed[directory] = struct{}{}
	}
	entries, err := tree.readDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if _, ok := allowed[entry.Name()]; !ok {
			return fmt.Errorf("service snapshot contains unexpected path %s", entry.Name())
		}
	}
	if err = validateUnitDirectoriesInTree(tree, root, rendered, inventory.Environment); err != nil {
		return err
	}
	if err = validateSnapshotCoverage(&inventory); err != nil {
		return err
	}
	names := make([]string, 0, len(rendered))
	for idx := range rendered {
		unit := &rendered[idx]
		names = append(names, unit.Name)
	}
	opts := &RenderOptions{
		Module: inventory.Module, UnitNames: names,
		renderRoot:         tree,
		ModuleManifestPath: inventory.ModuleManifestPath,
		OwnedPath:          inventory.OwnedPath, ModulePath: inventory.ModulePath, Units: inventory.Units,
		Environment: inventory.Environment, Namespace: inventory.Namespace,
		AppProject: inventory.AppProject, Promotable: true, Package: inventory.Package, Pin: inventory.Pin,
	}
	// Validate the applied graph with its original root. A service may import
	// shared inputs outside its own directory, and independently applied units
	// must be compared with one another, not checked in isolated subtrees.
	if _, err = validateTree(root, opts); err != nil {
		return err
	}
	actual, err := buildInventory(root, opts)
	if err != nil {
		return err
	}
	return validateInventory(&inventory, &actual, "service snapshot")
}

func renderedUnits(inventory *Inventory) []InventoryUnit {
	rendered := make([]InventoryUnit, 0, len(inventory.Units))
	for idx := range inventory.Units {
		unit := &inventory.Units[idx]
		if unit.Path != "" {
			rendered = append(rendered, *unit)
		}
	}
	return rendered
}

func validateSnapshotCoverage(inventory *Inventory) error {
	covered := make(map[string]bool)
	if inventory.ModulePath != "" {
		covered[inventory.ModulePath] = false
	}
	if inventory.ModuleManifestPath != "" {
		covered[inventory.ModuleManifestPath] = false
	}
	for idx := range inventory.Units {
		unit := &inventory.Units[idx]
		if unit.Path != "" {
			covered[unit.Path] = false
		}
	}
	for _, file := range inventory.Files {
		owner := ""
		for servicePath := range covered {
			if file.Path == servicePath || strings.HasPrefix(file.Path, servicePath+"/") {
				if owner != "" {
					return fmt.Errorf("service snapshot file %s belongs to overlapping service paths", file.Path)
				}
				owner = servicePath
			}
		}
		if owner == "" {
			return fmt.Errorf("service snapshot file %s is outside the exact service graph", file.Path)
		}
		covered[owner] = true
	}
	for servicePath, present := range covered {
		if !present {
			return fmt.Errorf("service snapshot path %s contains no files", servicePath)
		}
	}
	return nil
}

func validateInventoryUnits(inventory *Inventory) error {
	if inventory.Unit != "" {
		if len(inventory.Units) != 0 {
			return fmt.Errorf("standalone unit render inventory must not contain a module unit graph")
		}
		return nil
	}
	previous := ""
	for idx := range inventory.Units {
		unit := &inventory.Units[idx]
		if unit.Name == "" || unit.Module != inventory.Module {
			return fmt.Errorf("render inventory contains invalid unit graph entry %q/%q", unit.Module, unit.Name)
		}
		if err := validateInventoryContracts(unit); err != nil {
			return err
		}
		directory, ok := unitDirectory(unit.Kind)
		if !ok {
			return fmt.Errorf("render inventory unit %s has unknown kind %q", unit.Name, unit.Kind)
		}
		if previous != "" && unit.Name <= previous {
			return fmt.Errorf("render inventory unit graph is not strictly sorted")
		}
		previous = unit.Name
		expectedPath := filepath.ToSlash(filepath.Join(directory, unit.Name))
		if unit.Managed {
			if unit.Bootstrap {
				if unit.Path != expectedPath {
					return fmt.Errorf("managed bootstrap unit %s render path is %q, expected %q", unit.Name, unit.Path, expectedPath)
				}
				if inventory.OwnedPath != "" {
					if err := validateInventoryKubernetesOutput(unit.Name, unit.Output); err != nil {
						return err
					}
				}
			} else if unit.Path != "" || unit.Output != nil {
				return fmt.Errorf("managed unit %s without bootstrap output must not contain a rendered path or output", unit.Name)
			}
			continue
		}
		if unit.Path != expectedPath {
			return fmt.Errorf("unit %s render path is %q, expected %q", unit.Name, unit.Path, expectedPath)
		}
		if inventory.OwnedPath == "" {
			continue
		}
		if err := validateInventoryKubernetesOutput(unit.Name, unit.Output); err != nil {
			return err
		}
	}
	return nil
}

// validateInventoryContracts checks the structural well-formedness of a unit's
// recorded contract provenance: a known role and a well-formed digest. Cross-
// module resolution (whether a consumed contract is actually satisfied by the
// exposing module) needs the target GitOps checkout and lives in plan/publish.
func validateInventoryContracts(unit *InventoryUnit) error {
	for i := range unit.Contracts {
		contract := &unit.Contracts[i]
		switch contract.Role {
		case ContractRoleExposes, ContractRoleConsumes:
		default:
			return fmt.Errorf("unit %s contract for %s/%s has unknown role %q", unit.Name, contract.Service, contract.Endpoint, contract.Role)
		}
		if contract.Module == "" || contract.Service == "" || contract.Endpoint == "" {
			return fmt.Errorf("unit %s contract is missing module, service, or endpoint", unit.Name)
		}
		// contract.Module names a foreign module and, for a consumed contract, is
		// used to build a filesystem path when admission resolves it against the
		// target GitOps checkout (resolveGitopsModuleInventory). Rejecting an
		// unsafe value here, at render time, means a render can never even produce
		// an inventory a later admission bypass could exploit.
		if err := validatePathComponent("contract module", contract.Module); err != nil {
			return fmt.Errorf("unit %s contract for %s/%s: %w", unit.Name, contract.Service, contract.Endpoint, err)
		}
		if !digestPattern.MatchString(contract.Digest) {
			return fmt.Errorf("unit %s contract for %s/%s has invalid digest %q", unit.Name, contract.Service, contract.Endpoint, contract.Digest)
		}
	}
	return nil
}

func validateInventoryKubernetesOutput(service string, output *KubernetesOutputInventory) error {
	if output == nil {
		return fmt.Errorf("service %s has no restricted Kubernetes output evidence", service)
	}
	if output.Kind != builderv0.KubernetesDeploymentOutput_KUSTOMIZE.String() ||
		output.Profile != builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_RESTRICTED_PORTABLE_V1.String() ||
		output.ContractVersion != coreservices.KubernetesManifestContractVersion {
		return fmt.Errorf("service %s has incompatible Kubernetes output evidence", service)
	}
	if output.Validation == nil {
		return fmt.Errorf("service %s has no restricted Kubernetes validation evidence", service)
	}
	passed := builderv0.KubernetesManifestValidation_STATUS_PASSED.String()
	notRun := builderv0.KubernetesManifestValidation_STATUS_NOT_RUN.String()
	serverSideValidation := output.Validation.ServerSideValidation
	if output.Validation.StaticValidation != passed ||
		(serverSideValidation != passed && serverSideValidation != notRun) ||
		!output.Validation.Restricted ||
		output.Validation.Violations == nil ||
		len(output.Validation.Violations) != 0 {
		return fmt.Errorf("service %s has failed restricted Kubernetes validation evidence", service)
	}
	return nil
}

func validateInventory(inventory, actual *Inventory, label string) error {
	if actual.Digest != inventory.Digest {
		return fmt.Errorf("%s digest changed: inventory has %s, tree has %s", label, inventory.Digest, actual.Digest)
	}
	if len(actual.Files) != len(inventory.Files) {
		return fmt.Errorf("%s inventory changed: inventory has %d files, tree has %d", label, len(inventory.Files), len(actual.Files))
	}
	for i := range actual.Files {
		if actual.Files[i] != inventory.Files[i] {
			return fmt.Errorf("%s inventory changed at %s", label, actual.Files[i].Path)
		}
	}
	return nil
}

// validateTree validates a rendered owned tree and returns its effective
// manifest set — kustomize-built output for anything a kustomization covers,
// raw manifests for everything else. The effective set is the single source of
// truth callers reuse (e.g. sizing) instead of walking and building the tree a
// second time.
func validateTree(root string, opts *RenderOptions) ([]manifest, error) {
	tree, err := inspectRenderRoot(root, opts)
	if err != nil {
		return nil, err
	}
	defer releaseRenderRoot(tree, opts)
	root = tree.path
	if err = validateRenderDirectories(tree, root, opts); err != nil {
		return nil, err
	}
	var manifests []manifest
	var kustomizations []kustomization
	err = tree.walkRegularFiles(root, func(_, relative string, _ os.FileInfo) error {
		// The render record at the tree root is not a manifest.
		if relative == InventoryFilename {
			return nil
		}
		data, readErr := tree.read(root, relative)
		if readErr != nil {
			return fmt.Errorf("read %s: %w", relative, readErr)
		}
		if !utf8.Valid(data) {
			return fmt.Errorf("%s is not UTF-8", relative)
		}
		extension := strings.ToLower(filepath.Ext(relative))
		manifestFile := extension == yamlExtension || extension == ymlExtension || extension == jsonExtension
		var decoded []manifest
		var customization *kustomization
		if manifestFile {
			decoded, customization, err = decodeYAML(relative, data)
			if err != nil {
				return err
			}
		}
		// The CLI-owned bootstrap ApplicationSet template legitimately carries Argo
		// Go-template expressions ({{.tenant}}, {{.server}}, …); every other file —
		// including any ApplicationSet outside the bootstrap path — is held to the
		// no-unresolved-placeholder rule.
		// An ExternalSecret's target template is an External Secrets expression
		// by design; the per-value inspection admits one exactly there, so its
		// file is held to that narrower rule instead of this whole-file one.
		if placeholderPattern.Match(data) && !isBootstrapApplicationSet(relative, decoded) && !onlyExternalSecrets(decoded) {
			return fmt.Errorf("%s contains an unresolved placeholder", relative)
		}
		if manifestFile {
			manifests = append(manifests, decoded...)
			if customization != nil {
				kustomizations = append(kustomizations, *customization)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(manifests) == 0 && len(kustomizations) == 0 {
		return nil, fmt.Errorf("rendered tree contains no Kubernetes manifests")
	}
	contract, err := selectProjectContract(manifests, opts.AppProject)
	if err != nil {
		return nil, err
	}
	if contract != nil && !contract.present && opts.ModulePath != "" && opts.Namespace != "" {
		contract.destinations[opts.Namespace] = struct{}{}
		contract.clusterResources["/Namespace"] = struct{}{}
	}
	for _, item := range manifests {
		if validationErr := validateManifest(item, contract, false); validationErr != nil {
			return nil, fmt.Errorf("%s: %w", item.path, validationErr)
		}
	}
	if !opts.Promotable {
		return manifests, nil
	}
	effective, err := promotableEffectiveManifests(tree, root, manifests, kustomizations, contract, opts)
	if err != nil {
		return nil, err
	}
	if err = validateObjectIdentities(effective, opts.Namespace); err != nil {
		return nil, err
	}
	return effective, nil
}

// promotableEffectiveManifests builds the kustomize output, validates the
// promotable ruleset against both the built output and any manifests no
// kustomization covers, without counting covered source files a second time.
func promotableEffectiveManifests(tree *renderRoot, root string, manifests []manifest, kustomizations []kustomization, contract *projectContract, opts *RenderOptions) ([]manifest, error) {
	covered, effective, err := renderKustomizations(tree, root, manifests, kustomizations, opts)
	if err != nil {
		return nil, err
	}
	for _, item := range effective {
		if err := validateManifest(item, contract, true); err != nil {
			return nil, fmt.Errorf("%s: %w", item.path, err)
		}
	}
	result := append([]manifest(nil), effective...)
	for _, item := range manifests {
		source := strings.SplitN(item.path, "#", 2)[0]
		if covered[source] {
			continue
		}
		if err := validateManifest(item, contract, true); err != nil {
			return nil, fmt.Errorf("%s: %w", item.path, err)
		}
		result = append(result, item)
	}
	return result, nil
}

func validateRenderDirectories(tree *renderRoot, root string, opts *RenderOptions) error {
	if opts.Unit == "" && opts.CheckUnitDirectories {
		return validateUnitDirectoriesInTree(tree, root, renderedUnits(&Inventory{Units: opts.Units}), opts.Environment)
	}
	return nil
}

func validateUnitDirectoriesInTree(tree *renderRoot, root string, units []InventoryUnit, environment string) error {
	byDirectory := make(map[string]map[string]struct{})
	for idx := range units {
		unit := &units[idx]
		directory, ok := unitDirectory(unit.Kind)
		if !ok {
			return fmt.Errorf("rendered unit %s has unknown kind %q", unit.Name, unit.Kind)
		}
		if _, exists := byDirectory[directory]; !exists {
			byDirectory[directory] = make(map[string]struct{})
		}
		byDirectory[directory][unit.Name] = struct{}{}
	}
	directories := make([]string, 0, len(byDirectory))
	for directory := range byDirectory {
		directories = append(directories, directory)
	}
	sort.Strings(directories)
	for _, directory := range directories {
		unitRoot := filepath.Join(root, directory)
		entries, err := tree.readDir(unitRoot)
		if err != nil {
			return fmt.Errorf("read rendered unit graph: %w", err)
		}
		expected := make(map[string]struct{}, len(byDirectory[directory]))
		for name := range byDirectory[directory] {
			expected[name] = struct{}{}
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				return fmt.Errorf("rendered unit graph contains unexpected file %s", entry.Name())
			}
			if _, exists := expected[entry.Name()]; !exists {
				return fmt.Errorf("rendered unit graph contains unexpected unit %s", entry.Name())
			}
			delete(expected, entry.Name())
		}
		if len(expected) > 0 {
			missing := make([]string, 0, len(expected))
			for name := range expected {
				missing = append(missing, name)
			}
			sort.Strings(missing)
			return fmt.Errorf("rendered unit graph is missing units %v", missing)
		}
		for name := range byDirectory[directory] {
			info, err := tree.stat(unitRoot, filepath.Join(name, "overlays", environment))
			if err != nil {
				return fmt.Errorf("unit %s environment overlay %s: %w", name, environment, err)
			}
			if !info.IsDir() {
				return fmt.Errorf("unit %s environment overlay %s is not a directory", name, environment)
			}
		}
	}
	return nil
}

func decodeYAML(path string, data []byte) ([]manifest, *kustomization, error) {
	var manifests []manifest
	var customization *kustomization
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	for document := 1; ; document++ {
		var value any
		err := decoder.Decode(&value)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("%s document %d: decode YAML: %w", path, document, err)
		}
		if value == nil {
			continue
		}
		root, ok := value.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("%s document %d: YAML root must be a mapping", path, document)
		}
		if strings.EqualFold(filepath.Ext(path), ".json") && root["apiVersion"] == nil && root["kind"] == nil {
			continue
		}
		base := strings.ToLower(filepath.Base(path))
		if base == "kustomization.yaml" || base == "kustomization.yml" || base == "kustomization" || root["kind"] == kindKustomization {
			if customization != nil {
				return nil, nil, fmt.Errorf("%s contains multiple Kustomization documents", path)
			}
			found, kustomizeErr := validateKustomization(path, root)
			if kustomizeErr != nil {
				return nil, nil, kustomizeErr
			}
			customization = &kustomization{
				path: path, directory: filepath.ToSlash(filepath.Dir(filepath.FromSlash(path))),
				references: found,
			}
			continue
		}
		decoded, err := decodeManifest(path, fmt.Sprintf("%d", document), root)
		if err != nil {
			return nil, nil, err
		}
		manifests = append(manifests, decoded...)
	}
	return manifests, customization, nil
}

func decodeManifest(path, location string, root map[string]any) ([]manifest, error) {
	apiVersion, _ := root["apiVersion"].(string)
	kind, _ := root["kind"].(string)
	if apiVersion == "" || kind == "" {
		return nil, fmt.Errorf("%s document %s: Kubernetes manifest requires apiVersion and kind", path, location)
	}
	if apiVersion == "v1" && kind == "List" {
		items, ok := root["items"].([]any)
		if !ok {
			return nil, fmt.Errorf("%s document %s: Kubernetes List items must be an array", path, location)
		}
		var manifests []manifest
		for index, raw := range items {
			item, ok := raw.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s document %s item %d: Kubernetes manifest must be a mapping", path, location, index)
			}
			decoded, err := decodeManifest(path, fmt.Sprintf("%s.items[%d]", location, index), item)
			if err != nil {
				return nil, err
			}
			manifests = append(manifests, decoded...)
		}
		return manifests, nil
	}
	group := apiVersion
	if slash := strings.IndexByte(group, '/'); slash >= 0 {
		group = group[:slash]
	} else {
		group = ""
	}
	return []manifest{{path: fmt.Sprintf("%s#%s", path, location), group: group, kind: kind, value: root}}, nil
}

func validateKustomization(path string, root map[string]any) ([]string, error) {
	if generators, ok := root["secretGenerator"].([]any); ok && len(generators) > 0 {
		return nil, fmt.Errorf("%s: kustomize secretGenerator values are not allowed", path)
	}
	if err := inspectValue(root, nil, false); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var references []string
	for _, key := range kustomizeFileListKeys {
		values, _ := root[key].([]any)
		for _, raw := range values {
			value, ok := raw.(string)
			if !ok {
				continue
			}
			if parsed, err := url.Parse(value); err == nil && parsed.Scheme != "" {
				return nil, fmt.Errorf("%s: remote kustomize %s %q is not allowed", path, key, value)
			}
			clean := filepath.Clean(filepath.Join(filepath.Dir(filepath.FromSlash(path)), filepath.FromSlash(value)))
			if filepath.IsAbs(filepath.FromSlash(value)) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
				return nil, fmt.Errorf("%s: kustomize %s %q escapes the owned tree", path, key, value)
			}
			if key == resourcesKey || key == "bases" || key == "components" {
				references = append(references, filepath.ToSlash(clean))
			}
		}
	}
	images, _ := root["images"].([]any)
	for _, raw := range images {
		image, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := image["name"].(string)
		digest, _ := image["digest"].(string)
		if digest == "" {
			continue
		}
		if !digestPattern.MatchString(digest) {
			return nil, fmt.Errorf("%s: kustomize image %q has invalid digest %q", path, name, digest)
		}
	}
	return references, nil
}

func renderKustomizations(tree *renderRoot, root string, manifests []manifest, kustomizations []kustomization, opts *RenderOptions) (map[string]bool, []manifest, error) {
	covered := map[string]bool{}
	byDirectory := map[string]kustomization{}
	for _, customization := range kustomizations {
		byDirectory[customization.directory] = customization
	}
	// Source documents are inputs, not independently applied manifests.
	for _, customization := range kustomizations {
		markKustomizationCoverage(customization, byDirectory, covered, map[string]bool{})
	}
	pending := map[appliedRoot]bool{}
	for _, applied := range inventoryAppliedRoots(opts) {
		if _, err := tree.stat(root, filepath.FromSlash(applied.directory)); os.IsNotExist(err) && !opts.CheckUnitDirectories {
			// Older direct renders inventory a plain unit directory.
			applied.directory = filepath.ToSlash(filepath.Dir(filepath.Dir(applied.directory)))
			if opts.Unit != "" && applied.directory == filepath.ToSlash(serviceRenderPath(opts.Module, opts.Unit)) {
				if _, statErr := tree.stat(root, filepath.FromSlash(applied.directory)); os.IsNotExist(statErr) {
					// A directly supplied standalone unit occupies the root;
					// RenderService uses the module-qualified flow layout.
					applied.directory = "."
				}
			}
		}
		pending[applied] = true
	}
	declared := map[appliedRoot]bool{}
	applicationDirectories := map[string]bool{}
	addApplications := func(documents []manifest) error {
		for _, doc := range documents {
			roots, err := applicationRoots(doc, opts)
			if err != nil {
				return err
			}
			for _, applied := range roots {
				pending[applied] = true
				declared[applied] = true
				applicationDirectories[applied.directory] = true
			}
		}
		return nil
	}
	var raw []manifest
	for _, doc := range manifests {
		if !covered[strings.SplitN(doc.path, "#", 2)[0]] {
			raw = append(raw, doc)
		}
	}
	if err := addApplications(raw); err != nil {
		return nil, nil, err
	}
	built := map[appliedRoot]bool{}
	outputs := map[appliedRoot][]manifest{}
	for {
		progress := false
		for _, applied := range sortedAppliedRoots(pending) {
			if built[applied] {
				continue
			}
			built[applied], progress = true, true
			directory, err := tree.checkedPath(filepath.Join(root, filepath.FromSlash(applied.directory)))
			if err != nil {
				return nil, nil, err
			}
			decoded, err := overlayManifestsInTree(tree, directory)
			if err != nil {
				return nil, nil, err
			}
			for i := range decoded {
				decoded[i].namespace = &applied.namespace
				decoded[i].path = strings.ReplaceAll(decoded[i].path, root+string(filepath.Separator), "")
				if strings.HasPrefix(decoded[i].path, "kustomize:") {
					decoded[i].path = strings.Replace(decoded[i].path, "#", "/rendered.yaml#", 1)
				}
			}
			// Plain directory sources also belong to their applying root only.
			if _, customized := byDirectory[applied.directory]; !customized {
				for _, doc := range decoded {
					covered[strings.SplitN(doc.path, "#", 2)[0]] = true
				}
			}
			outputs[applied] = decoded
			if err := addApplications(decoded); err != nil {
				return nil, nil, err
			}
		}
		if !progress {
			break
		}
	}
	// A concrete Application owns destination defaulting for its root.
	var effective []manifest
	for _, applied := range sortedAppliedRoots(pending) {
		if applicationDirectories[applied.directory] && !declared[applied] {
			continue
		}
		effective = append(effective, outputs[applied]...)
	}
	return covered, effective, nil
}

func markKustomizationCoverage(customization kustomization, byDirectory map[string]kustomization, covered, visiting map[string]bool) {
	if visiting[customization.directory] {
		return
	}
	visiting[customization.directory] = true
	for _, reference := range customization.references {
		if nested, ok := byDirectory[reference]; ok {
			markKustomizationCoverage(nested, byDirectory, covered, visiting)
		} else {
			covered[reference] = true
		}
	}
	delete(visiting, customization.directory)
}

func selectProjectContract(manifests []manifest, selected string) (*projectContract, error) {
	projects := map[string]*projectContract{}
	for _, item := range manifests {
		if item.group != argoAPIGroup || item.kind != kindAppProject {
			continue
		}
		name := metadataString(item.value, "name")
		if name == "" {
			return nil, fmt.Errorf("%s: AppProject metadata.name is required", item.path)
		}
		contract := &projectContract{name: name, present: true, destinations: map[string]struct{}{}, clusterResources: map[string]struct{}{}}
		spec, _ := item.value["spec"].(map[string]any)
		destinations, _ := spec["destinations"].([]any)
		for _, raw := range destinations {
			destination, _ := raw.(map[string]any)
			namespace, _ := destination["namespace"].(string)
			server, _ := destination["server"].(string)
			clusterName, _ := destination["name"].(string)
			if strings.Contains(namespace, "*") || strings.Contains(server, "*") || strings.Contains(clusterName, "*") {
				return nil, fmt.Errorf("%s: AppProject %s contains wildcard authority", item.path, contract.name)
			}
			if namespace != "" {
				contract.destinations[namespace] = struct{}{}
			}
		}
		whitelist, _ := spec["clusterResourceWhitelist"].([]any)
		for _, raw := range whitelist {
			resource, _ := raw.(map[string]any)
			group, _ := resource["group"].(string)
			kind, _ := resource["kind"].(string)
			if strings.Contains(group, "*") || strings.Contains(kind, "*") {
				return nil, fmt.Errorf("%s: AppProject %s contains wildcard cluster authority", item.path, contract.name)
			}
			if kind != "" {
				contract.clusterResources[group+"/"+kind] = struct{}{}
			}
		}
		projects[name] = contract
	}
	if selected != "" {
		contract, ok := projects[selected]
		if !ok {
			if len(projects) == 0 {
				return &projectContract{
					// Declared via --app-project with no manifest present: the real
					// AppProject (with snapshotAuthority-derived whitelists) is written
					// at publish time FROM this same tree, so authority is consistent
					// by construction — validate structure, not the empty whitelist.
					name: selected, declared: true, destinations: map[string]struct{}{}, clusterResources: map[string]struct{}{},
				}, nil
			}
			return nil, fmt.Errorf("selected AppProject %q is not present in rendered manifests", selected)
		}
		return contract, nil
	}
	if len(projects) == 1 {
		for _, contract := range projects {
			return contract, nil
		}
	}
	if len(projects) > 1 {
		return nil, fmt.Errorf("multiple AppProjects rendered; select one explicitly")
	}
	return nil, nil
}

func validateManifest(item manifest, contract *projectContract, promotable bool) error {
	if item.group == argoAPIGroup && item.kind == kindApplicationSet && isBootstrapApplicationSetPath(item.path) {
		return validateApplicationSet(item, contract)
	}
	if item.kind == "Secret" {
		if promotable {
			return fmt.Errorf("secret resources are not allowed")
		}
		for _, key := range []string{dataKey, "stringData"} {
			if values, ok := item.value[key].(map[string]any); ok && len(values) > 0 {
				return fmt.Errorf("values of a Kubernetes Secret are not allowed")
			}
		}
	}
	knownClusterScoped := item.knownClusterScoped()
	customClusterScoped := item.group != "" && !isBuiltInAPIGroup(item.group) &&
		item.group != argoAPIGroup && metadataString(item.value, "namespace") == ""
	if knownClusterScoped || customClusterScoped {
		if contract == nil {
			return fmt.Errorf("cluster-scoped %s is outside an AppProject contract", item.kind)
		}
		if contract.declared {
			// Flag-declared contract: publish derives its authority from this
			// tree via snapshotAuthority, so per-kind whitelist checks are
			// self-consistent by construction and skipped at render time.
			return nil
		}
		if _, allowed := contract.clusterResources[item.group+"/"+item.kind]; !allowed {
			return fmt.Errorf("cluster-scoped %s is not declared by AppProject %s", item.kind, contract.name)
		}
		if item.kind == kindNamespace {
			name := metadataString(item.value, "name")
			if _, allowed := contract.destinations[name]; !allowed {
				return fmt.Errorf("namespace %s is outside AppProject %s destinations", name, contract.name)
			}
		}
	}
	if item.group == argoAPIGroup && item.kind == "Application" && contract != nil {
		spec, _ := item.value["spec"].(map[string]any)
		project, _ := spec["project"].(string)
		if project != contract.name {
			return fmt.Errorf("application project %q differs from selected AppProject %q", project, contract.name)
		}
	}
	var allowCredentialReference, allowExpression func([]string) bool
	if item.group == externalSecretsGroup && item.kind == kindExternalSecret {
		allowCredentialReference = externalSecretCredentialReference
		allowExpression = externalSecretTemplateDataPath
	}
	return inspectValueAllowingExpressions(item.value, nil, promotable, allowCredentialReference, allowExpression)
}

// onlyExternalSecrets reports whether a decoded file holds ExternalSecrets and
// nothing else.
func onlyExternalSecrets(manifests []manifest) bool {
	if len(manifests) == 0 {
		return false
	}
	for _, item := range manifests {
		if item.group != externalSecretsGroup || item.kind != kindExternalSecret {
			return false
		}
	}
	return true
}

// externalSecretTemplateDataPath is spec.target.template.data.<key> of an
// ExternalSecret: a value External Secrets evaluates in the cluster over the
// keys the ExternalSecret reads, never a value the tree carries.
func externalSecretTemplateDataPath(path []string) bool {
	return len(path) == 5 &&
		path[0] == "spec" &&
		path[1] == keyPatchTarget &&
		path[2] == "template" &&
		path[3] == dataKey
}

// externalSecretTemplateAction reports whether a template value computes from
// the secret it is delivered into. One with no action is a literal, and a
// literal under a credential-named key is a credential in the tree.
func externalSecretTemplateAction(value any) bool {
	text, ok := value.(string)
	return ok && strings.Contains(text, "{{") && strings.Contains(text, "}}")
}

// bootstrapApplicationSetPath is the single tree location the promotion driver
// writes its ApplicationSet to; only that file earns the placeholder exemption.
const (
	bootstrapDir                = "bootstrap"
	bootstrapApplicationSetPath = bootstrapDir + "/applicationset.yaml"
)

// isBootstrapApplicationSet reports whether a decoded file is the CLI-owned
// bootstrap ApplicationSet — identified by both its path and its exclusive
// ApplicationSet content — whose Argo Go-template body is exempt from the codefly
// placeholder rule. An ApplicationSet anywhere else is not exempt.
func isBootstrapApplicationSet(relative string, manifests []manifest) bool {
	if filepath.ToSlash(relative) != bootstrapApplicationSetPath {
		return false
	}
	if len(manifests) == 0 {
		return false
	}
	for _, item := range manifests {
		if item.group != argoAPIGroup || item.kind != kindApplicationSet {
			return false
		}
	}
	return true
}

// isBootstrapApplicationSetPath reports whether a decoded manifest originates in
// the CLI-owned top-level bootstrap directory — either the raw ApplicationSet file
// or the Kustomize-rendered bootstrap output. Manifest paths carry a "kustomize:"
// prefix and/or a #document suffix that are trimmed before the directory check, so
// an ApplicationSet rendered from any other directory (e.g. a service overlay) is
// not treated as CLI-owned.
func isBootstrapApplicationSetPath(path string) bool {
	clean := strings.TrimPrefix(filepath.ToSlash(strings.SplitN(path, "#", 2)[0]), "kustomize:")
	return filepath.ToSlash(filepath.Dir(clean)) == bootstrapDir
}

// validateApplicationSet holds the bootstrap ApplicationSet to the same promotion
// authority as the Applications it replaces — its template project must match the
// selected AppProject and every concrete authority value stays credential-free,
// wildcard-free, and safely schemed — while tolerating the Argo Go-template
// expressions its generators resolve at stamp time.
func validateApplicationSet(item manifest, contract *projectContract) error {
	spec, _ := item.value["spec"].(map[string]any)
	template, _ := spec["template"].(map[string]any)
	templateSpec, _ := template["spec"].(map[string]any)
	project, _ := templateSpec["project"].(string)
	if contract != nil {
		switch project {
		case contract.name:
		case "{{ .project }}":
			// Stamped per component: every element names the selected project.
			if err := validateComponentProjects(spec, contract.name); err != nil {
				return err
			}
		default:
			return fmt.Errorf("ApplicationSet template project %q differs from selected AppProject %q", project, contract.name)
		}
	}
	return inspectTemplatedValue(item.value, nil)
}

// validateComponentProjects walks the list generators' elements of a bootstrap
// ApplicationSet and holds each component to the selected project.
func validateComponentProjects(spec map[string]any, selected string) error {
	var walk func(value any) error
	walk = func(value any) error {
		switch typed := value.(type) {
		case map[string]any:
			if elements, ok := typed["elements"].([]any); ok {
				for _, raw := range elements {
					element, _ := raw.(map[string]any)
					// The fields the Application template consumes, every
					// one required: the overlay is the path Argo syncs, the
					// project its authority. An element naming none of them
					// is another generator's — the tenant matrix — and skipped.
					named := 0
					for _, key := range []string{"component", "overlay", "project", "namespace"} {
						if _, present := element[key]; present {
							named++
						}
					}
					if named == 0 {
						continue
					}
					project, _ := element["project"].(string)
					component, _ := element["component"].(string)
					overlay, _ := element["overlay"].(string)
					namespace, _ := element["namespace"].(string)
					if component == "" || overlay == "" || project == "" || namespace == "" {
						return fmt.Errorf("ApplicationSet component %q (overlay %q, project %q, namespace %q) lacks a component, overlay, project or namespace, or carries one that is not a string; every stamped Application needs all four", component, overlay, project, namespace)
					}
					if project != selected {
						return fmt.Errorf("ApplicationSet component %q is stamped under project %q, which is not the selected AppProject %q", component, project, selected)
					}
				}
			}
			for _, child := range typed {
				if err := walk(child); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range typed {
				if err := walk(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(spec["generators"])
}

// inspectTemplatedValue applies the authority and credential guards of
// inspectValueAllowingReferences while skipping strings that carry an Argo
// Go-template expression, whose value is resolved per tenant outside the snapshot.
func inspectTemplatedValue(value any, path []string) error {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			next := extendPath(path, key)
			if isCredentialKey(key, isConfigurationDataPath(path)) && scalarHasValue(child) {
				return fmt.Errorf("%s contains credential value", strings.Join(next, "."))
			}
			if err := inspectTemplatedValue(child, next); err != nil {
				return err
			}
		}
	case []any:
		for index, child := range typed {
			if err := inspectTemplatedValue(child, extendPath(path, fmt.Sprintf("[%d]", index))); err != nil {
				return err
			}
		}
	case string:
		if strings.Contains(typed, "{{") {
			return nil
		}
		if isURLBearingPath(path) {
			if err := validateURLValue(strings.Join(path, "."), typed); err != nil {
				return err
			}
		}
		if isAuthorityPath(path) && strings.Contains(typed, "*") {
			return fmt.Errorf("%s contains wildcard authority", strings.Join(path, "."))
		}
	}
	return nil
}

func isBuiltInAPIGroup(group string) bool {
	switch group {
	case "apps", "autoscaling", "batch", "coordination.k8s.io", "discovery.k8s.io",
		"events.k8s.io", "extensions", "networking.k8s.io", "policy",
		rbacAPIGroup, "scheduling.k8s.io", "storage.k8s.io":
		return true
	default:
		return false
	}
}

func inspectValue(value any, path []string, promotable bool) error {
	return inspectValueAllowingReferences(value, path, promotable, nil)
}

func inspectValueAllowingReferences(
	value any,
	path []string,
	promotable bool,
	allowCredentialReference func([]string) bool,
) error {
	return inspectValueAllowingExpressions(value, path, promotable, allowCredentialReference, nil)
}

// inspectValueAllowingExpressions is inspectValueAllowingReferences that also
// admits, at the paths allowExpression names, a template expression evaluated
// in the cluster: it may carry template delimiters, and may sit under a
// credential-named key as long as it computes from something rather than
// being a literal.
func inspectValueAllowingExpressions(
	value any,
	path []string,
	promotable bool,
	allowCredentialReference func([]string) bool,
	allowExpression func([]string) bool,
) error {
	expressionAt := func(path []string, value any) bool {
		return allowExpression != nil && allowExpression(path) && externalSecretTemplateAction(value)
	}
	switch typed := value.(type) {
	case map[string]any:
		if name, ok := typed["name"].(string); ok && isCredentialKey(name, true) && scalarHasValue(typed["value"]) {
			return fmt.Errorf("%s.value contains credential value", strings.Join(path, "."))
		}
		for key, child := range typed {
			next := extendPath(path, key)
			if isCredentialKey(key, isConfigurationDataPath(path)) && scalarHasValue(child) &&
				(allowCredentialReference == nil || !allowCredentialReference(next)) && !expressionAt(next, child) {
				return fmt.Errorf("%s contains credential value", strings.Join(next, "."))
			}
			if key == "image" && promotable {
				image, ok := child.(string)
				if ok && !digestImagePattern.MatchString(image) {
					return fmt.Errorf("%s image %q is not digest-pinned", strings.Join(next, "."), image)
				}
			}
			if err := inspectValueAllowingExpressions(child, next, promotable, allowCredentialReference, allowExpression); err != nil {
				return err
			}
		}
	case []any:
		for index, child := range typed {
			if err := inspectValueAllowingExpressions(
				child,
				extendPath(path, fmt.Sprintf("[%d]", index)),
				promotable,
				allowCredentialReference,
				allowExpression,
			); err != nil {
				return err
			}
		}
	case string:
		if placeholderPattern.MatchString(typed) && !expressionAt(path, typed) {
			return fmt.Errorf("%s contains an unresolved placeholder", strings.Join(path, "."))
		}
		if isURLBearingPath(path) {
			if err := validateURLValue(strings.Join(path, "."), typed); err != nil {
				return err
			}
		}
		if isAuthorityPath(path) && strings.Contains(typed, "*") {
			return fmt.Errorf("%s contains wildcard authority", strings.Join(path, "."))
		}
	}
	return nil
}

func externalSecretCredentialReference(path []string) bool {
	return len(path) == 4 &&
		path[0] == "spec" &&
		path[1] == dataKey &&
		strings.HasPrefix(path[2], "[") &&
		path[3] == "secretKey"
}

func isURLBearingPath(path []string) bool {
	for index := len(path) - 1; index >= 0; index-- {
		part := path[index]
		if strings.HasPrefix(part, "[") {
			continue
		}
		normalized := strings.ToLower(strings.NewReplacer("-", "", "_", "", ".", "").Replace(part))
		return normalized == "server" ||
			normalized == "sourcerepos" ||
			strings.HasSuffix(normalized, "url") ||
			strings.HasSuffix(normalized, "uri")
	}
	return false
}

func extendPath(path []string, part string) []string {
	extended := make([]string, len(path)+1)
	copy(extended, path)
	extended[len(path)] = part
	return extended
}

func validateURLValue(path, value string) error {
	if digestPattern.MatchString(value) {
		return nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" {
		return nil
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https", "grpcs", "ssh":
	default:
		return fmt.Errorf("%s contains unsafe URL scheme %q", path, parsed.Scheme)
	}
	if parsed.User != nil {
		return fmt.Errorf("%s URL contains credentials", path)
	}
	if strings.Contains(parsed.Hostname(), "*") {
		return fmt.Errorf("%s URL contains wildcard authority", path)
	}
	return nil
}

// manifestFieldCredentialFragments name credential material inside a manifest
// SCHEMA field name (clientSecret, privateKey, accessKey), matched against the
// separator-free lowercase spelling. They are a different concept from core's
// configuration-name classifier and exist only because core's cannot see them:
// core splits on separators, so a camelCase field such as privateKey is one word
// to it, and applying core's wider marker list (SECRET, SESSION, CONNECTION,
// AUTH, ...) to schema fields would refuse ordinary Kubernetes structure such as
// volumes[].secret.secretName or spec.sessionAffinity. TOKEN is deliberately
// absent: whether a key naming a token is a credential is core's decision (see
// isCredentialKey).
var manifestFieldCredentialFragments = []string{
	"password", "passwd", "credential", "privatekey", "clientsecret", "accesskey", "secretkey",
}

var manifestFieldNormalizer = strings.NewReplacer("-", "", "_", "", ".", "")

// isCredentialKey reports whether a manifest key names credential material, so a
// non-empty inline value under it is a leaked credential.
//
// Core is the single source of truth for configuration names — an env entry's
// name, a key of a ConfigMap/Secret data map. Those are environment variable
// names, and many are codefly carriers whose name embeds module, service and
// endpoint names, so they go through resources.IsSensitiveEnvironmentVariable,
// which classifies the configuration key a carrier holds and never its
// structure: CODEFLY__ENDPOINT__SAAS__AUTH_GATEWAY__GRPC__GRPC is an address,
// not a credential, although the service is called auth-gateway. A plaintext
// value the guard sees under a name core calls sensitive escaped promotion, and
// one core does not (MAX_OUTPUT_TOKENS: a model output-token limit, core#645)
// is ordinary configuration the guard must not refuse.
//
// Every key, configuration name or schema field, is also checked against the
// camelCase field fragments above, and any key mentioning a token defers to
// core, which keeps every TOKEN sensitive except a whole-word TOKENS count.
//
// A codefly configuration carrier is scanned on its configuration KEY alone,
// read by core's one parser of a carrier (resources.ConfigurationCarrierKey):
// the module, service and configuration-name segments in front of it are
// structure, so a group called credential-policy or client-secret-policy
// does not make its CLIENT_ID a credential, exactly as core classifies it.
// Anything that is not a carrier is scanned whole, as before.
func isCredentialKey(key string, configurationName bool) bool {
	classify := resources.IsSensitiveKey
	scanned := key
	if configurationName {
		classify = resources.IsSensitiveEnvironmentVariable
		if classify(key) {
			return true
		}
		if configurationKey, ok := resources.ConfigurationCarrierKey(key); ok {
			scanned = configurationKey
		}
	}
	normalized := strings.ToLower(manifestFieldNormalizer.Replace(scanned))
	for _, fragment := range manifestFieldCredentialFragments {
		if strings.Contains(normalized, fragment) {
			return true
		}
	}
	return strings.Contains(normalized, "token") && classify(key)
}

// isConfigurationDataPath reports whether the map at path holds configuration
// names as its keys: the data/stringData map of a ConfigMap or Secret.
func isConfigurationDataPath(path []string) bool {
	if len(path) != 1 {
		return false
	}
	return path[0] == dataKey || path[0] == "stringData"
}

func scalarHasValue(value any) bool {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed) != ""
	case []byte:
		return len(typed) > 0
	default:
		return false
	}
}

func isAuthorityPath(path []string) bool {
	for _, part := range path {
		switch strings.ToLower(part) {
		case "sourcerepos", "sourcenamespaces", "destination", "destinations",
			"clusterresourcewhitelist", "namespaceresourcewhitelist",
			"apigroups", "resources", "verbs", "nonresourceurls":
			return true
		}
	}
	if len(path) == 0 {
		return false
	}
	key := strings.ToLower(path[len(path)-1])
	return key == "host" || key == "hostname" || key == "server" || key == "address" || key == "url" || key == "repourl"
}

func metadataString(value map[string]any, key string) string {
	metadata, _ := value["metadata"].(map[string]any)
	result, _ := metadata[key].(string)
	return result
}

// inventoryHead is the inventory as the render options declare it, before the
// tree's files are measured: what the cell record is derived from.
func inventoryHead(opts *RenderOptions) Inventory {
	inventory := Inventory{
		SchemaVersion: SchemaVersion,
		Module:        opts.Module, Unit: opts.Unit, Environment: opts.Environment,
		Namespace: opts.Namespace, AppProject: opts.AppProject, OwnedPath: filepath.ToSlash(opts.OwnedPath),
		ModulePath: filepath.ToSlash(opts.ModulePath), Package: opts.Package, Pin: opts.Pin,
		ModuleManifestPath: filepath.ToSlash(opts.ModuleManifestPath),
		HostsDelivery:      opts.HostsDelivery || hostsDeliveryAPI(opts),
		Units:              append([]InventoryUnit(nil), opts.Units...),
	}
	inventory.Delivery = opts.Delivered
	inventory.MeshEdges = opts.MeshEdges
	if len(opts.WorkspaceConfigurationDigests) > 0 {
		inventory.WorkspaceConfigurationDigests = make(map[string]string, len(opts.WorkspaceConfigurationDigests))
		for group, digest := range opts.WorkspaceConfigurationDigests {
			inventory.WorkspaceConfigurationDigests[group] = digest
		}
	}
	if len(inventory.Units) == 0 {
		serviceDir, _ := unitDirectory(UnitKindService)
		for _, name := range opts.UnitNames {
			inventory.Units = append(inventory.Units, InventoryUnit{
				Kind:   UnitKindService,
				Module: opts.Module, Name: name,
				Path: filepath.ToSlash(filepath.Join(serviceDir, name)),
			})
		}
	}
	sort.Slice(inventory.Units, func(i, j int) bool {
		return inventory.Units[i].Name < inventory.Units[j].Name
	})
	return inventory
}

func buildInventory(root string, opts *RenderOptions) (Inventory, error) {
	tree, err := inspectRenderRoot(root, opts)
	if err != nil {
		return Inventory{}, err
	}
	defer releaseRenderRoot(tree, opts)
	inventory := inventoryHead(opts)
	hash := sha256.New()
	err = tree.walkRegularFiles(root, func(_, relative string, info os.FileInfo) error {
		if relative == InventoryFilename {
			return nil
		}
		file, openErr := tree.open(root, relative)
		if openErr != nil {
			return fmt.Errorf("open %s: %w", relative, openErr)
		}
		defer file.Close()
		fileHash := sha256.New()
		if _, copyErr := io.Copy(fileHash, file); copyErr != nil {
			return fmt.Errorf("hash %s: %w", relative, copyErr)
		}
		digest := hex.EncodeToString(fileHash.Sum(nil))
		inventory.Files = append(inventory.Files, InventoryFile{Path: filepath.ToSlash(relative), SHA256: "sha256:" + digest, Size: info.Size()})
		return nil
	})
	if err != nil {
		return Inventory{}, err
	}
	sort.Slice(inventory.Files, func(i, j int) bool { return inventory.Files[i].Path < inventory.Files[j].Path })
	inventory.Digest = inventoryDigest(hash, inventory.Files)
	return inventory, nil
}

// inventoryDigest is the render digest: one line per file, in path order.
func inventoryDigest(hash hash.Hash, files []InventoryFile) string {
	for _, file := range files {
		fmt.Fprintf(hash, "%s\x00%s\x00%d\n", file.Path, file.SHA256, file.Size)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

// filesOutside is the inventory's files that are not under directory.
func filesOutside(files []InventoryFile, directory string) []InventoryFile {
	if directory == "" {
		return files
	}
	kept := make([]InventoryFile, 0, len(files))
	for _, file := range files {
		if file.Path == directory || strings.HasPrefix(file.Path, directory+"/") {
			continue
		}
		kept = append(kept, file)
	}
	return kept
}

// validateInventoryOutside validates the tree against its inventory with one
// directory left out of the comparison on both sides: the delivery overlay,
// which publish owns and settles after a render (the signed manifest, its
// delivery Job, the kustomization that lists them). A render records the
// overlay as it provisionally wrote it; publish replaces it and the branch
// carries publish's version, so a tree is "edited since its render" only
// when a file the render owns moved.
func validateInventoryOutside(inventory, actual *Inventory, directory, label string) error {
	recorded := Inventory{Files: filesOutside(inventory.Files, directory)}
	recorded.Digest = inventoryDigest(sha256.New(), recorded.Files)
	measured := Inventory{Files: filesOutside(actual.Files, directory)}
	measured.Digest = inventoryDigest(sha256.New(), measured.Files)
	return validateInventory(&recorded, &measured, label)
}

func walkRegularFiles(root string, visit func(path, relative string, info os.FileInfo) error) error {
	// The walk is confined to root: an entry that resolves outside it is
	// refused by the operating system, and a symbolic link is refused here
	// before anything follows it.
	confined, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer confined.Close()
	return fs.WalkDir(confined.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		relative := filepath.FromSlash(name)
		if entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s: symbolic links are not allowed in rendered output", relative)
		}
		if entry.IsDir() {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s: non-regular files are not allowed in rendered output", relative)
		}
		return visit(filepath.Join(root, relative), relative, info)
	})
}

func replaceOwnedTree(root *os.Root, stage, destination string) error {
	if _, err := root.Lstat(destination); err == nil {
		if err = exchangeDirectoriesInRoot(root, stage, destination); err != nil {
			return fmt.Errorf("atomically replace rendered owned tree: %w", err)
		}
		if err = root.RemoveAll(stage); err != nil {
			return fmt.Errorf("remove previous owned tree: %w", err)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect previous owned tree: %w", err)
	}
	if err := root.Rename(stage, destination); err != nil {
		return fmt.Errorf("install rendered owned tree: %w", err)
	}
	return nil
}

func copyTree(source, destination string) error {
	return filepath.Walk(source, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		return copyTreeEntry(destination, path, relative, info)
	})
}

func copyTreeEntry(destination, path, relative string, info os.FileInfo) error {
	target := filepath.Join(destination, relative)
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s: symbolic links are not allowed", relative)
	}
	if info.IsDir() {
		return os.MkdirAll(target, info.Mode().Perm())
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s: non-regular files are not allowed", relative)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	input, err := openWithin(filepath.Dir(path), filepath.Base(path))
	if err != nil {
		return err
	}
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		input.Close()
		return err
	}
	writer := bufio.NewWriter(output)
	_, copyErr := io.Copy(writer, input)
	inputErr := input.Close()
	flushErr := writer.Flush()
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	if inputErr != nil {
		return inputErr
	}
	if flushErr != nil {
		return flushErr
	}
	return closeErr
}

// hostsDeliveryAPI reports whether the module being rendered serves the
// environment's delivery API — the host's own module, whose delivery must
// follow its units rather than go beside them.
func hostsDeliveryAPI(opts *RenderOptions) bool {
	if opts.Host == nil {
		return false
	}
	module, _, _ := opts.Host.DeliveryEndpoint()
	return module != "" && module == opts.Module
}

// Authority classification uses the schema Kustomize itself uses. Object
// uniqueness never depends on this partial scope knowledge.
func (item manifest) knownClusterScoped() bool {
	apiVersion, _ := item.value[keyAPIVersion].(string)
	typeMeta := kyaml.TypeMeta{APIVersion: apiVersion, Kind: item.kind}
	if _, known := openapi.IsNamespaceScoped(typeMeta); !known {
		return clusterScopedKyamlForgets[typeMeta]
	}
	return openapi.IsCertainlyClusterScoped(typeMeta)
}

// clusterScopedKyamlForgets holds the cluster-scoped kinds kyaml v0.21.3 knows
// and the pinned v0.21.1 does not. kustomize v0.21.3's kube-openapi requires
// structured-merge-diff/v7, which breaks every client-go build, so core v0.19.4
// holds kustomize at v0.21.1; without this table an admission policy would
// read as namespaced and escape the AppProject's cluster-resource check.
// It is consulted only when kyaml has no answer, and
// TestKyamlStillForgetsTheAdmissionPolicyScopes fails once it has one, at which
// point this table is deleted.
var clusterScopedKyamlForgets = map[kyaml.TypeMeta]bool{
	{APIVersion: "admissionregistration.k8s.io/v1", Kind: "ValidatingAdmissionPolicy"}:        true,
	{APIVersion: "admissionregistration.k8s.io/v1", Kind: "ValidatingAdmissionPolicyBinding"}: true,
}
