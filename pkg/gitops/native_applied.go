package gitops

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
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
// through the host's read-only applied-state endpoint, until it holds the
// withdrawal this run delivered.
func (d *NativeDeliverer) WaitForWithdrawal(ctx context.Context, withdrawal *solutionhost.ModuleManifest) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(withdrawalAttempts)*withdrawalInterval)
	defer cancel()
	if withdrawal == nil || !withdrawal.Removed || d.ObserverToken == nil {
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

// AppliedManifest is the host's applied record of a binding:
// GET <accounts REST>/platform/_delivery/applied/<binding>.
type AppliedManifest struct {
	Binding    string `json:"binding"`
	Generation uint64 `json:"generation"`
	Digest     string `json:"digest"`
	Release    string `json:"release"`
	Removed    bool   `json:"removed"`
	// Pending is a newer generation the host received and withheld, with the
	// host's own reason: "delivered" is not "applied", and the run says which.
	Pending *struct {
		Generation uint64 `json:"generation"`
		Reason     string `json:"reason"`
	} `json:"pending,omitempty"`
}

// WaitForApplied holds a delivery to its outcome: the host applied this
// generation, or withheld it and said why. A receipt says the carrier was
// accepted into the inbox; only the applied state says the module is served.
func (d *NativeDeliverer) WaitForApplied(ctx context.Context, manifest *solutionhost.ModuleManifest) error {
	if manifest == nil || d.ObserverToken == nil {
		return fmt.Errorf("applied-state observation requires a manifest and an observer")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(withdrawalAttempts)*withdrawalInterval)
	defer cancel()
	for attempt := 0; ; attempt++ {
		applied, found, err := d.appliedState(ctx, manifest.Binding)
		if err != nil {
			return err
		}
		if found && applied.Pending != nil && applied.Pending.Generation >= manifest.Generation {
			return fmt.Errorf("the host received binding %s generation %d and withheld it: %s",
				manifest.Binding, applied.Pending.Generation, applied.Pending.Reason)
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

// appliedState reads the host's applied record of binding; found is false on 404.
func (d *NativeDeliverer) appliedState(ctx context.Context, binding string) (*AppliedManifest, bool, error) {
	if d.Applied != nil {
		return d.Applied(ctx, binding)
	}
	token, err := d.ObserverToken()
	if err != nil {
		return nil, false, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(d.DeliveryURL, "/")+"/platform/_delivery/applied/"+url.PathEscape(binding), nil)
	if err != nil {
		return nil, false, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	client := d.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, false, fmt.Errorf("observe applied state: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, false, nil
	}
	if response.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("host refused applied-state observation (%s)", response.Status)
	}
	var applied AppliedManifest
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<16)).Decode(&applied); err != nil {
		return nil, false, fmt.Errorf("read applied state: %w", err)
	}
	return &applied, true, nil
}

func (d *NativeDeliverer) withdrawalApplied(ctx context.Context, withdrawal *solutionhost.ModuleManifest, digest string) (bool, error) {
	token, err := d.ObserverToken()
	if err != nil {
		return false, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(d.DeliveryURL, "/")+"/platform/_delivery/applied/"+url.PathEscape(withdrawal.Binding), nil)
	if err != nil {
		return false, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	client := d.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return false, fmt.Errorf("observe withdrawal: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if response.StatusCode != http.StatusOK {
		return false, fmt.Errorf("host refused applied-state observation (%s)", response.Status)
	}
	var applied AppliedManifest
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<16)).Decode(&applied); err != nil {
		return false, fmt.Errorf("read applied withdrawal: %w", err)
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
