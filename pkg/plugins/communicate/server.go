package communicate

import (
	corev1 "github.com/codefly-dev/cli/proto/v1/core"
	factoryv1 "github.com/codefly-dev/cli/proto/v1/services/factory"
	"github.com/codefly-dev/core/shared"
)

type QuestionHandler interface {
	Process(req *corev1.InformationRequest) (*corev1.Answer, error)
}

type ServerContext struct {
	Handler QuestionHandler
	Method  corev1.Method
	logger  shared.BaseLogger
	done    bool
}

func (c *ServerContext) Done() bool {
	return c.done
}

func (c *ServerContext) Communicate(answer *corev1.Answer) (*corev1.Engage, error) {
	return &corev1.Engage{Method: c.Method, Answer: answer}, nil
}

func (c *ServerContext) Process(request *corev1.InformationRequest) (*corev1.Answer, error) {
	return c.Handler.Process(request)
}

func NewServerContext(method corev1.Method, logger shared.BaseLogger) *ServerContext {
	return &ServerContext{
		Method: method,
		logger: logger,
	}
}

type ServerManager struct {
	channels map[corev1.Method]*ServerContext
	logger   shared.BaseLogger
}

func (m *ServerManager) Register(channels ...*corev1.Channel) error {
	for _, c := range channels {
		m.channels[c.Method] = NewServerContext(c.Method, m.logger)
	}
	return nil
}

func (m *ServerManager) RequiresCommunication(req any) (*ServerContext, bool) {
	method := ToMethod(req)
	if s, ok := m.channels[method]; ok {
		return s, true
	}
	return nil, false
}

func ToMethod(req any) corev1.Method {
	switch req.(type) {
	case *factoryv1.CreateRequest:
		return Create
	case *factoryv1.SyncRequest:
		return Sync
	default:
		return corev1.Method_UNKNOWN
	}
}

func NewServerManager(logger shared.BaseLogger) *ServerManager {
	return &ServerManager{
		logger:   logger,
		channels: make(map[corev1.Method]*ServerContext),
	}
}
