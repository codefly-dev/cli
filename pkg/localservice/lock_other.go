//go:build !darwin && !linux

package localservice

import (
	"context"
	"fmt"
)

func (m *manager) acquireServiceLock(_ context.Context, ref ServiceRef) (func(), error) {
	if !labelPattern.MatchString(ref.Label) {
		return nil, fmt.Errorf("service label %q is invalid", ref.Label)
	}
	return func() {}, nil
}
