package common

import "testing"

func TestScopedWorkspaceName(t *testing.T) {
	tests := []struct {
		workspace string
		scope     string
		want      string
	}{
		{workspace: "warden-platform", want: "warden-platform"},
		{workspace: "warden-platform", scope: "dev", want: "warden-platform-dev"},
		{workspace: " warden-platform ", scope: " stable ", want: "warden-platform-stable"},
	}
	for _, test := range tests {
		if got := scopedWorkspaceName(test.workspace, test.scope); got != test.want {
			t.Errorf("scopedWorkspaceName(%q, %q) = %q, want %q", test.workspace, test.scope, got, test.want)
		}
	}
}
