// Package toolbox defines Codefly's transport-neutral tool surface.
//
// MCP is one adapter over a Registry. An in-process agent loop can list and
// call the same definitions directly without importing the MCP protocol or
// round-tripping JSON-RPC.
package toolbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

var ErrUnknownTool = errors.New("unknown tool")

type PropertySchema struct {
	Type        string   `json:"type"`
	Description string   `json:"description,omitempty"`
	Enum        []string `json:"enum,omitempty"`
}

type InputSchema struct {
	Type       string                    `json:"type"`
	Properties map[string]PropertySchema `json:"properties,omitempty"`
	Required   []string                  `json:"required,omitempty"`
}

type Tool struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	InputSchema InputSchema `json:"inputSchema"`
}

type Content struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

func TextContent(text string) Content {
	return Content{Type: "text", Text: text}
}

func ErrorContent(err error) Content {
	return Content{Type: "text", Text: "Error: " + err.Error()}
}

type Handler func(context.Context, map[string]string) ([]Content, error)

type entry struct {
	definition Tool
	handler    Handler
}

// Registry owns an ordered, concurrently callable set of tools.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]entry
	order   []string
	closed  bool
}

func NewRegistry() *Registry {
	return &Registry{entries: make(map[string]entry)}
}

// Register adds or atomically replaces a tool. Replacement preserves list
// order, which keeps adapter output stable.
func (r *Registry) Register(definition Tool, handler Handler) error {
	if r == nil {
		return fmt.Errorf("tool registry is unavailable")
	}
	if definition.Name == "" {
		return fmt.Errorf("tool name is required")
	}
	if handler == nil {
		return fmt.Errorf("handler for tool %q is required", definition.Name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return fmt.Errorf("tool registry is closed")
	}
	if _, exists := r.entries[definition.Name]; !exists {
		r.order = append(r.order, definition.Name)
	}
	r.entries[definition.Name] = entry{definition: definition, handler: handler}
	return nil
}

// Definitions returns tool metadata in registration order.
func (r *Registry) Definitions() []Tool {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	definitions := make([]Tool, 0, len(r.order))
	for _, name := range r.order {
		definitions = append(definitions, cloneTool(r.entries[name].definition))
	}
	return definitions
}

// Call invokes a tool in-process.
func (r *Registry) Call(ctx context.Context, name string, arguments map[string]string) ([]Content, error) {
	if r == nil {
		return nil, fmt.Errorf("tool registry is unavailable")
	}
	r.mu.RLock()
	if r.closed {
		r.mu.RUnlock()
		return nil, fmt.Errorf("tool registry is closed")
	}
	registered, ok := r.entries[name]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownTool, name)
	}
	return registered.handler(ctx, arguments)
}

// Close prevents new registrations and calls. It is idempotent.
func (r *Registry) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
}

func cloneTool(tool Tool) Tool {
	clone := tool
	clone.InputSchema.Required = append([]string(nil), tool.InputSchema.Required...)
	if tool.InputSchema.Properties != nil {
		clone.InputSchema.Properties = make(map[string]PropertySchema, len(tool.InputSchema.Properties))
		for name, property := range tool.InputSchema.Properties {
			property.Enum = append([]string(nil), property.Enum...)
			clone.InputSchema.Properties[name] = property
		}
	}
	return clone
}
