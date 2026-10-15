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

// appliedManifest is the host's applied record of a binding:
// GET <accounts REST>/platform/_delivery/applied/<binding>.
type appliedManifest struct {
	Binding    string `json:"binding"`
	Generation uint64 `json:"generation"`
	Digest     string `json:"digest"`
	Release    string `json:"release"`
	Removed    bool   `json:"removed"`
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
	var applied appliedManifest
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
