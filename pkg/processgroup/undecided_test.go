package processgroup

import (
	"errors"
	"fmt"
	"testing"
)

func TestSplitUndecidedKeepsEverythingElseAnError(t *testing.T) {
	broken := errors.New("lock process-group registry: lock was not acquired")
	cleanup := errors.Join(
		fmt.Errorf("reconcile registered group 11: %w", undecided(11, errUnproven)),
		fmt.Errorf("reconcile registered group 12: %w", undecided(12, errUnproven)),
		fmt.Errorf("reconcile registered group 13: %w", errors.New("terminate: operation not permitted")),
		broken,
	)
	kept, err := splitUndecided(cleanup, func(pgid int) bool { return pgid == 12 })
	if len(kept) != 1 || kept[0].PGID != 11 {
		t.Fatalf("kept = %v, want only group 11", kept)
	}
	var own *UndecidedGroupError
	if !errors.As(err, &own) || own.PGID != 12 {
		t.Fatalf("the run's own undecided group 12 is not in the error: %v", err)
	}
	if !errors.Is(err, broken) || !errors.Is(err, errUnproven) {
		t.Fatalf("a failure that is not an undecided group left the error: %v", err)
	}
	if joined, ok := err.(interface{ Unwrap() []error }); !ok || len(joined.Unwrap()) != 3 {
		t.Fatalf("error = %v, want the three failures that are this run's", err)
	}

	if kept, err := splitUndecided(nil, nil); kept != nil || err != nil {
		t.Fatalf("no failure became %v, %v", kept, err)
	}
}
