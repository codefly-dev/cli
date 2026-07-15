package agents

import "testing"

func TestGenerateCommandReturnsErrors(t *testing.T) {
	if GenerateCmd.RunE == nil || GenerateCmd.Run != nil {
		t.Fatal("generate command must return errors through RunE")
	}
	if err := GenerateCmd.Args(GenerateCmd, []string{"extra"}); err == nil {
		t.Fatal("generate command accepted positional arguments")
	}
}
