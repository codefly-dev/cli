// Package config implements `codefly config` — the write half of Codefly's
// configuration contract.
//
// ARCHITECTURE: core/configurations READS
// `<scope>/configurations/<profile>/<name>[.secret].env`. Until now nothing
// WROTE it, so every workspace grew a bespoke shell script that generated
// credentials and hand-assembled those files. Those scripts are unreviewable,
// duplicate the format, and — as seen in practice — commit secrets, rotate
// credentials another service still holds, and leave files world-readable.
//
// This command makes provisioning a Codefly operation:
//
//	codefly config set internal-auth CODEFLY_INTERNAL_TOKEN=...      # explicit value
//	codefly config generate internal-auth CODEFLY_INTERNAL_TOKEN     # random value
//	codefly config check internal-auth CODEFLY_INTERNAL_TOKEN        # readiness, no value
//	codefly config list                                              # names and keys only
//
// Values are written, never printed. `set`, `check`, and `list` never emit a
// configuration value on any stream, so these commands are safe in CI logs and
// in an agent transcript.
package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

// Cmd is the `codefly config` command group.
var Cmd = &cobra.Command{
	Use:   "config",
	Short: "Provision and inspect service and workspace configuration values",
	Long: `Config writes and inspects the configuration files Codefly reads at run and
deploy time: <scope>/configurations/<profile>/<name>[.secret].env.

Configuration is Codefly's, so provisioning it is a Codefly operation — not a
shell script in your repository. Values are written but never printed.

Secrets are refused when git would track the destination file, are stored 0600
inside a 0700 directory, and are never overwritten without --force so re-running
setup cannot rotate a credential another service already holds.`,
}

// scope flags shared by every subcommand.
var (
	serviceRef  string
	environment string
	plaintext   bool
	force       bool
)

func init() {
	for _, sub := range []*cobra.Command{setCmd, generateCmd, checkCmd, listCmd} {
		sub.Flags().StringVar(&serviceRef, "service", "",
			"Target a service's configuration (`module/service`, or a unique service name). Default: the workspace configuration")
		sub.Flags().StringVar(&environment, "env", "",
			"Environment whose configuration profile is used. Default: the workspace's local environment")
		Cmd.AddCommand(sub)
	}
	setCmd.Flags().BoolVar(&plaintext, "plaintext", false,
		"Write a non-secret configuration (<name>.env) instead of <name>.secret.env")
	setCmd.Flags().BoolVar(&force, "force", false,
		"Replace keys that already have a value")
	generateCmd.Flags().BoolVar(&force, "force", false,
		"Regenerate keys that already have a value (rotates the credential)")
	generateCmd.Flags().String("format", string(FormatHex), "Encoding of the generated value: hex or base64")
	generateCmd.Flags().Int("bytes", 32, "Number of random bytes to generate")
	checkCmd.Flags().BoolVar(&plaintext, "plaintext", false,
		"Check a non-secret configuration (<name>.env)")
	listCmd.Flags().BoolVar(&plaintext, "plaintext", false,
		"List non-secret configurations (<name>.env)")
}

var setCmd = &cobra.Command{
	Use:   "set <name> KEY=VALUE [KEY=VALUE...]",
	Short: "Write configuration values from explicit input",
	Long: `Set stores KEY=VALUE pairs in a configuration Codefly reads.

Existing keys are preserved unless --force is passed, so re-running a setup
sequence is idempotent. The value is taken from the argument; nothing is echoed.

Examples:
  codefly config set internal-auth CODEFLY_INTERNAL_TOKEN=$TOKEN
  codefly config set runtime EXECUTION_SCHEDULER_TOKEN=$TOKEN --service mind/mind
  codefly config set edge EDGE_URL=https://localhost:8080 --plaintext`,
	Args: cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		cli.Init()

		target, root, err := resolveTarget(ctx, args[0], !plaintext)
		if err != nil {
			return err
		}
		doc, err := Load(target)
		if err != nil {
			return err
		}
		var written, kept []string
		for _, assignment := range args[1:] {
			key, value, ok := strings.Cut(assignment, "=")
			key = strings.TrimSpace(key)
			if !ok || key == "" {
				return fmt.Errorf("codefly config set: %q is not KEY=VALUE", assignment)
			}
			if doc.Has(key) && !force {
				kept = append(kept, key)
				continue
			}
			doc.Set(key, value)
			written = append(written, key)
		}
		if len(written) > 0 {
			if err := Write(target, doc); err != nil {
				return fmt.Errorf("codefly config set: %w", err)
			}
		}
		report(target.Display(root), written, kept)
		return nil
	},
}

var generateCmd = &cobra.Command{
	Use:   "generate <name> KEY [KEY...]",
	Short: "Provision configuration values with generated secrets",
	Long: `Generate stores cryptographically random values for the named keys.

This is the credential-provisioning path that replaces hand-written setup
scripts. Keys that already hold a value are left alone unless --force is passed,
so running it again never rotates a credential another service is holding.

Generated configurations are always secrets: 0600, in a 0700 directory, refused
when git would track the file. The value is never printed.

Examples:
  codefly config generate internal-auth CODEFLY_INTERNAL_TOKEN CODEFLY_GATEWAY_TOKEN
  codefly config generate mutation_permit ED25519_SEED_BASE64 --format base64 --service coordination/work-coordinator`,
	Args: cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		cli.Init()

		format, err := ParseFormat(mustString(cmd, "format"))
		if err != nil {
			return fmt.Errorf("codefly config generate: %w", err)
		}
		size, err := cmd.Flags().GetInt("bytes")
		if err != nil {
			return fmt.Errorf("codefly config generate: %w", err)
		}

		target, root, err := resolveTarget(ctx, args[0], true)
		if err != nil {
			return err
		}
		doc, err := Load(target)
		if err != nil {
			return err
		}
		var written, kept []string
		for _, key := range args[1:] {
			key = strings.TrimSpace(key)
			if key == "" {
				return fmt.Errorf("codefly config generate: empty key")
			}
			if doc.Has(key) && !force {
				kept = append(kept, key)
				continue
			}
			value, err := Generate(format, size)
			if err != nil {
				return fmt.Errorf("codefly config generate: %w", err)
			}
			doc.Set(key, value)
			written = append(written, key)
		}
		if len(written) > 0 {
			if err := Write(target, doc); err != nil {
				return fmt.Errorf("codefly config generate: %w", err)
			}
		}
		report(target.Display(root), written, kept)
		return nil
	},
}

var checkCmd = &cobra.Command{
	Use:   "check <name> [KEY...]",
	Short: "Verify that configuration keys are present without printing them",
	Long: `Check reports whether a configuration holds a non-empty value for each key.

It exits non-zero and names the missing keys when any is absent, which makes it
the readiness gate a launcher can run before starting a service. It never prints
a value.

Examples:
  codefly config check internal-auth CODEFLY_INTERNAL_TOKEN CODEFLY_GATEWAY_TOKEN
  codefly config check runtime EXECUTION_SCHEDULER_TOKEN --service mind/mind`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		cli.Init()

		target, root, err := resolveTarget(ctx, args[0], !plaintext)
		if err != nil {
			return err
		}
		doc, err := Load(target)
		if err != nil {
			return err
		}
		keys := args[1:]
		if len(keys) == 0 {
			keys = doc.Keys()
			if len(keys) == 0 {
				return fmt.Errorf("codefly config check: %s holds no configuration", target.Display(root))
			}
		}
		var missing []string
		for _, key := range keys {
			if !doc.Has(strings.TrimSpace(key)) {
				missing = append(missing, key)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("codefly config check: %s is missing %s",
				target.Display(root), strings.Join(missing, ", "))
		}
		fmt.Printf("ok: %s holds %d value(s)\n", target.Display(root), len(keys))
		return nil
	},
}

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List configuration names and their keys",
	Long: `List shows every configuration in the selected scope and profile with the key
names it holds. Values are never printed.

Examples:
  codefly config list
  codefly config list --service mind/mind
  codefly config list --env aws`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		cli.Init()

		scopeDir, root, profile, err := resolveScope(ctx)
		if err != nil {
			return err
		}
		dir := filepath.Join(scopeDir, "configurations", profile)
		suffix := ".secret.env"
		if plaintext {
			suffix = ".env"
		}
		names, err := configurationNames(dir, suffix)
		if err != nil {
			return fmt.Errorf("codefly config list: %w", err)
		}
		if len(names) == 0 {
			fmt.Printf("no configurations in %s\n", relativeTo(root, dir))
			return nil
		}
		for _, name := range names {
			target := Target{ScopeDir: scopeDir, Profile: profile, Name: name, Secret: !plaintext}
			doc, err := Load(target)
			if err != nil {
				return fmt.Errorf("codefly config list: %w", err)
			}
			fmt.Printf("%s: %s\n", name, strings.Join(doc.Keys(), ", "))
		}
		return nil
	},
}

// configurationNames lists configuration names in dir carrying the suffix. The
// non-secret suffix `.env` also matches `.secret.env`, so secret files are
// filtered out explicitly.
func configurationNames(dir string, suffix string) ([]string, error) {
	dirEntries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var names []string
	for _, dirEntry := range dirEntries {
		if dirEntry.IsDir() {
			continue
		}
		name := dirEntry.Name()
		if !strings.HasSuffix(name, suffix) {
			continue
		}
		if suffix == ".env" && strings.HasSuffix(name, ".secret.env") {
			continue
		}
		names = append(names, strings.TrimSuffix(name, suffix))
	}
	sort.Strings(names)
	return names, nil
}

// resolveTarget resolves the scope, profile, and configuration name into one
// target file.
func resolveTarget(ctx context.Context, name string, secret bool) (Target, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Target{}, "", fmt.Errorf("codefly config: configuration name is required")
	}
	if strings.ContainsAny(name, `/\`) {
		return Target{}, "", fmt.Errorf("codefly config: configuration name %q must not contain a path separator", name)
	}
	scopeDir, root, profile, err := resolveScope(ctx)
	if err != nil {
		return Target{}, "", err
	}
	return Target{ScopeDir: scopeDir, Profile: profile, Name: name, Secret: secret}, root, nil
}

// targetIn is resolveTarget against an explicitly loaded workspace.
func targetIn(ctx context.Context, workspace *resources.Workspace, name string, secret bool) (Target, string, error) {
	scopeDir, root, profile, err := resolveScopeIn(ctx, workspace)
	if err != nil {
		return Target{}, "", err
	}
	return Target{ScopeDir: scopeDir, Profile: profile, Name: name, Secret: secret}, root, nil
}

// resolveScope loads the active workspace and resolves the scope within it.
func resolveScope(ctx context.Context) (string, string, string, error) {
	workspace, err := common.LoadWorkspace(ctx)
	if err != nil {
		return "", "", "", fmt.Errorf("codefly config: %w", err)
	}
	return resolveScopeIn(ctx, workspace)
}

// resolveScopeIn returns the directory that owns `configurations/` (the
// workspace root, or a service root when --service is set), the workspace root
// for display, and the environment's configuration profile.
func resolveScopeIn(ctx context.Context, workspace *resources.Workspace) (string, string, string, error) {
	profile, err := resolveProfile(workspace)
	if err != nil {
		return "", "", "", err
	}
	root := workspace.Dir()
	if strings.TrimSpace(serviceRef) == "" {
		return root, root, profile, nil
	}
	ref, err := workspace.FindUniqueServiceAndModuleByName(ctx, serviceRef)
	if err != nil {
		return "", "", "", fmt.Errorf("codefly config: resolving --service %q: %w", serviceRef, err)
	}
	service, err := workspace.LoadService(ctx, ref)
	if err != nil {
		return "", "", "", fmt.Errorf("codefly config: loading service %q: %w", serviceRef, err)
	}
	return service.Dir(), root, profile, nil
}

// resolveProfile selects the configuration profile directory name. --env picks
// a declared environment; otherwise the workspace's local environment is used,
// matching what `codefly run` loads.
func resolveProfile(workspace *resources.Workspace) (string, error) {
	name := strings.TrimSpace(environment)
	if name == "" {
		name = "local"
	}
	if env := workspace.FindEnvironment(name); env != nil {
		profile, err := env.ConfigurationProfileName()
		if err != nil {
			return "", fmt.Errorf("codefly config: environment %q: %w", name, err)
		}
		return profile, nil
	}
	if strings.TrimSpace(environment) != "" {
		return "", fmt.Errorf("codefly config: workspace %q declares no environment %q", workspace.Name, name)
	}
	// A workspace with no declared environments still has the implicit local one.
	profile, err := resources.LocalEnvironment().ConfigurationProfileName()
	if err != nil {
		return "", fmt.Errorf("codefly config: %w", err)
	}
	return profile, nil
}

// report prints what changed, by key name only.
func report(display string, written, kept []string) {
	if len(written) > 0 {
		fmt.Printf("%s: set %s\n", display, strings.Join(written, ", "))
	}
	if len(kept) > 0 {
		fmt.Printf("%s: kept existing %s (pass --force to replace)\n", display, strings.Join(kept, ", "))
	}
	if len(written) == 0 && len(kept) == 0 {
		fmt.Printf("%s: nothing to do\n", display)
	}
}

func relativeTo(root, path string) string {
	if rel, err := filepath.Rel(root, path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return path
}

func mustString(cmd *cobra.Command, name string) string {
	value, err := cmd.Flags().GetString(name)
	if err != nil {
		return ""
	}
	return value
}
