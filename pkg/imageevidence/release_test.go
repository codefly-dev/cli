package imageevidence

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/stretchr/testify/require"
)

func published(t *testing.T, evidence ...*builderv0.ImageSBOM) (string, Index) {
	t.Helper()
	documents, err := Documents(evidence)
	require.NoError(t, err)
	directory := t.TempDir()
	index, err := Publish(directory, documents)
	require.NoError(t, err)
	return directory, index
}

func TestPublishWritesEveryDocumentAndAnIndexThatFindsThem(t *testing.T) {
	first := "sha256:" + strings.Repeat("a", 64)
	second := "sha256:" + strings.Repeat("b", 64)

	directory, index := published(t,
		scanned(first, "linux/amd64", &builderv0.ImageSubject{Service: "web/api", Role: "app"}),
		scanned(second, "linux/arm64", &builderv0.ImageSubject{Service: "web/api", Role: "migration"}),
	)

	require.Len(t, index.Images, 2)
	for _, item := range index.Images {
		payload, err := os.ReadFile(filepath.Join(directory, item.Path))
		require.NoError(t, err, "index points at a document that was not written")
		require.Contains(t, string(payload), "CycloneDX")
		require.Equal(t, MediaType, item.MediaType)
		require.True(t, strings.HasPrefix(item.Digest, "sha256:"))
	}

	manifest, err := os.ReadFile(filepath.Join(directory, IndexFilename))
	require.NoError(t, err)
	var decoded Index
	require.NoError(t, json.Unmarshal(manifest, &decoded))
	require.Equal(t, index, decoded, "the written index must be what Publish reported")
}

// A consumer holding an image reference resolves its evidence by digest, so the
// digest a document was scanned from has to survive into the manifest.
func TestPublishIndexResolvesEvidenceByDigestAndPlatform(t *testing.T) {
	digest := "sha256:" + strings.Repeat("c", 64)

	_, index := published(t,
		scanned(digest, "linux/amd64", &builderv0.ImageSubject{Service: "web/api", Role: "app"}),
		scanned(digest, "linux/arm64", &builderv0.ImageSubject{Service: "web/api", Role: "app"}),
	)

	require.Len(t, index.Images, 2, "each platform of a multi-architecture image is its own scan")
	platforms := map[string]string{}
	for _, item := range index.Images {
		require.Equal(t, digest, item.Digest)
		platforms[item.Platform] = item.Path
	}
	require.Len(t, platforms, 2)
	require.NotEqual(t, platforms["linux/amd64"], platforms["linux/arm64"])
}

func TestPublishKeepsEveryServiceAssociationOnASharedDigest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("d", 64)

	_, index := published(t,
		scanned(digest, "linux/amd64", &builderv0.ImageSubject{Service: "web/api", Role: "app"}),
		scanned(digest, "linux/amd64", &builderv0.ImageSubject{Service: "billing/worker", Role: "app"}),
	)

	require.Len(t, index.Images, 1, "one digest is stored once")
	require.ElementsMatch(t, []Association{
		{Service: "web/api", Role: "app"},
		{Service: "billing/worker", Role: "app"},
	}, index.Images[0].Associations)
}

// The same build publishing twice must produce a byte-identical manifest, so a
// release artifact can be compared rather than only inspected.
func TestPublishOrdersTheIndexDeterministically(t *testing.T) {
	first := "sha256:" + strings.Repeat("e", 64)
	second := "sha256:" + strings.Repeat("0", 64)

	forward, err := Documents([]*builderv0.ImageSBOM{
		scanned(first, "linux/amd64", &builderv0.ImageSubject{Service: "web/api"}),
		scanned(second, "linux/amd64", &builderv0.ImageSubject{Service: "web/api"}),
	})
	require.NoError(t, err)
	reversed, err := Documents([]*builderv0.ImageSBOM{
		scanned(second, "linux/amd64", &builderv0.ImageSubject{Service: "web/api"}),
		scanned(first, "linux/amd64", &builderv0.ImageSubject{Service: "web/api"}),
	})
	require.NoError(t, err)

	forwardDirectory, reversedDirectory := t.TempDir(), t.TempDir()
	_, err = Publish(forwardDirectory, forward)
	require.NoError(t, err)
	_, err = Publish(reversedDirectory, reversed)
	require.NoError(t, err)

	forwardManifest, err := os.ReadFile(filepath.Join(forwardDirectory, IndexFilename))
	require.NoError(t, err)
	reversedManifest, err := os.ReadFile(filepath.Join(reversedDirectory, IndexFilename))
	require.NoError(t, err)
	require.Equal(t, string(forwardManifest), string(reversedManifest))
}

func TestPublishCreatesTheEvidenceDirectory(t *testing.T) {
	documents, err := Documents([]*builderv0.ImageSBOM{
		scanned("sha256:"+strings.Repeat("f", 64), "linux/amd64", &builderv0.ImageSubject{Service: "web/api"}),
	})
	require.NoError(t, err)

	directory := filepath.Join(t.TempDir(), "sbom", "image")
	index, err := Publish(directory, documents)
	require.NoError(t, err)
	require.Len(t, index.Images, 1)
	require.FileExists(t, filepath.Join(directory, IndexFilename))
}
