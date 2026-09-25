package deploy

import (
	"context"
	"fmt"
	"io"
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
	"github.com/codefly-dev/cli/pkg/solutionrun"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

var (
	secretsEnv          string
	secretsDryRun       bool
	secretsMetadataOnly bool
	secretsAllowMissing bool
	secretsYes          bool
)

// SecretsCmd seeds an environment's secret store with what its render reads.
var SecretsCmd = &cobra.Command{
	Use:   "secrets",
	Short: "Plan and write the secret-store values a rendered environment's ExternalSecrets read",
	Long: `Reads every ExternalSecret ` + "`deploy gitops render`" + ` projected for --env under
deployments/modules, and resolves each remote property to a source: kept (already
stored), derived (federation credentials and the registrar's digests of them),
propagated (a configuration value another remote key already holds), generated
(declared random by the environment's service-secrets.generate), or required
(supplied by the operator, and named). The store is the backend behind the
SecretStore the render names, read from the environment's cluster.

Values are never printed: the plan names keys and sources only.

--dry-run prints the plan and writes nothing. --metadata-only additionally never
reads a stored value: it knows which remote keys exist, not what they hold.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, done := common.NewContext()
		defer done()
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
		if env.ServiceSecrets == nil {
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
		store, err := resolveSecretStore(ctx, env, rendered)
		if err != nil {
			return err
		}
		federation, err := solutionrun.DeployedFederationSecrets(ctx, workspace)
		if err != nil {
			return err
		}
		plan, err := deploysecrets.Build(ctx, &deploysecrets.Inputs{
			Rendered:     rendered,
			Federation:   federation,
			Generators:   env.ServiceSecrets.Generate,
			Services:     workspaceServiceUniques(ctx, workspace),
			Store:        store,
			ReadPayloads: !secretsMetadataOnly,
		})
		if err != nil {
			return err
		}
		printSecretsPlan(cmd.OutOrStdout(), env.Name, rendered, plan)
		if secretsDryRun {
			return nil
		}
		if len(plan.Changes()) == 0 {
			cli.Info("Nothing to write")
			return nil
		}
		if !secretsYes && !models.Confirm(ctx, fmt.Sprintf("Write %d remote keys to %s?", len(plan.Changes()), plan.Store), false) {
			return fmt.Errorf("write not confirmed")
		}
		written, err := plan.Apply(ctx, store, secretsAllowMissing)
		for _, key := range written {
			cli.Info("Wrote %s", key)
		}
		return err
	},
}

// resolveSecretStore resolves the one store every rendered ExternalSecret reads
// through, from the environment's cluster.
func resolveSecretStore(ctx context.Context, env *environments.Environment, rendered gitops.RenderedEnvironment) (deploysecrets.Store, error) {
	first := rendered.Secrets[0]
	for _, secret := range rendered.Secrets[1:] {
		if secret.Store != first.Store || (first.Store.Kind == "SecretStore" && secret.Namespace != first.Namespace) {
			return nil, fmt.Errorf("the render reads through more than one secret store (%s and %s); planning several stores at once is not supported yet", first.Store.Name, secret.Store.Name)
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

// workspaceServiceUniques lists every service the workspace composes. A module
// or service that does not load is skipped: it is not one this environment
// renders either.
func workspaceServiceUniques(ctx context.Context, workspace *resources.Workspace) []string {
	var uniques []string
	for _, ref := range workspace.Modules {
		mod, err := workspace.LoadModuleFromReference(ctx, ref)
		if err != nil {
			continue
		}
		for _, svc := range mod.ServiceReferences {
			uniques = append(uniques, resources.ServiceUnique(mod.Name, svc.Name))
		}
	}
	return uniques
}

func printSecretsPlan(out io.Writer, environment string, rendered gitops.RenderedEnvironment, plan *deploysecrets.Plan) {
	fmt.Fprintf(out, "Environment %s — store %s\n", environment, plan.Store)
	fmt.Fprintf(out, "Rendered modules: %s\n", strings.Join(rendered.Modules, ", "))
	if len(rendered.Skipped) > 0 {
		fmt.Fprintf(out, "Skipped (rendered for another environment): %s\n", strings.Join(rendered.Skipped, ", "))
	}
	for _, note := range plan.Notes {
		fmt.Fprintf(out, "note: %s\n", note)
	}
	if len(plan.Credentials) > 0 {
		fmt.Fprintln(out, "\nFederation credentials")
		table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		for _, credential := range plan.Credentials {
			fmt.Fprintf(table, "  %s\t%s\t%s\n", credential.Action, credential.Credential, credential.Source)
		}
		_ = table.Flush()
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
			fmt.Fprintf(table, "  %s\t%s\t%s\n", property.Action, property.Property, property.Source)
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
}
