package engine

import (
	"os"
	"path/filepath"
	"testing"

	codev0 "github.com/codefly-dev/core/generated/go/codefly/services/code/v0"
	"github.com/codefly-dev/core/resources"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type inertManagedFlow struct{}

func (*inertManagedFlow) Stop() error              { return nil }
func (*inertManagedFlow) Shutdown() error          { return nil }
func (*inertManagedFlow) AgentCacheKeys() []string { return nil }

func TestWorkspaceHostRequiresExplicitRoot(t *testing.T) {
	if _, err := NewWorkspaceHost(Config{}); err == nil {
		t.Fatal("expected an empty root to fail")
	}
}

func TestWorkspaceHostAllowsMissingRootForTypedOperationFailures(t *testing.T) {
	root := filepath.Join(t.TempDir(), "checkout-not-created")
	host, err := NewWorkspaceHost(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close() })
	if host.Root() == "" {
		t.Fatal("missing root was not retained")
	}
}

func TestWorkspaceHostResolvesServiceRootsAgainstHost(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "modules", "api"), 0o755); err != nil {
		t.Fatal(err)
	}
	host, err := NewWorkspaceHost(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close() })

	service, err := host.Service(ServiceTarget{Root: "modules/api"})
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(root, "modules", "api"))
	if err != nil {
		t.Fatal(err)
	}
	if service.target.Root != want {
		t.Fatalf("service root = %q, want %q", service.target.Root, want)
	}
}

func TestWorkspaceHostRejectsSymlinkServiceEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "linked-service")); err != nil {
		t.Fatal(err)
	}
	host, err := NewWorkspaceHost(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close() })

	if _, err := host.Service(ServiceTarget{Root: "linked-service"}); err == nil {
		t.Fatal("expected symlink root escape to fail")
	}
}

func TestWorkspaceHostRejectsServiceOutsideRoot(t *testing.T) {
	host, err := NewWorkspaceHost(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close() })

	if _, err := host.Service(ServiceTarget{Root: "../outside"}); err == nil {
		t.Fatal("expected root escape to fail")
	}
}

func TestParentResourceDirFindsNearestTypedBoundary(t *testing.T) {
	root := t.TempDir()
	moduleDir := filepath.Join(root, "modules", "platform")
	serviceDir := filepath.Join(moduleDir, "services", "warden")
	codeDir := filepath.Join(serviceDir, "code", "crates")
	if err := os.MkdirAll(codeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		filepath.Join(root, resources.WorkspaceConfigurationName):     "name: workspace\nlayout: modules\n",
		filepath.Join(moduleDir, resources.ModuleConfigurationName):   "name: platform\n",
		filepath.Join(serviceDir, resources.ServiceConfigurationName): "name: warden\nversion: 0.0.0\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	tests := map[string]struct {
		name string
		want string
	}{
		"workspace": {name: resources.WorkspaceConfigurationName, want: root},
		"module":    {name: resources.ModuleConfigurationName, want: moduleDir},
		"service":   {name: resources.ServiceConfigurationName, want: serviceDir},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := parentResourceDir(codeDir, test.name)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("resource dir=%q, want %q", got, test.want)
			}
		})
	}
}

func TestFlowManagerOwnsRegisteredFlow(t *testing.T) {
	manager := NewFlowManager()
	flow := &inertManagedFlow{}
	if err := manager.Register("module/service", flow); err != nil {
		t.Fatalf("Register() = %v", err)
	}
	id, active := manager.Active()
	if id != "module/service" || active != flow {
		t.Fatalf("Active() = (%q, %p), want (%q, %p)", id, active, "module/service", flow)
	}
	if got := manager.Get("module/service"); got != flow {
		t.Fatalf("Get() = %p, want %p", got, flow)
	}
	if !manager.Release("module/service", flow) {
		t.Fatal("Release() = false, want true")
	}
	if _, active := manager.Active(); active != nil {
		t.Fatalf("Active() after release = %p, want nil", active)
	}
}

func TestFlowManagerCloseRejectsNewFlows(t *testing.T) {
	manager := NewFlowManager()
	if err := manager.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	if err := manager.Register("module/service", &inertManagedFlow{}); err == nil {
		t.Fatal("Register() after Close() succeeded")
	}
}

func TestFlowManagerRestoresPreviousActiveFlow(t *testing.T) {
	manager := NewFlowManager()
	first := &inertManagedFlow{}
	second := &inertManagedFlow{}
	if err := manager.Register("first", first); err != nil {
		t.Fatal(err)
	}
	if err := manager.Register("second", second); err != nil {
		t.Fatal(err)
	}
	if !manager.Release("second", second) {
		t.Fatal("release second flow failed")
	}
	id, active := manager.Active()
	if id != "first" || active != first {
		t.Fatalf("Active() = (%q, %p), want first flow %p", id, active, first)
	}
}

func TestTransientAgentErrorsDoNotReplayInternalFailures(t *testing.T) {
	if !isTransientAgentError(status.Error(codes.Unavailable, "connection lost")) {
		t.Fatal("Unavailable should reconnect once")
	}
	if isTransientAgentError(status.Error(codes.Internal, "operation failed")) {
		t.Fatal("Internal must not replay a potentially mutating operation")
	}
}

func TestCodeRetryOnlyReplaysReadOnlyOperations(t *testing.T) {
	if !codeRequestRetrySafe(&codev0.CodeRequest{
		Operation: &codev0.CodeRequest_ReadFile{ReadFile: &codev0.ReadFileRequest{Path: "README.md"}},
	}) {
		t.Fatal("ReadFile should be safe to retry")
	}
	if codeRequestRetrySafe(&codev0.CodeRequest{
		Operation: &codev0.CodeRequest_WriteFile{WriteFile: &codev0.WriteFileRequest{Path: "README.md"}},
	}) {
		t.Fatal("WriteFile must not be replayed")
	}
	if codeRequestRetrySafe(&codev0.CodeRequest{
		Operation: &codev0.CodeRequest_ShellExec{ShellExec: &codev0.ShellExecRequest{}},
	}) {
		t.Fatal("ShellExec must not be replayed")
	}
}

func TestWorkspaceHostCloseClosesToolbox(t *testing.T) {
	host, err := NewWorkspaceHost(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	tools := host.Toolbox()
	if err := host.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := tools.Call(t.Context(), "missing", nil); err == nil {
		t.Fatal("tool call succeeded after host close")
	}
	if _, err := host.Service(ServiceTarget{}); err == nil {
		t.Fatal("service binding succeeded after host close")
	}
	if host.Source() != nil || host.Flows() != nil || host.Toolbox() != nil {
		t.Fatal("host exposed owned components after close")
	}
}
