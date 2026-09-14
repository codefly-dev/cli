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
	index, err := Publish(directory, documents, true)
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
	_, err = Publish(forwardDirectory, forward, true)
	require.NoError(t, err)
	_, err = Publish(reversedDirectory, reversed, true)
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
	index, err := Publish(directory, documents, true)
	require.NoError(t, err)
	require.Len(t, index.Images, 1)
	require.FileExists(t, filepath.Join(directory, IndexFilename))
}

// The directory is reused across builds. A superseded document left behind lets
// a consumer that globs the directory rather than reading the index attribute a
// previous release's image to this one.
func TestPublishRemovesEvidenceSupersededByALaterBuild(t *testing.T) {
	superseded := "sha256:" + strings.Repeat("a", 64)
	current := "sha256:" + strings.Repeat("b", 64)
	directory := t.TempDir()

	first, err := Documents([]*builderv0.ImageSBOM{
		scanned(superseded, "linux/amd64", &builderv0.ImageSubject{Service: "web/api"}),
	})
	require.NoError(t, err)
	_, err = Publish(directory, first, true)
	require.NoError(t, err)
	require.FileExists(t, filepath.Join(directory, first[0].Name))

	second, err := Documents([]*builderv0.ImageSBOM{
		scanned(current, "linux/amd64", &builderv0.ImageSubject{Service: "web/api"}),
	})
	require.NoError(t, err)
	index, err := Publish(directory, second, true)
	require.NoError(t, err)

	require.Len(t, index.Images, 1)
	require.FileExists(t, filepath.Join(directory, second[0].Name))
	require.NoFileExists(t, filepath.Join(directory, first[0].Name), "a superseded release's evidence survived into this one")
}

// Pruning must be bounded to this mechanism's own output: the directory can be
// shared, and a source SBOM named after its module and service is not ours to
// delete.
func TestPublishLeavesFilesItDidNotWriteAlone(t *testing.T) {
	directory := t.TempDir()
	foreign := filepath.Join(directory, "web--api.cdx.json")
	require.NoError(t, os.WriteFile(foreign, []byte("{}\n"), 0o644))
	notes := filepath.Join(directory, "notes.txt")
	require.NoError(t, os.WriteFile(notes, []byte("keep me\n"), 0o644))

	documents, err := Documents([]*builderv0.ImageSBOM{
		scanned("sha256:"+strings.Repeat("c", 64), "linux/amd64", &builderv0.ImageSubject{Service: "web/api"}),
	})
	require.NoError(t, err)
	_, err = Publish(directory, documents, true)
	require.NoError(t, err)

	require.FileExists(t, foreign, "a source SBOM sharing the directory was deleted")
	require.FileExists(t, notes)
}

// Writing the index before pruning, and pruning last, is what keeps a failed
// run honest: the previous release stays internally consistent instead of
// becoming an index that points at documents which were never written.
func TestPublishLeavesThePreviousReleaseIntactWhenADocumentCannotBeWritten(t *testing.T) {
	previousDigest := "sha256:" + strings.Repeat("a", 64)
	directory := t.TempDir()

	previous, err := Documents([]*builderv0.ImageSBOM{
		scanned(previousDigest, "linux/amd64", &builderv0.ImageSubject{Service: "web/api"}),
	})
	require.NoError(t, err)
	_, err = Publish(directory, previous, true)
	require.NoError(t, err)

	next, err := Documents([]*builderv0.ImageSBOM{
		scanned("sha256:"+strings.Repeat("b", 64), "linux/amd64", &builderv0.ImageSubject{Service: "web/api"}),
		scanned("sha256:"+strings.Repeat("c", 64), "linux/amd64", &builderv0.ImageSubject{Service: "web/api"}),
	})
	require.NoError(t, err)
	// A directory where the second document belongs makes its write fail.
	require.NoError(t, os.Mkdir(filepath.Join(directory, next[1].Name), 0o755))

	_, err = Publish(directory, next, true)
	require.Error(t, err, "a document that cannot be written must fail the publication")

	manifest, err := os.ReadFile(filepath.Join(directory, IndexFilename))
	require.NoError(t, err)
	var index Index
	require.NoError(t, json.Unmarshal(manifest, &index))
	require.Len(t, index.Images, 1)
	require.Equal(t, previousDigest, index.Images[0].Digest, "the index advertised a release that was never published")
	require.FileExists(t, filepath.Join(directory, index.Images[0].Path), "the index points at a document that is gone")
}

// A build that does not push scans an image that exists only in the local
// daemon, whose digest resolves in no registry. Without this the artifact is
// indistinguishable from evidence for a shipped image.
func TestPublishRecordsWhetherDigestsAreRegistryBacked(t *testing.T) {
	documents, err := Documents([]*builderv0.ImageSBOM{
		scanned("sha256:"+strings.Repeat("d", 64), "linux/amd64", &builderv0.ImageSubject{Service: "web/api"}),
	})
	require.NoError(t, err)

	local, err := Publish(t.TempDir(), documents, false)
	require.NoError(t, err)
	require.False(t, local.RegistryBacked)

	pushed, err := Publish(t.TempDir(), documents, true)
	require.NoError(t, err)
	require.True(t, pushed.RegistryBacked)
}
