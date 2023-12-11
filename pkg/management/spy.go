package management

import (
	"context"

	"github.com/codefly-dev/core/agents"
	agentsv1 "github.com/codefly-dev/core/proto/v1/go/agents"
	basev1 "github.com/codefly-dev/core/proto/v1/go/base"
	"github.com/codefly-dev/core/shared"
)

type Spy struct {
	Session *basev1.Session
	Storage Storage
}

type Storage interface {
	StartSession(session *basev1.Session) error
	AddLog(log *agentsv1.Log) // Agent callback
	Close()
}

func (s *Spy) Close() {
	s.Storage.Close()
}

func (s *Spy) Activate(ctx context.Context) error {
	logger := shared.GetLogger(ctx).With("development.SpyActivate")
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
	logger := shared.GetLogger(ctx).With("development.NewSpy")
	storage, err := NewSqliteStorage()
	if err != nil {
		return nil, logger.Wrapf(err, "cannot create storage")
	}
	return &Spy{Session: session, Storage: storage}, nil
}
