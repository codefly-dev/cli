package engine

import (
	"context"
	"fmt"
	"sync"

	"github.com/codefly-dev/cli/pkg/sourceworkspace"
	codecore "github.com/codefly-dev/core/code"
	codev0 "github.com/codefly-dev/core/generated/go/codefly/services/code/v0"
)

// Source is Codefly's language-neutral behavior for a rooted source tree.
// Language plugins override or extend this behavior; adapters do not.
type Source struct {
	mu     sync.RWMutex
	server *codecore.DefaultCodeServer
}

// ExecuteCode executes a language-neutral Code request.
func (s *Source) ExecuteCode(ctx context.Context, request *codev0.CodeRequest) (*codev0.CodeResponse, error) {
	if s == nil {
		return nil, fmt.Errorf("source behavior is closed")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.server == nil {
		return nil, fmt.Errorf("source behavior is closed")
	}
	return s.server.Execute(ctx, request)
}

// Close releases source observation resources.
func (s *Source) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.server == nil {
		return nil
	}
	err := s.server.Close()
	s.server = nil
	return err
}

// DetectSourceAgent selects a language service agent from source evidence. This
// is a Codefly policy: Mind and other adapters ask for typed behavior and stay
// unaware of language toolchains.
func DetectSourceAgent(ctx context.Context, root string) (string, error) {
	agent, err := sourceworkspace.SelectPlugin(ctx, root)
	if err != nil {
		return "", err
	}
	return agent.String(), nil
}
