package composition

import (
	"bytes"
	"errors"
	"io"
	"os"
	"testing"

	core "github.com/codefly-dev/core/composition"
	"github.com/stretchr/testify/require"
)

// Bytes a render built are published by digest, rooted against garbage
// collection, served back exactly, and named as a runtime release artifact.
func TestRuntimeArtifactPublicationRealRegistry(t *testing.T) {
	if os.Getenv("CODEFLY_COMPOSITION_OCI_QUALIFY") != "1" {
		t.Skip("set CODEFLY_COMPOSITION_OCI_QUALIFY=1 for a disposable TLS Distribution registry")
	}
	endpoint, client, _, _, collect := startArtifactRegistry(t)
	data := []byte("canonical frontend archive bytes")
	artifact, err := PublishRuntimeArtifact(t.Context(), endpoint+"/platform/frontend-bundles", "frontend", CanonicalArchiveMediaType, data, client)
	require.NoError(t, err)
	require.Equal(t, core.ArtifactRuntime, artifact.Purpose)
	require.Equal(t, contentDigest(data), artifact.Digest)
	require.Equal(t, "oci://"+endpoint+"/platform/frontend-bundles@"+artifact.Digest, artifact.URI)
	again, err := PublishRuntimeArtifact(t.Context(), endpoint+"/platform/frontend-bundles", "frontend", CanonicalArchiveMediaType, data, client)
	require.NoError(t, err)
	require.Equal(t, artifact, again)
	collect()
	reader, err := fetchArtifact(t.Context(), client, &artifact)
	require.NoError(t, err)
	served, err := io.ReadAll(reader)
	require.NoError(t, errors.Join(err, reader.Close()))
	require.True(t, bytes.Equal(data, served), "a retained runtime artifact survives garbage collection")

	_, err = PublishRuntimeArtifact(t.Context(), endpoint+"/platform/frontend-bundles:tag", "frontend", CanonicalArchiveMediaType, data, client)
	require.Error(t, err)
	_, err = PublishRuntimeArtifact(t.Context(), endpoint+"/platform/frontend-bundles", "frontend", "", data, client)
	require.Error(t, err)
}
