package expose

import (
	"strings"
	"testing"
)

func TestServiceCommandReturnsNotImplementedError(t *testing.T) {
	if ServiceCmd.RunE == nil || ServiceCmd.Run != nil {
		t.Fatal("expose service command is not exclusively RunE")
	}
	err := ServiceCmd.RunE(ServiceCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("RunE error = %v", err)
	}
}
