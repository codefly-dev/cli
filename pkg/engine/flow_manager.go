package engine

import (
	"fmt"
	"reflect"
	"sync"

	"github.com/codefly-dev/core/services"
)

// ManagedFlow is the lifecycle surface a host needs in order to own a flow.
// The CLI orchestration implementation satisfies it, but engine does not import
// that implementation.
type ManagedFlow interface {
	Stop() error
	Shutdown() error
	AgentCacheKeys() []string
}

// FlowManager owns the long-lived orchestration flows started through one
// WorkspaceHost. It replaces process-global flow lookup for host-based
// adapters while orchestration.CurrentFlow remains as a legacy compatibility
// hook for callers that have not migrated yet.
type FlowManager struct {
	mu       sync.RWMutex
	flows    map[string]ManagedFlow
	order    []string
	activeID string
	closed   bool
}

func newFlowManager() *FlowManager {
	return &FlowManager{flows: make(map[string]ManagedFlow)}
}

// Register transfers ownership of flow to the manager until Release or Stop.
func (m *FlowManager) Register(id string, flow ManagedFlow) error {
	if m == nil {
		return fmt.Errorf("flow manager is unavailable")
	}
	if id == "" {
		return fmt.Errorf("flow id is required")
	}
	if nilManagedFlow(flow) {
		return fmt.Errorf("flow is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return fmt.Errorf("flow manager is closed")
	}
	if _, exists := m.flows[id]; exists {
		return fmt.Errorf("flow %q is already running", id)
	}
	m.flows[id] = flow
	m.order = append(m.order, id)
	m.activeID = id
	return nil
}

// Active returns the most recently registered live flow.
func (m *FlowManager) Active() (string, ManagedFlow) {
	if m == nil {
		return "", nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.activeID, m.flows[m.activeID]
}

// Get resolves a host-owned flow by its stable id.
func (m *FlowManager) Get(id string) ManagedFlow {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.flows[id]
}

// Release forgets flow only when the caller still owns the registered
// instance. It returns whether ownership was released. It does not stop the
// flow; callers use this after natural exit.
func (m *FlowManager) Release(id string, flow ManagedFlow) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if current := m.flows[id]; !sameManagedFlow(current, flow) {
		return false
	}
	m.removeLocked(id)
	return true
}

// Stop stops and releases one host-owned flow.
func (m *FlowManager) Stop(id string, destroy bool) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	flow := m.flows[id]
	if !nilManagedFlow(flow) {
		m.removeLocked(id)
	}
	m.mu.Unlock()
	return teardownFlow(flow, destroy)
}

// Close stops every flow owned by the manager. It is idempotent.
func (m *FlowManager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	flows := make([]ManagedFlow, 0, len(m.flows))
	for _, flow := range m.flows {
		flows = append(flows, flow)
	}
	m.flows = make(map[string]ManagedFlow)
	m.order = nil
	m.activeID = ""
	m.mu.Unlock()

	var first error
	for _, flow := range flows {
		if err := teardownFlow(flow, false); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (m *FlowManager) removeLocked(id string) {
	delete(m.flows, id)
	for index, candidate := range m.order {
		if candidate == id {
			m.order = append(m.order[:index], m.order[index+1:]...)
			break
		}
	}
	m.activeID = ""
	if len(m.order) > 0 {
		m.activeID = m.order[len(m.order)-1]
	}
}

func sameManagedFlow(left, right ManagedFlow) bool {
	if nilManagedFlow(left) || nilManagedFlow(right) {
		return nilManagedFlow(left) && nilManagedFlow(right)
	}
	leftValue, rightValue := reflect.ValueOf(left), reflect.ValueOf(right)
	if leftValue.Type() != rightValue.Type() || !leftValue.Type().Comparable() {
		return false
	}
	return leftValue.Interface() == rightValue.Interface()
}

func teardownFlow(flow ManagedFlow, destroy bool) error {
	if nilManagedFlow(flow) {
		return nil
	}
	var err error
	if destroy {
		err = flow.Shutdown()
	} else {
		err = flow.Stop()
	}
	for _, key := range flow.AgentCacheKeys() {
		services.ClearAgent(key)
	}
	return err
}

func nilManagedFlow(flow ManagedFlow) bool {
	if flow == nil {
		return true
	}
	value := reflect.ValueOf(flow)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
