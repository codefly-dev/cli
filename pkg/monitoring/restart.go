package monitoring

import (
	"context"
	"sync"
	"time"

	"github.com/codefly-dev/core/agents/services"

	runtimev1 "github.com/codefly-dev/core/generated/v1/go/proto/services/runtime"

	"github.com/codefly-dev/core/shared"
)

type RestartTracker struct {
	unique  string
	runtime services.Runtime
	sync.RWMutex
}

func (t *RestartTracker) Start(ctx context.Context, events chan<- ServiceEvent) error {
	logger := shared.GetLogger(ctx).With("monitoring.RestartTracker")
	logger.Debugf("runtime wants to restart -- will ping the runtime for started status")
	ticker := time.NewTicker(1 * time.Second)
	go func() {
		for {
			<-ticker.C
			if t.runtime == nil {
				continue
			}
			req, err := t.runtime.Information(ctx, &runtimev1.InformationRequest{})
			if err != nil {
				logger.Debugf("cannot get status from runtime: %v", err)
				continue
			}
			if req.Status == services.RestartWantedState {
				logger.Debugf("restart wanted: sending restart wanted message")
				events <- ServiceEvent{
					Unique: t.unique,
					Event:  "RestartWanted",
				}
			}
		}
	}()
	return nil
}

func (t *RestartTracker) Stop() {
}
