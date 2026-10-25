package deploy

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

func TestCompositionRenderChecksLaterModuleBeforeFirstRender(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	root := t.TempDir()
	t.Chdir(root)
	previous := gitOpsEnv
	gitOpsEnv = "staging"
	t.Cleanup(func() { gitOpsEnv = previous })
	descriptor, err := proto.Marshal(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{{Name: proto.String("api.proto"), Package: proto.String("bad"), Syntax: proto.String("proto3"), Service: []*descriptorpb.ServiceDescriptorProto{{Name: proto.String("Api"), Method: []*descriptorpb.MethodDescriptorProto{{Name: proto.String("Unmarked")}}}}}}})
	require.NoError(t, err)
	files := map[string]string{
		resources.WorkspaceConfigurationName: "name: preflight\nlayout: modules\nmodules:\n  - name: alpha\n  - name: zulu\nenvironments:\n  - name: staging\n",
		"modules/alpha/module.codefly.yaml":  "kind: module\nname: alpha\n",
		"modules/zulu/module.codefly.yaml":   "kind: module\nname: zulu\n",
		"modules/zulu/module.package.codefly.yaml": fmt.Sprintf(`schema: codefly/module-package/v2
kind: module-package
id: example/zulu
version: 0.1.0
minimum-codefly-version: ">=0.3.32 <1.0.0"
artifact-roots: [contracts]
services:
  - name: api
    endpoints: [grpc]
    api-contracts:
      - endpoint: grpc
        kind: protobuf
        package: bad
        path: contracts/api.binpb
        digest: sha256:%x
contracts:
  composition: ">=2.0.0 <3.0.0"
`, sha256.Sum256(descriptor)),
		"modules/zulu/contracts/api.binpb": string(descriptor),
		// Zulu holds every method it publishes to a policy wherever it is
		// delivered, so its unmarked method refuses even with no host.
		"modules/zulu/module.contract.codefly.yaml": "schema: codefly/module-contract/v1\nprincipal: zulu\nnamespaces: []\nqueues: []\nscope_ceilings: []\nbindings: []\ndestinations: []\nrequire_method_policies: true\n",
	}
	for name, content := range files {
		path := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0644))
	}
	// Alpha would enter the coordinator first. The later invalid policy must
	// be reported before even that first module's readiness/render pipeline.
	err = renderComposition(t.Context())
	require.ErrorContains(t, err, "/bad.Api/Unmarked declares no permission policy")
	require.ErrorContains(t, err, "require_method_policies")
	_, err = os.Stat(filepath.Join(root, "deployments"))
	require.ErrorIs(t, err, os.ErrNotExist)
}
