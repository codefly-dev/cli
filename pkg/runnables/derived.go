package runnables

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	// IndexSchema versions the derived-operation index.
	IndexSchema = "codefly/runnable-operations/v1"
	// IndexFileName is the index a composition reads to prepare bindings.
	IndexFileName = "index.json"
	// PackageFileName is the canonical package one derived operation carries.
	PackageFileName = "runnable-package.json"
	// OperationFileName is the execution policy and authority installed beside
	// that package rather than digested into it.
	OperationFileName = "operation.json"
	// DerivedDir is the module-relative root of the derived tree.
	DerivedDir = "contracts/runnables"
)

// Index is the set of Runnable operations a module derives from the methods
// its published contracts mark. It is the file a composition reads to prepare
// bindings: every row locates a package and states the method it adapts.
type Index struct {
	Schema     string       `json:"schema"`
	Workspace  string       `json:"workspace"`
	Module     string       `json:"module"`
	Operations []IndexEntry `json:"operations"`
}

// IndexEntry is one derived operation. Version identifies the release; Digest
// is what a binding pins, because the package's content is what a consumer
// agreed to and the version only has to be a valid identity.
type IndexEntry struct {
	Name          string `json:"name"`
	Version       string `json:"version"`
	Digest        string `json:"digest"`
	Service       string `json:"service"`
	Endpoint      string `json:"endpoint"`
	Method        string `json:"method"`
	InputMessage  string `json:"input_message"`
	OutputMessage string `json:"output_message"`
	Path          string `json:"path"`
}

// Source spells where an operation was derived from, as list and show report
// it: the service, the endpoint it is published on, and the method's own name.
func (e *IndexEntry) Source() string {
	method := e.Method
	if cut := strings.LastIndex(method, "/"); cut >= 0 {
		method = method[cut+1:]
	}
	return strings.Join([]string{e.Service, e.Endpoint, method}, "/")
}

// ValidateRelativePath rejects a path that does not stay inside the tree it is
// resolved against: absolute, backslash-separated, NUL-bearing, or carrying a
// "." or ".." segment. It is applied to every path read out of an index and to
// every path written into the tree, so neither side depends on the other
// having checked.
//
// Lexical checking alone is not containment — a symlink inside the tree still
// resolves outside it — so readers additionally resolve through os.Root. This
// exists so a bad path is refused with a message naming it, rather than
// surfacing as an opaque openat failure.
func ValidateRelativePath(label, value string) error {
	if value == "" || filepath.IsAbs(value) || strings.ContainsAny(value, "\\\x00") || strings.HasPrefix(value, "/") {
		return fmt.Errorf("%s %q is not a path inside the module", label, value)
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("%s %q is not a path inside the module", label, value)
		}
	}
	return nil
}

// LoadIndex reads a module's derived-operation index.
//
// A module that has generated nothing has no contracts/runnables directory at
// all, which is not an error: nothing derived is a valid answer and the caller
// still lists what the module declares. A directory that exists WITHOUT an
// index is an error — the packages under it are operations the module
// publishes, and answering "none" for them would hide a module's whole derived
// surface behind one deleted file.
func LoadIndex(moduleDir string) (*Index, error) {
	derivedDir := filepath.Join(moduleDir, filepath.FromSlash(DerivedDir))
	data, err := os.ReadFile(filepath.Join(derivedDir, IndexFileName))
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		entries, dirErr := os.ReadDir(derivedDir)
		if dirErr != nil {
			if os.IsNotExist(dirErr) {
				return nil, nil
			}
			return nil, dirErr
		}
		if len(entries) == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("%s holds %d entrie(s) but no %s; run `codefly generate runnables` to rewrite it",
			derivedDir, len(entries), IndexFileName)
	}
	var index Index
	if err = json.Unmarshal(data, &index); err != nil {
		return nil, fmt.Errorf("cannot read derived runnable index: %w", err)
	}
	if index.Schema != IndexSchema {
		return nil, fmt.Errorf("derived runnable index schema %q is not %s", index.Schema, IndexSchema)
	}
	for i := range index.Operations {
		if err = ValidateRelativePath("derived runnable path", index.Operations[i].Path); err != nil {
			return nil, err
		}
	}
	return &index, nil
}

// Operation is the execution policy and authority one derived method
// declares. It is written beside the package rather than inside it because
// policy and authority are installed with a binding: one contract may be
// installed twice under different policies, so digesting them into the package
// would make two installations of one contract two releases.
type Operation struct {
	Method         string   `json:"method"`
	AttemptTimeout string   `json:"attempt_timeout"`
	TotalTimeout   string   `json:"total_timeout"`
	MaxAttempts    uint32   `json:"max_attempts"`
	Backoff        string   `json:"backoff"`
	RetryableCodes []string `json:"retryable_codes,omitempty"`
	Audience       string   `json:"audience"`
	InvokeScopes   []Scope  `json:"invoke_scopes"`
	LookupScopes   []Scope  `json:"lookup_scopes"`
	LookupMethod   string   `json:"lookup_method,omitempty"`
}

// Scope is one authority a binding is minted for.
type Scope struct {
	ResourceKind string   `json:"resource_kind"`
	Actions      []string `json:"actions"`
	ResourceIDs  []string `json:"resource_ids,omitempty"`
}

// Derived is one operation a module derived: the index row that names it, the
// package it points at, the policy installed beside that package, and the
// owner agent that package records.
type Derived struct {
	Entry     IndexEntry
	Package   *basev0.RunnablePackage
	Operation *Operation
	// Agent is the owner agent's identifier, resolved when the package is
	// read. A package whose agent cannot be read fails the load rather than
	// listing blank: a listing that cannot say what builds an operation is
	// reporting a broken package as a working one.
	Agent string
}

// LoadDerivedOperations reads every operation a module derived. A module that
// derived none answers with nothing, not an error.
func LoadDerivedOperations(moduleDir string) ([]Derived, error) {
	index, err := LoadIndex(moduleDir)
	if err != nil || index == nil {
		return nil, err
	}
	operations := make([]Derived, 0, len(index.Operations))
	for i := range index.Operations {
		operation, loadErr := LoadDerived(moduleDir, &index.Operations[i])
		if loadErr != nil {
			return nil, loadErr
		}
		operations = append(operations, operation)
	}
	return operations, nil
}

// LoadDerived reads the files one index row points at. It is separate from
// LoadDerivedOperations so a caller looking for one operation by name pays for
// — and fails on — only the row it asked about: a broken package belonging to
// some other operation must not fail a question that was never about it.
func LoadDerived(moduleDir string, entry *IndexEntry) (Derived, error) {
	root, err := os.OpenRoot(moduleDir)
	if err != nil {
		return Derived{}, err
	}
	defer func() { _ = root.Close() }()

	pkg := &basev0.RunnablePackage{}
	if err = readDerivedFile(root, entry, PackageFileName, func(data []byte) error {
		return protojson.Unmarshal(data, pkg)
	}); err != nil {
		return Derived{}, err
	}
	operation := &Operation{}
	if err = readDerivedFile(root, entry, OperationFileName, func(data []byte) error {
		return json.Unmarshal(data, operation)
	}); err != nil {
		return Derived{}, err
	}
	agent, err := resources.AgentFromProto(pkg.GetAgent())
	if err != nil {
		return Derived{}, fmt.Errorf("derived runnable %s records an agent that cannot be read: %w", entry.Name, err)
	}
	return Derived{Entry: *entry, Package: pkg, Operation: operation, Agent: agent.Identifier()}, nil
}

// readDerivedFile reads one of a row's files THROUGH an os.Root anchored at
// the module directory. Resolving inside the root is what makes the read safe:
// an index is data on disk — in a composed workspace it arrives inside a
// third-party module package — so a row naming "../../../.ssh/id_rsa", or a
// symlink pointing there, must not be followed. A decode failure quotes the
// bytes it choked on, which is why reaching outside the module at all is the
// thing that has to be impossible rather than merely unlikely.
func readDerivedFile(root *os.Root, entry *IndexEntry, name string, decode func([]byte) error) error {
	relative := path.Join(entry.Path, name)
	file, err := root.Open(relative)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(file)
	if err != nil {
		return err
	}
	if err = decode(data); err != nil {
		return fmt.Errorf("cannot read %s: %w", filepath.Join(root.Name(), filepath.FromSlash(relative)), err)
	}
	return nil
}

// Identity projects a derived operation into the shape the declared runnables
// are listed in, so one listing answers "what can this module be asked to do"
// without the caller knowing which rows were authored. The facility spellings
// are core's own YAML ones, so a derived row and a declared row name the same
// facility the same way.
func (d *Derived) Identity() Identity {
	return Identity{
		Module:    d.Package.GetIdentity().GetModule(),
		Name:      d.Package.GetIdentity().GetName(),
		Version:   d.Package.GetIdentity().GetVersion(),
		Agent:     d.Agent,
		Protocol:  d.Package.GetContract().GetProtocol(),
		Execution: newPackageExecution(d.Package.GetExecution()),
		Source:    d.Entry.Source(),
	}
}

func newPackageExecution(execution *basev0.RunnableExecution) Execution {
	facilities := make([]string, 0, len(execution.GetFacilities()))
	for _, facility := range execution.GetFacilities() {
		spelling, known := resources.RunnableFacilityOf(facility.GetKind())
		if !known {
			spelling = resources.RunnableFacility(facility.GetKind().String())
		}
		facilities = append(facilities, string(spelling))
	}
	return Execution{
		Facilities:     facilities,
		Timeout:        execution.GetTimeout().AsDuration().String(),
		Cancellation:   cancellationSpelling(execution.GetCancellation()),
		Recovery:       recoverySpelling(execution.GetRecovery()),
		Concurrency:    execution.GetConcurrency(),
		MaxInputBytes:  execution.GetMaxInputBytes(),
		MaxOutputBytes: execution.GetMaxOutputBytes(),
	}
}

func cancellationSpelling(cancellation basev0.RunnableExecution_Cancellation) string {
	if cancellation == basev0.RunnableExecution_CANCELLATION_SIGNAL {
		return string(resources.RunnableCancellationSignal)
	}
	return string(resources.RunnableCancellationNone)
}

func recoverySpelling(recovery basev0.RunnableExecution_Recovery) string {
	if recovery == basev0.RunnableExecution_RECOVERY_RECEIPT {
		return string(resources.RunnableRecoveryReceipt)
	}
	return string(resources.RunnableRecoveryRecompute)
}

// CanonicalJSON returns the canonical proto3 JSON form of a message: proto
// field names, object keys sorted, no whitespace. It is the form core takes a
// package digest over, so the file on disk and the bytes that were hashed to
// produce its digest are the same bytes.
//
// protojson deliberately varies its whitespace between calls, so marshalling
// straight to a file would make every --check run report drift that is not
// there.
//
// TODO(codefly-dev/core#552): call runnable.CanonicalJSON once core exports
// the normalization its digestOf performs, and delete this copy.
func CanonicalJSON(message proto.Message) ([]byte, error) {
	encoded, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(message)
	if err != nil {
		return nil, err
	}
	var generic any
	if err := json.Unmarshal(encoded, &generic); err != nil {
		return nil, err
	}
	return json.Marshal(generic)
}
