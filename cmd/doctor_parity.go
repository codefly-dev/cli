package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/cli/pkg/runnables"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/tui"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// Stable diagnostic codes of `codefly doctor parity`.
const (
	codeParityConfiguration    = "parity_configuration"
	codeParityBinding          = "parity_binding"
	codeParityComposition      = "parity_composition"
	codeParityUnresolved       = "parity_unresolved"
	codeParityDifferenceStale  = "parity_difference_stale"
	codeParityDifferenceBroken = "parity_difference_invalid"
)

// ParityFileName declares, in the workspace root, the differences between two
// of the workspace's environments that are deliberate, each with its reason.
const (
	ParityFileName   = "parity.codefly.yaml"
	paritySchema     = "codefly/environment-parity/v1"
	parityAnyKey     = "*"
	workspaceConfKey = resources.WorkspaceConfigurationPrefix + "__"
	workspaceSecKey  = resources.WorkspaceSecretConfigurationPrefix + "__"
)

// parityDifference is one declared difference: exactly one of a configuration
// key (group/key, or group/* for every key of the group), a binding
// (module/binding) or a service (module/service), against the environments
// `against` names (one, or a list) or, with none, every environment compared
// with.
type parityDifference struct {
	Against       environmentNames `yaml:"against,omitempty"`
	Configuration string           `yaml:"configuration,omitempty"`
	Binding       string           `yaml:"binding,omitempty"`
	Service       string           `yaml:"service,omitempty"`
	Reason        string           `yaml:"reason"`
}

// environmentNames is one environment name or a list of them.
type environmentNames []string

func (names *environmentNames) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		*names = environmentNames{node.Value}
		return nil
	}
	var list []string
	if err := node.Decode(&list); err != nil {
		return err
	}
	*names = list
	return nil
}

// appliesTo reports whether the difference is declared against target.
func (d parityDifference) appliesTo(target string) bool {
	return len(d.Against) == 0 || slices.Contains(d.Against, target)
}

func (d parityDifference) kind() (string, string) {
	switch {
	case d.Configuration != "":
		return codeParityConfiguration, d.Configuration
	case d.Binding != "":
		return codeParityBinding, d.Binding
	default:
		return codeParityComposition, d.Service
	}
}

// paritySide is what one environment composes and fills, read from the
// workspace exactly as its run or render reads it.
type paritySide struct {
	env string
	// values are the workspace configuration keys the environment knows,
	// normalized GROUP__KEY, each with whether it is filled and how it is
	// spelled for a reader.
	values map[string]parityValue
	// bindings are module/binding, delivered or omitted (with the reason).
	bindings map[string]parityBinding
	// refused are the modules whose request the render refuses, with why.
	refused map[string]string
	// excluded are the services the environment withdraws, each with the
	// consumers it is withdrawn from.
	excluded map[string][]string
	// selections are the runnable bindings the environment selects, each with
	// its scopes: what the generated runnable-bindings group is derived from.
	selections map[string]string
}

type parityValue struct {
	name   string
	filled bool
}

type parityBinding struct {
	delivered bool
	reason    string
}

// parityOff are the spellings that leave a key unfilled: a switch that is off,
// an empty JSON document and a per-profile placeholder nothing supplied are
// the same, to a feature, as an empty value.
var parityOff = []string{"", "off", "false", "0", "no", "disabled", "none", "{}", "[]", "${profile}"}

func filled(value string) bool {
	return !slices.Contains(parityOff, strings.ToLower(strings.TrimSpace(value)))
}

func normalizedKey(group, key string) string {
	return resources.UniqueToKey(group) + "__" + resources.UniqueToKey(key)
}

// readParitySide reads one environment: its workspace configuration through
// the profile chain it selects, the values and secrets it delivers itself
// (service-config, service-secrets), the bindings its render resolves, and
// the services it withdraws.
func readParitySide(ctx context.Context, ws *resources.Workspace, env *environments.Environment) (*paritySide, error) {
	side := &paritySide{env: env.Name, values: map[string]parityValue{}, bindings: map[string]parityBinding{},
		refused: map[string]string{}, excluded: map[string][]string{}, selections: map[string]string{}}
	provided, err := configurations.ReadWorkspaceConfigurations(ctx, ws, env.Runtime(), orchestration.ModuleArtifactResolverFor(ws))
	if err != nil {
		return nil, fmt.Errorf("read the workspace configurations of environment %s: %w", env.Name, err)
	}
	for _, info := range provided.Infos {
		// The runnable bindings are codefly's output, generated per run from
		// the environment's selections and addresses; their inputs are
		// compared instead (selections, below).
		if info.GetName() == runnables.RunnableBindingsGroup {
			continue
		}
		for _, value := range info.GetConfigurationValues() {
			key := normalizedKey(info.GetName(), value.GetKey())
			// A secret counts as filled by being declared: its value lives in a
			// store this command never reads.
			side.values[key] = parityValue{name: info.GetName() + "/" + value.GetKey(), filled: value.GetSecret() || filled(value.GetValue())}
		}
	}
	deliver := func(variable, prefix string, isFilled bool) {
		rest, ok := strings.CutPrefix(variable, prefix)
		if !ok || !strings.Contains(rest, "__") {
			return
		}
		group, key, _ := strings.Cut(rest, "__")
		name := strings.ToLower(group) + "/" + strings.ToLower(key)
		if known, ok := side.values[group+"__"+key]; ok {
			name = known.name
		}
		if isFilled || side.values[group+"__"+key].filled {
			side.values[group+"__"+key] = parityValue{name: name, filled: true}
		}
	}
	if env.ServiceConfig != nil {
		for _, mapping := range env.ServiceConfig.Services {
			for variable, value := range mapping.Values {
				deliver(variable, workspaceConfKey, filled(value))
			}
		}
	}
	if env.ServiceSecrets != nil {
		for _, mapping := range env.ServiceSecrets.Services {
			for variable := range mapping.RemoteKeys {
				deliver(variable, workspaceSecKey, true)
			}
		}
		for _, generator := range env.ServiceSecrets.Generate {
			if generator.Scope != environments.SecretGeneratorScopeWorkspace {
				continue
			}
			for _, key := range generator.Keys {
				normalized := normalizedKey(generator.Configuration, key)
				side.values[normalized] = parityValue{name: generator.Configuration + "/" + key, filled: true}
			}
		}
	}
	modules, err := gitops.ResolveModuleBindings(ctx, ws, env)
	if err != nil {
		return nil, err
	}
	for _, module := range modules {
		if module.Refused != nil {
			side.refused[module.Module] = module.Refused.Error()
			continue
		}
		for _, id := range module.Delivered {
			side.bindings[module.Module+"/"+id] = parityBinding{delivered: true}
		}
		for _, omitted := range module.Omitted {
			side.bindings[module.Module+"/"+omitted.ID] = parityBinding{reason: omitted.Reason}
		}
	}
	for id, selection := range env.RunnableScopeSelections {
		encoded, _ := json.Marshal(selection)
		side.selections[id] = string(encoded)
	}
	for _, excluded := range env.ExcludeServices {
		consumers := slices.Clone(excluded.WithdrawnFrom)
		sort.Strings(consumers)
		side.excluded[excluded.Service] = consumers
	}
	return side, nil
}

// parityFinding is one way the two environments differ.
type parityFinding struct {
	code, subject, message string
}

// compareParity lists every way local and target differ, both directions: a
// key, binding or service one fills or composes and the other does not.
func compareParity(local, target *paritySide) []parityFinding {
	var findings []parityFinding
	for _, key := range sortedUnion(local.values, target.values) {
		mine, theirs := local.values[key], target.values[key]
		name := mine.name
		if name == "" {
			name = theirs.name
		}
		switch {
		case theirs.filled && !mine.filled:
			findings = append(findings, parityFinding{codeParityConfiguration, name,
				fmt.Sprintf("%s is filled in %s and empty, off or absent in %s: what %s runs, %s never exercises", name, target.env, local.env, target.env, local.env)})
		case mine.filled && !theirs.filled:
			findings = append(findings, parityFinding{codeParityConfiguration, name,
				fmt.Sprintf("%s is filled in %s and empty, off or absent in %s: what works in %s fails there", name, local.env, target.env, local.env)})
		}
	}
	for _, key := range sortedUnion(local.bindings, target.bindings) {
		mine, inLocal := local.bindings[key]
		theirs, inTarget := target.bindings[key]
		module, _, _ := strings.Cut(key, "/")
		switch {
		case !inLocal && local.refused[module] != "", !inTarget && target.refused[module] != "":
			continue // reported once, per module, below
		case theirs.delivered && !mine.delivered:
			findings = append(findings, parityFinding{codeParityBinding, key,
				fmt.Sprintf("binding %s is delivered in %s and omitted in %s (%s): calls through it work in %s and are refused or disabled in %s", key, target.env, local.env, mine.reason, target.env, local.env)})
		case mine.delivered && !theirs.delivered:
			findings = append(findings, parityFinding{codeParityBinding, key,
				fmt.Sprintf("binding %s is delivered in %s and omitted in %s (%s)", key, local.env, target.env, theirs.reason)})
		}
	}
	for _, module := range sortedUnion(local.refused, target.refused) {
		mine, theirs := local.refused[module], target.refused[module]
		switch {
		case mine != "" && theirs != "":
			findings = append(findings, parityFinding{codeParityUnresolved, module,
				fmt.Sprintf("module %s's bindings cannot be resolved in either environment, so they were not compared: %s", module, mine)})
		case mine != "":
			findings = append(findings, parityFinding{codeParityBinding, module + "/*",
				fmt.Sprintf("the render refuses module %s in %s and not in %s: %s", module, local.env, target.env, mine)})
		default:
			findings = append(findings, parityFinding{codeParityBinding, module + "/*",
				fmt.Sprintf("the render refuses module %s in %s and not in %s: %s", module, target.env, local.env, theirs)})
		}
	}
	for _, service := range sortedUnion(local.excluded, target.excluded) {
		mine, outLocal := local.excluded[service]
		theirs, outTarget := target.excluded[service]
		switch {
		case outLocal && !outTarget:
			findings = append(findings, parityFinding{codeParityComposition, service,
				fmt.Sprintf("%s composes %s and %s withdraws it", target.env, service, local.env)})
		case outTarget && !outLocal:
			findings = append(findings, parityFinding{codeParityComposition, service,
				fmt.Sprintf("%s composes %s and %s withdraws it", local.env, service, target.env)})
		case !slices.Equal(mine, theirs):
			findings = append(findings, parityFinding{codeParityComposition, service,
				fmt.Sprintf("both withdraw %s, from different consumers: %s from [%s], %s from [%s]", service, local.env, strings.Join(mine, ", "), target.env, strings.Join(theirs, ", "))})
		}
	}
	for _, id := range sortedUnion(local.selections, target.selections) {
		mine, inLocal := local.selections[id]
		theirs, inTarget := target.selections[id]
		switch {
		case !inLocal:
			findings = append(findings, parityFinding{codeParityBinding, "runnable/" + id,
				fmt.Sprintf("%s selects runnable binding %s (%s) and %s does not", target.env, id, theirs, local.env)})
		case !inTarget:
			findings = append(findings, parityFinding{codeParityBinding, "runnable/" + id,
				fmt.Sprintf("%s selects runnable binding %s (%s) and %s does not", local.env, id, mine, target.env)})
		case mine != theirs:
			findings = append(findings, parityFinding{codeParityBinding, "runnable/" + id,
				fmt.Sprintf("runnable binding %s selects %s in %s and %s in %s", id, mine, local.env, theirs, target.env)})
		}
	}
	return findings
}

func sortedUnion[V any](a, b map[string]V) []string {
	keys := map[string]bool{}
	for key := range a {
		keys[key] = true
	}
	for key := range b {
		keys[key] = true
	}
	out := make([]string, 0, len(keys))
	for key := range keys {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// loadParityDifferences reads the workspace's declared differences, strictly.
// No file declares none.
func loadParityDifferences(dir string) ([]parityDifference, error) {
	data, err := os.ReadFile(filepath.Join(dir, ParityFileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var document struct {
		Schema      string             `yaml:"schema"`
		Differences []parityDifference `yaml:"differences"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err = decoder.Decode(&document); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", ParityFileName, err)
	}
	if document.Schema != paritySchema {
		return nil, fmt.Errorf("%s: schema must be %s", ParityFileName, paritySchema)
	}
	for i, difference := range document.Differences {
		named := 0
		for _, field := range []string{difference.Configuration, difference.Binding, difference.Service} {
			if field != "" {
				named++
			}
		}
		if named != 1 || strings.TrimSpace(difference.Reason) == "" {
			return nil, fmt.Errorf("%s: differences[%d] names exactly one of configuration, binding or service, and gives its reason", ParityFileName, i)
		}
	}
	return document.Differences, nil
}

// declares reports whether a difference covers a finding.
func (d parityDifference) declares(target string, finding parityFinding) bool {
	if !d.appliesTo(target) {
		return false
	}
	code, subject := d.kind()
	if code != finding.code {
		return false
	}
	if code != codeParityConfiguration {
		return subject == finding.subject
	}
	group, key, _ := strings.Cut(subject, "/")
	findingGroup, findingKey, _ := strings.Cut(finding.subject, "/")
	return resources.UniqueToKey(group) == resources.UniqueToKey(findingGroup) &&
		(key == parityAnyKey || resources.UniqueToKey(key) == resources.UniqueToKey(findingKey))
}

// environmentParity compares env with against and returns the report: one
// failure per undeclared difference, one per declared difference that no
// longer matches anything (so the list only shrinks).
func environmentParity(ctx context.Context, dir, local, against string) *workspaceReadinessReport {
	report := &workspaceReadinessReport{SchemaVersion: doctorWorkspaceSchemaVersion, Environment: local, Status: readinessStatusReady}
	ws := checkWorkspace(ctx, &workspaceReadinessOptions{dir: dir}, report)
	if ws == nil || !checkModulesMaterialized(ctx, ws, report) {
		return report
	}
	differences, err := loadParityDifferences(ws.Dir())
	if err != nil {
		report.add(codeParityDifferenceBroken, "declared differences", checkStatusFail, err.Error(),
			fmt.Sprintf("each entry of %s names one configuration (group/key), binding (module/binding) or service (module/service), and a reason", ParityFileName))
		return report
	}
	var sides []*paritySide
	for _, name := range []string{local, against} {
		env, selectErr := environments.Select(ws, name)
		var side *paritySide
		if selectErr == nil {
			side, selectErr = readParitySide(ctx, ws, env)
		}
		if selectErr != nil {
			report.add(codeEnvironmentNotFound, "environment "+name, checkStatusFail, selectErr.Error(), "")
			return report
		}
		sides = append(sides, side)
	}
	used := make([]bool, len(differences))
	compared := 0
	for _, finding := range compareParity(sides[0], sides[1]) {
		compared++
		declared := slices.IndexFunc(differences, func(d parityDifference) bool { return d.declares(against, finding) })
		if declared >= 0 {
			used[declared] = true
			continue
		}
		status := checkStatusFail
		if finding.code == codeParityUnresolved {
			status = "warn"
		}
		report.add(finding.code, finding.subject, status, finding.message,
			fmt.Sprintf("make %s and %s agree, or declare the difference with its reason in %s", local, against, ParityFileName))
	}
	for i, difference := range differences {
		if used[i] || !difference.appliesTo(against) {
			continue
		}
		_, subject := difference.kind()
		report.add(codeParityDifferenceStale, "declared difference "+subject, checkStatusFail,
			fmt.Sprintf("%s declares a difference for %s that %s and %s no longer have", ParityFileName, subject, local, against),
			"delete the entry")
	}
	if report.Status == readinessStatusReady {
		report.add("", "parity", "ok", fmt.Sprintf("%s and %s fill, deliver and compose the same, %d declared difference(s) aside", local, against, compared), "")
	}
	return report
}

var (
	doctorParityEnv     string
	doctorParityAgainst string
	doctorParityJSON    bool
	doctorParityTimeout time.Duration
)

var DoctorParityCmd = &cobra.Command{
	Use:          "parity",
	SilenceUsage: true,
	Short:        "Compare two environments of the workspace both ways: configuration, bindings, composition",
	Long: `Compare what two environments of this workspace fill and compose, in both
directions, so a run of one says something about the other:

  configuration  every workspace configuration key one environment fills (set,
                 not empty, not off) the other fills too: through its profile
                 chain, its service-config values or the secrets it delivers;
  bindings       every module binding one environment's render delivers the
                 other's delivers too, resolved exactly as a render resolves
                 them (an optional binding whose slots are empty is omitted),
                 and the same runnable bindings are selected with the same
                 scopes (the generated runnable-bindings group is derived from
                 them, so it is not compared itself);
  composition    every service one environment composes the other composes,
                 withdrawn from the same consumers.

A difference that is deliberate is declared, with its reason, in ` + ParityFileName + `
at the workspace root:

  schema: ` + paritySchema + `
  differences:
    - against: [<environment>, …]    # optional: every environment when absent
      configuration: <group>/<key>   # or <group>/* ; or binding: <module>/<binding> ; or service: <module>/<service>
      reason: <why the two must differ>

A declared difference that no longer matches fails too, so the list only
shrinks. Read-only: it reads committed configuration and never resolves a
secret value.

Stable diagnostic codes: parity_configuration, parity_binding,
parity_composition, parity_unresolved, parity_difference_stale,
parity_difference_invalid.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if doctorParityAgainst == "" {
			return fmt.Errorf("--against names the environment to compare %s with", doctorParityEnv)
		}
		if doctorParityJSON {
			defer cli.ProtectResultStream()()
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), doctorParityTimeout)
		defer cancel()
		report := environmentParity(ctx, "", doctorParityEnv, doctorParityAgainst)
		if doctorParityJSON {
			payload, err := json.MarshalIndent(report, "", "  ")
			if err != nil {
				return err
			}
			fmt.Println(string(payload))
			if report.Status != readinessStatusReady {
				return machineReadableDoctorError{errors.New("environments differ")}
			}
			return nil
		}
		fmt.Println(tui.RenderHeader(1, fmt.Sprintf("codefly doctor parity: %s against %s", doctorParityEnv, doctorParityAgainst)))
		for i := range report.Checks {
			printWorkspaceDiagnostic(&report.Checks[i])
		}
		if report.Status != readinessStatusReady {
			return fmt.Errorf("%s and %s differ", doctorParityEnv, doctorParityAgainst)
		}
		return nil
	},
}

func init() {
	DoctorParityCmd.Flags().StringVar(&doctorParityEnv, "env", "local", "Environment the run under test uses")
	DoctorParityCmd.Flags().StringVar(&doctorParityAgainst, "against", "", "Environment it must match (required)")
	DoctorParityCmd.Flags().BoolVar(&doctorParityJSON, "json", false, "Print a machine-readable report to stdout; log lines go to stderr")
	DoctorParityCmd.Flags().DurationVar(&doctorParityTimeout, "timeout", 2*time.Minute, "Overall bound")
	DoctorCmd.AddCommand(DoctorParityCmd)
}
