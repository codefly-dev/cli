package management

import (
	"github.com/codefly-dev/cli/pkg/plugins"
	corev1 "github.com/codefly-dev/cli/proto/v1/core"
	managementv1 "github.com/codefly-dev/cli/proto/v1/management"
	"github.com/codefly-dev/core/shared"
)

type Spy struct {
	Session *corev1.Session
	Storage Storage
}

type Storage interface {
	StartSession(session *corev1.Session) error
	AddLog(log *managementv1.Log) // Plugin callback
	Close()
}

func (s *Spy) Close() {
	s.Storage.Close()
}

func (s *Spy) Activate() error {
	logger := shared.NewLogger("development.SpyActivate")
	storage, err := NewSqliteStorage()
	if err != nil {
		return logger.Wrapf(err, "cannot create storage")
	}
	s.Storage = storage
	// Session will take a snapshot of all the dependencies
	logger.TODO("we need to define some fuzzy equality on snapshots")
	err = s.Storage.StartSession(s.Session)
	if err != nil {
		return err
	}
	plugins.RegisterLogCallback(s.Storage.AddLog)
	return nil
}

func NewSpy(session *corev1.Session) (*Spy, error) {
	logger := shared.NewLogger("development.NewSpy")
	storage, err := NewSqliteStorage()
	if err != nil {
		return nil, logger.Wrapf(err, "cannot create storage")
	}
	return &Spy{Session: session, Storage: storage}, nil
}
