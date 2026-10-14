package composition

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"

	corecomposition "github.com/codefly-dev/core/composition"
)

// CanonicalArchiveMediaType is the representation of a canonical archive
// (core's CanonicalArchive): the media type a frontend bundle's payload and
// release artifact declare.
const CanonicalArchiveMediaType = "application/x-tar"

// ArtifactCacheDir is the artifact cache of a workspace: the digest-named
// files an acquisition retains and consults before any fetch, and the one
// read-only source a host's native bundle adapter reads a bundle from.
func ArtifactCacheDir(workspaceDir string) string {
	return filepath.Join(workspaceDir, ".codefly", "selection-artifacts")
}

// EnsureArtifactCacheDir creates the workspace's artifact cache when it does
// not exist yet and returns its absolute path: a host opens it read-only when
// it starts, before any bundle is retained there.
func EnsureArtifactCacheDir(workspaceDir string) (string, error) {
	session := &SelectionSession{Root: workspaceDir}
	cache, err := session.openArtifactCache()
	if err != nil {
		return "", err
	}
	defer func() { _ = cache.Close() }()
	return filepath.Abs(cache.Name())
}

// RetainCanonicalArchive archives a directory in core's canonical form and
// retains the bytes in the workspace's artifact cache under their digest,
// through the writer every acquisition uses (written beside, digest-verified,
// renamed into place), so the cache stays content-addressed: a corrupt or
// altered file fails its digest wherever it is read, and is written again
// here. It returns the digest and the retained path; an archive already
// retained is read, not rewritten.
func RetainCanonicalArchive(ctx context.Context, workspaceDir, root string) (string, string, error) {
	archive, digest, err := corecomposition.CanonicalArchive(root)
	if err != nil {
		return "", "", err
	}
	session := &SelectionSession{Root: workspaceDir}
	cache, err := session.openArtifactCache()
	if err != nil {
		return "", "", err
	}
	defer func() { _ = cache.Close() }()
	name := strings.TrimPrefix(digest, "sha256:")
	if artifactMatchesAt(ctx, cache, name, digest) {
		return digest, filepath.Join(cache.Name(), name), nil
	}
	path, err := retainArtifact(ctx, cache, digest, bytes.NewReader(archive))
	if err != nil {
		return "", "", err
	}
	return digest, path, nil
}
