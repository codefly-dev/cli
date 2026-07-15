package kinds

import (
	"context"
	"strings"
	"testing"
)

func TestServiceCommandReturnsErrors(t *testing.T) {
	if ServiceCmd.RunE == nil || ServiceCmd.Run != nil {
		t.Fatal("agent kind service command must return errors through RunE")
	}
	if err := ServiceCmd.Args(ServiceCmd, []string{"extra"}); err == nil {
		t.Fatal("agent kind service command accepted positional arguments")
	}
	if err := serviceInfo(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "--agent is required") {
		t.Fatalf("empty agent error = %v", err)
	}
}
