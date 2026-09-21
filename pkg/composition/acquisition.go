package composition

import (
	"context"
	"crypto/rand"
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
const artifactHTTPS = "https"

func contentDigest(data []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(data)) }

type AcquiredArtifact struct {
	Requirement core.Acquisition `json:"requirement"`
	Path        string           `json:"path"`
}

// Acquire downloads only Core's computed requirements, preserving each declared
// representation. Registry manifests never stand in for their referenced blobs.
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

func artifactMatches(ctx context.Context, path, digest string) bool {
	return artifactMatchesAt(ctx, nil, path, digest)
}

func artifactMatchesAt(ctx context.Context, directory *os.Root, path, digest string) bool {
	file, err := openInputFile(ctx, directory, path)
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxSelectionArtifactBytes {
		return false
	}
	hash := sha256.New()
	n, err := io.Copy(hash, &approvedInputReader{ctx: ctx, reader: io.LimitReader(file, maxSelectionArtifactBytes+1)})
	if err != nil || n > maxSelectionArtifactBytes {
		return false
	}
	return fmt.Sprintf("sha256:%x", hash.Sum(nil)) == digest
}

func (session *SelectionSession) acquireArtifact(ctx context.Context, client *http.Client, artifact core.ReleaseArtifact) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	location, err := url.Parse(artifact.URI)
	if err != nil || (location.Scheme != artifactHTTPS && location.Scheme != "oci") || location.Host == "" || location.User != nil || location.RawQuery != "" || location.Fragment != "" {
		return "", errors.New("artifact requires an HTTPS or OCI location without credentials, query or fragment")
	}
	if location.Scheme == "oci" {
		if _, err = artifactRepository(&artifact); err != nil {
			return "", err
		}
	}
	// Core validates the digest before returning an acquisition. Validate again
	// here because it also determines a filesystem path.
	digest := strings.TrimPrefix(artifact.Digest, "sha256:")
	if len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" || !strings.HasPrefix(artifact.Digest, "sha256:") {
		return "", errors.New("invalid artifact digest")
	}
	directory, err := session.openArtifactCache()
	if err != nil {
		return "", err
	}
	defer func() { _ = directory.Close() }()
	path := filepath.Join(directory.Name(), digest)
	if artifactMatchesAt(ctx, directory, digest, artifact.Digest) {
		return path, nil
	}
	reader, err := fetchArtifact(ctx, client, &artifact)
	if err != nil {
		return "", err
	}
	defer func() { _ = reader.Close() }()
	return retainArtifact(ctx, directory, artifact.Digest, reader)
}

func (session *SelectionSession) openArtifactCache() (*os.Root, error) {
	product, err := os.OpenRoot(session.Root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = product.Close() }()
	path := filepath.Join(".codefly", "selection-artifacts")
	if err = product.MkdirAll(path, 0o700); err != nil {
		return nil, err
	}
	return product.OpenRoot(path)
}

func artifactHTTPClient(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	transport := *client
	roundTripper := client.Transport
	if roundTripper == nil {
		roundTripper = http.DefaultTransport
	}
	transport.Transport = httpsArtifactTransport{roundTripper}
	previousRedirect := client.CheckRedirect
	transport.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if request.URL.Scheme != artifactHTTPS || request.URL.User != nil {
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
	return &transport
}

// Auth challenge/token requests must obey the same transport policy as bytes.
type httpsArtifactTransport struct{ http.RoundTripper }

func (transport httpsArtifactTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != artifactHTTPS || request.URL.User != nil {
		return nil, errors.New("artifact transport requires HTTPS without URL credentials")
	}
	return transport.RoundTripper.RoundTrip(request)
}

func fetchArtifact(ctx context.Context, client *http.Client, artifact *core.ReleaseArtifact) (io.ReadCloser, error) {
	transport := artifactHTTPClient(client)
	if strings.HasPrefix(artifact.URI, "oci:") {
		return fetchOCIArtifact(ctx, transport, artifact)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, artifact.URI, nil)
	if err != nil {
		return nil, err
	}
	response, err := transport.Do(request)
	if err != nil {
		return nil, errors.Join(ctx.Err(), errors.New("artifact acquisition failed; verify connectivity and repository authorization"))
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		return nil, fmt.Errorf("artifact acquisition returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > maxSelectionArtifactBytes {
		_ = response.Body.Close()
		return nil, errors.New("artifact exceeds size limit")
	}
	return response.Body, nil
}

func retainArtifact(ctx context.Context, directory *os.Root, digest string, reader io.Reader) (string, error) {
	name := strings.TrimPrefix(digest, "sha256:")
	if len(name) != 64 || strings.Trim(name, "0123456789abcdef") != "" || digest != "sha256:"+name {
		return "", errors.New("invalid artifact cache digest")
	}
	temporaryName := ".acquire-" + rand.Text()
	temporary, err := directory.OpenFile(temporaryName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	defer func() { _ = temporary.Close(); _ = directory.Remove(temporaryName) }()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(temporary, hash), &approvedInputReader{ctx: ctx, reader: io.LimitReader(reader, maxSelectionArtifactBytes+1)})
	if err != nil {
		return "", err
	}
	if n > maxSelectionArtifactBytes {
		return "", errors.New("artifact exceeds size limit")
	}
	if fmt.Sprintf("sha256:%x", hash.Sum(nil)) != digest {
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
	if err := directory.Rename(temporaryName, name); err != nil {
		return "", err
	}
	return filepath.Join(directory.Name(), name), nil
}
