package deploy

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	"github.com/codefly-dev/cli/pkg/deployments"
	"github.com/codefly-dev/cli/pkg/deploysecrets"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

var (
	secretsEnv          string
	secretsDryRun       bool
	secretsMetadataOnly bool
	secretsAllowMissing bool
	secretsYes          bool
	secretsModules      []string
	secretsMintKeys     []string
	secretsExportInput  string
	secretsInput        string
	secretsSupplied     string
	secretsRotate       []string
)

// SecretsCmd seeds an environment's secret store with what its render reads.
var SecretsCmd = &cobra.Command{
	Use:   "secrets",
	Short: "Plan and write the secret-store values a rendered environment's ExternalSecrets read",
	Long: `Reads every ExternalSecret ` + "`deploy gitops render`" + ` projected for --env under
deployments/modules, and resolves each remote property to a source: kept (already
stored), propagated (a configuration value another remote key already holds),
configured (the environment's own configurations/<profile>/<group>.secret.env
holds the value, as written by ` + "`codefly config set`" + `), generated (declared random
by the environment's service-secrets.generate), or required (supplied by the
operator, and named). The store is the backend behind the SecretStore the render
names, read from the environment's cluster; a render reading through several
stores is planned and written store by store.

A stored value is never rotated unless --rotate names it: a property the store
already holds is kept even when a configuration file holds one too, and the two
disagreeing is refused rather than resolved. A configured key with an empty value supplies nothing, so
a tracked placeholder file still leaves its properties to be supplied.

Each property is resolved by the secret key the render reads out of it, not by
the property name the environment files that key under, so an environment that
maps keys to its store's own property names resolves exactly like one that does
not. Both are reported.

Values are never printed: the plan names keys and sources only.

--supplied names a JSON object of secret key to value for the required ones: a
supplied value is written where nothing holds the key, never over a stored value
and never in place of a declared generator. An empty value is accepted only for
a key service-secrets.may-be-empty declares.

--dry-run prints the plan and writes nothing. --metadata-only additionally never
reads a stored value: it knows which remote keys exist, not what they hold.

--module limits the plan to the remote keys the named modules' services read.
Every other remote key is still read, so a scoped key keeps agreeing with what
the store already holds, but nothing outside the scope is planned or written.

--rotate <module>/<service>/<configuration>:<KEY> (or <configuration>:<KEY> for a
workspace configuration, or the CODEFLY__… secret key) replaces a GENERATED key's
stored value with a new one, in every remote key holding it, and rewrites every
stored value derived from it in the same apply: a stored URL whose password is
the old value gets the new one. Any other stored value embedding the old one,
and any key service-secrets.generate does not declare, is refused by name. Each
write is a new version, refused if the key moved since the plan read it; a
rotation failing part-way restores what it wrote. The CLI changes the store
only: force-sync the ExternalSecrets, make the server accept the new credential
(a restart only does that for an agent that re-applies it on every start), then
restart its consumers. An identifier or signing key is never rotated.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, done := common.NewContext()
		defer done()
		if secretsInput != "" {
			return runSecretsInput(ctx, cmd)
		}
		if secretsExportInput != "" && (len(secretsModules) != 0 || len(secretsMintKeys) != 0 || secretsAllowMissing || secretsMetadataOnly || secretsSupplied != "") {
			return fmt.Errorf("--export-input exports the full render without module, mint, allow-missing, metadata-only or supplied options")
		}
		if secretsMetadataOnly && !secretsDryRun {
			return fmt.Errorf("--metadata-only plans without reading the store, so it cannot be applied: pass --dry-run")
		}
		workspace, err := common.LoadWorkspaceWithPinnedModules(ctx)
		if err != nil {
			return err
		}
		env, err := orchestration.SelectEnvironment(workspace, secretsEnv)
		if err != nil {
			return err
		}
		if !env.ServiceSecrets.Stored() {
			return fmt.Errorf("environment %q declares no service-secrets store", env.Name)
		}
		rendered, err := gitops.RenderedServiceSecrets(workspace.Dir(), env.Name)
		if err != nil {
			return err
		}
		if len(rendered.Secrets) == 0 {
			cli.Info("No rendered service of environment %s reads a secret", env.Name)
			return nil
		}
		for _, module := range secretsModules {
			if !slices.Contains(rendered.Modules, module) {
				return fmt.Errorf("--module %s: not a module rendered for %s (rendered: %s)", module, env.Name, strings.Join(rendered.Modules, ", "))
			}
		}
		if secretsExportInput != "" {
			return exportSecretsInput(secretsExportInput, env, rendered, workspaceServiceUniques(ctx, workspace, env))
		}
		supplied, err := readSuppliedValues(secretsSupplied, rendered, env.ServiceSecrets.MayBeEmpty)
		if err != nil {
			return err
		}
		groups := renderedByStore(rendered)
		rotate, err := rotationKeys(rendered, groups, secretsRotate)
		if err != nil {
			return err
		}
		// The operator's own configuration files, read once for the whole
		// environment: they are a property of the workspace, not of a store, so
		// every per-store plan below resolves against the same index.
		configured, err := deploysecrets.ReadConfigured(ctx, workspace, env)
		if err != nil {
			return err
		}
		// A render that reads through several stores (the cell's store and a
		// per-key store such as an identity provider's) is planned and written
		// store by store: one plan writes one store, and each is confirmed on
		// its own.
		for _, group := range groups {
			store, err := resolveSecretStore(ctx, env, group)
			if err != nil {
				return err
			}
			plan, err := deploysecrets.Build(ctx, &deploysecrets.Inputs{
				Rendered:     group,
				Generators:   env.ServiceSecrets.Generate,
				Services:     workspaceServiceUniques(ctx, workspace, env),
				Store:        store,
				ReadPayloads: !secretsMetadataOnly,
				Modules:      secretsModules,
				MayBeEmpty:   env.ServiceSecrets.MayBeEmpty,
				MintKeys:     secretsMintKeys,
				Supplied:     supplied,
				Configured:   configured,
				Rotate:       readBy(group, rotate),
			})
			if err != nil {
				return err
			}
			printSecretsPlan(cmd.OutOrStdout(), env.Name, group, plan)
			if err := finishSecretsPlan(ctx, plan, store, secretsDryRun, secretsAllowMissing, func() bool {
				return secretsYes || models.Confirm(ctx, fmt.Sprintf("Write %d remote keys to %s?", len(plan.Changes()), plan.Store), false)
			}); err != nil {
				return err
			}
		}
		return nil
	},
}

// renderedByStore splits a render into one RenderedEnvironment per secret
// store — per namespace for a namespaced SecretStore — in the order the stores
// are first read, each carrying the whole render's module lists. One plan
// writes one store; a render reading through the cell's store and a per-key
// store is seeded store by store rather than refused.
func renderedByStore(rendered gitops.RenderedEnvironment) []gitops.RenderedEnvironment {
	type storeKey struct{ name, kind, namespace string }
	var order []storeKey
	groups := map[storeKey][]gitops.RenderedServiceSecret{}
	for _, secret := range rendered.Secrets {
		key := storeKey{name: secret.Store.Name, kind: secret.Store.Kind}
		if secret.Store.Kind == "SecretStore" {
			key.namespace = secret.Namespace
		}
		if _, seen := groups[key]; !seen {
			order = append(order, key)
		}
		groups[key] = append(groups[key], secret)
	}
	out := make([]gitops.RenderedEnvironment, 0, len(order))
	for _, key := range order {
		group := rendered
		group.Secrets = groups[key]
		out = append(out, group)
	}
	return out
}

// finishSecretsPlan validates completeness before even a no-op apply succeeds.
func finishSecretsPlan(ctx context.Context, plan *deploysecrets.Plan, store deploysecrets.Store, dryRun, allowMissing bool, confirm func() bool) error {
	if dryRun {
		return nil
	}
	if err := plan.ValidateApply(allowMissing); err != nil {
		return err
	}
	if len(plan.Changes()) == 0 {
		cli.Info("Nothing to write")
		return nil
	}
	if !confirm() {
		return fmt.Errorf("write not confirmed")
	}
	written, err := plan.Apply(ctx, store, allowMissing)
	for _, write := range written {
		verb := "Wrote"
		if write.Restored {
			verb = "Restored the previous document of"
		}
		cli.Info("%s %s as version %s (%s)", verb, write.Key, write.Version, strings.Join(write.Properties, ", "))
	}
	return err
}

// rotationKeys resolves every --rotate selector, refusing one no rendered
// ExternalSecret reads or whose configuration spans stores.
func rotationKeys(rendered gitops.RenderedEnvironment, groups []gitops.RenderedEnvironment, selectors []string) ([]string, error) {
	keys := make([]string, 0, len(selectors))
	for _, selector := range selectors {
		key, err := deploysecrets.RotationKey(selector)
		if err != nil {
			return nil, err
		}
		if !deploysecrets.ReadsKey(rendered, key) {
			return nil, fmt.Errorf("--rotate %s: no rendered ExternalSecret reads %s", selector, key)
		}
		if err = deploysecrets.CheckRotationStores(groups, key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, nil
}

// readBy keeps the rotated keys one store group reads; each is rotated by the
// plan of the store holding it.
func readBy(group gitops.RenderedEnvironment, keys []string) []string {
	var read []string
	for _, key := range keys {
		if deploysecrets.ReadsKey(group, key) {
			read = append(read, key)
		}
	}
	return read
}

// resolveSecretStore resolves the one store every ExternalSecret of a
// renderedByStore group reads through, from the environment's cluster. A mixed
// group is a programming error and is refused by name.
func resolveSecretStore(ctx context.Context, env *environments.Environment, rendered gitops.RenderedEnvironment) (deploysecrets.Store, error) {
	first := rendered.Secrets[0]
	for _, secret := range rendered.Secrets[1:] {
		if secret.Store != first.Store || (first.Store.Kind == "SecretStore" && secret.Namespace != first.Namespace) {
			return nil, fmt.Errorf("the render reads through more than one secret store (%s and %s); a plan writes one store", first.Store.Name, secret.Store.Name)
		}
	}
	if env.Cluster == nil || env.Cluster.Context == "" {
		return nil, fmt.Errorf("environment %q must declare cluster.context: its secret store is resolved from the cluster", env.Name)
	}
	kubeconfig, err := deployments.GetK8sConfig(ctx, env)
	if err != nil {
		return nil, err
	}
	return deploysecrets.ResolveStore(ctx, deploysecrets.ExecRunner,
		deploysecrets.ClusterTarget{Kubeconfig: kubeconfig, Context: env.Cluster.Context}, first.Store, first.Namespace)
}

// workspaceServiceUniques lists every service the workspace composes in env. A module
// or service that does not load is skipped: it is not one this environment
// renders either.
func workspaceServiceUniques(ctx context.Context, workspace *resources.Workspace, env *environments.Environment) []string {
	var uniques []string
	for _, ref := range workspace.Modules {
		mod, err := workspace.LoadModuleFromReference(ctx, ref)
		if err != nil {
			continue
		}
		for _, svc := range env.ComposedServices(mod) {
			uniques = append(uniques, resources.ServiceUnique(mod.Name, svc.Name))
		}
	}
	return uniques
}

func printSecretsPlan(out io.Writer, environment string, rendered gitops.RenderedEnvironment, plan *deploysecrets.Plan) {
	fmt.Fprintf(out, "Environment %s — store %s\n", environment, plan.Store)
	fmt.Fprintf(out, "Rendered modules: %s\n", strings.Join(rendered.Modules, ", "))
	if len(plan.Modules) > 0 {
		fmt.Fprintf(out, "Scoped to modules: %s\n", strings.Join(plan.Modules, ", "))
	}
	if len(rendered.Skipped) > 0 {
		fmt.Fprintf(out, "Skipped (rendered for another environment): %s\n", strings.Join(rendered.Skipped, ", "))
	}
	for _, secret := range plan.Secrets {
		state := "exists"
		switch {
		case !secret.Exists:
			state = "missing"
		case !secret.HasVersion:
			state = "exists, no enabled version"
		case !secret.Read:
			state = "exists, not read"
		}
		fmt.Fprintf(out, "\n%s [%s] read by %s\n", secret.RemoteKey, state, strings.Join(secret.Services, ", "))
		table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		for _, property := range secret.Properties {
			// The key, not only the property: the property says where the value is
			// filed, the key says what it is. An operator reading "require
			// postgres_user" cannot tell which of the environment's secrets that is.
			fmt.Fprintf(table, "  %s\t%s\t%s\t%s\n", property.Action, property.Property, strings.Join(property.Keys, ", "), property.Source)
		}
		_ = table.Flush()
	}
	fmt.Fprintln(out)
	if changes := plan.Changes(); len(changes) > 0 && !plan.Unverified() {
		fmt.Fprintf(out, "Would write %d remote keys: %s\n", len(changes), strings.Join(changes, ", "))
	}
	if plan.Unverified() {
		fmt.Fprintln(out, "Plan is metadata-only: existing remote keys were not read, so it cannot be applied.")
	}
	if required := plan.Required(); len(required) > 0 {
		fmt.Fprintf(out, "Must be supplied (%d):\n", len(required))
		for _, key := range required {
			fmt.Fprintf(out, "  %s\n", key)
		}
	}
}

func init() {
	SecretsCmd.Flags().StringVar(&secretsEnv, "env", "local", "Environment whose rendered ExternalSecrets to seed")
	SecretsCmd.Flags().BoolVar(&secretsDryRun, "dry-run", false, "Print the plan and write nothing")
	SecretsCmd.Flags().BoolVar(&secretsMetadataOnly, "metadata-only", false, "With --dry-run: never read a stored value, only which remote keys exist")
	SecretsCmd.Flags().BoolVar(&secretsAllowMissing, "allow-missing", false, "Write what can be resolved even when some properties must still be supplied")
	SecretsCmd.Flags().BoolVarP(&secretsYes, "yes", "y", false, "Write without an interactive confirmation")
	SecretsCmd.Flags().StringSliceVar(&secretsModules, "module", nil, "Plan only these modules' remote keys (comma-separated or repeated)")
	SecretsCmd.Flags().StringSliceVar(&secretsMintKeys, "mint", nil, "Acknowledge first creation of these signing secret keys (comma-separated or repeated); never rotate existing keys")
	SecretsCmd.Flags().StringVar(&secretsExportInput, "export-input", "", "Export declarative planner inputs for execution by an infrastructure-owned publisher; reads no stored values")
	SecretsCmd.Flags().StringVar(&secretsInput, "input", "", "Run exported planner inputs under an infrastructure-bound Azure workload identity")
	SecretsCmd.Flags().StringVar(&secretsSupplied, "supplied", "", "JSON object of operator values by secret key, for keys nothing stores or generates; never printed")
	SecretsCmd.MarkFlagsMutuallyExclusive("export-input", "input")
	SecretsCmd.Flags().StringSliceVar(&secretsRotate, "rotate", nil, "Replace a generated key's stored value and every stored value derived from it: <module>/<service>/<configuration>:<KEY>, <configuration>:<KEY>, or the CODEFLY__ key (repeatable)")
}
