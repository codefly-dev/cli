package run

import (
	"strings"
	"testing"
)

func TestServiceCommandReturnsErrors(t *testing.T) {
	if ServiceCmd.RunE == nil || ServiceCmd.Run != nil {
		t.Fatal("run service must return errors through RunE")
	}
	if err := ServiceCmd.Args(ServiceCmd, []string{"one", "two"}); err == nil {
		t.Fatal("run service accepted two service selectors")
	}
}

func TestServiceCommandIncludesRunProfileFlag(t *testing.T) {
	flag := ServiceCmd.Flags().Lookup("profile")
	if flag == nil {
		t.Fatal("run service has no --profile flag")
	}
	if flag.Usage != "Named workspace run profile" {
		t.Fatalf("--profile help = %q", flag.Usage)
	}
}

func TestHeadlessServiceCommandRejectsAmbiguousWorkspaceContext(t *testing.T) {
	t.Chdir("../../pkg/orchestration/testdata/module-layout")

	_, _, _, err := loadRequiredServiceForRun(t.Context(), nil, true)
	if err == nil {
		t.Fatal("headless service command accepted ambiguous workspace context")
	}
	if !strings.Contains(err.Error(), "pass the service name explicitly") ||
		!strings.Contains(err.Error(), "frontend") ||
		!strings.Contains(err.Error(), "gateway") {
		t.Fatalf("headless service command error = %q", err)
	}
}

func TestRunServiceOpenRequiresCLIServer(t *testing.T) {
	err := validateOpenDashboardFlag(true, false)
	if err == nil || err.Error() != "--open requires --cli-server" {
		t.Fatalf("validateOpenDashboardFlag(open, no cli-server) = %v, want %q", err, "--open requires --cli-server")
	}
	if err := validateOpenDashboardFlag(true, true); err != nil {
		t.Fatalf("validateOpenDashboardFlag(open, cli-server) = %v, want nil", err)
	}
	if err := validateOpenDashboardFlag(false, false); err != nil {
		t.Fatalf("validateOpenDashboardFlag(no open, no cli-server) = %v, want nil", err)
	}
}

func TestShouldIsolateInvocation(t *testing.T) {
	tests := []struct {
		name                string
		temporaryPorts      bool
		namingScopeExplicit bool
		want                bool
	}{
		{name: "disposable run takes an identity", temporaryPorts: true, want: true},
		{name: "stable run keeps the names it must find again"},
		{
			name:                "an explicit scope is the caller naming the run",
			temporaryPorts:      true,
			namingScopeExplicit: true,
		},
		{
			// `--naming-scope ""` clears a workspace-declared scope. Replacing
			// it with a generated one would answer a request for no scope with
			// a scope.
			name:                "an explicitly empty scope asks for no scope at all",
			temporaryPorts:      true,
			namingScopeExplicit: true,
		},
		{name: "a named stable run is untouched", namingScopeExplicit: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := shouldIsolateInvocation(test.temporaryPorts, test.namingScopeExplicit); got != test.want {
				t.Fatalf("shouldIsolateInvocation(%v, %v) = %v, want %v",
					test.temporaryPorts, test.namingScopeExplicit, got, test.want)
			}
		})
	}
}

func TestSetupOnlyRunsDoNotWait(t *testing.T) {
	tests := []struct {
		name     string
		loadOnly bool
		initOnly bool
		want     bool
	}{
		{name: "normal", want: true},
		{name: "load only", loadOnly: true},
		{name: "init only", initOnly: true},
		{name: "both", loadOnly: true, initOnly: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldWaitForRun(tt.loadOnly, tt.initOnly); got != tt.want {
				t.Fatalf("shouldWaitForRun() = %v, want %v", got, tt.want)
			}
		})
	}
}
