package deploy

import "testing"

func TestInitCommandReturnsMissingWorkspaceError(t *testing.T) {
	if InitCmd.RunE == nil || InitCmd.Run != nil {
		t.Fatal("deploy init is not using RunE exclusively")
	}
	if err := InitCmd.Args(InitCmd, []string{"unexpected"}); err == nil {
		t.Fatal("unexpected argument was accepted")
	}

	t.Chdir(t.TempDir())
	if err := setup(); err == nil {
		t.Fatal("setup without workspace returned success")
	}
}
