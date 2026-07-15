package common

import (
	"context"
	"os"
	"testing"
)

func TestLoadWithServicePathOverrideEReturnsWorkspaceError(t *testing.T) {
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	if _, _, _, err := LoadWithServicePathOverrideE(context.Background(), "."); err == nil {
		t.Fatal("path override unexpectedly succeeded outside a workspace")
	}
}

func TestWithSilenceERejectsNilWorkspace(t *testing.T) {
	if err := WithSilenceE(context.Background(), nil, nil); err == nil {
		t.Fatal("WithSilenceE accepted a nil workspace")
	}
}
