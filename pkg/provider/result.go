package provider

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/codefly-dev/core/tui"
)

// ResultSchemaVersion versions the JSON envelope. Bump it only when a field is
// renamed or removed; adding a field is backward-compatible.
const ResultSchemaVersion = 1

// Result is the deterministic envelope every provider command emits. It never
// carries a secret value or a raw provider body: only binding identities,
// declared references by key, and machine-readable diagnostics.
type Result struct {
	SchemaVersion int           `json:"schema_version"`
	Command       string        `json:"command"`
	Environment   string        `json:"environment,omitempty"`
	Binding       string        `json:"binding,omitempty"`
	Status        Status        `json:"status"`
	Exit          int           `json:"exit"`
	Bindings      []BindingView `json:"bindings,omitempty"`
	Schema        *SchemaDoc    `json:"schema,omitempty"`
	Diagnostics   []Diagnostic  `json:"diagnostics,omitempty"`
}

// SchemaDoc is the deterministic description of the binding format, emitted by
// `codefly provider list --schema`. It surfaces the accepted enumerations and
// the declared output contracts so authors can write bindings without guessing.
type SchemaDoc struct {
	Schema           string   `json:"schema"`
	Modes            []string `json:"modes"`
	DeletionPolicies []string `json:"deletion_policies"`
	OutputContracts  []string `json:"output_contracts"`
}

// BindingView is a redacted, deterministic projection of a binding. Input
// values are never included — only their keys — so no secret or public value
// can leak through the command surface.
type BindingView struct {
	Name           string   `json:"name"`
	Provider       string   `json:"provider"`
	Mode           string   `json:"mode"`
	DeletionPolicy string   `json:"deletion_policy"`
	OutputContract string   `json:"output_contract,omitempty"`
	Endpoints      []string `json:"endpoints,omitempty"`
	InputKeys      []string `json:"input_keys,omitempty"`
	Valid          bool     `json:"valid"`
}

// NewResult builds an envelope for a command, defaulting to success.
func NewResult(command, environment string) *Result {
	return &Result{
		SchemaVersion: ResultSchemaVersion,
		Command:       command,
		Environment:   environment,
		Status:        StatusSuccess,
	}
}

// Fail records a diagnostic and, unless a stronger status is already set,
// classifies the result as invalid.
func (r *Result) Fail(code, format string, args ...any) {
	r.Diagnostics = append(r.Diagnostics, Diagnostic{Code: code, Message: fmt.Sprintf(format, args...)})
	if r.Status == StatusSuccess {
		r.Status = StatusInvalid
	}
}

// Emit renders the result — JSON or human — and returns an error carrying the
// stable exit code for any non-success outcome. The error is a no-op for the
// process boundary beyond its exit code and machine-readable flag.
func (r *Result) Emit(jsonOut bool) error {
	r.Exit = r.Status.ExitCode()

	if jsonOut {
		payload, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(payload))
		if r.Status == StatusSuccess {
			return nil
		}
		return &commandError{status: r.Status, emitted: true, err: errors.New(string(r.Status))}
	}

	r.renderHuman()
	if r.Status == StatusSuccess {
		return nil
	}
	return &commandError{status: r.Status, err: fmt.Errorf("provider %s: %s", r.Command, r.Status)}
}

func (r *Result) renderHuman() {
	fmt.Println(tui.RenderHeader(1, fmt.Sprintf("codefly provider %s", r.Command)))
	for i := range r.Bindings {
		view := &r.Bindings[i]
		validity := "valid"
		if !view.Valid {
			validity = "invalid"
		}
		fmt.Println(tui.RenderInfo(fmt.Sprintf("%s → %s [%s, %s] (%s)", view.Name, view.Provider, view.Mode, view.DeletionPolicy, validity)))
	}
	for _, diagnostic := range r.Diagnostics {
		line := fmt.Sprintf("%s [%s]", diagnostic.Message, diagnostic.Code)
		if diagnostic.Binding != "" {
			line = fmt.Sprintf("%s: %s", diagnostic.Binding, line)
		}
		fmt.Println(tui.RenderWarning(line))
	}
}

// commandError carries a stable process exit code for a provider command
// outcome. When emitted is set, the command already printed a complete
// machine-readable payload and the process boundary must not append prose.
type commandError struct {
	status  Status
	emitted bool
	err     error
}

func (e *commandError) Error() string         { return e.err.Error() }
func (e *commandError) Unwrap() error         { return e.err }
func (e *commandError) ExitCode() int         { return e.status.ExitCode() }
func (e *commandError) MachineReadable() bool { return e.emitted }
