package management

import (
	"time"

	agentsv1 "github.com/codefly-dev/core/proto/v1/go/agents"
	basev1 "github.com/codefly-dev/core/proto/v1/go/base"

	"github.com/codefly-dev/core/configurations"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// NewProjectSnapshot creates a new ProjectSnapshot instance.
func NewProjectSnapshot(name string) *basev1.ProjectSnapshot {
	return &basev1.ProjectSnapshot{
		Uuid: uuid.NewString(),
		Name: name,
	}
}

// NewApplicationSnapshot creates a new ApplicationSnapshot instance.
func NewApplicationSnapshot(name string, project *basev1.ProjectSnapshot) *basev1.ApplicationSnapshot {
	return &basev1.ApplicationSnapshot{
		Uuid:    uuid.NewString(),
		Name:    name,
		Project: project,
	}
}

// NewPartialSnapshot creates a new PartialSnapshot instance.
func NewPartialSnapshot(name string, project *basev1.ProjectSnapshot, applications []*basev1.ApplicationSnapshot) *basev1.PartialSnapshot {
	return &basev1.PartialSnapshot{
		Uuid:         uuid.NewString(),
		Name:         name,
		Project:      project,
		Applications: applications,
	}
}

func ToPartialSnapshot(partial *configurations.Partial) *basev1.PartialSnapshot {
	return NewPartialSnapshot(partial.Name, NewProjectSnapshot(partial.Name), ToApplicationSnapshots(partial.Applications))
}

func ToApplicationSnapshots(applications []string) []*basev1.ApplicationSnapshot {
	var refs []*basev1.ApplicationSnapshot
	for _, name := range applications {
		refs = append(refs, NewApplicationSnapshot(name, nil))
	}
	return refs
}

// NewLog creates a new Log instance.
func NewLog(at time.Time, application, service, message string, kind agentsv1.Log_Kind) *agentsv1.Log {
	return &agentsv1.Log{
		At:          timestamppb.New(at),
		Application: application,
		Service:     service,
		Kind:        kind,
		Message:     message,
	}
}

// NewPartialSession creates a new LogSession instance.
func NewPartialSession(partial *configurations.Partial) *basev1.Session {
	session := &basev1.Session{
		Uuid:    uuid.NewString(),
		At:      timestamppb.New(time.Now()),
		Session: &basev1.Session_Partial{Partial: ToPartialSnapshot(partial)},
	}
	return session
}
