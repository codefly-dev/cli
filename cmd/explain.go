package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/codefly-dev/cli/pkg/helpprovider"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

const (
	defaultHelpProvider = "codefly-help"
	maxHelpNames        = 50
)

var ExplainCmd = &cobra.Command{
	Use:   "explain [command...]",
	Short: "Show static help with an optional workspace-aware AI explanation",
	Long: `Show the complete static help for a command and, when a help provider is installed,
append a contextual AI explanation.

The provider receives command help and a bounded inventory of workspace,
module, service, job, and environment names. It never receives source files.
If the provider is unavailable or not configured, the static help still succeeds.`,
	Example: `  codefly explain build service
  codefly explain deploy module
  CODEFLY_HELP_PROVIDER=codefly-help codefly explain run service`,
	RunE: func(command *cobra.Command, args []string) error {
		target, err := findExplainTarget(RootCmd, args)
		if err != nil {
			return err
		}
		staticHelp, err := renderCommandHelp(target)
		if err != nil {
			return fmt.Errorf("render help for %s: %w", target.CommandPath(), err)
		}
		if _, err := fmt.Fprint(command.OutOrStdout(), staticHelp); err != nil {
			return fmt.Errorf("print static help: %w", err)
		}

		provider, configured := helpProviderFromEnvironment()
		if !configured {
			_, _ = fmt.Fprintln(command.ErrOrStderr(), "\nAI explanation unavailable: install codefly-help or set CODEFLY_HELP_PROVIDER.")
			return nil
		}

		workspace := readHelpWorkspaceContext()
		explanation, err := provider.explain(command.Context(), target.CommandPath(), staticHelp, workspace)
		if err != nil {
			_, _ = fmt.Fprintf(command.ErrOrStderr(), "\nAI explanation unavailable: %v\n", err)
			return nil
		}
		_, err = fmt.Fprintf(command.OutOrStdout(), "\nContextual explanation (AI-generated):\n\n%s\n", explanation)
		return err
	},
}

func findExplainTarget(root *cobra.Command, path []string) (*cobra.Command, error) {
	current := root
	for _, part := range path {
		var match *cobra.Command
		for _, candidate := range current.Commands() {
			if candidate.Name() == part {
				match = candidate
				break
			}
			for _, alias := range candidate.Aliases {
				if alias == part {
					match = candidate
					break
				}
			}
		}
		if match == nil {
			return nil, rejectUnknownSubcommand(current, []string{part})
		}
		current = match
	}
	return current, nil
}

func renderCommandHelp(command *cobra.Command) (string, error) {
	var output bytes.Buffer
	previous := command.OutOrStdout()
	command.SetOut(&output)
	defer command.SetOut(previous)
	if err := command.Help(); err != nil {
		return "", err
	}
	return output.String(), nil
}

type helpProvider struct {
	path string
	args []string
}

func helpProviderFromEnvironment() (*helpProvider, bool) {
	name := strings.TrimSpace(os.Getenv("CODEFLY_HELP_PROVIDER"))
	if name == "" {
		name = defaultHelpProvider
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return nil, false
	}
	return &helpProvider{path: path}, true
}

func (provider *helpProvider) explain(ctx context.Context, commandPath, staticHelp, workspace string) (string, error) {
	requestBody := helpprovider.Request{
		ProtocolVersion: helpprovider.ProtocolVersion,
		Application:     RootCmd.Name(),
		Command:         commandPath,
		StaticHelp:      staticHelp,
		Context:         json.RawMessage(workspace),
	}
	payload, err := json.Marshal(requestBody)
	if err != nil {
		return "", fmt.Errorf("encode provider request: %w", err)
	}

	providerContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	process := exec.CommandContext(providerContext, provider.path, provider.args...)
	process.Stdin = bytes.NewReader(payload)
	output := newHelpProviderOutput(2 << 20)
	process.Stdout = &output
	if err := process.Run(); err != nil {
		if providerContext.Err() != nil {
			return "", fmt.Errorf("provider timed out: %w", providerContext.Err())
		}
		return "", fmt.Errorf("provider failed: %w", err)
	}
	if output.exceeded {
		return "", fmt.Errorf("provider response exceeds 2 MiB")
	}

	var response helpprovider.Response
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		return "", fmt.Errorf("decode provider response: %w", err)
	}
	if response.ProtocolVersion != helpprovider.ProtocolVersion {
		return "", fmt.Errorf("provider protocol version %d is not supported", response.ProtocolVersion)
	}
	response.Explanation = strings.TrimSpace(response.Explanation)
	if response.Explanation == "" {
		return "", fmt.Errorf("provider returned an empty explanation")
	}
	return response.Explanation, nil
}

type helpProviderOutput struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func newHelpProviderOutput(limit int) helpProviderOutput {
	return helpProviderOutput{limit: limit}
}

func (output *helpProviderOutput) Write(data []byte) (int, error) {
	written := len(data)
	remaining := output.limit - output.Len()
	if remaining <= 0 {
		output.exceeded = true
		return written, nil
	}
	if len(data) > remaining {
		output.exceeded = true
		data = data[:remaining]
	}
	_, _ = output.Buffer.Write(data)
	return written, nil
}

type helpNamedResource struct {
	Name string `yaml:"name" json:"name"`
}

type helpModuleReference struct {
	Name     string              `yaml:"name"`
	Path     string              `yaml:"path,omitempty"`
	Services []helpNamedResource `yaml:"services,omitempty"`
}

type helpWorkspaceFile struct {
	Name         string                `yaml:"name"`
	Layout       string                `yaml:"layout"`
	Modules      []helpModuleReference `yaml:"modules,omitempty"`
	Services     []helpNamedResource   `yaml:"services,omitempty"`
	Jobs         []helpNamedResource   `yaml:"jobs,omitempty"`
	Environments []helpNamedResource   `yaml:"environments,omitempty"`
}

type helpModuleFile struct {
	Services []helpNamedResource `yaml:"services,omitempty"`
	Jobs     []helpNamedResource `yaml:"jobs,omitempty"`
}

type helpWorkspaceInventory struct {
	Workspace    string   `json:"workspace,omitempty"`
	Layout       string   `json:"layout,omitempty"`
	CurrentPath  string   `json:"current_path,omitempty"`
	Modules      []string `json:"modules,omitempty"`
	Services     []string `json:"services,omitempty"`
	Jobs         []string `json:"jobs,omitempty"`
	Environments []string `json:"environments,omitempty"`
}

func readHelpWorkspaceContext() string {
	workingDirectory, err := os.Getwd()
	if err != nil {
		return `{"workspace_detected":false}`
	}
	return helpWorkspaceContext(workingDirectory)
}

func helpWorkspaceContext(start string) string {
	root, data := findHelpWorkspaceFile(start)
	if root == "" {
		return `{"workspace_detected":false}`
	}
	var workspace helpWorkspaceFile
	if yaml.Unmarshal(data, &workspace) != nil {
		return `{"workspace_detected":false,"reason":"workspace metadata could not be parsed"}`
	}

	inventory := helpWorkspaceInventory{Workspace: workspace.Name, Layout: workspace.Layout}
	if relative, err := filepath.Rel(root, start); err == nil && relative != "." && !pathEscapesRoot(relative) {
		inventory.CurrentPath = filepath.ToSlash(relative)
	}
	for _, module := range workspace.Modules {
		inventory.Modules = append(inventory.Modules, module.Name)
		for _, service := range module.Services {
			inventory.Services = append(inventory.Services, module.Name+"/"+service.Name)
		}
		moduleData, ok := readHelpModuleFile(root, module)
		if !ok {
			continue
		}
		var moduleFile helpModuleFile
		if yaml.Unmarshal(moduleData, &moduleFile) != nil {
			continue
		}
		for _, service := range moduleFile.Services {
			inventory.Services = append(inventory.Services, module.Name+"/"+service.Name)
		}
		for _, job := range moduleFile.Jobs {
			inventory.Jobs = append(inventory.Jobs, module.Name+"/"+job.Name)
		}
	}
	if workspace.Layout == "flat" {
		if len(inventory.Modules) == 0 && workspace.Name != "" {
			inventory.Modules = append(inventory.Modules, workspace.Name)
		}
		for _, service := range workspace.Services {
			inventory.Services = append(inventory.Services, service.Name)
		}
		for _, job := range workspace.Jobs {
			inventory.Jobs = append(inventory.Jobs, job.Name)
		}
	}
	for _, environment := range workspace.Environments {
		inventory.Environments = append(inventory.Environments, environment.Name)
	}
	inventory.Modules = normalizeHelpNames(inventory.Modules)
	inventory.Services = normalizeHelpNames(inventory.Services)
	inventory.Jobs = normalizeHelpNames(inventory.Jobs)
	inventory.Environments = normalizeHelpNames(inventory.Environments)
	encoded, err := json.Marshal(inventory)
	if err != nil {
		return `{"workspace_detected":false}`
	}
	return string(encoded)
}

func findHelpWorkspaceFile(start string) (string, []byte) {
	directory, err := filepath.Abs(start)
	if err != nil {
		return "", nil
	}
	for {
		data, err := os.ReadFile(filepath.Join(directory, "workspace.codefly.yaml"))
		if err == nil {
			return directory, data
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", nil
		}
		directory = parent
	}
}

func readHelpModuleFile(root string, module helpModuleReference) ([]byte, bool) {
	directory := filepath.Join(root, "modules", module.Name)
	if module.Path != "" {
		if filepath.IsAbs(module.Path) {
			directory = filepath.Clean(module.Path)
		} else {
			directory = filepath.Join(root, module.Path)
		}
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, false
	}
	resolvedDirectory, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return nil, false
	}
	relative, err := filepath.Rel(resolvedRoot, resolvedDirectory)
	if err != nil || pathEscapesRoot(relative) {
		return nil, false
	}
	data, err := os.ReadFile(filepath.Join(resolvedDirectory, "module.codefly.yaml"))
	return data, err == nil
}

func pathEscapesRoot(relative string) bool {
	return relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func normalizeHelpNames(names []string) []string {
	seen := make(map[string]struct{}, len(names))
	result := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	sort.Strings(result)
	if len(result) > maxHelpNames {
		result = result[:maxHelpNames]
	}
	return result
}
