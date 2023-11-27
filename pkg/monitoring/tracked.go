package monitoring

import (
	"github.com/codefly-dev/core/configurations"
	runtimev1 "github.com/codefly-dev/core/proto/v1/go/services/runtime"
)

type Tracked interface {
	Name() string   // For display
	Unique() string // For identification in the system
	GetStatus() (ProcessState, error)
	GetUsage() (*Usage, error)
	Kill() error
}

type ProcessState int

const (
	Unknown ProcessState = iota
	NotFound
	Running
	InterruptibleSleep
	UninterruptibleSleep
	Stopped
	Zombie
	Dead
	TracingStop
	Idle
	Parked
	Waking
)

type Usage struct {
	Memory float64 // In KB
	CPU    float64
}

func NewTracked(service *configurations.Service, tracker *runtimev1.Tracker) (Tracked, error) {
	switch tracker.Tracker.(type) {
	case *runtimev1.Tracker_ProcessTracker:
		return &TrackedProcess{
			name:   service.Name,
			unique: service.Unique(),
			PID:    int(tracker.Tracker.(*runtimev1.Tracker_ProcessTracker).ProcessTracker.PID),
		}, nil
	case *runtimev1.Tracker_DockerTracker:
		return &TrackedContainer{
			name:   service.Name,
			unique: service.Unique(),
		}, nil
	default:
		return nil, nil
	}
}

type TrackedContainer struct {
	name   string
	unique string
}

func (t TrackedContainer) GetStatus() (ProcessState, error) {
	return Unknown, nil
}

func (t TrackedContainer) GetUsage() (*Usage, error) {
	return &Usage{CPU: 100.0, Memory: 100.0}, nil
}

func (t TrackedContainer) Name() string {
	return t.name
}

func (t TrackedContainer) Unique() string {
	return t.unique
}

func (t TrackedContainer) Kill() error {
	return nil
}
