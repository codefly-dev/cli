package monitoring

import (
	"context"
	"sync"
	"time"

	"github.com/codefly-dev/core/agents/services"
	runtimev1 "github.com/codefly-dev/core/generated/go/services/runtime/v1"
	"github.com/codefly-dev/core/wool"
)

type RestartTracker struct {
	unique  string
	runtime services.Runtime
	sync.RWMutex
}

func (t *RestartTracker) Start(ctx context.Context, events chan<- ServiceEvent) error {
	w := wool.Get(ctx).In("monitoring.RestartTracker")
	ticker := time.NewTicker(1 * time.Second)
	go func() {
		for {
			<-ticker.C
			if t.runtime == nil {
				continue
			}
			req, err := t.runtime.Information(ctx, &runtimev1.InformationRequest{})
			if err != nil {
				w.Debug("cannot get status from runtime", wool.ErrField(err))
				continue
			}
			if req.Status == services.RestartWantedState {
				w.Debug("restart wanted: sending restart wanted message")
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
