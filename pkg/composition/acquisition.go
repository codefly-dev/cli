package composition

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	core "github.com/codefly-dev/core/composition"
)

const maxSelectionArtifactBytes int64 = 1 << 30

func contentDigest(data []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(data)) }

type AcquiredArtifact struct {
	Requirement core.Acquisition `json:"requirement"`
	Path        string           `json:"path"`
}

// Acquire downloads only Core's computed requirements. OCI is not treated as
// an archive URL: it needs a registry transport with digest-preserving semantics.
func (session *SelectionSession) Acquire(ctx context.Context, client *http.Client) ([]AcquiredArtifact, error) {
	var artifacts []AcquiredArtifact
	err := session.withResolved(ctx, func(snapshot *selectionSnapshot, resolved *core.ResolvedComposition) error {
		requirements := resolved.Record().Acquisitions
		for i := range requirements {
			requirement := &requirements[i]
			path, err := session.acquireArtifact(ctx, client, requirement.Artifact)
			if err != nil {
				return fmt.Errorf("%s/%s: %w", requirement.Target, requirement.Artifact.Name, err)
			}
			artifacts = append(artifacts, AcquiredArtifact{Requirement: *requirement, Path: path})
		}
		if err := resolved.CheckLocalInputs(); err != nil {
			return err
		}
		return session.unchanged(snapshot)
	})
	return artifacts, err
}

func artifactMatches(path, digest string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxSelectionArtifactBytes {
		return false
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, maxSelectionArtifactBytes+1)); err != nil {
		return false
	}
	return fmt.Sprintf("sha256:%x", hash.Sum(nil)) == digest
}

func (session *SelectionSession) acquireArtifact(ctx context.Context, client *http.Client, artifact core.ReleaseArtifact) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	location, err := url.Parse(artifact.URI)
	if err != nil || location.Scheme != "https" || location.Host == "" || location.User != nil || location.RawQuery != "" || location.Fragment != "" {
		return "", errors.New("artifact requires an HTTPS transport; no source or registry fallback is allowed")
	}
	// Core validates the digest before returning an acquisition. Validate again
	// here because it also determines a filesystem path.
	digest := strings.TrimPrefix(artifact.Digest, "sha256:")
	if len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" || !strings.HasPrefix(artifact.Digest, "sha256:") {
		return "", errors.New("invalid artifact digest")
	}
	directory := filepath.Join(session.Root, ".codefly", "selection-artifacts")
	if mkdirErr := os.MkdirAll(directory, 0o700); mkdirErr != nil {
		return "", mkdirErr
	}
	path := filepath.Join(directory, digest)
	if artifactMatches(path, artifact.Digest) {
		return path, nil
	}
	if client == nil {
		client = http.DefaultClient
	}
	transport := *client
	previousRedirect := client.CheckRedirect
	transport.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if request.URL.Scheme != "https" || request.URL.User != nil {
			return errors.New("artifact redirect must remain HTTPS without credentials")
		}
		if len(via) >= 10 {
			return errors.New("too many artifact redirects")
		}
		if previousRedirect != nil {
			return previousRedirect(request, via)
		}
		return nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, artifact.URI, nil)
	if err != nil {
		return "", err
	}
	response, err := transport.Do(request)
	if err != nil {
		return "", errors.New("artifact acquisition failed; verify connectivity and repository authorization")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("artifact acquisition returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > maxSelectionArtifactBytes {
		return "", errors.New("artifact exceeds size limit")
	}
	temporary, err := os.CreateTemp(directory, ".acquire-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = temporary.Close(); _ = os.Remove(temporary.Name()) }()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(temporary, hash), io.LimitReader(response.Body, maxSelectionArtifactBytes+1))
	if err != nil {
		return "", err
	}
	if n > maxSelectionArtifactBytes {
		return "", errors.New("artifact exceeds size limit")
	}
	if fmt.Sprintf("sha256:%x", hash.Sum(nil)) != artifact.Digest {
		return "", core.ErrDigestMismatch
	}
	if err := temporary.Sync(); err != nil {
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := os.Rename(temporary.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}
