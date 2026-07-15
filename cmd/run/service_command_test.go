package run

import "testing"

func TestServiceCommandReturnsErrors(t *testing.T) {
	if ServiceCmd.RunE == nil || ServiceCmd.Run != nil {
		t.Fatal("run service must return errors through RunE")
	}
	if err := ServiceCmd.Args(ServiceCmd, []string{"one", "two"}); err == nil {
		t.Fatal("run service accepted two service selectors")
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
