package agents

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/blang/semver"
	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/sourceworkspace"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

var (
	sourcePromotionCLIDir   string
	sourcePromotionFixtures []string
)

const (
	latestAgentVersion = "latest"
	fallbackMarker     = "fallback"
)

var PromoteSourceCmd = &cobra.Command{
	Use:   "promote-source <publisher/name:version>",
	Short: "Qualify and generate a source-workspace compatibility pin change",
	Long: `Qualify one released source plugin against every marker it owns, then
update only the CLI compatibility roster. Each fixture is exercised through
codefly test source with the exact candidate artifact in an isolated cache.
The generated pin remains an ordinary reviewable CLI change.`,
	Example: `  codefly agent promote-source codefly.dev/go:0.0.37 \
    --cli-dir ../cli --fixture go.mod=./test-fixtures/go`,
	Args: cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		ctx, stop := common.SignalContext(ctx)
		defer stop()

		fixtures, err := parseSourcePromotionFixtures(sourcePromotionFixtures)
		if err != nil {
			return err
		}
		home, err := os.MkdirTemp("", "codefly-source-promotion-*")
		if err != nil {
			return fmt.Errorf("create isolated Codefly home: %w", err)
		}
		defer os.RemoveAll(home)
		result, err := promoteSourcePlugin(ctx, sourcePromotionOptions{
			agentSpec: args[0],
			cliDir:    sourcePromotionCLIDir,
			fixtures:  fixtures,
		}, func(ctx context.Context, agent *resources.Agent, marker, fixture string) error {
			return runSourceQualification(ctx, agent, marker, fixture, home)
		})
		if err != nil {
			return err
		}
		for _, proof := range result.proofs {
			cli.Info("Qualified %s through %s (%s)", args[0], proof.marker, proof.fixture)
		}
		cli.Header(1, "Generated source-workspace promotion %s -> %s", result.previousVersion, result.version)
		cli.Info("Updated only %s; review and commit this deterministic roster change", result.rosterPath)
		return nil
	},
}

type sourcePromotionOptions struct {
	agentSpec string
	cliDir    string
	fixtures  map[string]string
}

type sourcePromotionProof struct {
	marker  string
	fixture string
}

type sourcePromotionResult struct {
	rosterPath      string
	previousVersion string
	version         string
	proofs          []sourcePromotionProof
}

type sourceQualificationRunner func(context.Context, *resources.Agent, string, string) error

func parseSourcePromotionFixtures(values []string) (map[string]string, error) {
	fixtures := make(map[string]string, len(values))
	for _, value := range values {
		marker, dir, ok := strings.Cut(value, "=")
		marker = strings.TrimSpace(marker)
		dir = strings.TrimSpace(dir)
		if !ok || marker == "" || dir == "" {
			return nil, fmt.Errorf("source promotion fixture %q must be marker=directory", value)
		}
		if _, exists := fixtures[marker]; exists {
			return nil, fmt.Errorf("source promotion fixture repeats marker %q", marker)
		}
		fixtures[marker] = dir
	}
	return fixtures, nil
}

func promoteSourcePlugin(ctx context.Context, options sourcePromotionOptions, qualify sourceQualificationRunner) (sourcePromotionResult, error) {
	if !strings.Contains(options.agentSpec, ":") {
		return sourcePromotionResult{}, fmt.Errorf("source promotion agent must include an exact version")
	}
	agent, err := resources.ParseAgent(ctx, resources.ServiceAgent, options.agentSpec)
	if err != nil {
		return sourcePromotionResult{}, fmt.Errorf("invalid source promotion agent: %w", err)
	}
	if agent.Version == latestAgentVersion || strings.HasPrefix(agent.Version, "v") {
		return sourcePromotionResult{}, fmt.Errorf("source promotion agent must use a canonical exact version")
	}
	candidate, err := semver.Parse(agent.Version)
	if err != nil {
		return sourcePromotionResult{}, fmt.Errorf("source promotion agent has invalid version %q", agent.Version)
	}

	cliDir, err := filepath.Abs(options.cliDir)
	if err != nil {
		return sourcePromotionResult{}, fmt.Errorf("resolve CLI directory: %w", err)
	}
	rosterPath := filepath.Join(cliDir, filepath.FromSlash(sourceworkspace.CompatibilityRosterRelativePath))
	roster, err := sourceworkspace.LoadCompatibilityRoster(rosterPath)
	if err != nil {
		return sourcePromotionResult{}, err
	}
	pluginIndex := -1
	for i, plugin := range roster.Plugins {
		if plugin.Publisher == agent.Publisher && plugin.Name == agent.Name {
			pluginIndex = i
			break
		}
	}
	if pluginIndex < 0 {
		return sourcePromotionResult{}, fmt.Errorf("agent %s/%s is not in the source-workspace compatibility roster", agent.Publisher, agent.Name)
	}
	plugin := roster.Plugins[pluginIndex]
	pinned, err := semver.Parse(plugin.Version)
	if err != nil {
		return sourcePromotionResult{}, err
	}
	if !candidate.GT(pinned) {
		return sourcePromotionResult{}, fmt.Errorf("source promotion version %s must be newer than pin %s", candidate, pinned)
	}

	requiredMarkers := append([]string(nil), plugin.Markers...)
	if len(requiredMarkers) == 0 {
		requiredMarkers = []string{fallbackMarker}
	}
	if len(options.fixtures) != len(requiredMarkers) {
		return sourcePromotionResult{}, fmt.Errorf("source promotion for %s/%s requires one --fixture for each marker: %s",
			agent.Publisher, agent.Name, strings.Join(requiredMarkers, ", "))
	}
	proofs := make([]sourcePromotionProof, 0, len(requiredMarkers))
	for _, marker := range requiredMarkers {
		fixture, ok := options.fixtures[marker]
		if !ok {
			return sourcePromotionResult{}, fmt.Errorf("source promotion is missing --fixture %s=directory", marker)
		}
		absoluteFixture, err := filepath.Abs(fixture)
		if err != nil {
			return sourcePromotionResult{}, fmt.Errorf("resolve %s fixture: %w", marker, err)
		}
		info, err := os.Stat(absoluteFixture)
		if err != nil || !info.IsDir() {
			return sourcePromotionResult{}, fmt.Errorf("source promotion fixture for %s is not a directory: %s", marker, absoluteFixture)
		}
		if marker != fallbackMarker {
			if _, markerErr := os.Stat(filepath.Join(absoluteFixture, marker)); markerErr != nil {
				return sourcePromotionResult{}, fmt.Errorf("source promotion fixture %s does not contain marker %s: %w", absoluteFixture, marker, markerErr)
			}
		}
		selected, evidence, err := roster.SelectPlugin(absoluteFixture)
		if err != nil {
			return sourcePromotionResult{}, fmt.Errorf("select source promotion fixture for %s: %w", marker, err)
		}
		if selected.Publisher != plugin.Publisher || selected.Name != plugin.Name ||
			evidence.String() != sourcePromotionEvidence(marker) {
			return sourcePromotionResult{}, fmt.Errorf("source promotion fixture %s selects %s through %s, want %s/%s through %s",
				absoluteFixture, selected.Identifier(), evidence, plugin.Publisher, plugin.Name, sourcePromotionEvidence(marker))
		}
		if err := qualify(ctx, agent, marker, absoluteFixture); err != nil {
			return sourcePromotionResult{}, fmt.Errorf("qualify %s through marker %s: %w", agent.Identifier(), marker, err)
		}
		proofs = append(proofs, sourcePromotionProof{marker: marker, fixture: absoluteFixture})
	}

	previousVersion := roster.Plugins[pluginIndex].Version
	roster.Plugins[pluginIndex].Version = agent.Version
	if err := sourceworkspace.WriteCompatibilityRoster(rosterPath, roster); err != nil {
		return sourcePromotionResult{}, err
	}
	return sourcePromotionResult{
		rosterPath:      rosterPath,
		previousVersion: previousVersion,
		version:         agent.Version,
		proofs:          proofs,
	}, nil
}

func sourcePromotionEvidence(marker string) string {
	if marker == fallbackMarker {
		return marker
	}
	return "marker:" + marker
}

func runSourceQualification(ctx context.Context, agent *resources.Agent, _ string, fixture, home string) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve Codefly executable: %w", err)
	}
	command := exec.CommandContext(ctx, executable,
		"--timestamps=false",
		"--plugin-path", home,
		"test", "source",
		"--dir", fixture,
		"--agent", agent.Identifier(),
		"--qualification",
	)
	command.Dir = fixture
	command.Env = sourcePromotionEnvironment(home)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	return command.Run()
}

func sourcePromotionEnvironment(home string) []string {
	environment := make([]string, 0, len(os.Environ())+3)
	for _, entry := range os.Environ() {
		name, _, ok := strings.Cut(entry, "=")
		if ok && (name == resources.CodeflyHomeEnv || name == "CODEFLY_AGENT_SOURCE" || name == "GOWORK" || name == "CI" || name == "CODEFLY_COLOR") {
			continue
		}
		environment = append(environment, entry)
	}
	return append(environment,
		resources.CodeflyHomeEnv+"="+home,
		"GOWORK=off",
		"CI=1",
		"CODEFLY_COLOR=never",
	)
}

func init() {
	PromoteSourceCmd.Flags().StringVar(&sourcePromotionCLIDir, "cli-dir", ".", "Codefly CLI checkout whose compatibility roster will be updated")
	PromoteSourceCmd.Flags().StringArrayVar(&sourcePromotionFixtures, "fixture", nil, "Qualified source marker and checkout as marker=directory (repeat for every owned marker)")
}
