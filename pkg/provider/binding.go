package provider

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

const (
	// BindingsFileName is the workspace-root document that declares external
	// provider bindings. It is a Codefly-owned sidecar rather than a field on
	// the core Environment resource, so an older CLI that does not understand
	// providers simply never reads it. `codefly doctor workspace` and
	// `codefly provider doctor` surface that silent-ignore risk explicitly.
	BindingsFileName = "provider-bindings.codefly.yaml"

	// BindingsSchemaV0 is the only schema this CLI understands. An unknown
	// schema is rejected rather than silently ignored.
	BindingsSchemaV0 = "codefly.provider-bindings/v0"
)

// ErrUnknownSchema is returned by LoadDocument when the bindings file declares a
// schema this CLI does not understand. Callers classify it distinctly so an old
// CLI reading a newer document surfaces a version diagnostic instead of silently
// ignoring the bindings.
var ErrUnknownSchema = errors.New("unknown provider-bindings schema")

// ManagementMode declares how the host reconciles a binding.
type ManagementMode string

const (
	// ModeObserve permits remote reads only; no mutation is ever expressed.
	ModeObserve ManagementMode = "observe"
	// ModeManaged permits host-driven remote mutation through the coordinator.
	ModeManaged ManagementMode = "managed"
	// ModeDisabled declares an inert binding; no agent is ever started for it.
	ModeDisabled ManagementMode = "disabled"
)

// DeletionPolicy declares what `codefly provider destroy` may remove.
type DeletionPolicy string

const (
	// DeletionRetain is the default: remote resources are never deleted.
	DeletionRetain DeletionPolicy = "retain"
	// DeletionDeleteOwned permits deleting exactly the resources this binding
	// owns or has explicitly adopted.
	DeletionDeleteOwned DeletionPolicy = "delete-owned"
)

// Document is the parsed provider-bindings.codefly.yaml. Bindings are
// environment-scoped: the outer key is the environment name (matching the
// --env selector), the inner key is the stable binding name.
type Document struct {
	Schema       string                         `yaml:"schema"`
	Environments map[string]map[string]*Binding `yaml:"environments"`
}

// Binding is one external provider binding. Name and Environment are populated
// from the document's map keys at load time and are not part of the YAML.
type Binding struct {
	Provider       string            `yaml:"provider"`
	Mode           ManagementMode    `yaml:"mode"`
	DeletionPolicy DeletionPolicy    `yaml:"deletion-policy,omitempty"`
	Input          map[string]string `yaml:"input,omitempty"`
	Output         *Output           `yaml:"output,omitempty"`
	Spec           map[string]any    `yaml:"spec,omitempty"`
	Endpoints      []string          `yaml:"endpoints,omitempty"`
	APIOrigin      string            `yaml:"api-origin,omitempty"`
	Sink           *Sink             `yaml:"sink,omitempty"`

	Name        string `yaml:"-"`
	Environment string `yaml:"-"`
}

// Output declares the configuration contract a binding projects into.
type Output struct {
	Contract      string `yaml:"contract"`
	Configuration string `yaml:"configuration,omitempty"`
}

// Sink declares the durable secret-sink and state policy for a binding.
type Sink struct {
	State string `yaml:"state,omitempty"`
}

// LoadDocument reads the provider-bindings document from the workspace
// directory. The bool reports whether the file exists; a missing file is not an
// error — providers are opt-in. A present but malformed or unknown-schema
// document is an error.
func LoadDocument(dir string) (*Document, bool, error) {
	data, err := os.ReadFile(filepath.Join(dir, BindingsFileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}

	var doc Document
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&doc); err != nil {
		return nil, true, fmt.Errorf("cannot parse %s: %w", BindingsFileName, err)
	}
	if doc.Schema != BindingsSchemaV0 {
		return &doc, true, fmt.Errorf("%w: %q declares %q, this CLI understands %q — upgrade codefly", ErrUnknownSchema, BindingsFileName, doc.Schema, BindingsSchemaV0)
	}

	for envName, bindings := range doc.Environments {
		for name, binding := range bindings {
			binding.Name = name
			binding.Environment = envName
		}
	}
	return &doc, true, nil
}

// ForEnvironment returns the bindings declared for an environment, sorted by
// name for deterministic output.
func (d *Document) ForEnvironment(env string) []*Binding {
	if d == nil {
		return nil
	}
	declared, ok := d.Environments[env]
	if !ok {
		return nil
	}
	bindings := make([]*Binding, 0, len(declared))
	for _, binding := range declared {
		bindings = append(bindings, binding)
	}
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].Name < bindings[j].Name })
	return bindings
}

// EffectiveDeletionPolicy resolves the default: an unset policy retains.
func (b *Binding) EffectiveDeletionPolicy() DeletionPolicy {
	if b.DeletionPolicy == "" {
		return DeletionRetain
	}
	return b.DeletionPolicy
}

// View builds the redacted, deterministic projection of a binding. Input values
// are dropped; only their keys survive, so no value can leak.
func (b *Binding) View(valid bool) BindingView {
	view := BindingView{
		Name:           b.Name,
		Provider:       b.Provider,
		Mode:           string(b.Mode),
		DeletionPolicy: string(b.EffectiveDeletionPolicy()),
		Endpoints:      append([]string(nil), b.Endpoints...),
		Valid:          valid,
	}
	if b.Output != nil {
		view.OutputContract = b.Output.Contract
	}
	for key := range b.Input {
		view.InputKeys = append(view.InputKeys, key)
	}
	sort.Strings(view.InputKeys)
	return view
}

// Binding returns the named binding within an environment.
func (d *Document) Binding(env, name string) (*Binding, bool) {
	if d == nil {
		return nil, false
	}
	declared, ok := d.Environments[env]
	if !ok {
		return nil, false
	}
	binding, ok := declared[name]
	return binding, ok
}
