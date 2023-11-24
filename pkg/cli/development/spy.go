package development

import (
	corev1 "github.com/codefly-dev/cli/proto/v1/core"
	"github.com/codefly-dev/core/shared"
)

type Spy struct {
	Session *corev1.Session
	Storage Storage
}

type Storage interface {
	Init(session *corev1.Session) error
}

func (s *Spy) Report() {
	// Update the session time

}

func (s *Spy) Activate() error {
	storage, err := NewSqliteStorage()
	if err != nil {
		return err
	}
	s.Storage = storage
	err = s.Storage.Init(s.Session)
	if err != nil {
		return err
	}
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
