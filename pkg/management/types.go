package management

import (
	"time"

	corev1 "github.com/codefly-dev/cli/proto/v1/core"
	managementv1 "github.com/codefly-dev/cli/proto/v1/management"
	"github.com/codefly-dev/core/configurations"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// NewProjectReference creates a new ProjectReference instance.
func NewProjectReference(name string) *corev1.ProjectReference {
	return &corev1.ProjectReference{
		Name: name,
	}
}

// NewApplicationReference creates a new ApplicationReference instance.
func NewApplicationReference(name string, project *corev1.ProjectReference) *corev1.ApplicationReference {
	return &corev1.ApplicationReference{
		Name:    name,
		Project: project,
	}
}

// NewPartialReference creates a new PartialReference instance.
func NewPartialReference(name string, project *corev1.ProjectReference, applications []*corev1.ApplicationReference) *corev1.PartialReference {
	return &corev1.PartialReference{
		Name:         name,
		Project:      project,
		Applications: applications,
	}
}

func ToPartialReference(partial *configurations.Partial) *corev1.PartialReference {
	return NewPartialReference(partial.Name, NewProjectReference(partial.Name), ToApplicationReferences(partial.Applications))
}

func ToApplicationReferences(applications []string) []*corev1.ApplicationReference {
	var refs []*corev1.ApplicationReference
	for _, name := range applications {
		refs = append(refs, NewApplicationReference(name, nil))
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
		At:      timestamppb.New(time.Now()),
		Session: &corev1.Session_Partial{Partial: ToPartialReference(partial)},
	}
	return session
}
