package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/cli/pkg/control"
	"github.com/codefly-dev/core/resources"
)

// serviceModuleService is the common param set for per-service tools.
const serviceModuleService = "module and service are required"

// getServiceOnly resolves module and service from workspace without starting the plugin.
func (s *Server) getServiceOnly(ctx context.Context, moduleName, serviceName string) (*resources.Service, error) {
	if s.workspace == nil {
		return nil, fmt.Errorf("no workspace loaded - run from a codefly workspace directory")
	}
	if moduleName == "" || serviceName == "" {
		return nil, errors.New(serviceModuleService)
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

func (s *Server) serviceScope(ctx context.Context, args map[string]string) (control.ServiceScope, error) {
	if args["module"] == "" || args["service"] == "" {
		return nil, errors.New(serviceModuleService)
	}
	return s.plane.Service(ctx, serviceRef(args))
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
	path := args["path"]
	if path == "" {
		return nil, fmt.Errorf("path is required")
	}
	if s.vfs == nil {
		scope, err := s.serviceScope(ctx, args)
		if err != nil {
			return nil, err
		}
		data, err := scope.ReadFile(ctx, path)
		if err != nil {
			return nil, fmt.Errorf("read_file: %w", err)
		}
		return []Content{TextContent(string(data))}, nil
	}
	svc, err := s.getServiceOnly(ctx, args["module"], args["service"])
	if err != nil {
		return nil, err
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
	data, err = s.vfs.ReadFile(fullPath)
	if err != nil {
		return nil, fmt.Errorf("read_file: %w", err)
	}
	return []Content{TextContent(string(data))}, nil
}

// writeFile writes content to a file in the service directory.
func (s *Server) writeFile(ctx context.Context, args map[string]string) ([]Content, error) {
	path := args["path"]
	content := args["content"]
	if path == "" {
		return nil, fmt.Errorf("path is required")
	}
	if s.vfs == nil {
		scope, err := s.serviceScope(ctx, args)
		if err != nil {
			return nil, err
		}
		if err := scope.WriteFile(ctx, path, []byte(content)); err != nil {
			return nil, fmt.Errorf("write_file: %w", err)
		}
		return []Content{TextContent("ok")}, nil
	}
	svc, err := s.getServiceOnly(ctx, args["module"], args["service"])
	if err != nil {
		return nil, err
	}
	fullPath, err := filepath.Abs(filepath.Join(svc.Dir(), path))
	if err != nil {
		return nil, err
	}
	serviceDir, _ := filepath.Abs(svc.Dir())
	if serviceDir != fullPath && !strings.HasPrefix(fullPath, serviceDir+string(os.PathSeparator)) {
		return nil, fmt.Errorf("path escapes service directory")
	}
	if mkErr := s.vfs.MkdirAll(filepath.Dir(fullPath), 0755); mkErr != nil {
		return nil, fmt.Errorf("write_file mkdir: %w", mkErr)
	}
	if wErr := s.vfs.WriteFile(fullPath, []byte(content), 0644); wErr != nil {
		return nil, fmt.Errorf("write_file: %w", wErr)
	}
	return []Content{TextContent("ok")}, nil
}

// build delegates single-service behavior to the shared control/engine path.
func (s *Server) build(ctx context.Context, args map[string]string) ([]Content, error) {
	scope, err := s.serviceScope(ctx, args)
	if err != nil {
		return nil, err
	}
	result, err := scope.Build(ctx, control.BuildRequest{})
	if err != nil {
		return nil, fmt.Errorf("build: %w", err)
	}
	if !result.Succeeded {
		return []Content{TextContent("build failed: " + result.Output)}, nil
	}
	return []Content{TextContent("build success\n" + result.Output)}, nil
}

// runChecks runs a single command in the service directory (e.g. "go test ./..."). Does not start the service.
func (s *Server) runChecks(ctx context.Context, args map[string]string) ([]Content, error) {
	scope, err := s.serviceScope(ctx, args)
	if err != nil {
		return nil, err
	}
	command := args["command"]
	if command == "" {
		command = "go test ./..."
	}
	result, err := scope.RunChecks(ctx, control.CheckRequest{Command: command})
	if err != nil {
		return nil, err
	}
	if !result.Passed {
		return []Content{TextContent("run_checks failed:\n" + result.Output)}, nil
	}
	return []Content{TextContent(result.Output)}, nil
}

// stop stops the service runtime if it was started (e.g. by run_checks or externally).
func (s *Server) stop(ctx context.Context, args map[string]string) ([]Content, error) {
	scope, err := s.serviceScope(ctx, args)
	if err != nil {
		return nil, err
	}
	if err := scope.Stop(ctx); err != nil {
		return nil, fmt.Errorf("stop: %w", err)
	}
	return []Content{TextContent("stopped")}, nil
}

// listServiceCommands returns commands registered by the service agent (via the
// control plane).
func (s *Server) listServiceCommands(ctx context.Context, args map[string]string) ([]Content, error) {
	commands, err := s.plane.ListCommands(ctx, serviceRef(args))
	if err != nil {
		return nil, fmt.Errorf("cannot list commands: %w", err)
	}
	type cmdInfo struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Usage       string   `json:"usage,omitempty"`
		Tags        []string `json:"tags,omitempty"`
		Destructive bool     `json:"destructive,omitempty"`
	}
	out := make([]cmdInfo, 0, len(commands))
	for _, cmd := range commands {
		out = append(out, cmdInfo{
			Name:        cmd.Name,
			Description: cmd.Description,
			Usage:       cmd.Usage,
			Tags:        cmd.Tags,
			Destructive: cmd.Destructive,
		})
	}
	data, _ := json.MarshalIndent(out, "", "  ")
	return []Content{TextContent(string(data))}, nil
}

// runServiceCommand executes a command on the service agent (via the control
// plane).
func (s *Server) runServiceCommand(ctx context.Context, args map[string]string) ([]Content, error) {
	command := args["command"]
	if command == "" {
		return nil, fmt.Errorf("command is required")
	}
	var cmdArgs []string
	if argsStr := args["args"]; argsStr != "" {
		// Try JSON array first, fall back to space-separated.
		if err := json.Unmarshal([]byte(argsStr), &cmdArgs); err != nil {
			cmdArgs = strings.Split(argsStr, " ")
		}
	}
	result, err := s.plane.RunCommand(ctx, control.RunCommandRequest{
		Service: serviceRef(args),
		Command: command,
		Args:    cmdArgs,
	})
	if err != nil {
		return nil, fmt.Errorf("command failed: %w", err)
	}
	if result.ExitCode != 0 {
		return []Content{TextContent(fmt.Sprintf("FAILED: %s", result.Output))}, nil
	}
	return []Content{TextContent(result.Output)}, nil
}

// serviceRef joins the module/service args into the "module/service" reference
// the control plane resolves.
func serviceRef(args map[string]string) string {
	module, service := args["module"], args["service"]
	if module == "" {
		return service
	}
	return module + "/" + service
}
