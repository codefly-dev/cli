package observability

import (
	"context"

	"github.com/codefly-dev/core/agents"
	basev1 "github.com/codefly-dev/core/generated/go/base/v1"
	agentv1 "github.com/codefly-dev/core/generated/go/services/agent/v1"
	"github.com/codefly-dev/core/wool"
)

type Spy struct {
	Session *basev1.Session
	Storage Storage
}

type Storage interface {
	StartSession(session *basev1.Session) error
	AddLog(log *agentv1.Log) // Agent callback
	Close()
}

func (s *Spy) Close() {
	s.Storage.Close()
}

func (s *Spy) Activate(ctx context.Context) error {
	w := wool.Get(ctx).In("development.SpyActivate")
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
	agents.RegisterLogCallback(s.Storage.AddLog)
	return nil
}

func NewSpy(ctx context.Context, session *basev1.Session) (*Spy, error) {
	w := wool.Get(ctx).In("development.NewSpy")
	storage, err := NewSqliteStorage()
	if err != nil {
		return nil, logger.Wrapf(err, "cannot create storage")
	}
	return &Spy{Session: session, Storage: storage}, nil
}
