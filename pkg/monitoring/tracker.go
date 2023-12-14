package monitoring

import (
	"context"
	"sync"
	"time"

	"github.com/codefly-dev/core/agents/services"

	"github.com/codefly-dev/core/generated/v1/go/proto/services/runtime"

	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
)

type ServiceEvent struct {
	Unique string
	Event  string
}

type Tracker interface {
	Start(ctx context.Context, events chan<- ServiceEvent) error
	Stop()
	// Tracks() []*applications.Tracked
}

/*
First target tracker
*/

type SingleTracker struct {
	Tracked Tracked
	Runtime services.Runtime

	// latest
	usage  *Usage
	status ProcessState

	// internal
	ctx    context.Context
	cancel func()
	sync.RWMutex
	stopping bool
}

func (t *SingleTracker) Stop() {
	t.RWMutex.Lock()
	defer t.RWMutex.Unlock()
	if t.cancel != nil {
		t.cancel()
	}
	t.stopping = true
}

func NewSingleTracker(ctx context.Context, service *configurations.Service, runtime services.Runtime, tracker *runtime.Tracker) (*SingleTracker, error) {
	logger := shared.GetLogger(ctx).With("monitoring.NewSingleTracker<%s>", service.Name)
	tracked, err := NewTracked(service, tracker)
	if err != nil {
		return nil, logger.Wrapf(err, "cannot create tracked")
	}
	ctx, cancel := context.WithCancel(ctx)
	return &SingleTracker{Tracked: tracked, Runtime: runtime, ctx: ctx, cancel: cancel}, nil
}

func (t *SingleTracker) Start(ctx context.Context, events chan<- ServiceEvent) error {
	logger := shared.GetLogger(ctx).With("monitoring.SingleTracker.Start")
	ticker := time.NewTicker(1 * time.Second)
	go func() {
		for {
			select {
			case <-t.ctx.Done():
				return
			case <-ticker.C:
				if t.Runtime == nil {
					continue
				}
				t.RWMutex.RLock()
				if t.stopping {
					return
				}
				t.RWMutex.RUnlock()
				req, err := t.Runtime.Information(ctx, &runtime.InformationRequest{})
				if err != nil {
					logger.Debugf("cannot get status from runtime: %v", err)
					continue
				}
				if req.Status == services.RestartWantedState {
					logger.Debugf("runtime wants to restart")
					events <- ServiceEvent{
						Unique: t.Tracked.Unique(),
						Event:  "RestartWanted",
					}
					t.RWMutex.Lock()
					t.stopping = true
					t.RWMutex.Unlock()
				}
				if t.Tracked == nil {
					return
				}
				status, err := t.Tracked.GetStatus(ctx)
				if err == nil {
					t.RWMutex.Lock()
					t.status = status
					t.RWMutex.Unlock()
				}
				usage, err := t.Tracked.GetUsage(ctx)
				if err == nil {
					t.RWMutex.Lock()
					t.usage = usage
					t.RWMutex.Unlock()
				} else {
					logger.TODO("cant get usage ")
					t.RWMutex.Lock()
					t.usage = &Usage{}
					t.RWMutex.Unlock()
				}
			}
		}
	}()
	return nil
}

/*
Multiple targets tracker
*/

type GroupTracker struct{}

//
//func (g GroupTracker) Tracks() []*applications.Tracked {
//	panic("implement me")
//}

func (g GroupTracker) Start(ctx context.Context, events chan<- ServiceEvent) error {
	// TODO implement me
	panic("implement me")
}

func (g GroupTracker) Stop() {
	// TODO implement me
	panic("implement me")
}

func NewGroupTracker(ctx context.Context, service *configurations.Service, runtime services.Runtime, trackers []*runtime.Tracker) (*GroupTracker, error) {
	return &GroupTracker{}, nil
}

/*
Name tracker
*/

type ServiceTracker struct {
	current map[string]Tracker
	sync.RWMutex
	events   chan<- ServiceEvent
	trackers map[string]*runtime.TrackerList
}

func (t *ServiceTracker) OnHold(ctx context.Context, service *configurations.Service, runtime services.Runtime) error {
	logger := shared.GetLogger(ctx).With("monitoring.ServiceTracker.OnHold<%s>", service.Name)
	tracker := &RestartTracker{unique: service.Unique(), runtime: runtime}
	// Start errors first or start working in a non-blocking way
	err := tracker.Start(ctx, t.events)
	if err != nil {
		return logger.Wrapf(err, "cannot start on-hold")
	}
	t.RWMutex.Lock()
	t.current[service.Unique()] = tracker
	t.RWMutex.Unlock()
	return nil
}

func (t *ServiceTracker) Track(ctx context.Context, service *configurations.Service, runtime services.Runtime, trackers []*runtime.Tracker) error {
	logger := shared.GetLogger(ctx).With("monitoring.ServiceTracker.Track<%s>", service.Name)
	tracker, err := CreateTracker(ctx, service, runtime, trackers)
	if err != nil {
		return logger.Wrapf(err, "cannot create tracker")
	}
	if tracker == nil {
		return nil
	}
	// Start errors first or start working in a non-blocking way
	err = tracker.Start(ctx, t.events)
	if err != nil {
		return logger.Wrapf(err, "cannot start tracker")
	}
	t.RWMutex.Lock()
	//t.trackers[service.Unique()] = &runtime.TrackerList{Trackers: trackers}
	t.current[service.Unique()] = tracker
	t.RWMutex.Unlock()
	return nil
}

func (t *ServiceTracker) Untrack(service *configurations.Service) error {
	t.RWMutex.Lock()
	defer t.RWMutex.Unlock()
	unique := service.Unique()
	if v, ok := t.current[unique]; ok {
		v.Stop()
	}
	delete(t.current, unique)
	delete(t.trackers, unique)

	return nil
}

//
//func (t *ServiceTracker) Tracks() []*applications.Tracked {
//	var tracks []*applications.Tracked
//	for _, tracker := range t.current {
//		tracks = append(tracks, tracker.Tracks()...)
//	}
//	return tracks
//}

func CreateTracker(ctx context.Context, service *configurations.Service, runtime services.Runtime, trackers []*runtime.Tracker) (Tracker, error) {
	if len(trackers) == 0 {
		return nil, nil
	}
	if len(trackers) == 1 {
		return NewSingleTracker(ctx, service, runtime, trackers[0])
	}
	return NewGroupTracker(ctx, service, runtime, trackers)
}

func NewServiceTracker(events chan<- ServiceEvent) (*ServiceTracker, error) {
	tracker := &ServiceTracker{
		events:   events,
		current:  make(map[string]Tracker),
		trackers: make(map[string]*runtime.TrackerList),
	}
	return tracker, nil
}
