// Package workingrender is the one place a workspace keeps what `codefly
// deploy gitops render` writes before `publish` promotes it: the working
// render.
//
// A working render is scoped to the environment it was rendered for,
// <workspace>/.codefly/renders/<environment>/modules/<module>, so two
// environments rendered in one checkout sit side by side and neither can
// overwrite the other. It lives under the CLI-owned, gitignored `.codefly/`
// scratch directory rather than under `deployments/`, because a workspace's
// `deployments/` tree is what its environments deliver from: a gitops path
// (`deployments/modules`, `deployments/environments/<env>/modules`) is a path
// an Argo application follows, and a render written there would put one
// environment's tree on another environment's delivery path the moment it was
// committed. A delivered tree only ever changes through publish, which copies
// the working render into a clone of the delivery repository at the
// environment's own gitops path.
//
// Every reader and writer of a working render goes through this package, so
// "which render is this?" is answered by the path before any inventory is
// read.
package workingrender

import (
	"fmt"
	"os"
	"path/filepath"
)

// Dir is the workspace-relative root of every working render.
const Dir = ".codefly/renders"

// Root is every environment's working renders in a workspace.
func Root(workspaceDir string) string {
	return filepath.Join(workspaceDir, filepath.FromSlash(Dir))
}

// Environment is one environment's working render.
func Environment(workspaceDir, environment string) string {
	return filepath.Join(Root(workspaceDir), environment)
}

// Modules holds one module tree per module rendered for environment.
func Modules(workspaceDir, environment string) string {
	return filepath.Join(Environment(workspaceDir, environment), "modules")
}

// Module is the working render of one module for one environment.
func Module(workspaceDir, environment, module string) string {
	return filepath.Join(Modules(workspaceDir, environment), module)
}

// Prepare creates environment's working render directory and makes the root
// ignore itself, so a working render can never be committed, whatever the
// workspace's own .gitignore says. Every writer calls it before it writes.
func Prepare(workspaceDir, environment string) error {
	if environment == "" || filepath.Base(environment) != environment || environment == "." || environment == ".." {
		return fmt.Errorf("a working render is scoped to one named environment, not %q", environment)
	}
	if err := os.MkdirAll(Modules(workspaceDir, environment), 0o755); err != nil {
		return fmt.Errorf("create the working render of environment %s: %w", environment, err)
	}
	ignore := filepath.Join(Root(workspaceDir), ".gitignore")
	if data, err := os.ReadFile(ignore); err == nil && string(data) == "*\n" {
		return nil
	}
	if err := os.WriteFile(ignore, []byte("*\n"), 0o600); err != nil {
		return fmt.Errorf("gitignore the working renders: %w", err)
	}
	return nil
}

// Record is where the outcome of the last render of module for environment is
// kept: beside the tree rather than in it, because the tree is what publish
// delivers and a failed render leaves the previous tree in place. Publish
// reads it to refuse a tree no render vouches for now.
func Record(workspaceDir, environment, module string) string {
	return filepath.Join(Environment(workspaceDir, environment), "records", module+".json")
}

// RecordOf answers whose working render destination is — the workspace, the
// environment and the module — when it is one, so a writer that is handed
// only a destination records the render it produced there. A destination
// that is not a module's working render (a service render, a scratch tree)
// answers false.
func RecordOf(destination string) (workspaceDir, environment, module string, ok bool) {
	destination = filepath.Clean(destination)
	modules := filepath.Dir(destination)
	envDir := filepath.Dir(modules)
	root := filepath.Dir(envDir)
	if filepath.Base(modules) != "modules" || filepath.Base(root) != filepath.Base(Dir) || filepath.Base(filepath.Dir(root)) != filepath.Dir(filepath.FromSlash(Dir)) {
		return "", "", "", false
	}
	workspaceDir = filepath.Dir(filepath.Dir(root))
	environment, module = filepath.Base(envDir), filepath.Base(destination)
	if Module(workspaceDir, environment, module) != destination {
		return "", "", "", false
	}
	return workspaceDir, environment, module, true
}
