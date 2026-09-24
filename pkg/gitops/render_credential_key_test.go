package gitops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
)

const modelOutputTokensKey = "CODEFLY__WORKSPACE_CONFIGURATION__MODEL_INSTALLATION__MAX_OUTPUT_TOKENS"

// The render guard classifies configuration names through core's
// resources.IsSensitiveKey, the same function orchestration promotes secrets
// with. A model output-token limit is a count, not a credential (core#645), so a
// render carrying it must not be refused; every credential name must still be.
func TestRenderGuardClassifiesConfigurationNamesThroughCore(t *testing.T) {
	public := []string{modelOutputTokensKey, "MAX_OUTPUT_TOKENS", "TOKENS_LIMIT", "LOG_LEVEL"}
	credential := []string{
		"ACCESS_TOKEN", "CODEFLY_INTERNAL_TOKEN", "TOKEN", "MAX_TOKEN", "TOKENS",
		"JWT_SECRET", "WEBHOOK_SECRET", "CLIENT_SECRET", "DATABASE_PASSWORD", "API_KEY",
	}
	for _, key := range public {
		if resources.IsSensitiveKey(key) {
			t.Fatalf("precondition: core classifies %s as sensitive", key)
		}
		for name, value := range configurationShapes(key) {
			if err := inspectValue(value, nil, false); err != nil {
				t.Errorf("%s as %s: refused: %v", key, name, err)
			}
			if err := inspectTemplatedValue(value, nil); err != nil {
				t.Errorf("%s as %s (templated): refused: %v", key, name, err)
			}
		}
	}
	for _, key := range credential {
		for name, value := range configurationShapes(key) {
			err := inspectValue(value, nil, false)
			if err == nil || !strings.Contains(err.Error(), "credential value") {
				t.Errorf("%s as %s: want credential refusal, got %v", key, name, err)
			}
			err = inspectTemplatedValue(value, nil)
			if name != "env entry" && (err == nil || !strings.Contains(err.Error(), "credential value")) {
				t.Errorf("%s as %s (templated): want credential refusal, got %v", key, name, err)
			}
		}
	}
}

func configurationShapes(key string) map[string]map[string]any {
	return map[string]map[string]any{
		"ConfigMap data key": {"data": map[string]any{key: "value"}},
		"Secret stringData":  {"stringData": map[string]any{key: "value"}},
		"env entry":          {"env": []any{map[string]any{"name": key, "value": "value"}}},
	}
}

// Schema field names keep the camelCase credential fragments core cannot see,
// but core's wider configuration markers never reach ordinary manifest structure.
func TestRenderGuardSchemaFieldsKeepCamelCaseCredentialsAndStructure(t *testing.T) {
	for _, field := range []string{"privateKey", "accessKey", "clientSecret", "password", "bearerToken", "token"} {
		err := inspectValue(map[string]any{"spec": map[string]any{field: "value"}}, nil, false)
		if err == nil || !strings.Contains(err.Error(), "credential value") {
			t.Errorf("%s: want credential refusal, got %v", field, err)
		}
	}
	structure := map[string]any{
		"spec": map[string]any{
			"sessionAffinity": "ClientIP",
			"volumes":         []any{map[string]any{"secret": map[string]any{"secretName": "tls"}}},
		},
	}
	if err := inspectValue(structure, nil, false); err != nil {
		t.Fatalf("ordinary structure refused: %v", err)
	}
}

// End to end: the exact render that was refused with
// "data.CODEFLY__WORKSPACE_CONFIGURATION__MODEL_INSTALLATION__MAX_OUTPUT_TOKENS
// contains credential value".
func TestRenderAcceptsModelOutputTokenLimitAndRefusesInlineToken(t *testing.T) {
	configMap := func(key string) string {
		return pinnedDeployment + `---
apiVersion: v1
kind: ConfigMap
metadata:
  name: workspace
data:
  ` + key + `: "4096"
`
	}
	render := func(manifests string) error {
		_, err := RenderOwnedTree(context.Background(), &RenderOptions{
			Destination: filepath.Join(t.TempDir(), "owned"), Module: "payments", Environment: "production",
		}, func(ctx context.Context, root string) error {
			return os.WriteFile(filepath.Join(root, "manifests.yaml"), []byte(manifests), 0o644)
		})
		return err
	}
	if err := render(configMap(modelOutputTokensKey)); err != nil {
		t.Fatalf("model output-token limit refused: %v", err)
	}
	for _, key := range []string{"CODEFLY_INTERNAL_TOKEN", "ACCESS_TOKEN", "JWT_SECRET"} {
		err := render(configMap(key))
		if err == nil || !strings.Contains(err.Error(), "credential value") {
			t.Errorf("%s: want credential refusal, got %v", key, err)
		}
	}
}

// A carrier name embeds module, service and endpoint names; the guard must
// classify the configuration key it carries, never that structure. These are
// the exact names v0.1.167 refused while rendering real modules: the endpoint
// addresses of a service called auth-gateway.
func TestRenderGuardIgnoresCarrierStructure(t *testing.T) {
	public := []string{
		"CODEFLY__ENDPOINT__SAAS__AUTH_GATEWAY__GRPC__GRPC",
		"CODEFLY__ENDPOINT__SAAS__AUTH_GATEWAY__REST__REST",
		"CODEFLY__SELF_ENDPOINT__SAAS__AUTH_GATEWAY__GRPC__GRPC",
		"CODEFLY__SERVICE_CONFIGURATION__SAAS__AUTH_GATEWAY__IDENTITY__AUTHORITY_ISSUER",
		"AUTHORITY_ISSUER",
	}
	credential := []string{
		"JWT_SECRET", "WEBHOOK_SECRET", "ACCESS_TOKEN",
		"CODEFLY__SERVICE_CONFIGURATION__SAAS__AUTH_GATEWAY__IDENTITY__CLIENT_SECRET",
		"CODEFLY__SERVICE_CONFIGURATION__SAAS__AUTH_GATEWAY__IDENTITY__ACCESS_TOKEN",
		"CODEFLY__SERVICE_SECRET_CONFIGURATION__SAAS__AUTH_GATEWAY__IDENTITY__AUTHORITY_ISSUER",
	}
	for _, key := range public {
		for name, value := range configurationShapes(key) {
			if err := inspectValue(value, nil, false); err != nil {
				t.Errorf("%s as %s: refused: %v", key, name, err)
			}
			if err := inspectTemplatedValue(value, nil); err != nil {
				t.Errorf("%s as %s (templated): refused: %v", key, name, err)
			}
		}
	}
	for _, key := range credential {
		for name, value := range configurationShapes(key) {
			err := inspectValue(value, nil, false)
			if err == nil || !strings.Contains(err.Error(), "credential value") {
				t.Errorf("%s as %s: want credential refusal, got %v", key, name, err)
			}
			err = inspectTemplatedValue(value, nil)
			if name != "env entry" && (err == nil || !strings.Contains(err.Error(), "credential value")) {
				t.Errorf("%s as %s (templated): want credential refusal, got %v", key, name, err)
			}
		}
	}

	// End to end, the ConfigMap shape the render refused.
	manifests := pinnedDeployment + `---
apiVersion: v1
kind: ConfigMap
metadata:
  name: endpoints
data:
  CODEFLY__ENDPOINT__SAAS__AUTH_GATEWAY__GRPC__GRPC: "auth-gateway.saas.svc.cluster.local:8080"
  CODEFLY__ENDPOINT__SAAS__AUTH_GATEWAY__REST__REST: "http://auth-gateway.saas.svc.cluster.local:8081"
`
	_, err := RenderOwnedTree(context.Background(), &RenderOptions{
		Destination: filepath.Join(t.TempDir(), "owned"), Module: "payments", Environment: "production",
	}, func(ctx context.Context, root string) error {
		return os.WriteFile(filepath.Join(root, "manifests.yaml"), []byte(manifests), 0o644)
	})
	if err != nil {
		t.Fatalf("endpoint addresses refused: %v", err)
	}
}
