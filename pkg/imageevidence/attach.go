package imageevidence

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// Annotations recorded on an attached document. The platform is what
// distinguishes the evidence of one architecture from another when both are
// attached to the same multi-architecture image, and the services are what a
// consumer reads to learn whose image it is holding.
const (
	AnnotationTitle    = "org.opencontainers.image.title"
	AnnotationPlatform = "dev.codefly.image.platform"
	AnnotationServices = "dev.codefly.image.services"
)

// Attachment records one document attached to one image in a registry.
type Attachment struct {
	// Subject is the digest of the image the evidence refers to.
	Subject string
	// Platform is the OCI platform the attached inventory covers.
	Platform string
	// Digest is the digest of the artifact manifest holding the document.
	Digest string
}

// Attach publishes every document into the registry as an OCI 1.1 referrers
// attachment on the image it was scanned from. It is the registry-attachment
// form of image evidence: a consumer holding nothing but an image digest
// discovers the inventory through the referrers API of the repository it pulled
// from, without the release directory Publish writes having been carried
// anywhere.
//
// An image is attached to once per repository its evidence names, so a digest
// claimed by services that pushed it to different repositories carries evidence
// in each of them.
//
// Every attachment is made against the image already in the registry: the
// subject descriptor is read from the registry rather than assembled locally, so
// a digest that was never pushed, or that no longer resolves, fails here instead
// of producing an attachment that refers to nothing.
//
// insecure reports which registries are reached without TLS, so evidence
// resolves a registry the same way the push that put the image there did. A nil
// predicate resolves every registry over HTTPS.
//
// The attachments made before a failure are returned with the error. They are
// already in the registry, and reporting the failure as though nothing landed
// would understate the coverage that exists.
func Attach(ctx context.Context, documents []Document, insecure func(registry string) bool, options ...remote.Option) ([]Attachment, error) {
	options = append([]remote.Option{
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
	}, options...)

	attachments := make([]Attachment, 0, len(documents))
	for index := range documents {
		document := &documents[index]
		repositories, err := repositories(document, insecure)
		if err != nil {
			return attachments, err
		}
		for _, repository := range repositories {
			attachment, err := attach(document, repository, options)
			if err != nil {
				return attachments, err
			}
			attachments = append(attachments, attachment)
		}
	}
	return attachments, nil
}

func attach(document *Document, repository name.Repository, options []remote.Option) (Attachment, error) {
	image := repository.Digest(document.Digest)
	subject, err := remote.Head(image, options...)
	if err != nil {
		return Attachment{}, fmt.Errorf("cannot resolve %s to attach image evidence to: %w", image, err)
	}
	artifact, err := artifact(document, subject)
	if err != nil {
		return Attachment{}, err
	}
	digest, err := artifact.Digest()
	if err != nil {
		return Attachment{}, fmt.Errorf("cannot digest the image evidence of %s: %w", image, err)
	}
	if err := remote.Write(repository.Digest(digest.String()), artifact, options...); err != nil {
		return Attachment{}, fmt.Errorf("cannot attach image evidence to %s: %w", image, err)
	}
	return Attachment{
		Subject:  document.Digest,
		Platform: document.Platform,
		Digest:   digest.String(),
	}, nil
}

// artifact builds the OCI artifact carrying one document. The document is the
// single layer; the manifest names the image it describes as its subject, which
// is what makes it discoverable through the referrers API.
//
// Nothing in it varies from run to run — no timestamp is recorded — so the same
// evidence attached twice is byte-identical and the registry stores one
// referrer rather than accumulating a copy per build.
func artifact(document *Document, subject *v1.Descriptor) (v1.Image, error) {
	built, err := mutate.Append(empty.Image, mutate.Addendum{
		Layer:       static.NewLayer(document.Payload, MediaType),
		Annotations: map[string]string{AnnotationTitle: document.Name},
	})
	if err != nil {
		return nil, fmt.Errorf("cannot build the image evidence artifact for %s: %w", document.Digest, err)
	}
	built = mutate.MediaType(built, types.OCIManifestSchema1)
	// An artifact declares its type through the config media type: the OCI 1.1
	// spec falls back to it when artifactType is absent, which is how a
	// referrers query filters for CycloneDX rather than for every referrer.
	built = mutate.ConfigMediaType(built, MediaType)
	annotated, ok := mutate.Annotations(built, annotations(document)).(v1.Image)
	if !ok {
		return nil, fmt.Errorf("cannot annotate the image evidence artifact for %s", document.Digest)
	}
	referring, ok := mutate.Subject(annotated, *subject).(v1.Image)
	if !ok {
		return nil, fmt.Errorf("cannot bind the image evidence artifact for %s to its image", document.Digest)
	}
	return referring, nil
}

func annotations(document *Document) map[string]string {
	annotations := map[string]string{AnnotationTitle: document.Name}
	if document.Platform != "" {
		annotations[AnnotationPlatform] = document.Platform
	}
	services := make([]string, 0, len(document.Associations))
	for _, association := range document.Associations {
		if association.Service != "" {
			services = append(services, association.Service)
		}
	}
	if len(services) > 0 {
		sort.Strings(services)
		// Several services can claim one image, each carrying its own name.
		annotations[AnnotationServices] = strings.Join(slices.Compact(services), ",")
	}
	return annotations
}

// repositories resolves the registry repositories a document's evidence belongs
// in, from the references of the services it covers. Evidence with no reference
// to attach to is a failure rather than a skip: a build cannot report complete
// coverage for an image it could not name.
func repositories(document *Document, insecure func(registry string) bool) ([]name.Repository, error) {
	var repositories []name.Repository
	seen := map[string]bool{}
	for _, association := range document.Associations {
		if association.Reference == "" {
			continue
		}
		reference, err := name.ParseReference(association.Reference)
		if err != nil {
			return nil, fmt.Errorf("cannot parse the image reference %q of %s: %w", association.Reference, document.Digest, err)
		}
		repository := reference.Context()
		if seen[repository.Name()] {
			continue
		}
		seen[repository.Name()] = true
		if insecure != nil && insecure(repository.RegistryStr()) {
			repository, err = name.NewRepository(repository.Name(), name.Insecure)
			if err != nil {
				return nil, fmt.Errorf("cannot resolve the insecure registry of %q: %w", association.Reference, err)
			}
		}
		repositories = append(repositories, repository)
	}
	if len(repositories) == 0 {
		return nil, fmt.Errorf("image evidence for %s names no image reference to attach it to", document.Digest)
	}
	slices.SortFunc(repositories, func(a, b name.Repository) int { return strings.Compare(a.Name(), b.Name()) })
	return repositories, nil
}
