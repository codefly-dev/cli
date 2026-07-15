package models

import (
	"errors"
	"fmt"
	"testing"
)

func TestSelectRejectsEmptyEntriesWithoutOpeningTUI(t *testing.T) {
	entry, err := Select("pick one", nil)
	if err == nil || entry != nil {
		t.Fatalf("Select(nil) = (%v, %v)", entry, err)
	}
}

func TestPromptCancelledIsStableSentinel(t *testing.T) {
	if !errors.Is(fmt.Errorf("wrapped: %w", ErrPromptCancelled), ErrPromptCancelled) {
		t.Fatal("prompt cancellation sentinel is not errors.Is-compatible")
	}
}
