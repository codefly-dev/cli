package monitoring

import (
	"context"
	"sync"
	"time"

	"github.com/codefly-dev/cli/pkg/plugins/services"
	runtimev1 "github.com/codefly-dev/cli/proto/v1/services/runtime"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
)

type ServiceEvent struct {
	Unique string
	Event  string
}

type Tracker interface {
	Start(events chan<- ServiceEvent) error
	Stop()
	// Tracks() []*applications.Tracked
}

/*
First target tracker
*/

type SingleTracker struct {
	Tracked Tracked
	Runtime services.IRuntime

	// latest
	usage  *Usage
	status ProcessState

	// internal
	ctx    context.Context
	cancel func()
	*sync.RWMutex
	stopping bool
}

func (t *SingleTracker) Stop() {
	t.Lock()
	defer t.Unlock()
	if t.cancel != nil {
		t.cancel()
	}
	t.stopping = true
}

func NewSingleTracker(service *configurations.Service, runtime services.IRuntime, tracker *runtimev1.Tracker) (*SingleTracker, error) {
	logger := shared.NewLogger("monitoring.NewSingleTracker<%s>", service.Name)
	tracked, err := NewTracked(service, tracker)
	if err != nil {
		return nil, logger.Wrapf(err, "cannot create tracked")
	}
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	return &SingleTracker{Tracked: tracked, Runtime: runtime, ctx: ctx, cancel: cancel}, nil
}

func (t *SingleTracker) Start(events chan<- ServiceEvent) error {
	logger := shared.NewLogger("monitoring.SingleTracker.Start")
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
				t.RLock()
				if t.stopping {
					return
				}
				t.RUnlock()
				req, err := t.Runtime.Information(&runtimev1.InformationRequest{})
				if err != nil {
					logger.Debugf("cannot get status from runtime: %v", err)
					continue
				}
				if req.Status == services.RestartWanted {
					logger.Debugf("runtime wants to restart")
					events <- ServiceEvent{
						Unique: t.Tracked.Unique(),
						Event:  "RestartWanted",
					}
					t.Lock()
					t.stopping = true
					t.Unlock()
				}
				if t.Tracked == nil {
					return
				}
				status, err := t.Tracked.GetStatus()
				if err == nil {
					t.Lock()
					t.status = status
					t.Unlock()
				}
				usage, err := t.Tracked.GetUsage()
				if err == nil {
					t.Lock()
					t.usage = usage
					t.Unlock()
				} else {
					logger.TODO("cant get usage ")
					t.Lock()
					t.usage = &Usage{}
					t.Unlock()
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

func (g GroupTracker) Start(events chan<- ServiceEvent) error {
	// TODO implement me
	panic("implement me")
}

func (g GroupTracker) Stop() {
	// TODO implement me
	panic("implement me")
}

func NewGroupTracker(service *configurations.Service, runtime services.IRuntime, trackers []*runtimev1.Tracker) (*GroupTracker, error) {
	return &GroupTracker{}, nil
}

/*
Name tracker
*/

type ServiceTracker struct {
	current map[string]Tracker
	*sync.RWMutex
	events   chan<- ServiceEvent
	trackers map[string]*runtimev1.TrackerList
}

func (t *ServiceTracker) OnHold(service *configurations.Service, runtime services.IRuntime) error {
	logger := shared.NewLogger("monitoring.ServiceTracker.OnHold<%s>", service.Name)
	tracker := &RestartTracker{unique: service.Unique(), runtime: runtime}
	// Start errors first or start working in a non-blocking way
	err := tracker.Start(t.events)
	if err != nil {
		return logger.Wrapf(err, "cannot start on-hold")
	}
	t.Lock()
	t.current[service.Unique()] = tracker
	t.Unlock()
	return nil
}

func (t *ServiceTracker) Track(ctx context.Context, service *configurations.Service, runtime services.IRuntime, trackers []*runtimev1.Tracker) error {
	logger := shared.NewLogger("monitoring.ServiceTracker.Track<%s>", service.Name)
	tracker, err := CreateTracker(service, runtime, trackers)
	if err != nil {
		return logger.Wrapf(err, "cannot create tracker")
	}
	if tracker == nil {
		return nil
	}
	// Start errors first or start working in a non-blocking way
	err = tracker.Start(t.events)
	if err != nil {
		return logger.Wrapf(err, "cannot start tracker")
	}
	t.Lock()
	t.trackers[service.Unique()] = &runtimev1.TrackerList{Trackers: trackers}
	t.current[service.Unique()] = tracker
	t.Unlock()
	return nil
}

func (t *ServiceTracker) Untrack(service *configurations.Service) error {
	t.Lock()
	defer t.Unlock()
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

func CreateTracker(service *configurations.Service, runtime services.IRuntime, trackers []*runtimev1.Tracker) (Tracker, error) {
	if len(trackers) == 0 {
		return nil, nil
	}
	if len(trackers) == 1 {
		return NewSingleTracker(service, runtime, trackers[0])
	}
	return NewGroupTracker(service, runtime, trackers)
}

func NewServiceTracker(events chan<- ServiceEvent) (*ServiceTracker, error) {
	tracker := &ServiceTracker{
		events:   events,
		current:  make(map[string]Tracker),
		trackers: make(map[string]*runtimev1.TrackerList),
	}
	return tracker, nil
}
