package build

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/imageevidence"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func scannedImage(digest, service string) *builderv0.ImageSBOM {
	return &builderv0.ImageSBOM{
		Digest:   digest,
		Platform: "linux/amd64",
		Subjects: []*builderv0.ImageSubject{{Service: service, Role: "app"}},
		Bom: &agentv0.Bom{
			BomFormat: "CycloneDX", SpecVersion: "1.5", Version: 1,
			Metadata:   &agentv0.Metadata{Component: &agentv0.Component{Name: "image", Version: "v1"}},
			Components: []*agentv0.Component{{Name: "libc", Version: "2.39"}},
		},
	}
}

// Collecting evidence runs a container scanner and fails a build whose agent
// cannot serve image scope, which no released agent does yet, so the flag has to
// stay off until a caller asks for it.
func TestModuleCommandKeepsImageEvidenceOptIn(t *testing.T) {
	flag := ModuleCmd.Flags().Lookup("image-sbom")
	if flag == nil {
		t.Fatal("build module does not accept --image-sbom")
	}
	if flag.DefValue != "false" {
		t.Fatalf("--image-sbom defaults to %q, want off", flag.DefValue)
	}
	directory := ModuleCmd.Flags().Lookup("image-sbom-dir")
	if directory == nil {
		t.Fatal("build module does not accept --image-sbom-dir")
	}
	if want := filepath.Join(".codefly", "sbom", "image"); directory.DefValue != want {
		t.Fatalf("--image-sbom-dir defaults to %q, want %q", directory.DefValue, want)
	}
}

// A module builds each service through its own flow. Publishing one flow at a
// time would overwrite the index with the service built last, so every service's
// evidence has to reach the publisher together.
func TestPublishCollectedImageEvidenceKeepsEveryServiceInOneIndex(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, resources.WorkspaceConfigurationName),
		[]byte("name: evidence\nlayout: modules\n"), 0o600))
	workspace, err := resources.LoadWorkspaceFromDir(context.Background(), dir)
	require.NoError(t, err)

	previousDirectory := imageSBOMDirectory
	t.Cleanup(func() { imageSBOMDirectory = previousDirectory })
	imageSBOMDirectory = filepath.Join("evidence", "image")

	accounts := "sha256:" + strings.Repeat("a", 64)
	store := "sha256:" + strings.Repeat("b", 64)
	require.NoError(t, publishCollectedImageEvidence(workspace, map[string][]*builderv0.ImageSBOM{
		"users/accounts": {scannedImage(accounts, "users/accounts")},
		"users/store":    {scannedImage(store, "users/store")},
	}))

	payload, err := os.ReadFile(filepath.Join(dir, "evidence", "image", imageevidence.IndexFilename))
	require.NoError(t, err)
	var index imageevidence.Index
	require.NoError(t, json.Unmarshal(payload, &index))

	digests := map[string]bool{}
	for _, item := range index.Images {
		digests[item.Digest] = true
	}
	require.Len(t, index.Images, 2)
	require.True(t, digests[accounts] && digests[store], "the index lost a service's image: %+v", index.Images)
}
