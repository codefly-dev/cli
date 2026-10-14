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

// WaitForWithdrawal observes committed presence through the host's existing
// read-only publisher endpoint. A delivery receipt only confirms the inbox.
func (d *NativeDeliverer) WaitForWithdrawal(ctx context.Context, presence *solutionhost.SolutionHostBinding) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(endpointDeliveryAttempts)*endpointDeliveryInterval)
	defer cancel()
	if presence == nil || !presence.Removed || d.ObserverToken == nil {
		return fmt.Errorf("withdrawal completion requires a tombstone and an applied-state observer")
	}
	digest, err := presence.Digest()
	if err != nil {
		return err
	}
	for attempt := 0; attempt < endpointDeliveryAttempts; attempt++ {
		applied, err := d.withdrawalApplied(ctx, presence, digest)
		if err != nil {
			return err
		}
		if applied {
			return nil
		}
		if attempt+1 == endpointDeliveryAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(endpointDeliveryInterval):
		}
	}
	return fmt.Errorf("binding %s withdrawal generation %d is received but not applied; retained for the next composition refresh", presence.Binding, presence.Generation)
}

func (d *NativeDeliverer) withdrawalApplied(ctx context.Context, presence *solutionhost.SolutionHostBinding, digest string) (bool, error) {
	token, err := d.ObserverToken()
	if err != nil {
		return false, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(d.DeliveryURL, "/")+"/platform/_delivery/applied/"+url.PathEscape(presence.Binding), nil)
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
	var applied struct {
		Host           solutionhost.HostTarget `json:"host"`
		Binding        string                  `json:"binding"`
		Generation     uint64                  `json:"generation"`
		PresenceDigest string                  `json:"presenceDigest"`
		Release        string                  `json:"release"`
		Removed        bool                    `json:"removed"`
		AppliedAt      time.Time               `json:"appliedAt"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<16)).Decode(&applied); err != nil {
		return false, fmt.Errorf("read applied withdrawal: %w", err)
	}
	if applied.Host != presence.Host || applied.Binding != presence.Binding || applied.AppliedAt.IsZero() {
		return false, fmt.Errorf("applied withdrawal identity does not match binding %s", presence.Binding)
	}
	if applied.Generation < presence.Generation {
		return false, nil
	}
	if applied.Generation != presence.Generation || applied.PresenceDigest != digest || applied.Release != presence.Release.Identity() || !applied.Removed {
		return false, fmt.Errorf("applied withdrawal does not match binding %s generation %d", presence.Binding, presence.Generation)
	}
	return true, nil
}
