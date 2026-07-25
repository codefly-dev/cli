package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
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

func newSource(root string) *Source {
	return &Source{server: codecore.NewDefaultCodeServer(root)}
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
func DetectSourceAgent(root string) (string, error) {
	agent, err := sourceworkspace.SelectPlugin(root)
	if err != nil {
		return "", err
	}
	return agent.String(), nil
}

// DetectFormulaAgent selects the plugin that owns an explicit formula command.
// Formula interpretation remains in Codefly; transport adapters do not learn
// language runner syntax.
func DetectFormulaAgent(command []string) string {
	if len(command) == 0 {
		return ""
	}
	executable := strings.ToLower(filepath.Base(command[0]))
	switch executable {
	case "go":
		return fmt.Sprintf("go:%s", sourceworkspace.GenericGoPluginVersion)
	case "python", "python3", "pytest", "uv":
		return fmt.Sprintf("python:%s", sourceworkspace.GenericPythonPluginVersion)
	case "cargo", "rustc":
		return fmt.Sprintf("rust:%s", sourceworkspace.RustPluginVersion)
	case "npm", "npx", "node", "pnpm", "yarn":
		return fmt.Sprintf("nextjs:%s", sourceworkspace.NextJSPluginVersion)
	case "swift":
		return fmt.Sprintf("swift:%s", sourceworkspace.SwiftPluginVersion)
	default:
		return ""
	}
}
