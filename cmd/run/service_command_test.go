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
