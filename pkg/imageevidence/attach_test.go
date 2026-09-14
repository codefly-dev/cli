package imageevidence

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/stretchr/testify/require"
)

// registryAt runs a real OCI registry in the test process. Attachment is a
// registry protocol, not a local file format, so the behaviour under test only
// exists against something that speaks it.
func registryAt(t *testing.T, referrers bool) string {
	t.Helper()
	server := httptest.NewServer(registry.New(registry.WithReferrersSupport(referrers)))
	t.Cleanup(server.Close)
	return strings.TrimPrefix(server.URL, "http://")
}

// pushed puts an image in the registry and returns the reference a build would
// have used for it together with the digest the push resolved to.
func pushed(t *testing.T, host, repository string) (string, string) {
	t.Helper()
	reference := host + "/" + repository + ":build"
	tag, err := name.ParseReference(reference)
	require.NoError(t, err)
	image, err := random.Image(256, 1)
	require.NoError(t, err)
	require.NoError(t, remote.Write(tag, image))
	digest, err := image.Digest()
	require.NoError(t, err)
	return reference, digest.String()
}

func documentsFor(t *testing.T, evidence ...*builderv0.ImageSBOM) []Document {
	t.Helper()
	documents, err := Documents(evidence)
	require.NoError(t, err)
	return documents
}

// manifestOf reads back an attached artifact. The annotations live on the
// manifest itself, which is what a consumer resolves a referrer to.
func manifestOf(t *testing.T, host, repository, digest string) *v1.Manifest {
	t.Helper()
	reference, err := name.NewDigest(host + "/" + repository + "@" + digest)
	require.NoError(t, err)
	artifact, err := remote.Image(reference)
	require.NoError(t, err)
	manifest, err := artifact.Manifest()
	require.NoError(t, err)
	return manifest
}

func referrersOf(t *testing.T, host, repository, digest string) []v1.Descriptor {
	t.Helper()
	subject, err := name.NewDigest(host + "/" + repository + "@" + digest)
	require.NoError(t, err)
	index, err := remote.Referrers(subject)
	require.NoError(t, err)
	manifest, err := index.IndexManifest()
	require.NoError(t, err)
	return manifest.Manifests
}

// The guarantee a registry attachment adds over the published directory: a
// consumer holding only a digest finds the inventory through the registry.
func TestAttachMakesEvidenceDiscoverableFromTheImageDigestAlone(t *testing.T) {
	host := registryAt(t, true)
	reference, digest := pushed(t, host, "web/api")

	documents := documentsFor(t, scanned(digest, "linux/amd64",
		&builderv0.ImageSubject{Service: "web/api", Role: "app", Reference: reference}))
	attachments, err := Attach(context.Background(), documents)
	require.NoError(t, err)
	require.Len(t, attachments, 1)
	require.Equal(t, digest, attachments[0].Subject)

	referrers := referrersOf(t, host, "web/api", digest)
	require.Len(t, referrers, 1, "the pushed image carries no evidence")
	require.Equal(t, MediaType, referrers[0].ArtifactType,
		"a consumer filtering the referrers API for CycloneDX would not find it")

	manifest := manifestOf(t, host, "web/api", attachments[0].Digest)
	require.Equal(t, "linux/amd64", manifest.Annotations[AnnotationPlatform])
	require.NotNil(t, manifest.Subject, "the artifact is not bound to any image")
	require.Equal(t, digest, manifest.Subject.Digest.String())

	artifact, err := name.NewDigest(host + "/web/api@" + attachments[0].Digest)
	require.NoError(t, err)
	attached, err := remote.Image(artifact)
	require.NoError(t, err)
	layers, err := attached.Layers()
	require.NoError(t, err)
	require.Len(t, layers, 1)
	content, err := layers[0].Uncompressed()
	require.NoError(t, err)
	defer content.Close()
	payload, err := io.ReadAll(content)
	require.NoError(t, err)
	require.Equal(t, documents[0].Payload, payload, "the attached document is not the published one")
}

// Rebuilding and re-pushing unchanged evidence must not accumulate a referrer
// per build, or a long-lived image collects one copy of its inventory per run.
func TestAttachingTheSameEvidenceTwiceLeavesOneReferrer(t *testing.T) {
	host := registryAt(t, true)
	reference, digest := pushed(t, host, "web/api")
	documents := documentsFor(t, scanned(digest, "linux/amd64",
		&builderv0.ImageSubject{Service: "web/api", Reference: reference}))

	first, err := Attach(context.Background(), documents)
	require.NoError(t, err)
	second, err := Attach(context.Background(), documents)
	require.NoError(t, err)
	require.Equal(t, first, second, "the same evidence produced a different artifact")

	require.Len(t, referrersOf(t, host, "web/api", digest), 1)
}

// #651 forbids reporting complete coverage against a stale or missing image.
// The subject is read from the registry, so evidence for a digest that is not
// there fails rather than attaching to nothing.
func TestAttachRefusesADigestTheRegistryDoesNotHave(t *testing.T) {
	host := registryAt(t, true)
	reference, _ := pushed(t, host, "web/api")
	stale := "sha256:" + strings.Repeat("a", 64)

	_, err := Attach(context.Background(), documentsFor(t, scanned(stale, "linux/amd64",
		&builderv0.ImageSubject{Service: "web/api", Reference: reference})))
	require.Error(t, err)
	require.Contains(t, err.Error(), stale)
}

// Evidence that names no image cannot be attached anywhere, and silently
// skipping it would report complete coverage for an image nothing covers.
func TestAttachRefusesEvidenceThatNamesNoImage(t *testing.T) {
	_, err := Attach(context.Background(), documentsFor(t,
		scanned("sha256:"+strings.Repeat("b", 64), "linux/amd64",
			&builderv0.ImageSubject{Service: "web/api"})))
	require.Error(t, err)
	require.Contains(t, err.Error(), "names no image reference")
}

// Each platform of a multi-architecture image is its own scan, so each has to
// reach the registry as its own attachment rather than one overwriting the other.
func TestAttachCoversEveryPlatformSeparately(t *testing.T) {
	host := registryAt(t, true)
	reference, amd64 := pushed(t, host, "web/api")
	_, arm64 := pushed(t, host, "web/api")
	require.NotEqual(t, amd64, arm64)

	attachments, err := Attach(context.Background(), documentsFor(t,
		scanned(amd64, "linux/amd64", &builderv0.ImageSubject{Service: "web/api", Reference: reference}),
		scanned(arm64, "linux/arm64", &builderv0.ImageSubject{Service: "web/api", Reference: reference}),
	))
	require.NoError(t, err)
	require.Len(t, attachments, 2)

	covered := map[string]string{}
	for _, attachment := range attachments {
		require.Len(t, referrersOf(t, host, "web/api", attachment.Subject), 1,
			"%s carries no evidence of its own", attachment.Platform)
		manifest := manifestOf(t, host, "web/api", attachment.Digest)
		covered[attachment.Subject] = manifest.Annotations[AnnotationPlatform]
	}
	require.Equal(t, map[string]string{amd64: "linux/amd64", arm64: "linux/arm64"}, covered)
}

// One digest claimed by two services is stored once; the attachment has to name
// both, or the registry copy loses associations the published index keeps.
func TestAttachRecordsEveryServiceSharingADigest(t *testing.T) {
	host := registryAt(t, true)
	reference, digest := pushed(t, host, "web/api")

	attachments, err := Attach(context.Background(), documentsFor(t,
		scanned(digest, "linux/amd64", &builderv0.ImageSubject{Service: "web/api", Reference: reference}),
		scanned(digest, "linux/amd64", &builderv0.ImageSubject{Service: "billing/worker", Reference: reference}),
	))
	require.NoError(t, err)
	require.Len(t, attachments, 1, "one digest is attached to once")

	require.Len(t, referrersOf(t, host, "web/api", digest), 1)
	manifest := manifestOf(t, host, "web/api", attachments[0].Digest)
	require.Equal(t, "billing/worker,web/api", manifest.Annotations[AnnotationServices])
}

// Two agents can scan one digest and return genuinely different inventories,
// which Documents keeps apart rather than collapsing. Both have to reach the
// registry: attaching only one would publish one service's inventory as the
// other's coverage, the same over-claim the published directory avoids.
func TestAttachKeepsDivergentInventoriesOfOneImageApart(t *testing.T) {
	host := registryAt(t, true)
	reference, digest := pushed(t, host, "web/api")

	documents := documentsFor(t,
		divergent(digest, "openssl", &builderv0.ImageSubject{Service: "web/api", Reference: reference}),
		divergent(digest, "zlib", &builderv0.ImageSubject{Service: "billing/worker", Reference: reference}),
	)
	require.Len(t, documents, 2, "divergent scans collapsed before reaching the registry")

	attachments, err := Attach(context.Background(), documents)
	require.NoError(t, err)
	require.Len(t, attachments, 2)
	require.NotEqual(t, attachments[0].Digest, attachments[1].Digest,
		"one inventory overwrote the other in the registry")

	referrers := referrersOf(t, host, "web/api", digest)
	require.Len(t, referrers, 2, "the image carries only one of its two inventories")
}

// A digest that two services pushed to different repositories needs evidence in
// each: a consumer pulling from one repository can only query that one.
func TestAttachReachesEveryRepositoryTheImageWasPushedTo(t *testing.T) {
	host := registryAt(t, true)
	reference, digest := pushed(t, host, "web/api")
	mirror := host + "/billing/worker:build"
	tag, err := name.ParseReference(mirror)
	require.NoError(t, err)
	image, err := remote.Image(mustDigest(t, reference, digest))
	require.NoError(t, err)
	require.NoError(t, remote.Write(tag, image))

	attachments, err := Attach(context.Background(), documentsFor(t,
		scanned(digest, "linux/amd64",
			&builderv0.ImageSubject{Service: "web/api", Reference: reference},
			&builderv0.ImageSubject{Service: "billing/worker", Reference: mirror}),
	))
	require.NoError(t, err)
	require.Len(t, attachments, 2)

	require.Len(t, referrersOf(t, host, "web/api", digest), 1)
	require.Len(t, referrersOf(t, host, "billing/worker", digest), 1)
}

// Registries that predate OCI 1.1 serve no referrers API. The evidence still has
// to land, through the fallback tag, or attachment would only work on the newest
// registries while reporting success everywhere.
func TestAttachWorksOnARegistryWithoutTheReferrersAPI(t *testing.T) {
	host := registryAt(t, false)
	reference, digest := pushed(t, host, "web/api")

	_, err := Attach(context.Background(), documentsFor(t, scanned(digest, "linux/amd64",
		&builderv0.ImageSubject{Service: "web/api", Reference: reference})))
	require.NoError(t, err)

	referrers := referrersOf(t, host, "web/api", digest)
	require.Len(t, referrers, 1, "the fallback tag carries no evidence")
	require.Equal(t, MediaType, referrers[0].ArtifactType)
}

func mustDigest(t *testing.T, reference, digest string) name.Digest {
	t.Helper()
	parsed, err := name.ParseReference(reference)
	require.NoError(t, err)
	return parsed.Context().Digest(digest)
}
