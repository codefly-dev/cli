package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/codefly-dev/cli/pkg/deploysecrets"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/spf13/cobra"
)

// Only declarations travel to the publisher. Existing values and generated
// material are resolved by the normal planner inside its authorized execution.
type secretsInputDocument struct {
	Schema      string
	Environment string
	Rendered    gitops.RenderedEnvironment
	Generators  []environments.EnvironmentSecretGenerator
	Services    []string
	MayBeEmpty  []string
}

const secretsInputSchema = "codefly.secrets-input/v1"

func exportSecretsInput(path string, env *environments.Environment, rendered gitops.RenderedEnvironment, services []string) error {
	document := secretsInputDocument{Schema: secretsInputSchema, Environment: env.Name,
		Rendered: rendered, Generators: env.ServiceSecrets.Generate, Services: services,
		MayBeEmpty: env.ServiceSecrets.MayBeEmpty}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("cannot exclusively create secret planner input file")
	}
	err = json.NewEncoder(file).Encode(document)
	closeErr := file.Close()
	return errors.Join(err, closeErr)
}

func readSecretsInput(input io.Reader, storeName string) (secretsInputDocument, error) {
	var document secretsInputDocument
	decoder := json.NewDecoder(io.LimitReader(input, 1024*1024+1))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&document) != nil || decoder.Decode(new(any)) != io.EOF ||
		document.Schema != secretsInputSchema || document.Environment == "" ||
		storeName == "" || len(document.Rendered.Secrets) == 0 {
		return document, errors.New("invalid secret planner input; contents withheld")
	}
	for _, secret := range document.Rendered.Secrets {
		if secret.Store.Kind != "ClusterSecretStore" || secret.Store.Name != storeName {
			return document, errors.New("planner input does not match the infrastructure-bound ClusterSecretStore")
		}
	}
	declarations := environments.EnvironmentServiceSecrets{
		SecretStore: document.Rendered.Secrets[0].Store, Generate: document.Generators,
	}
	if declarations.Validate() != nil {
		return document, errors.New("invalid secret generator declarations; contents withheld")
	}
	return document, nil
}

func runSecretsInput(ctx context.Context, cmd *cobra.Command) error {
	// The exported document cannot select its destination or impersonate a reader.
	// Infrastructure binds these from the live store and its existing writer Job.
	if os.Getenv("CODEFLY_AZURE_CREDENTIAL") != "workload" {
		return errors.New("secret planner input execution requires Azure workload identity")
	}
	if secretsExportInput != "" || secretsMetadataOnly || len(secretsModules) != 0 || len(secretsMintKeys) != 0 || secretsAllowMissing {
		return errors.New("secret planner input execution requires the complete input without mint or allow-missing")
	}
	file, err := os.Open(secretsInput)
	if err != nil {
		return errors.New("cannot open secret planner input")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1024*1024 {
		return errors.New("secret planner input must be a regular file of at most 1 MiB")
	}
	document, err := readSecretsInput(file, os.Getenv("CODEFLY_SECRETS_STORE_NAME"))
	if err != nil {
		return err
	}
	supplied, err := readSuppliedValues(secretsSupplied, document.Rendered, document.MayBeEmpty)
	if err != nil {
		return err
	}
	store, err := deploysecrets.AzurePublisherStore(ctx, os.Getenv("CODEFLY_SECRETS_AZURE_VAULT_URL"), os.Getenv("AZURE_TENANT_ID"))
	if err != nil {
		return err
	}
	plan, err := deploysecrets.Build(ctx, &deploysecrets.Inputs{
		Rendered: document.Rendered, Generators: document.Generators, Services: document.Services,
		MayBeEmpty: document.MayBeEmpty, Store: store, ReadPayloads: true, Supplied: supplied,
	})
	if err != nil {
		return err
	}
	printSecretsPlan(cmd.OutOrStdout(), document.Environment, document.Rendered, plan)
	return finishSecretsPlan(ctx, plan, store, secretsDryRun, false, func() bool { return secretsYes })
}

// readSuppliedValues reads the operator's values for keys nothing stores or
// generates: one JSON object of secret key to string. Errors never quote the
// file, which holds credentials.
func readSuppliedValues(path string, rendered gitops.RenderedEnvironment, mayBeEmpty []string) (map[string]string, error) {
	if path == "" {
		return nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open supplied values")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1024*1024 {
		return nil, errors.New("supplied values must be a regular file of at most 1 MiB")
	}
	var supplied map[string]string
	decoder := json.NewDecoder(io.LimitReader(file, 1024*1024+1))
	if decoder.Decode(&supplied) != nil || decoder.Decode(new(any)) != io.EOF || supplied == nil {
		return nil, errors.New("supplied values must be one JSON object of secret key to string; contents withheld")
	}
	if err := deploysecrets.ValidateSupplied(rendered, supplied, mayBeEmpty); err != nil {
		return nil, err
	}
	return supplied, nil
}
