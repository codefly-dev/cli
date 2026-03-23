package agents

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

type agentYAML struct {
	Publisher string `yaml:"publisher"`
	Kind      string `yaml:"kind"`
	Name      string `yaml:"name"`
	Version   string `yaml:"version"`
}

// BuildCmd builds an agent binary from source and installs it locally.
var BuildCmd = &cobra.Command{
	Use:   "build",
	Short: "Build an agent binary from source and install it locally",
	Long: `Build compiles the agent in the current (or specified) directory and
installs the binary to ~/.codefly/agents/ so it can be loaded by the
Gateway daemon.

The directory must contain an agent.codefly.yaml with publisher, kind,
name, and version fields.

When run inside the codefly.dev monorepo, local replace directives for
wool and core are added automatically so go mod tidy succeeds without
requiring published module versions.

Use --all from the agents/services/ directory (or any parent containing
agent directories) to build every agent that has an agent.codefly.yaml.

Examples:
  cd agents/services/go-generic && codefly agent build
  codefly agent build --dir ./agents/services/go-generic
  cd agents/services && codefly agent build --all`,
	Run: func(cmd *cobra.Command, args []string) {
		all, _ := cmd.Flags().GetBool("all")
		dir, _ := cmd.Flags().GetString("dir")

		if all {
			if dir == "" {
				var err error
				dir, err = os.Getwd()
				if err != nil {
					cli.Error("Cannot get working directory: %v", err)
					cli.Exit()
				}
			}
			absDir, err := filepath.Abs(dir)
			if err != nil {
				cli.Error("Cannot resolve directory: %v", err)
				cli.Exit()
			}
			if err := buildAllAgents(absDir); err != nil {
				cli.Error("Build --all failed: %v", err)
				cli.ExitError()
			}
			return
		}

		if dir == "" {
			var err error
			dir, err = os.Getwd()
			if err != nil {
				cli.Error("Cannot get working directory: %v", err)
				cli.Exit()
			}
		}

		absDir, err := filepath.Abs(dir)
		if err != nil {
			cli.Error("Cannot resolve directory: %v", err)
			cli.Exit()
		}

		if err := buildAgent(absDir); err != nil {
			cli.Error("Build failed: %v", err)
			cli.ExitError()
		}
	},
}

func init() {
	BuildCmd.Flags().String("dir", "", "Agent source directory (default: current directory)")
	BuildCmd.Flags().Bool("all", false, "Build all agents found in the current directory tree")
}

// buildAllAgents discovers all directories containing agent.codefly.yaml
// under root and builds each one.
func buildAllAgents(root string) error {
	var agents []string
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("read directory %s: %w", root, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		yamlPath := filepath.Join(root, e.Name(), "agent.codefly.yaml")
		if _, err := os.Stat(yamlPath); err == nil {
			agents = append(agents, filepath.Join(root, e.Name()))
		}
	}

	if len(agents) == 0 {
		return fmt.Errorf("no agent directories found under %s", root)
	}

	cli.Header(1, "Building %d agents", len(agents))
	var failed []string
	for i, agentDir := range agents {
		cli.Header(2, "[%d/%d] %s", i+1, len(agents), filepath.Base(agentDir))
		if err := buildAgent(agentDir); err != nil {
			cli.Error("  Failed: %v", err)
			failed = append(failed, filepath.Base(agentDir))
			continue
		}
	}

	if len(failed) > 0 {
		return fmt.Errorf("%d agent(s) failed to build: %s", len(failed), strings.Join(failed, ", "))
	}

	cli.Header(1, "All %d agents built successfully", len(agents))
	return nil
}

// monorepoModules lists modules that may need local replace directives
// when building inside the codefly.dev monorepo.
var monorepoModules = []struct {
	Module string
	SubDir string
}{
	{"github.com/codefly-dev/core/wool/otel", "wool/otel"},
	{"github.com/codefly-dev/core/wool", "wool"},
	{"github.com/codefly-dev/core", "core"},
}

// findMonorepoRoot walks up from dir looking for the codefly.dev monorepo
// root (identified by having both wool/ and core/ directories).
func findMonorepoRoot(dir string) string {
	cur := dir
	for {
		woolDir := filepath.Join(cur, "wool")
		coreDir := filepath.Join(cur, "core")
		if isDir(woolDir) && isDir(coreDir) {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		cur = parent
	}
}

func isDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func goModRequires(dir, module string) bool {
	f, err := os.Open(filepath.Join(dir, "go.mod"))
	if err != nil {
		return false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), module) {
			return true
		}
	}
	return false
}

func addReplace(dir, module, localPath string) error {
	cmd := exec.Command("go", "mod", "edit",
		"-replace", fmt.Sprintf("%s=%s", module, localPath))
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func dropReplace(dir, module string) error {
	cmd := exec.Command("go", "mod", "edit", "-dropreplace", module)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func buildAgent(dir string) error {
	yamlPath := filepath.Join(dir, "agent.codefly.yaml")
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		return fmt.Errorf("read agent.codefly.yaml in %s: %w (is this an agent directory?)", dir, err)
	}

	var ag agentYAML
	if err := yaml.Unmarshal(data, &ag); err != nil {
		return fmt.Errorf("parse agent.codefly.yaml: %w", err)
	}
	if ag.Name == "" || ag.Version == "" || ag.Publisher == "" {
		return fmt.Errorf("agent.codefly.yaml must have publisher, name, and version")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("get home dir: %w", err)
	}

	subdir := "services"
	if ag.Kind == "codefly:application" {
		subdir = "applications"
	} else if ag.Kind == "codefly:module" {
		subdir = "modules"
	}

	nativeDir := filepath.Join(home, ".codefly", "agents", subdir, ag.Publisher)
	binaryName := fmt.Sprintf("%s__%s", ag.Name, ag.Version)
	nativePath := filepath.Join(nativeDir, binaryName)

	if err := os.MkdirAll(nativeDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", nativeDir, err)
	}

	// Auto-detect monorepo and add local replace directives.
	// When core is required (directly or transitively), wool and wool/otel
	// must also be replaced because they are not published to a public registry.
	monoRoot := findMonorepoRoot(dir)
	var addedReplaces []string
	if monoRoot != "" {
		cli.Info("Monorepo detected at %s", monoRoot)
		requiresCore := goModRequires(dir, "github.com/codefly-dev/core")
		for _, m := range monorepoModules {
			if !goModRequires(dir, m.Module) && !requiresCore {
				continue
			}
			localPath := filepath.Join(monoRoot, m.SubDir)
			if !isDir(localPath) {
				continue
			}
			cli.Info("  Adding replace: %s => %s", m.Module, localPath)
			if err := addReplace(dir, m.Module, localPath); err != nil {
				return fmt.Errorf("add replace for %s: %w", m.Module, err)
			}
			addedReplaces = append(addedReplaces, m.Module)
		}
	}

	// Clean up replace directives when done (even on error).
	defer func() {
		for _, mod := range addedReplaces {
			_ = dropReplace(dir, mod)
		}
	}()

	cli.Info("Tidying modules...")
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	tidy.Stdout = os.Stdout
	tidy.Stderr = os.Stderr
	if err := tidy.Run(); err != nil {
		return fmt.Errorf("go mod tidy: %w", err)
	}

	cli.Header(1, "Building %s:%s (%s/%s)", ag.Name, ag.Version, runtime.GOOS, runtime.GOARCH)
	build := exec.Command("go", "build", "-o", nativePath, ".")
	build.Dir = dir
	build.Stdout = os.Stdout
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		return fmt.Errorf("go build (native): %w", err)
	}
	cli.Info("Installed: %s", nativePath)

	containerDir := filepath.Join(home, ".codefly", "containers", "agents", subdir, ag.Publisher)
	containerPath := filepath.Join(containerDir, binaryName)
	if err := os.MkdirAll(containerDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", containerDir, err)
	}

	cli.Info("Building Linux/amd64 static binary...")
	ldflags := `-extldflags "-static"`
	crossBuild := exec.Command("go", "build", "-ldflags", ldflags, "-o", containerPath, ".")
	crossBuild.Dir = dir
	crossBuild.Env = append(os.Environ(),
		"CGO_ENABLED=0",
		"GOOS=linux",
		"GOARCH=amd64",
	)
	crossBuild.Stdout = os.Stdout
	crossBuild.Stderr = os.Stderr
	if err := crossBuild.Run(); err != nil {
		cli.Info("Warning: Linux cross-build failed (non-fatal): %v", err)
	} else {
		cli.Info("Installed (container): %s", containerPath)
	}

	cli.Header(1, "Agent %s:%s built successfully", ag.Name, ag.Version)
	return nil
}
