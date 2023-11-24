package management

import (
	"time"

	corev1 "github.com/codefly-dev/cli/proto/v1/core"
	managementv1 "github.com/codefly-dev/cli/proto/v1/management"
	"github.com/codefly-dev/core/configurations"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// NewProjectSnapshot creates a new ProjectSnapshot instance.
func NewProjectSnapshot(name string) *corev1.ProjectSnapshot {
	return &corev1.ProjectSnapshot{
		Uuid: uuid.NewString(),
		Name: name,
	}
}

// NewApplicationSnapshot creates a new ApplicationSnapshot instance.
func NewApplicationSnapshot(name string, project *corev1.ProjectSnapshot) *corev1.ApplicationSnapshot {
	return &corev1.ApplicationSnapshot{
		Uuid:    uuid.NewString(),
		Name:    name,
		Project: project,
	}
}

// NewPartialSnapshot creates a new PartialSnapshot instance.
func NewPartialSnapshot(name string, project *corev1.ProjectSnapshot, applications []*corev1.ApplicationSnapshot) *corev1.PartialSnapshot {
	return &corev1.PartialSnapshot{
		Uuid:         uuid.NewString(),
		Name:         name,
		Project:      project,
		Applications: applications,
	}
}

func ToPartialSnapshot(partial *configurations.Partial) *corev1.PartialSnapshot {
	return NewPartialSnapshot(partial.Name, NewProjectSnapshot(partial.Name), ToApplicationSnapshots(partial.Applications))
}

func ToApplicationSnapshots(applications []string) []*corev1.ApplicationSnapshot {
	var refs []*corev1.ApplicationSnapshot
	for _, name := range applications {
		refs = append(refs, NewApplicationSnapshot(name, nil))
	}
	return refs
}

// NewLog creates a new Log instance.
func NewLog(at time.Time, application, service, message string, kind managementv1.Log_Kind) *managementv1.Log {
	return &managementv1.Log{
		At:          timestamppb.New(at),
		Application: application,
		Service:     service,
		Kind:        kind,
		Message:     message,
	}
}

// NewPartialSession creates a new LogSession instance.
func NewPartialSession(partial *configurations.Partial) *corev1.Session {
	session := &corev1.Session{
		Uuid:    uuid.NewString(),
		At:      timestamppb.New(time.Now()),
		Session: &corev1.Session_Partial{Partial: ToPartialSnapshot(partial)},
	}
	return session
}
