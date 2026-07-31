package provider

import (
	"context"
	"testing"

	hostprovider "github.com/codefly-dev/cli/pkg/provider"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func doctorSession(doc *hostprovider.Document) *session {
	return &session{
		ctx:      context.Background(),
		env:      &resources.Environment{Name: "local"},
		document: doc,
		result:   hostprovider.NewResult("doctor", "local"),
	}
}

func docWith(env string, bindings ...*hostprovider.Binding) *hostprovider.Document {
	m := map[string]*hostprovider.Binding{}
	for _, b := range bindings {
		m[b.Name] = b
	}
	return &hostprovider.Document{
		Schema:       hostprovider.BindingsSchemaV0,
		Environments: map[string]map[string]*hostprovider.Binding{env: m},
	}
}

func validManaged(name string) *hostprovider.Binding {
	return &hostprovider.Binding{Name: name, Provider: "acme/stripe:1.2.3", Mode: hostprovider.ModeManaged}
}

// Diagnosing an environment with no bindings is a success, not a failure:
// there is nothing to observe. Before the fix it reported
// coordinator_unavailable (exit 1).
func TestDiagnose_NoBindingsIsSuccess(t *testing.T) {
	for _, name := range []string{"missing file", "no bindings for env"} {
		t.Run(name, func(t *testing.T) {
			doc := (*hostprovider.Document)(nil)
			if name == "no bindings for env" {
				doc = docWith("production", validManaged("billing"))
			}
			s := doctorSession(doc)
			s.diagnose(nil)
			require.Equal(t, hostprovider.StatusSuccess, s.result.Status)
			require.Equal(t, 0, s.result.Status.ExitCode())
			require.Empty(t, s.result.Diagnostics)
		})
	}
}

// A present, valid binding still cannot run remote checks, so it gates.
func TestDiagnose_ValidBindingGates(t *testing.T) {
	s := doctorSession(docWith("local", validManaged("billing")))
	s.diagnose(nil)
	require.Equal(t, hostprovider.StatusInvalid, s.result.Status)
	require.Equal(t, 1, s.result.Status.ExitCode())
	require.Equal(t, hostprovider.CodeCoordinatorUnavailable, s.result.Diagnostics[0].Code)
}

// An invalid binding reports its validation findings and never masks them with
// the coordinator-unavailable gate.
func TestDiagnose_InvalidBindingReportsFindings(t *testing.T) {
	s := doctorSession(docWith("local", &hostprovider.Binding{Name: "billing", Provider: "no-version", Mode: hostprovider.ModeManaged}))
	s.diagnose(nil)
	require.Equal(t, hostprovider.StatusInvalid, s.result.Status)
	for _, d := range s.result.Diagnostics {
		require.NotEqual(t, hostprovider.CodeCoordinatorUnavailable, d.Code, "gate must not fire when a binding is invalid")
	}
}

// A named binding that is not declared fails closed with binding_not_found.
func TestDiagnose_NamedBindingMissing(t *testing.T) {
	s := doctorSession(docWith("local", validManaged("billing")))
	s.diagnose([]string{"absent"})
	require.True(t, s.failed)
	require.Equal(t, hostprovider.CodeBindingNotFound, s.result.Diagnostics[0].Code)
}
