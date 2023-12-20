package observability

import (
	"context"

	observabilityv1 "github.com/codefly-dev/core/generated/go/observability/v1"
)

type Spy struct {
	Session *observabilityv1.Session
	Storage Storage
}

type Storage interface {
	StartSession(session *observabilityv1.Session) error
	AddLog(log *observabilityv1.Log) // Agent callback
	Close()
}

func (s *Spy) Close() {
	s.Storage.Close()
}

func (s *Spy) Activate(context.Context) error {
	//w := wool.Get(ctx).In("development.SpyActivate")
	//storage, err := NewSqliteStorage()
	//if err != nil {
	//	return w.Wrapf(err, "cannot create storage")
	//}
	//s.Storage = storage
	// Session will take a snapshot of all the dependencies
	err := s.Storage.StartSession(s.Session)
	if err != nil {
		return err
	}
	//agents.RegisterLogCallback(s.Storage.AddLog)
	return nil
}

func NewSpy(ctx context.Context, session *observabilityv1.Session) (*Spy, error) {
	//w := wool.Get(ctx).In("development.NewSpy")
	//storage, err := NewSqliteStorage()
	//if err != nil {
	//	return nil, w.Wrapf(err, "cannot create storage")
	//}
	return &Spy{Session: session}, nil
}
