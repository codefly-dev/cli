package gitops

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"

	"github.com/Masterminds/sprig/v3"
	"github.com/codefly-dev/cli/pkg/environments"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const (
	producerUnique  = "runtime/store"
	connectionKey   = "CODEFLY__SERVICE_SECRET_CONFIGURATION__RUNTIME__STORE__POSTGRES__READ_WRITE_CONNECTION"
	readOnlyKey     = "CODEFLY__SERVICE_SECRET_CONFIGURATION__RUNTIME__STORE__POSTGRES__READ_ONLY_CONNECTION"
	writerPassword  = "CODEFLY__SERVICE_SECRET_CONFIGURATION__RUNTIME__STORE__POSTGRES__POSTGRES_READ_WRITE_PASSWORD"
	readerPassword  = "CODEFLY__SERVICE_SECRET_CONFIGURATION__RUNTIME__STORE__POSTGRES__POSTGRES_READ_ONLY_PASSWORD"
	audienceKey     = "CODEFLY__WORKSPACE_SECRET_CONFIGURATION__RUNTIME_EXECUTION__AUTHORITY_AUDIENCE"
	storeRemoteKey  = "example-runtime-store"
	tasksRemoteKey  = "example-runtime-tasks"
	inClusterTarget = "@store.example-runtime.svc.cluster.local:5432/runtime"
)

// connectionTemplate is the shape a Postgres producer declares: literals around
// a reference to one of its own passwords.
func connectionTemplate(role, passwordKey string) *basev0.ConfigurationValueTemplate {
	return &basev0.ConfigurationValueTemplate{Segments: []*basev0.ConfigurationValueTemplateSegment{
		{Content: &basev0.ConfigurationValueTemplateSegment_Literal{Literal: "postgresql://" + role + ":"}},
		{Content: &basev0.ConfigurationValueTemplateSegment_Reference{Reference: &basev0.ConfigurationValueReference{
			Configuration: "postgres",
			Key:           passwordKey,
			Escape:        basev0.ConfigurationValueEscape_CONFIGURATION_VALUE_ESCAPE_URL_USERINFO,
		}}},
		{Content: &basev0.ConfigurationValueTemplateSegment_Literal{Literal: inClusterTarget}},
	}}
}

// producerConfiguration is what a Postgres producer's restricted Deploy exposes:
// its two connections as keys without values, each with its assembly.
func producerConfiguration() map[string]*basev0.Configuration {
	return map[string]*basev0.Configuration{producerUnique: {
		Origin: producerUnique,
		Infos: []*basev0.ConfigurationInformation{{
			Name: "postgres",
			ConfigurationValues: []*basev0.ConfigurationValue{
				{Key: "read-only-connection", Secret: true, Template: connectionTemplate("codefly_runtime_ro", "POSTGRES_READ_ONLY_PASSWORD")},
				{Key: "read-write-connection", Secret: true, Template: connectionTemplate("codefly_runtime_rw", "POSTGRES_READ_WRITE_PASSWORD")},
			},
		}},
	}}
}

// stagingLikeSecrets resolves every key the way a real cell's store does: one
// remote entry per service, one property per key.
func stagingLikeSecrets() *environments.EnvironmentServiceSecrets {
	return &environments.EnvironmentServiceSecrets{
		SecretStore: environments.EnvironmentSecretStoreReference{Name: "cell-secrets", Kind: "ClusterSecretStore"},
		Defaults:    &environments.EnvironmentSecretRemoteRef{Key: "example-{module}-{service}", Property: "{key}"},
	}
}

func consumerProjection(t *testing.T, keys ...string) *externalSecret {
	t.Helper()
	templates := renderTemplates{}
	require.NoError(t, collectRenderTemplates(templates, producerConfiguration()))
	scope := unitScope{Workspace: "example", Module: "runtime", Namespace: "example-runtime", Templates: templates}
	projection, err := serviceSecretProjection(scope, "tasks", stagingLikeSecrets(), keys)
	require.NoError(t, err)
	return projection
}

// The consumer's ExternalSecret reads only primitives — its own plain keys from
// its own remote entry, and the producer's passwords from the producer's remote
// entry, where the producer's own ExternalSecret reads them — and assembles the
// connections in the cluster. The assembled keys are read from nowhere.
func TestConsumerExternalSecretAssemblesProducerTemplatesFromPrimitives(t *testing.T) {
	projection := consumerProjection(t, audienceKey, connectionKey, readOnlyKey)

	remotes := map[string]externalSecretRemote{}
	for _, entry := range projection.Spec.Data {
		remotes[entry.SecretKey] = entry.RemoteRef
	}
	require.Equal(t, map[string]externalSecretRemote{
		audienceKey:    {Key: tasksRemoteKey, Property: audienceKey},
		writerPassword: {Key: storeRemoteKey, Property: writerPassword},
		readerPassword: {Key: storeRemoteKey, Property: readerPassword},
	}, remotes)

	template := projection.Spec.Target.Template
	require.NotNil(t, template)
	require.Equal(t, "v2", template.EngineVersion)
	require.Equal(t, "Merge", template.MergePolicy)
	require.Equal(t, map[string]string{
		connectionKey: "postgresql://codefly_runtime_rw:{{ ." + writerPassword + ` | urlquery | replace "+" "%20" }}` + inClusterTarget,
		readOnlyKey:   "postgresql://codefly_runtime_ro:{{ ." + readerPassword + ` | urlquery | replace "+" "%20" }}` + inClusterTarget,
	}, template.Data)
}

// A consumer that reads no templated key gets the ExternalSecret it always got.
func TestConsumerWithoutTemplatedKeysIsUnchanged(t *testing.T) {
	projection := consumerProjection(t, audienceKey)
	require.Nil(t, projection.Spec.Target.Template)
	require.Equal(t, []externalSecretData{{SecretKey: audienceKey, RemoteRef: externalSecretRemote{Key: tasksRemoteKey, Property: audienceKey}}}, projection.Spec.Data)
}

// The expression External Secrets evaluates reproduces core's reference
// semantics byte for byte, and the assembled connection gives back the exact
// password — for generated passwords and for ones carrying every URL
// delimiter, a space, a plus, a percent, quotes, template delimiters and
// non-ASCII. It is evaluated the way External Secrets' v2 engine evaluates it:
// text/template with Sprig and missingkey=error over the fetched keys.
func TestExternalSecretTemplateRoundTripsPasswords(t *testing.T) {
	delivered := deliveredTemplate{producer: producerUnique, template: connectionTemplate("codefly_runtime_rw", "POSTGRES_READ_WRITE_PASSWORD")}
	expression, primitives, err := externalSecretTemplateExpression(delivered)
	require.NoError(t, err)
	require.Equal(t, []string{writerPassword}, primitives)
	engine, err := template.New("eso").Option("missingkey=error").Funcs(sprig.TxtFuncMap()).Parse(expression)
	require.NoError(t, err)

	for _, password := range []string{
		"4f3c2a1b0e9d8c7b6a5f4e3d2c1b0a99",
		"p@ss:w/rd?#[]",
		"with space+plus",
		"100%literal",
		`quote"'back\slash`,
		"{{ .inject }}",
		"$&,;=!*()",
		"unicodé-ß-密码",
	} {
		var assembled strings.Builder
		require.NoError(t, engine.Execute(&assembled, map[string]string{writerPassword: password}))
		want, err := resources.EvaluateConfigurationValueTemplate(delivered.template, func(configuration, key string) (string, bool) {
			return password, configuration == "postgres" && key == "POSTGRES_READ_WRITE_PASSWORD"
		})
		require.NoError(t, err)
		require.Equal(t, want, assembled.String())

		parsed, err := url.Parse(assembled.String())
		require.NoError(t, err)
		got, _ := parsed.User.Password()
		require.Equal(t, password, got)
		require.Equal(t, "codefly_runtime_rw", parsed.User.Username())
		require.Equal(t, "store.example-runtime.svc.cluster.local:5432", parsed.Host)
	}

	// A primitive the store does not hold fails the assembly instead of
	// producing a connection with an empty password.
	require.Error(t, engine.Execute(&strings.Builder{}, map[string]string{}))
}

func TestExternalSecretTemplateQuotesLiteralDelimiters(t *testing.T) {
	delivered := deliveredTemplate{producer: producerUnique, template: &basev0.ConfigurationValueTemplate{Segments: []*basev0.ConfigurationValueTemplateSegment{
		{Content: &basev0.ConfigurationValueTemplateSegment_Literal{Literal: `a{{ .x }}"b`}},
		{Content: &basev0.ConfigurationValueTemplateSegment_Reference{Reference: &basev0.ConfigurationValueReference{Configuration: "postgres", Key: "K"}}},
	}}}
	expression, primitives, err := externalSecretTemplateExpression(delivered)
	require.NoError(t, err)
	engine, err := template.New("eso").Option("missingkey=error").Funcs(sprig.TxtFuncMap()).Parse(expression)
	require.NoError(t, err)
	var assembled strings.Builder
	require.NoError(t, engine.Execute(&assembled, map[string]string{primitives[0]: "v"}))
	require.Equal(t, `a{{ .x }}"bv`, assembled.String())
}

// The projected ExternalSecret passes the promotable tree's checks: its
// template carries delimiters and sits under credential-named keys, and both
// are admitted exactly there.
func TestRenderAcceptsAssembledExternalSecret(t *testing.T) {
	projection := consumerProjection(t, audienceKey, connectionKey)
	encoded, err := yaml.Marshal(projection)
	require.NoError(t, err)
	manifests, _, err := decodeYAML("external-secret.yaml", encoded)
	require.NoError(t, err)
	require.NoError(t, validateManifest(manifests[0], nil, true))

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "external-secret.yaml"), encoded, 0o644))
	_, err = validateTree(root, &RenderOptions{Promotable: true})
	require.NoError(t, err)
}

// The admission is narrow: a template value with no action under a
// credential-named key is a credential in the tree, a delimiter anywhere else
// in an ExternalSecret is still an unresolved placeholder, and any other kind
// of manifest carrying one is still refused as a whole file.
func TestRenderStillRefusesLiteralsAndPlaceholdersOutsideTheTemplate(t *testing.T) {
	// A credential-named template key is admitted while it computes...
	computed := consumerProjection(t, connectionKey)
	computed.Spec.Target.Template.Data[writerPassword] = "{{ ." + writerPassword + " }}"
	encoded, err := yaml.Marshal(computed)
	require.NoError(t, err)
	manifests, _, err := decodeYAML("external-secret.yaml", encoded)
	require.NoError(t, err)
	require.NoError(t, validateManifest(manifests[0], nil, true))
	// ...and refused as a literal.
	literal := consumerProjection(t, connectionKey)
	literal.Spec.Target.Template.Data[writerPassword] = "plaintext"
	encoded, err = yaml.Marshal(literal)
	require.NoError(t, err)
	manifests, _, err = decodeYAML("external-secret.yaml", encoded)
	require.NoError(t, err)
	require.ErrorContains(t, validateManifest(manifests[0], nil, true), "credential value")

	elsewhere := consumerProjection(t, connectionKey)
	elsewhere.Metadata.Name = "{{ .name }}"
	encoded, err = yaml.Marshal(elsewhere)
	require.NoError(t, err)
	manifests, _, err = decodeYAML("external-secret.yaml", encoded)
	require.NoError(t, err)
	require.ErrorContains(t, validateManifest(manifests[0], nil, true), "unresolved placeholder")

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.yaml"), []byte(
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: c\n  namespace: n\ndata:\n  K: \"{{ .x }}\"\n"), 0o644))
	_, err = validateTree(root, &RenderOptions{Promotable: true})
	require.ErrorContains(t, err, "unresolved placeholder")
}

func TestCollectRenderTemplatesRefusesMalformedDeclarations(t *testing.T) {
	literalOnly := producerConfiguration()
	literalOnly[producerUnique].Infos[0].ConfigurationValues[0].Template = &basev0.ConfigurationValueTemplate{
		Segments: []*basev0.ConfigurationValueTemplateSegment{{Content: &basev0.ConfigurationValueTemplateSegment_Literal{Literal: "postgresql://x@y/z"}}},
	}
	require.ErrorContains(t, collectRenderTemplates(renderTemplates{}, literalOnly), "references none of its secrets")

	foreign := producerConfiguration()
	foreign[producerUnique].Origin = "runtime/other"
	require.ErrorContains(t, collectRenderTemplates(renderTemplates{}, foreign), "its producer's own values")

	valued := producerConfiguration()
	valued[producerUnique].Infos[0].ConfigurationValues[0].Value = "postgresql://leaked"
	require.ErrorContains(t, collectRenderTemplates(renderTemplates{}, valued), "both a value and a template")

	templates := renderTemplates{}
	require.NoError(t, collectRenderTemplates(templates, producerConfiguration()))
	changed := producerConfiguration()
	changed[producerUnique].Infos[0].ConfigurationValues[1].Template = connectionTemplate("someone_else", "POSTGRES_READ_WRITE_PASSWORD")
	require.ErrorContains(t, collectRenderTemplates(templates, changed), "two different templates")
}

func TestEnvironmentTemplateCannotAlsoAssembleAProducerKey(t *testing.T) {
	templates := renderTemplates{}
	require.NoError(t, collectRenderTemplates(templates, producerConfiguration()))
	secrets := stagingLikeSecrets()
	secrets.Services = map[string]environments.EnvironmentServiceSecretMapping{"tasks": {
		RemoteKeys: map[string]environments.EnvironmentSecretRemoteRef{connectionKey: {Key: "x"}},
		Template:   &environments.EnvironmentSecretTemplate{EngineVersion: "v2", MergePolicy: "Merge", Data: map[string]string{connectionKey: "{{ .x }}"}},
	}}
	scope := unitScope{Workspace: "example", Module: "runtime", Namespace: "example-runtime", Templates: templates}
	_, err := serviceSecretProjection(scope, "tasks", secrets, []string{connectionKey})
	require.ErrorContains(t, err, "also templated by the environment")
}
