package methodpolicy

import (
	"strings"
	"testing"

	"github.com/codefly-dev/core/composition"
)

func TestATemplateIsSpelledOneVariablePerSegment(t *testing.T) {
	for template, want := range map[string]string{
		"/v1/x/{name}":          "/v1/x/{name}",
		"/v1/x/{name=*}":        "/v1/x/{name}",
		"/v1/x/{name=**}":       "/v1/x/{name=**}",
		"/v1/x/{name=a/*}:verb": "/v1/x/a/{name}:verb",
		"/v1/x/{name=a/*/b/**}": "/v1/x/a/{name}/b/{name=**}",
		"/v1/{a}/{b=*}/{c=d/*}": "/v1/{a}/{b}/d/{c}",
		"/v1/x/{broken":         "/v1/x/{broken",
	} {
		if got := plainTemplate(template); got != want {
			t.Errorf("%s: %s, want %s", template, got, want)
		}
	}
}

func TestMultiPackagePoliciesRequireCatalog(t *testing.T) {
	_, err := endpointServes(nil, "accounts", &composition.ProvidedAPIContract{Endpoint: "connect"})
	if err == nil || !strings.Contains(err.Error(), "endpoint accounts/connect has no single protobuf package") || !strings.Contains(err.Error(), composition.APIContractCatalogFileName) {
		t.Fatalf("missing listener inventory: %v", err)
	}
}
