package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	codev0 "github.com/codefly-dev/core/generated/go/codefly/services/code/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/wool"
)

// serviceModuleService is the common param set for per-service tools.
const serviceModuleService = "module and service are required"

// getServiceOnly resolves module and service from workspace without starting the plugin.
func (s *Server) getServiceOnly(ctx context.Context, moduleName, serviceName string) (*resources.Service, error) {
	if s.workspace == nil {
		return nil, fmt.Errorf("no workspace loaded - run from a codefly workspace directory")
	}
	if moduleName == "" || serviceName == "" {
		return nil, fmt.Errorf(serviceModuleService)
	}
	mod, err := s.workspace.LoadModuleFromName(ctx, moduleName)
	if err != nil {
		return nil, fmt.Errorf("module not found: %s", moduleName)
	}
	svc, err := mod.LoadServiceFromName(ctx, serviceName)
	if err != nil {
		return nil, fmt.Errorf("service not found: %s/%s", moduleName, serviceName)
	}
	return svc, nil
}

// getServiceAndInstance loads workspace module/service and ensures the plugin is loaded with context.
// Caller must have s.workspace set.
func (s *Server) getServiceAndInstance(ctx context.Context, moduleName, serviceName string) (*resources.Service, *services.Instance, error) {
	if s.workspace == nil {
		return nil, nil, fmt.Errorf("no workspace loaded - run from a codefly workspace directory")
	}
	if moduleName == "" || serviceName == "" {
		return nil, nil, fmt.Errorf(serviceModuleService)
	}
	mod, err := s.workspace.LoadModuleFromName(ctx, moduleName)
	if err != nil {
		return nil, nil, fmt.Errorf("module not found: %s", moduleName)
	}
	svc, err := mod.LoadServiceFromName(ctx, serviceName)
	if err != nil {
		return nil, nil, fmt.Errorf("service not found: %s/%s", moduleName, serviceName)
	}
	instance, err := services.Load(ctx, s.workspace, mod, svc)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot load service instance: %w", err)
	}
	if err := instance.LoadBuilder(ctx); err != nil {
		return nil, nil, fmt.Errorf("cannot load builder: %w", err)
	}
	if _, err := instance.Builder.Load(ctx); err != nil {
		return nil, nil, fmt.Errorf("cannot load plugin context: %w", err)
	}
	return svc, instance, nil
}

// describe returns service metadata (name, type, language, file list) for Mind.
func (s *Server) describe(ctx context.Context, args map[string]string) ([]Content, error) {
	svc, err := s.getServiceOnly(ctx, args["module"], args["service"])
	if err != nil {
		return nil, err
	}
	info := map[string]any{
		"name":     svc.Name,
		"module":   args["module"],
		"language": "go",
	}
	if svc.Agent != nil {
		info["agent"] = svc.Agent.Name
	}
	files, _ := fileList(svc.Dir())
	info["files"] = files
	data, _ := json.MarshalIndent(info, "", "  ")
	return []Content{TextContent(string(data))}, nil
}

func fileList(dir string) ([]string, error) {
	var out []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == ".git" || info.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		out = append(out, rel)
		return nil
	})
	return out, err
}

// readFile reads a file from the service directory (path relative to service).
func (s *Server) readFile(ctx context.Context, args map[string]string) ([]Content, error) {
	svc, err := s.getServiceOnly(ctx, args["module"], args["service"])
	if err != nil {
		return nil, err
	}
	path := args["path"]
	if path == "" {
		return nil, fmt.Errorf("path is required")
	}
	fullPath, err := filepath.Abs(filepath.Join(svc.Dir(), path))
	if err != nil {
		return nil, err
	}
	serviceDir, _ := filepath.Abs(svc.Dir())
	if serviceDir != fullPath && !strings.HasPrefix(fullPath, serviceDir+string(os.PathSeparator)) {
		return nil, fmt.Errorf("path escapes service directory")
	}
	var data []byte
	if s.vfs != nil {
		data, err = s.vfs.ReadFile(fullPath)
	} else {
		data, err = os.ReadFile(fullPath)
	}
	if err != nil {
		return nil, fmt.Errorf("read_file: %w", err)
	}
	return []Content{TextContent(string(data))}, nil
}

// writeFile writes content to a file in the service directory.
func (s *Server) writeFile(ctx context.Context, args map[string]string) ([]Content, error) {
	svc, err := s.getServiceOnly(ctx, args["module"], args["service"])
	if err != nil {
		return nil, err
	}
	path := args["path"]
	content := args["content"]
	if path == "" {
		return nil, fmt.Errorf("path is required")
	}
	fullPath, err := filepath.Abs(filepath.Join(svc.Dir(), path))
	if err != nil {
		return nil, err
	}
	serviceDir, _ := filepath.Abs(svc.Dir())
	if serviceDir != fullPath && !strings.HasPrefix(fullPath, serviceDir+string(os.PathSeparator)) {
		return nil, fmt.Errorf("path escapes service directory")
	}
	if s.vfs != nil {
		if mkErr := s.vfs.MkdirAll(filepath.Dir(fullPath), 0755); mkErr != nil {
			return nil, fmt.Errorf("write_file mkdir: %w", mkErr)
		}
		if wErr := s.vfs.WriteFile(fullPath, []byte(content), 0644); wErr != nil {
			return nil, fmt.Errorf("write_file: %w", wErr)
		}
	} else {
		if mkErr := os.MkdirAll(filepath.Dir(fullPath), 0755); mkErr != nil {
			return nil, fmt.Errorf("write_file mkdir: %w", mkErr)
		}
		if wErr := os.WriteFile(fullPath, []byte(content), 0644); wErr != nil {
			return nil, fmt.Errorf("write_file: %w", wErr)
		}
	}
	return []Content{TextContent("ok")}, nil
}

// listSymbols calls the plugin Code.ListSymbols and returns symbols as JSON.
func (s *Server) listSymbols(ctx context.Context, args map[string]string) ([]Content, error) {
	w := wool.Get(ctx).In("mcp.listSymbols")
	_, instance, err := s.getServiceAndInstance(ctx, args["module"], args["service"])
	if err != nil {
		return nil, err
	}
	codeAgent, err := services.LoadCode(ctx, instance.Service)
	if err != nil {
		return nil, fmt.Errorf("cannot load code agent: %w", err)
	}
	req := &codev0.ListSymbolsRequest{}
	if f := args["file"]; f != "" {
		req.File = f
	}
	resp, err := codeAgent.ListSymbols(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("list_symbols: %w", err)
	}
	if resp.Status != nil && resp.Status.State == codev0.ListSymbolsStatus_ERROR {
		return nil, fmt.Errorf("list_symbols: %s", resp.Status.Message)
	}
	out := make([]map[string]any, 0, len(resp.Symbols))
	for _, sym := range resp.Symbols {
		entry := map[string]any{"name": sym.Name, "kind": sym.Kind.String()}
		if sym.Location != nil {
			entry["file"] = sym.Location.File
			entry["line"] = sym.Location.Line
		}
		if sym.Signature != "" {
			entry["signature"] = sym.Signature
		}
		out = append(out, entry)
	}
	w.Debug("list_symbols", wool.Field("count", len(out)))
	data, _ := json.MarshalIndent(out, "", "  ")
	return []Content{TextContent(string(data))}, nil
}

// build triggers the plugin Builder.Build. Uses minimal DockerBuildContext (empty repo for local).
func (s *Server) build(ctx context.Context, args map[string]string) ([]Content, error) {
	_, instance, err := s.getServiceAndInstance(ctx, args["module"], args["service"])
	if err != nil {
		return nil, err
	}
	req := &builderv0.BuildRequest{
		BuildContext: &builderv0.BuildContext{
			Kind: &builderv0.BuildContext_DockerBuildContext{
				DockerBuildContext: &builderv0.DockerBuildContext{
					DockerRepository: "codefly.local",
				},
			},
		},
	}
	resp, err := instance.Builder.Build(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("build: %w", err)
	}
	if resp.State != nil && resp.State.State == builderv0.BuildStatus_ERROR {
		return []Content{TextContent(fmt.Sprintf("build failed: %s", resp.State.Message))}, nil
	}
	return []Content{TextContent("build success")}, nil
}

// runChecks runs a single command in the service directory (e.g. "go test ./..."). Does not start the service.
func (s *Server) runChecks(ctx context.Context, args map[string]string) ([]Content, error) {
	svc, _, err := s.getServiceAndInstance(ctx, args["module"], args["service"])
	if err != nil {
		return nil, err
	}
	command := args["command"]
	if command == "" {
		command = "go test ./..."
	}
	// Run command in service dir via os/exec
	out, err := runCommandInDir(ctx, svc.Dir(), command)
	if err != nil {
		return []Content{TextContent(fmt.Sprintf("run_checks failed: %v\n%s", err, out))}, nil
	}
	return []Content{TextContent(out)}, nil
}

// stop stops the service runtime if it was started (e.g. by run_checks or externally).
func (s *Server) stop(ctx context.Context, args map[string]string) ([]Content, error) {
	_, instance, err := s.getServiceAndInstance(ctx, args["module"], args["service"])
	if err != nil {
		return nil, err
	}
	if err := instance.LoadRuntime(ctx, false); err != nil {
		return []Content{TextContent("stop: runtime not loaded (service may not be running)")}, nil
	}
	_, err = instance.Runtime.Stop(ctx, &runtimev0.StopRequest{})
	if err != nil {
		return nil, fmt.Errorf("stop: %w", err)
	}
	return []Content{TextContent("stopped")}, nil
}

func runCommandInDir(ctx context.Context, dir, command string) (string, error) {
	parts := strings.Fields(command)
	if len(parts) == 0 {
		return "", fmt.Errorf("empty command")
	}
	cmd := exec.CommandContext(ctx, parts[0], parts[1:]...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}
