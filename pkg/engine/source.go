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
	mu       sync.RWMutex
	server   *codecore.DefaultCodeServer
	analyzer codecore.SemanticAnalyzer
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

// SourceImports returns the per-file import inventory for this root in the
// given language. An agent assembled without the analyzer answers project info
// with the inventory omitted, and this is how the engine takes that work over.
// Extraction needs a real parser: a scanner cannot tell a syntax error from a
// complete file, and reporting a partial inventory as whole is the failure the
// inspection exists to prevent.
func (s *Source) SourceImports(ctx context.Context, language string) ([]*codev0.SourceFileInfo, error) {
	if s == nil {
		return nil, fmt.Errorf("source behavior is closed")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.server == nil {
		return nil, fmt.Errorf("source behavior is closed")
	}
	if s.analyzer == nil {
		return nil, fmt.Errorf("import inventory for language %q requires a source-semantics analyzer", language)
	}
	return s.analyzer.SourceImports(ctx, s.server.FS, s.server.SourceDir, language)
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
