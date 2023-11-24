package communicate

import (
	"context"
	"fmt"
	"strings"

	corev1 "github.com/codefly-dev/cli/proto/v1/core"
	"github.com/codefly-dev/core/shared"
)

// A Client receives an engagement request and returns an information request
// Often, the request will be a question, and the response will be received as part of the next engagement

type Client interface {
	Ready() bool
	Process(req *corev1.Engage) (*corev1.InformationRequest, error)
}

type ClientContext struct {
	Method corev1.Method
	Client Client
	round  int32
	states []*corev1.Answer
	ctx    context.Context
}

var _ Client = &ClientContext{}

func (c *ClientContext) NextRound() int32 {
	c.round++
	return c.round
}

func NewClientContext(ctx context.Context, method corev1.Method) (*ClientContext, error) {
	return &ClientContext{
		Method: method,
		ctx:    ctx,
	}, nil
}

// NewSequence creates a SequenceClient
func (c *ClientContext) NewSequence(qs ...*corev1.Question) (*Sequence, error) {
	seq := NewSequence(c.Method, qs...)
	c.Client = seq
	return seq, nil
}

func (c *ClientContext) Ready() bool {
	return c.Client.Ready()
}

func (c *ClientContext) Process(eng *corev1.Engage) (*corev1.InformationRequest, error) {
	if eng.Answer != nil {
		c.states = append(c.states, eng.Answer)
	}
	return c.Client.Process(eng)
}

func (c *ClientContext) Confirm(s int) *corev1.ConfirmAnswer {
	answer := c.states[s]
	if answer == nil {
		return nil
	}
	return answer.GetConfirm()
}

func (c *ClientContext) SafeConfirm(s int) (*corev1.ConfirmAnswer, error) {
	if len(c.states) < s {
		return nil, fmt.Errorf("no state for %d", s)
	}
	answer := c.states[s]
	if answer == nil {
		return nil, fmt.Errorf("no state for %d", s)
	}
	back := answer.GetConfirm()
	if back == nil {
		return nil, fmt.Errorf("state is not of the confirm type for %d", s)
	}
	return back, nil
}

func (c *ClientContext) Selection(i int) *corev1.SelectionAnswer {
	answer := c.states[i]
	if answer == nil {
		return nil
	}
	return answer.GetSelection()
}

func (c *ClientContext) Input(i int) *corev1.InputAnswer {
	answer := c.states[i]
	if answer == nil {
		return nil
	}
	return answer.GetInput()
}

func StateAsString(s *corev1.Answer) string {
	switch s.Value.(type) {
	case *corev1.Answer_Confirm:
		return s.GetConfirm().String()
	case *corev1.Answer_Selection:
		return s.GetSelection().String()
	case *corev1.Answer_Input:
		return s.GetInput().String()
	default:
		return ""
	}
}

func (c *ClientContext) Get() string {
	var ss []string
	for i, s := range c.states {
		ss = append(ss, fmt.Sprintf("%d: %s", i, StateAsString(s)))
	}
	return strings.Join(ss, " -> ")
}

type NoOpClientContext struct{}

func (c *NoOpClientContext) Process(req *corev1.Engage) (*corev1.InformationRequest, error) {
	return &corev1.InformationRequest{Done: true}, nil
}

var _ Client = &NoOpClientContext{}

func NewNoOpClientContext() *NoOpClientContext {
	return &NoOpClientContext{}
}

func (c *NoOpClientContext) Ready() bool {
	return true
}

// Dispatches the request to the appropriate client

type ClientManager struct {
	clients map[corev1.Method]*ClientContext
	logger  shared.BaseLogger
}

func (m *ClientManager) WithLogger(logger shared.BaseLogger) {
	m.logger = logger
}

func (m *ClientManager) Add(method corev1.Method, client *ClientContext) error {
	m.clients[method] = client
	return nil
}

func (m *ClientManager) Process(eng *corev1.Engage) (*corev1.InformationRequest, error) {
	if client, ok := m.clients[eng.Method]; ok {
		m.logger.Debugf("found client context: %v", client)
		return client.Process(eng)
	}
	m.logger.Debugf("FOUND NOTHING")
	return &corev1.InformationRequest{}, fmt.Errorf("no client for method: %v", eng.Method)
}

func NewClientManager() *ClientManager {
	return &ClientManager{
		clients: make(map[corev1.Method]*ClientContext),
	}
}
