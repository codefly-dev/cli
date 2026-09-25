package gitops

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/codefly-dev/cli/pkg/environments"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"google.golang.org/protobuf/proto"
)

// A producer can declare a secret value it exposes as a template over its own
// secret configuration values (core's ConfigurationValue.template): a Postgres
// agent's read-write connection is its role, address and database around a
// reference to its read-write password. A restricted render carries no value,
// so without the template the environment's store would have to hold the
// assembled string, typed by hand. With it, the consumer's ExternalSecret reads
// the producer's primitives from the producer's own remote keys and External
// Secrets assembles the value in the cluster: the store holds only primitives,
// and nothing here learns how the producer builds its values.

// deliveredTemplate is one producer's declared assembly of one secret key.
type deliveredTemplate struct {
	// producer is the module/service unique of the service that declared it.
	producer string
	template *basev0.ConfigurationValueTemplate
}

// renderTemplates maps a secret key exactly as a consumer reads it
// (CODEFLY__SERVICE_SECRET_CONFIGURATION__<MODULE>__<SERVICE>__<CONFIGURATION>__<KEY>)
// to the template its producer declared for it.
type renderTemplates map[string]deliveredTemplate

// collectRenderTemplates adds every template the deployed configurations
// declare. A key is named by its producer, so two declarations of one key can
// only come from the same producer, and they must agree.
func collectRenderTemplates(into renderTemplates, configurations map[string]*basev0.Configuration) error {
	uniques := make([]string, 0, len(configurations))
	for unique := range configurations {
		uniques = append(uniques, unique)
	}
	sort.Strings(uniques)
	for _, unique := range uniques {
		configuration := configurations[unique]
		for _, info := range configuration.GetInfos() {
			for _, value := range info.GetConfigurationValues() {
				if value.GetTemplate() == nil {
					continue
				}
				if err := resources.ValidateTemplatedConfigurationValue(value); err != nil {
					return fmt.Errorf("service %s: %w", unique, err)
				}
				if !referencesASecret(value.GetTemplate()) {
					// A template of literals only is a plain value under a
					// credential-named key; it would put that value in the tree.
					return fmt.Errorf("service %s: template of %s/%s references none of its secrets", unique, info.GetName(), value.GetKey())
				}
				origin := configuration.GetOrigin()
				if origin == resources.ConfigurationWorkspace || origin != unique {
					return fmt.Errorf("service %s exposes a configuration template under origin %q; a template assembles its producer's own values", unique, origin)
				}
				key := resources.ServiceSecretConfigurationKeyFromUnique(origin, info.GetName(), value.GetKey())
				delivered := deliveredTemplate{producer: origin, template: value.GetTemplate()}
				if prior, declared := into[key]; declared && (prior.producer != delivered.producer || !proto.Equal(prior.template, delivered.template)) {
					return fmt.Errorf("secret key %s is declared by two different templates", key)
				}
				into[key] = delivered
			}
		}
	}
	return nil
}

// templateVariable is what External Secrets can name as `.KEY` in a template:
// a Go template field name. Every core-derived secret key is one.
var templateVariable = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// externalSecretTemplateExpression translates a producer's template into an
// External Secrets v2 template expression (Go text/template with Sprig) over
// the secret keys of the producer's primitives, and returns those keys. It
// reproduces resources.EvaluateConfigurationValueTemplate byte for byte:
//
//   - a literal is copied as template text, or quoted as a template string
//     constant when it contains an action delimiter;
//   - a reference reads its primitive with `.KEY`, which fails on a missing key
//     under External Secrets' missingkey=error rather than assembling "";
//   - URL_USERINFO is urlquery (url.QueryEscape) with its "+" — the only byte it
//     emits for a space, since a literal plus is already %2B — written "%20".
func externalSecretTemplateExpression(delivered deliveredTemplate) (string, []string, error) {
	if err := resources.ValidateConfigurationValueTemplate(delivered.template); err != nil {
		return "", nil, err
	}
	var expression strings.Builder
	var primitives []string
	for _, segment := range delivered.template.GetSegments() {
		reference := segment.GetReference()
		if reference == nil {
			literal := segment.GetLiteral()
			if strings.Contains(literal, "{{") {
				expression.WriteString("{{ " + strconv.Quote(literal) + " }}")
			} else {
				expression.WriteString(literal)
			}
			continue
		}
		primitive := resources.ServiceSecretConfigurationKeyFromUnique(delivered.producer, reference.GetConfiguration(), reference.GetKey())
		if !templateVariable.MatchString(primitive) {
			return "", nil, fmt.Errorf("secret key %q cannot be named in an External Secrets template", primitive)
		}
		primitives = append(primitives, primitive)
		switch reference.GetEscape() {
		case basev0.ConfigurationValueEscape_CONFIGURATION_VALUE_ESCAPE_NONE:
			expression.WriteString("{{ ." + primitive + " }}")
		case basev0.ConfigurationValueEscape_CONFIGURATION_VALUE_ESCAPE_URL_USERINFO:
			expression.WriteString("{{ ." + primitive + ` | urlquery | replace "+" "%20" }}`)
		default:
			return "", nil, fmt.Errorf("secret key %s: escape %s has no External Secrets translation", primitive, reference.GetEscape())
		}
	}
	return expression.String(), primitives, nil
}

// producerSecretScope is the scope a producer's own secret keys resolve in —
// the same remote keys the producer's ExternalSecret reads, which is where
// `codefly deploy secrets` generates them.
func producerSecretScope(workspace, producer string) (environments.SecretScope, error) {
	module, service, ok := strings.Cut(producer, "/")
	if !ok || module == "" || service == "" {
		return environments.SecretScope{}, fmt.Errorf("producer %q is not a module/service unique", producer)
	}
	return environments.SecretScope{Workspace: workspace, Module: module, Service: service}, nil
}

func referencesASecret(template *basev0.ConfigurationValueTemplate) bool {
	for _, segment := range template.GetSegments() {
		if segment.GetReference() != nil {
			return true
		}
	}
	return false
}
