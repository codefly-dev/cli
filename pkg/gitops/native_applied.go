package gitops

import (
	"context"
	"fmt"
	"time"

	"github.com/codefly-dev/core/solutionhost"
)

// withdrawalAttempts and withdrawalInterval bound the wait for a delivered
// withdrawal to be APPLIED: a delivery receipt only confirms the inbox.
const (
	withdrawalAttempts = 40
	withdrawalInterval = 1500 * time.Millisecond
)

// WaitForWithdrawal observes the host's applied record of a withdrawn binding
// through its Applied callback, until it holds the exact withdrawal this run
// delivered.
func (d *NativeDeliverer) WaitForWithdrawal(ctx context.Context, withdrawal *solutionhost.ModuleManifest) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(withdrawalAttempts)*withdrawalInterval)
	defer cancel()
	if withdrawal == nil || !withdrawal.Removed || d.Applied == nil {
		return fmt.Errorf("withdrawal completion requires a withdrawal and an applied-state observer")
	}
	digest, err := withdrawal.Digest()
	if err != nil {
		return err
	}
	for attempt := 0; attempt < withdrawalAttempts; attempt++ {
		applied, err := d.withdrawalApplied(ctx, withdrawal, digest)
		if err != nil {
			return err
		}
		if applied {
			return nil
		}
		if attempt+1 == withdrawalAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(withdrawalInterval):
		}
	}
	return fmt.Errorf("binding %s withdrawal generation %d is received but not applied; retained for the next composition refresh", withdrawal.Binding, withdrawal.Generation)
}

// AppliedManifest is the host's applied identity of a binding, supplied by
// the composition callback.
type AppliedManifest struct {
	Binding    string
	Generation uint64
	Digest     string
	Release    string
	Removed    bool
}

// WaitForApplied holds a delivery to its outcome: the host applied this
// generation. A receipt says the carrier was accepted; only the applied state
// says the module is served.
func (d *NativeDeliverer) WaitForApplied(ctx context.Context, manifest *solutionhost.ModuleManifest) error {
	if manifest == nil || d.Applied == nil {
		return fmt.Errorf("applied-state observation requires a manifest and an observer")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(withdrawalAttempts)*withdrawalInterval)
	defer cancel()
	for attempt := 0; ; attempt++ {
		applied, found, err := d.Applied(ctx, manifest.Binding)
		if err != nil {
			return err
		}
		if found && applied.Generation >= manifest.Generation {
			return nil
		}
		if attempt+1 == withdrawalAttempts {
			return fmt.Errorf("binding %s generation %d is received and not applied after %s, and the host reports no reason",
				manifest.Binding, manifest.Generation, time.Duration(withdrawalAttempts)*withdrawalInterval)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(withdrawalInterval):
		}
	}
}

func (d *NativeDeliverer) withdrawalApplied(ctx context.Context, withdrawal *solutionhost.ModuleManifest, digest string) (bool, error) {
	applied, found, err := d.Applied(ctx, withdrawal.Binding)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	if applied == nil {
		return false, fmt.Errorf("host reported applied binding %s without its record", withdrawal.Binding)
	}
	if applied.Binding != withdrawal.Binding {
		return false, fmt.Errorf("applied record names binding %q, not %s", applied.Binding, withdrawal.Binding)
	}
	if applied.Generation < withdrawal.Generation {
		return false, nil
	}
	if applied.Generation != withdrawal.Generation || applied.Digest != digest || applied.Release != withdrawal.Release.Identity() || !applied.Removed {
		return false, fmt.Errorf("applied record does not match binding %s withdrawal generation %d", withdrawal.Binding, withdrawal.Generation)
	}
	return true, nil
}
